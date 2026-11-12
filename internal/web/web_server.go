package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jemyzhang/serial-over-socket-go/config"
	"github.com/jemyzhang/serial-over-socket-go/internal/serial"
	"github.com/jemyzhang/serial-over-socket-go/pkg/version"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Non-browser clients (sos-client, curl, ...) don't send
			// Origin at all; allow them.
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		// Only accept upgrades from pages served by this server itself.
		// This kills the "malicious website drives your serial ports"
		// attack (WebSocket is not covered by the browser same-origin
		// policy; this check is the only defence).
		// NOTE: if a reverse proxy fronts this server it must pass the
		// original Host through (proxy_set_header Host $host), otherwise
		// legitimate browser clients will be rejected here.
		return strings.EqualFold(u.Host, r.Host)
	},
}

// historyIDSeq disambiguates history entry IDs generated within the same
// nanosecond (two clients can add commands concurrently).
var historyIDSeq uint64

func nextHistoryID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&historyIDSeq, 1))
}

type WebSocketClient struct {
	ID         string
	Conn       *websocket.Conn
	RemoteAddr string
	ReadyTime  time.Time  // 就绪时间
	writeMu    sync.Mutex // WebSocket写入互斥锁

	// Mutable per-client state below is accessed from both the message
	// handling goroutine and the serial-data broadcast goroutines;
	// always go through the stateMu-guarded helpers.
	stateMu sync.RWMutex
	portID  string // selected port, "" = none
	ready   bool   // 是否已准备好接收数据
	rows    int    // 终端行数
	cols    int    // 终端列数

}

// Client state helpers (stateMu-guarded).
func (c *WebSocketClient) getPortID() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.portID
}
func (c *WebSocketClient) setPortID(id string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.portID = id
}
func (c *WebSocketClient) isReady() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.ready
}
func (c *WebSocketClient) setReady(ready bool, rows, cols int) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.ready = ready
	c.rows = rows
	c.cols = cols
}

type CommandEntry struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Timestamp int64  `json:"timestamp"`
}

type WebServer struct {
	config    *config.Config
	configMu  sync.RWMutex // guards all reads/writes of config fields
	serialMgr *serial.Manager
	clients   map[string]*WebSocketClient
	mu        sync.RWMutex
	// Dedicated mux + server instead of the global DefaultServeMux so the
	// server can be embedded (e.g. in a desktop GUI process) and shut
	// down gracefully.
	mux        *http.ServeMux
	httpServer *http.Server

	commandHistory []CommandEntry
	historyMu      sync.RWMutex
	// historyChanged is atomic, not historyMu-guarded: WS handler
	// goroutines set it while the interval loop takes it, and
	// holding historyMu across a cloud upload would serialize
	// every history operation for the upload's duration. The
	// interval loop owns the flag through tryHistoryUpload
	// (swap-take, restore on failure); backupSoonLoop never
	// clears it. The flag is only a hint (a redundant upload
	// costs nothing).
	historyChanged atomic.Bool
	// Deletion tombstones, guarded by historyMu (see
	// history_tombstones.go): ids deleted locally that no merge
	// may bring back from a cloud backup or an import.
	historyTombstoneIDs []string
	historyTombstoneSet map[string]bool
	// Coalesced "cloud copy is stale after a delete" signals for
	// backupSoonLoop (buffered 1; further signals while one is
	// pending are dropped).
	backupSoonCh chan struct{}
	// uploadMu serializes history uploads (snapshot + PUT +
	// prune) across the interval loop, manual backup, and
	// backup-soon: without it, a slow earlier upload could
	// finish last and overwrite a newer snapshot's file.
	// lastBackupMillis, guarded by uploadMu, is the millisecond
	// timestamp of the last backup filename issued; it only
	// moves forward, so filenames stay unique even when two
	// serialized uploads land in the same millisecond.
	uploadMu         sync.Mutex
	lastBackupMillis int64
	templates        *template.Template
	templatesFS      embed.FS
	staticFS         embed.FS
	assetStamp       string
}

func NewWebServer(cfg *config.Config, serialMgr *serial.Manager, templatesFS, staticFS embed.FS) (*WebServer, error) {
	ws := &WebServer{
		config:         cfg,
		serialMgr:      serialMgr,
		clients:        make(map[string]*WebSocketClient),
		commandHistory: make([]CommandEntry, 0),
		backupSoonCh:   make(chan struct{}, 1),
		templatesFS:    templatesFS,
		staticFS:       staticFS,
		assetStamp:     computeAssetStamp(staticFS),
	}

	// 加载嵌入的模板
	templates, err := template.ParseFS(templatesFS, "web/templates/*.html")
	if err != nil {
		log.Printf("Failed to load embedded templates: %v", err)
		// 使用默认模板
		templates = template.New("default")
	}
	ws.templates = templates

	// Register as a status handler to receive port change notifications
	serialMgr.RegisterStatusHandler(ws.handlePortStatusChange)

	return ws, nil
}

func (ws *WebServer) Start() error {
	// 0. Load deletion tombstones first, so the local file is
	// filtered by them too (see history_tombstones.go).
	ws.loadHistoryTombstones()

	// 1. Load from local store immediately
	ws.loadLocalHistory()

	// 2. Attempt remote WebDAV sync in background
	go ws.autoBackupLoop()

	// 3. Coalesced post-delete cloud uploads (best-effort; the
	// tombstones, not this loop, are the no-resurrection guarantee).
	go ws.backupSoonLoop()

	// 设置路由 — on a dedicated mux (not the global DefaultServeMux).
	mux := http.NewServeMux()
	mux.HandleFunc("/", ws.handleIndex)
	mux.HandleFunc("/ws", ws.handleWebSocket)
	mux.HandleFunc("/api/ports", ws.handleAPIPorts)
	mux.HandleFunc("/api/port/connect", ws.handleAPIPortConnect)
	mux.HandleFunc("/api/port/disconnect", ws.handleAPIPortDisconnect)
	mux.HandleFunc("/api/port/write", ws.handleAPIPortWrite)
	mux.HandleFunc("/api/port/config", ws.handleAPIPortConfig)
	mux.HandleFunc("/api/servers", ws.handleAPIServers)
	mux.HandleFunc("/api/server/connect", ws.handleAPIServerConnect)

	// WebDAV proxy – forwards requests to the *saved* WebDAV server (avoids
	// CORS). All endpoints use the server-side saved config and never
	// request-supplied credentials, so the server can't be used as an
	// open relay against arbitrary WebDAV servers.
	mux.HandleFunc("/api/webdav/test", ws.handleWebDAVTest)
	mux.HandleFunc("/api/webdav/list", ws.handleWebDAVList)
	mux.HandleFunc("/api/webdav/upload", ws.handleWebDAVUpload)
	mux.HandleFunc("/api/webdav/download", ws.handleWebDAVDownload)
	mux.HandleFunc("/api/webdav/delete", ws.handleWebDAVDelete)

	// PWA files - must be served from root scope
	mux.HandleFunc("/manifest.json", ws.handleManifest)
	mux.HandleFunc("/sw.js", ws.handleServiceWorker)

	// 静态文件服务 - 使用嵌入的文件系统
	staticSubFS, err := fs.Sub(ws.staticFS, "web/static")
	if err != nil {
		log.Printf("Failed to create static sub-filesystem: %v", err)
	} else {
		fileServer := http.FileServer(http.FS(staticSubFS))
		mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	}
	ws.mux = mux

	addr := ws.getServerAddr()
	log.Printf("Web server starting on http://%s", addr)

	ws.httpServer = &http.Server{Addr: addr, Handler: mux}
	return ws.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server, disconnects all serial ports
// and closes all WebSocket clients.
func (ws *WebServer) Shutdown(ctx context.Context) error {
	// 1. Stop serial ports first so no new data arrives.
	for _, portID := range ws.serialMgr.ListPorts() {
		_ = ws.serialMgr.DisconnectPort(portID)
	}

	// 2. Close all WebSocket clients.
	ws.mu.Lock()
	clients := make([]*WebSocketClient, 0, len(ws.clients))
	for _, c := range ws.clients {
		clients = append(clients, c)
	}
	ws.mu.Unlock()
	for _, c := range clients {
		c.Conn.Close()
	}

	// 3. Stop accepting new HTTP connections.
	if ws.httpServer != nil {
		return ws.httpServer.Shutdown(ctx)
	}
	return nil
}

func (ws *WebServer) handleManifest(w http.ResponseWriter, r *http.Request) {
	data, err := ws.staticFS.ReadFile("web/static/manifest.json")
	if err != nil {
		log.Printf("Failed to read manifest.json: %v", err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(data)
}

func (ws *WebServer) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	data, err := ws.staticFS.ReadFile("web/static/sw.js")
	if err != nil {
		log.Printf("Failed to read sw.js: %v", err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	// Service Worker must not be cached aggressively
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Write(data)
}

// computeAssetStamp derives a short content hash of app.js — the
// one WebUI asset that changes between builds. index.html appends
// it to the app.js URL (?v=<stamp>): caches keyed by exact URL —
// the service worker's cache-first Cache Storage above all — can
// then never serve a previous build's app.js under a new URL, no
// matter how stale the installed SW is. (A stale app.js once made
// new GUI features silently never engage in the desktop webview,
// whose installed SW also never surfaced its update banner.)
func computeAssetStamp(staticFS embed.FS) string {
	data, err := staticFS.ReadFile("web/static/js/app.js")
	if err != nil {
		return "0"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

func (ws *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := map[string]interface{}{
		"Title":      "Serial Over Socket - Web Interface",
		"Ports":      ws.serialMgr.ListPorts(),
		"Version":    version.Get(),
		"AssetStamp": ws.assetStamp,
	}

	err := ws.templates.ExecuteTemplate(w, "index.html", data)
	if err != nil {
		log.Printf("Template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (ws *WebServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	clientID := fmt.Sprintf("%s-%s", r.RemoteAddr, "websocket")
	client := &WebSocketClient{
		ID:         clientID,
		Conn:       conn,
		RemoteAddr: r.RemoteAddr,
		ReadyTime:  time.Now(),
	}

	ws.mu.Lock()
	ws.clients[clientID] = client
	ws.mu.Unlock()

	log.Printf("WebSocket client connected: %s", client.RemoteAddr)

	// 处理WebSocket消息
	go ws.handleWebSocketMessages(client)
}

func (ws *WebServer) handleWebSocketMessages(client *WebSocketClient) {
	defer func() {
		client.Conn.Close()
		ws.mu.Lock()
		delete(ws.clients, client.ID)
		ws.mu.Unlock()
		log.Printf("WebSocket client disconnected: %s", client.RemoteAddr)
	}()

	for {
		messageType, message, err := client.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		if messageType == websocket.TextMessage {
			ws.handleWebSocketCommand(client, message)
		}
	}
}

func (ws *WebServer) handleWebSocketCommand(client *WebSocketClient, message []byte) {
	var cmd map[string]interface{}
	if err := json.Unmarshal(message, &cmd); err != nil {
		log.Printf("Failed to parse WebSocket command: %v", err)
		return
	}

	command, ok := cmd["type"].(string)
	if !ok {
		log.Printf("Invalid command type")
		return
	}

	switch command {
	case "terminal_ready":
		rows, cols := 0, 0
		if r, ok := cmd["rows"].(float64); ok {
			rows = int(r)
		}
		if c, ok := cmd["cols"].(float64); ok {
			cols = int(c)
		}
		client.setReady(true, rows, cols)

		log.Printf("Terminal client ready: %s (%dx%d)", client.RemoteAddr, rows, cols)

		// 获取端口状态
		ports := ws.getPortsWithStatus()
		log.Printf("Sending ports to client: %v", ports)

		// 发送欢迎消息
		welcome := map[string]interface{}{
			"type":    "welcome",
			"message": "Connected to Serial Over Socket Web Terminal",
			"ports":   ports,
		}

		ws.sendJSON(client, welcome)

	case "select_port":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}

		client.setPortID(portID)
		ws.sendResponse(client, map[string]interface{}{
			"type":    "response",
			"status":  "success",
			"message": fmt.Sprintf("Port %s selected", portID),
		})

	case "connect_port":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}

		// Extract configuration parameters if provided
		if baudrate, ok := cmd["baudrate"].(float64); ok {
			// Get current port config and update it
			cfg := &config.SerialPortConfig{
				Device:   fmt.Sprintf("/dev/%s", portID),
				Baudrate: int(baudrate),
				Databits: 8,
				Stopbit:  1,
				Parity:   "N",
				Timeout:  10,
			}

			if databits, ok := cmd["databits"].(float64); ok {
				cfg.Databits = int(databits)
			}
			if stopbit, ok := cmd["stopbit"].(float64); ok {
				cfg.Stopbit = int(stopbit)
			}
			if parity, ok := cmd["parity"].(string); ok {
				cfg.Parity = parity
			}
			if timeout, ok := cmd["timeout"].(float64); ok {
				cfg.Timeout = int(timeout)
			}

			// Update port configuration before connecting
			err := ws.serialMgr.UpdatePortConfig(portID, cfg)
			if err != nil {
				log.Printf("Failed to update port config: %v", err)
				// Continue anyway, try to connect
			}

			// Persist the profile so future hotplug events restore these settings.
			profile := &config.PortProfile{
				Baudrate: cfg.Baudrate,
				Databits: cfg.Databits,
				Stopbit:  cfg.Stopbit,
				Parity:   cfg.Parity,
			}
			ws.savePortProfileKeys(portID, profile)
		}

		err := ws.serialMgr.ConnectPort(portID)
		if err != nil {
			ws.sendError(client, err.Error())
		} else {
			ws.sendResponse(client, map[string]interface{}{
				"type":   "port_status",
				"port":   portID,
				"status": "connected",
			})

			// Automatically select the connected port for this client
			client.setPortID(portID)
			log.Printf("Client %s auto-selected port %s after connect", client.RemoteAddr, portID)

			// Broadcast updated port list to all clients after a small delay
			// to ensure port status has been updated
			go func() {
				time.Sleep(100 * time.Millisecond)
				ws.broadcastPortList()
			}()
		}

	case "disconnect_port":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}

		err := ws.serialMgr.DisconnectPort(portID)
		if err != nil {
			ws.sendError(client, err.Error())
		} else {
			ws.sendResponse(client, map[string]interface{}{
				"type":   "port_status",
				"port":   portID,
				"status": "disconnected",
			})
		}

	case "add_port":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}

		// 创建默认配置
		cfg := &config.SerialPortConfig{
			Device:   fmt.Sprintf("/dev/%s", portID),
			Baudrate: 115200,
			Databits: 8,
			Stopbit:  1,
			Parity:   "N",
			Timeout:  10,
		}

		err := ws.serialMgr.AddPort(portID, cfg)
		if err != nil {
			ws.sendError(client, err.Error())
		} else {
			ws.sendResponse(client, map[string]interface{}{
				"type":   "port_status",
				"port":   portID,
				"status": "disconnected",
			})
		}

	case "configure_port":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}

		// Extract configuration parameters
		cfg := &config.SerialPortConfig{
			Device:   fmt.Sprintf("/dev/%s", portID),
			Baudrate: 115200,
			Databits: 8,
			Stopbit:  1,
			Parity:   "N",
			Timeout:  10,
		}

		if baudrate, ok := cmd["baudrate"].(float64); ok {
			cfg.Baudrate = int(baudrate)
		}
		if databits, ok := cmd["databits"].(float64); ok {
			cfg.Databits = int(databits)
		}
		if stopbit, ok := cmd["stopbit"].(float64); ok {
			cfg.Stopbit = int(stopbit)
		}
		if parity, ok := cmd["parity"].(string); ok {
			cfg.Parity = parity
		}
		if timeout, ok := cmd["timeout"].(float64); ok {
			cfg.Timeout = int(timeout)
		}

		// Update port configuration
		err := ws.serialMgr.UpdatePortConfig(portID, cfg)
		if err != nil {
			ws.sendError(client, err.Error())
			return
		}

		// Persist the updated profile (same logic as connect_port)
		profile := &config.PortProfile{
			Baudrate: cfg.Baudrate,
			Databits: cfg.Databits,
			Stopbit:  cfg.Stopbit,
			Parity:   cfg.Parity,
		}
		ws.savePortProfileKeys(portID, profile)

		ws.sendResponse(client, map[string]interface{}{
			"type":    "response",
			"status":  "success",
			"message": fmt.Sprintf("Port %s configuration saved", portID),
		})

	case "set_port_alias":
		portID, ok := cmd["port_id"].(string)
		if !ok {
			ws.sendError(client, "Missing port_id")
			return
		}
		alias, _ := cmd["alias"].(string)

		// Update or create profile entry with alias.
		// IMPORTANT: alias is only written to stable hardware keys (ByIDName,
		// description), NOT to the portID key (e.g. "ttyUSB0"), to prevent a
		// future device at the same node from inheriting this alias.
		keys := ws.resolveProfileKeys(portID)
		ws.updateConfig(func(cfg *config.Config) {
			if cfg.PortProfiles == nil {
				cfg.PortProfiles = make(map[string]*config.PortProfile)
			}
			for _, key := range keys {
				if key == portID {
					continue // skip portID key — not hardware-specific
				}
				if p, ok := cfg.PortProfiles[key]; ok && p != nil {
					p.Alias = alias
					cfg.PortProfiles[key] = p
				} else {
					// Create a minimal profile entry just to store the alias
					cfg.PortProfiles[key] = &config.PortProfile{Alias: alias}
				}
			}
		})
		ws.serialMgr.UpdatePortProfiles(ws.snapshotPortProfiles())
		log.Printf("Set alias for port %q: %q (keys=%v)", portID, alias, keys)

		ws.sendResponse(client, map[string]interface{}{
			"type":    "response",
			"status":  "success",
			"message": fmt.Sprintf("Alias for port %s saved", portID),
		})

		// Broadcast updated port list so clients see the new alias immediately
		go func() {
			time.Sleep(50 * time.Millisecond)
			ws.broadcastPortList()
		}()

	case "write_data":
		targetPortID := client.getPortID()
		// If not bound, try to get from request
		if targetPortID == "" {
			if pid, ok := cmd["port_id"].(string); ok {
				targetPortID = pid
			}
		}

		if targetPortID == "" {
			ws.sendError(client, "No port selected")
			return
		}

		data, ok := cmd["data"].(string)
		if !ok {
			ws.sendError(client, "Missing data")
			return
		}

		// 转发到串口
		_, err := ws.serialMgr.WriteData(targetPortID, []byte(data))
		if err != nil {
			ws.sendError(client, err.Error())
		}
		// 不发送响应，避免控制台噪音
		// 不回显数据，让串口设备自己回显

	case "get_ports":
		ws.sendResponse(client, map[string]interface{}{
			"type":  "ports",
			"ports": ws.getPortsWithStatus(),
		})

	case "get_history":
		ws.historyMu.RLock()
		hist := make([]CommandEntry, len(ws.commandHistory))
		copy(hist, ws.commandHistory)
		ws.historyMu.RUnlock()
		ws.sendResponse(client, map[string]interface{}{
			"type":    "history",
			"history": hist,
		})

	case "add_command":
		cmdStr, ok := cmd["command"].(string)
		cmdStr = strings.TrimSpace(cmdStr)
		if ok && cmdStr != "" {
			ws.historyMu.Lock()
			// Remove any existing entries matching cmdStr (moves command to top & prevents duplicates)
			var filtered []CommandEntry
			for _, entry := range ws.commandHistory {
				if entry.Command != cmdStr {
					filtered = append(filtered, entry)
				}
			}
			newEntry := CommandEntry{
				ID:        nextHistoryID(),
				Command:   cmdStr,
				Timestamp: time.Now().Unix(),
			}
			ws.commandHistory = append([]CommandEntry{newEntry}, filtered...)
			if len(ws.commandHistory) > 1000 {
				ws.commandHistory = ws.commandHistory[:1000]
			}
			newHist := make([]CommandEntry, len(ws.commandHistory))
			copy(newHist, ws.commandHistory)
			ws.historyMu.Unlock()

			// Broadcast update
			ws.broadcastJSON(map[string]interface{}{
				"type":    "history_update",
				"history": newHist,
			})

			// Save to local disk immediately
			ws.saveLocalHistory()
			ws.historyChanged.Store(true)
		}

		// If data is provided, also send it to the port
		if data, ok := cmd["data"].(string); ok && data != "" {
			targetPortID := client.getPortID()
			if targetPortID == "" {
				if pid, ok := cmd["port_id"].(string); ok {
					targetPortID = pid
				}
			}
			if targetPortID != "" {
				ws.serialMgr.WriteData(targetPortID, []byte(data))
			}
		}

	case "delete_command":
		idStr, ok := cmd["id"].(string)
		if ok && idStr != "" {
			ws.historyMu.Lock()
			removed := false
			var newHistory []CommandEntry
			for _, entry := range ws.commandHistory {
				if entry.ID != idStr {
					newHistory = append(newHistory, entry)
				} else {
					removed = true
				}
			}
			ws.commandHistory = newHistory
			if removed {
				// Tombstone in the SAME lock hold as the
				// removal: previously it was written after
				// unlock, and a merge landing in that window
				// resurrected the entry and saved it back to
				// disk. A merge either runs before this hold
				// (its entry is removed here) or after it
				// (filtered by the tombstone) — never in
				// between. Broadcast, save, and the backup
				// nudge below stay after unlock, when the
				// tombstone is already standing.
				ws.addHistoryTombstoneLocked(idStr)
			}
			newHist := make([]CommandEntry, len(ws.commandHistory))
			copy(newHist, ws.commandHistory)
			ws.historyMu.Unlock()

			ws.broadcastJSON(map[string]interface{}{
				"type":    "history_update",
				"history": newHist,
			})
			ws.saveLocalHistory()
			ws.historyChanged.Store(true)
			if removed {
				// Nudge the cloud copy toward the
				// post-delete list (coalesced; a clear-all
				// is N rapid deletes).
				ws.requestHistoryBackupSoon()
			}
		}

	case "edit_command":
		idStr, ok := cmd["id"].(string)
		newCmdStr, okCmd := cmd["command"].(string)
		newCmdStr = strings.TrimSpace(newCmdStr)
		if ok && okCmd && idStr != "" && newCmdStr != "" {
			ws.historyMu.Lock()
			for i, entry := range ws.commandHistory {
				if entry.ID == idStr {
					ws.commandHistory[i].Command = newCmdStr
					ws.commandHistory[i].Timestamp = time.Now().Unix()
					break
				}
			}
			ws.commandHistory = dedupHistory(ws.commandHistory)
			newHist := make([]CommandEntry, len(ws.commandHistory))
			copy(newHist, ws.commandHistory)
			ws.historyMu.Unlock()

			ws.broadcastJSON(map[string]interface{}{
				"type":    "history_update",
				"history": newHist,
			})
			ws.saveLocalHistory()
			ws.historyChanged.Store(true)
		}

	case "merge_history":
		if commandsRaw, ok := cmd["commands"].([]interface{}); ok {
			commandsBytes, _ := json.Marshal(commandsRaw)
			if newCommands := parseHistoryBytes(commandsBytes); newCommands != nil {
				ws.historyMu.Lock()
				// Merges never resurrect deletions: entries
				// deleted locally stay deleted even if the
				// imported file (or cloud backup) still has them.
				newCommands = filterTombstonedEntries(newCommands, ws.historyTombstoneSet)
				merged := append(newCommands, ws.commandHistory...)
				ws.commandHistory = dedupHistory(merged)
				if len(ws.commandHistory) > 1000 {
					ws.commandHistory = ws.commandHistory[:1000]
				}
				newHist := make([]CommandEntry, len(ws.commandHistory))
				copy(newHist, ws.commandHistory)
				ws.historyMu.Unlock()

				ws.broadcastJSON(map[string]interface{}{
					"type":    "history_update",
					"history": newHist,
				})
				ws.saveLocalHistory()
				ws.historyChanged.Store(true)
			}
		}

	case "replace_history":
		if commandsRaw, ok := cmd["commands"].([]interface{}); ok {
			commandsBytes, _ := json.Marshal(commandsRaw)
			if newCommands := parseHistoryBytes(commandsBytes); newCommands != nil {
				ws.historyMu.Lock()
				ws.commandHistory = dedupHistory(newCommands)
				if len(ws.commandHistory) > 1000 {
					ws.commandHistory = ws.commandHistory[:1000]
				}
				newHist := make([]CommandEntry, len(ws.commandHistory))
				copy(newHist, ws.commandHistory)
				// Replace is an explicit full restore, NOT a
				// merge: entries brought back this way have
				// their tombstones cleared (a deliberate
				// un-delete), or every later merge would
				// fight the restore. Entries absent from the
				// file keep their tombstones. The clear runs
				// in the SAME lock hold as the swap: a delete
				// landing between the swap and a delayed clear
				// would plant a tombstone the clear then
				// removes, losing the deletion (the entry
				// stays gone but no tombstone blocks a later
				// merge from resurrecting it). Broadcast and
				// the local save stay after unlock, when the
				// tombstone state is already final.
				restored := make([]string, 0, len(newHist))
				for _, e := range newHist {
					restored = append(restored, e.ID)
				}
				ws.clearHistoryTombstonesLocked(restored)
				ws.historyMu.Unlock()

				ws.broadcastJSON(map[string]interface{}{
					"type":    "history_update",
					"history": newHist,
				})
				ws.saveLocalHistory()
				ws.historyChanged.Store(true)
			}
		}

	case "backup_history_now":
		// Manual cloud backup (native GUI's 立即备份 button):
		// upload the server's current list. Network I/O must
		// not block the WS loop, so the upload runs in a
		// goroutine and reports back as backup_history_result.
		// historyChanged is left alone (see backupSoonLoop).
		go func() {
			if wd := ws.getWebDAV(); wd.URL == "" {
				ws.sendResponse(client, map[string]interface{}{
					"type":  "backup_history_result",
					"ok":    false,
					"error": "WebDAV not configured on server",
				})
				return
			}
			if err := ws.saveHistoryToWebDAV(); err != nil {
				ws.sendResponse(client, map[string]interface{}{
					"type":  "backup_history_result",
					"ok":    false,
					"error": err.Error(),
				})
				return
			}
			ws.sendResponse(client, map[string]interface{}{
				"type": "backup_history_result",
				"ok":   true,
			})
		}()

	case "get_webdav_config":
		// NOTE: the password is NEVER sent back to clients. Callers only
		// learn whether one is set; a new non-empty password replaces it,
		// an empty one keeps it, and clear_password wipes it.
		wd := ws.getWebDAV()
		ws.sendResponse(client, map[string]interface{}{
			"type": "webdav_config",
			"config": map[string]interface{}{
				"url":          wd.URL,
				"username":     wd.Username,
				"password_set": wd.Password != "",
				"interval":     wd.Interval,
				"max_backups":  wd.MaxBackups,
			},
		})

	case "set_webdav_config":
		clearPw, _ := cmd["clear_password"].(bool)
		ws.updateConfig(func(cfg *config.Config) {
			if url, ok := cmd["url"].(string); ok {
				cfg.WebDAV.URL = url
			}
			if username, ok := cmd["username"].(string); ok {
				cfg.WebDAV.Username = username
			}
			switch {
			case clearPw:
				cfg.WebDAV.Password = ""
			default:
				// A non-empty password replaces the stored one; an empty
				// or missing one keeps whatever is stored.
				if password, ok := cmd["password"].(string); ok && password != "" {
					cfg.WebDAV.Password = password
				}
			}
			if interval, ok := cmd["interval"].(float64); ok {
				cfg.WebDAV.Interval = int(interval)
			}
			if maxB, ok := cmd["maxBackups"].(float64); ok {
				cfg.WebDAV.MaxBackups = int(maxB)
			}
		})

		resp := map[string]interface{}{
			"type":    "response",
			"status":  "success",
			"message": "WebDAV configuration saved",
		}
		// Echo the ack token so the WebUI can order a config push before
		// a proxy request that depends on it.
		if ack, ok := cmd["_ack"]; ok {
			resp["_ack"] = ack
		}
		ws.sendResponse(client, resp)

		// Immediately attempt to sync history with new settings
		go func() {
			ws.loadHistoryFromWebDAV()
			// Broadcast merged history
			ws.historyMu.RLock()
			newHist := make([]CommandEntry, len(ws.commandHistory))
			copy(newHist, ws.commandHistory)
			ws.historyMu.RUnlock()
			ws.broadcastJSON(map[string]interface{}{
				"type":    "history_update",
				"history": newHist,
			})
		}()

	default:
		ws.sendError(client, fmt.Sprintf("Unknown command: %s", command))
	}
}

// API处理函数
func (ws *WebServer) handleAPIPorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ports := ws.serialMgr.ListPorts()
	response := map[string]interface{}{
		"status": "success",
		"ports":  ports,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ws *WebServer) handleAPIPortConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PortID string `json:"port_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	err := ws.serialMgr.ConnectPort(req.PortID)
	if err != nil {
		response := map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	response := map[string]interface{}{
		"status":  "success",
		"message": fmt.Sprintf("Port %s connected", req.PortID),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ws *WebServer) handleAPIPortDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PortID string `json:"port_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	err := ws.serialMgr.DisconnectPort(req.PortID)
	if err != nil {
		response := map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	response := map[string]interface{}{
		"status":  "success",
		"message": fmt.Sprintf("Port %s disconnected", req.PortID),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ws *WebServer) handleAPIPortWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		PortID string `json:"port_id"`
		Data   string `json:"data"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	_, err := ws.serialMgr.WriteData(req.PortID, []byte(req.Data))
	if err != nil {
		response := map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
		return
	}

	response := map[string]interface{}{
		"status":  "success",
		"message": "Data sent",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ws *WebServer) handleAPIPortConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Not implemented: use the WebSocket API (configure_port) instead.
	http.Error(w, "Not implemented: use the WebSocket API", http.StatusNotImplemented)
}

func (ws *WebServer) handleAPIServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Not implemented.
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) handleAPIServerConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Not implemented.
	http.Error(w, "Not implemented", http.StatusNotImplemented)
}

// wsWriteTimeout bounds a single WebSocket write so one wedged client
// can't stall data delivery to everyone else.
const wsWriteTimeout = 10 * time.Second

func (ws *WebServer) sendJSON(client *WebSocketClient, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("Failed to marshal JSON: %v", err)
		return
	}

	// 使用互斥锁保护WebSocket写入操作
	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	_ = client.Conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err = client.Conn.WriteMessage(websocket.TextMessage, jsonData)
	if err != nil {
		log.Printf("Failed to send WebSocket message: %v", err)
	}
}

func (ws *WebServer) sendResponse(client *WebSocketClient, data map[string]interface{}) {
	ws.sendJSON(client, data)
}

func (ws *WebServer) sendError(client *WebSocketClient, message string) {
	ws.sendResponse(client, map[string]interface{}{
		"type":    "error",
		"message": message,
	})
}

// 串口数据处理器 – 发送包含 portID 头部的二进制帧，支持多端口后台接收数据
func (ws *WebServer) HandleSerialData(portID string, data []byte) {
	// 协议格式: [1 byte portID长度][portID UTF-8字节][原始串口字节数据]
	portIDBytes := []byte(portID)
	portIDLen := byte(len(portIDBytes))
	payload := make([]byte, 1+len(portIDBytes)+len(data))
	payload[0] = portIDLen
	copy(payload[1:1+len(portIDBytes)], portIDBytes)
	copy(payload[1+len(portIDBytes):], data)

	for _, client := range ws.snapshotClients() {
		if client.isReady() {
			ws.sendBinary(client, payload)
		}
	}
}

// sendBinary 发送二进制 WebSocket 帧
func (ws *WebServer) sendBinary(client *WebSocketClient, data []byte) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	_ = client.Conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := client.Conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		log.Printf("Failed to send binary WebSocket message: %v", err)
	}
}

// snapshotClients returns the current client list. Broadcasts iterate the
// snapshot WITHOUT holding ws.mu so a slow client never blocks client
// add/remove (or other broadcasts).
func (ws *WebServer) snapshotClients() []*WebSocketClient {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	clients := make([]*WebSocketClient, 0, len(ws.clients))
	for _, c := range ws.clients {
		clients = append(clients, c)
	}
	return clients
}

// 获取带状态的端口列表
func (ws *WebServer) getPortsWithStatus() map[string]interface{} {
	ports := ws.serialMgr.ListPorts()
	portStatus := make(map[string]interface{})

	for _, portID := range ports {
		status, err := ws.serialMgr.GetPortStatus(portID)
		if err == nil {
			statusStr := "unknown"
			switch status {
			case serial.StatusDisconnected:
				statusStr = "disconnected"
			case serial.StatusConnecting:
				statusStr = "connecting"
			case serial.StatusConnected:
				statusStr = "connected"
			case serial.StatusError:
				statusStr = "error"
			}

			entry := map[string]interface{}{
				"status":    statusStr,
				"connected": status == serial.StatusConnected,
			}

			// Include current serial config
			if cfg := ws.serialMgr.GetPortConfig(portID); cfg != nil {
				entry["baudrate"] = cfg.Baudrate
				entry["databits"] = cfg.Databits
				entry["stopbit"] = cfg.Stopbit
				entry["parity"] = cfg.Parity
			}

			// Include device description and by-id name from cached PortInfo (fast, no disk I/O)
			if info := ws.serialMgr.GetPortInfo(portID); info != nil {
				desc := info.Product
				if info.Vendor != "" && info.Vendor != "Unknown" {
					desc = info.Vendor + " " + info.Product
				}
				if info.Serial != "" {
					desc += " " + info.Serial
				}
				entry["description"] = desc
				entry["by_id_name"] = info.ByIDName
			} else {
				// Fallback: scan from /dev/serial/by-id/ (slower, first call only)
				devicePath := fmt.Sprintf("/dev/%s", portID)
				if info, err := serial.GetPortInfo(devicePath); err == nil {
					desc := info.Product
					if info.Vendor != "" && info.Vendor != "Unknown" {
						desc = info.Vendor + " " + info.Product
					}
					if info.Serial != "" {
						desc += " " + info.Serial
					}
					entry["description"] = desc
					entry["by_id_name"] = info.ByIDName
				} else {
					entry["description"] = ""
					entry["by_id_name"] = ""
				}
			}

			// Include alias from port profiles
			entry["alias"] = ws.serialMgr.GetPortAlias(portID)

			portStatus[portID] = entry
		}
	}

	return portStatus
}

// resolveProfileKeys returns the list of profile keys for the given portID,
// in priority order: ByIDName → description → portID.
func (ws *WebServer) resolveProfileKeys(portID string) []string {
	// Prefer the cached PortInfo stored in the manager
	if info := ws.serialMgr.GetPortInfo(portID); info != nil {
		var keys []string
		if info.ByIDName != "" {
			keys = append(keys, info.ByIDName)
		}
		desc := info.Product
		if info.Vendor != "" && info.Vendor != "Unknown" {
			desc = info.Vendor + " " + info.Product
		}
		if info.Serial != "" {
			desc += " " + info.Serial
		}
		if desc != "" {
			keys = append(keys, desc)
		}
		keys = append(keys, portID)
		return keys
	}

	// Fallback: scan from filesystem
	devicePath := fmt.Sprintf("/dev/%s", portID)
	if info, err := serial.GetPortInfo(devicePath); err == nil {
		var keys []string
		if info.ByIDName != "" {
			keys = append(keys, info.ByIDName)
		}
		desc := info.Product
		if info.Vendor != "" && info.Vendor != "Unknown" {
			desc = info.Vendor + " " + info.Product
		}
		if info.Serial != "" {
			desc += " " + info.Serial
		}
		if desc != "" {
			keys = append(keys, desc)
		}
		keys = append(keys, portID)
		return keys
	}

	return []string{portID}
}

// savePortProfileKeys persists a PortProfile under all keys that identify the
// given port (ByIDName, description, portID). Existing profile data (e.g. Alias)
// is preserved and merged with the new profile.
// IMPORTANT: Alias is only written to stable hardware keys (ByIDName, description),
// NOT to the portID key (e.g. "ttyUSB0"), because portID is reassigned to
// different physical devices across reboots/replug events.
func (ws *WebServer) savePortProfileKeys(portID string, profile *config.PortProfile) {
	keys := ws.resolveProfileKeys(portID)
	primaryKey := keys[0] // most stable key

	ws.updateConfig(func(cfg *config.Config) {
		if cfg.PortProfiles == nil {
			cfg.PortProfiles = make(map[string]*config.PortProfile)
		}

		// Preserve existing alias from any existing profile under stable (non-portID) keys
		for _, key := range keys {
			if key == portID {
				continue // skip portID — alias must not be inherited from it
			}
			if existing, ok := cfg.PortProfiles[key]; ok && existing != nil && existing.Alias != "" {
				profile.Alias = existing.Alias
				break
			}
		}

		for _, key := range keys {
			if key == portID {
				// Save a copy WITHOUT alias under portID key so that future devices
				// assigned to the same node (e.g. ttyUSB0) don't inherit this alias.
				noAlias := *profile
				noAlias.Alias = ""
				cfg.PortProfiles[key] = &noAlias
			} else {
				cfg.PortProfiles[key] = profile
			}
		}
	})
	ws.serialMgr.UpdatePortProfiles(ws.snapshotPortProfiles())
	log.Printf("Saved port profile for %q (primary key=%q): baud=%d alias=%q",
		portID, primaryKey, profile.Baudrate, profile.Alias)
}

// 定期清理未就绪的客户端
func (ws *WebServer) cleanupClients() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ws.mu.Lock()
		for clientID, client := range ws.clients {
			// 如果客户端超过10秒未就绪，清理掉
			if !client.isReady() && time.Since(client.ReadyTime) > 10*time.Second {
				client.Conn.Close()
				delete(ws.clients, clientID)
				log.Printf("Cleaned up unready client: %s", client.RemoteAddr)
			}
		}
		ws.mu.Unlock()
	}
}

// snapshotPortProfiles returns a deep copy of the port profiles map,
// safe to hand to the serial manager.
func (ws *WebServer) snapshotPortProfiles() map[string]*config.PortProfile {
	ws.configMu.RLock()
	defer ws.configMu.RUnlock()
	if ws.config.PortProfiles == nil {
		return nil
	}
	cp := make(map[string]*config.PortProfile, len(ws.config.PortProfiles))
	for k, v := range ws.config.PortProfiles {
		if v != nil {
			vv := *v
			cp[k] = &vv
		}
	}
	return cp
}

// ------ config access helpers ------
// All reads/writes of ws.config fields must go through these helpers:
// the config is mutated by WebSocket command handlers while the backup
// loop and proxy handlers read it concurrently.

// getWebDAV returns a copy of the WebDAV configuration.
func (ws *WebServer) getWebDAV() config.WebDAVConfig {
	ws.configMu.RLock()
	defer ws.configMu.RUnlock()
	return ws.config.WebDAV
}

// getServerAddr returns "address:port" for the listen address.
func (ws *WebServer) getServerAddr() string {
	ws.configMu.RLock()
	defer ws.configMu.RUnlock()
	return fmt.Sprintf("%s:%d", ws.config.Server.Address, ws.config.Server.Port)
}

// updateConfig runs fn with the write lock held and persists the result.
// The save happens under the same lock so two concurrent writers can't
// interleave mutate/save and lose one update. Save failures are logged,
// not silently dropped (the old code ignored them).
func (ws *WebServer) updateConfig(fn func(cfg *config.Config)) {
	ws.configMu.Lock()
	defer ws.configMu.Unlock()
	fn(ws.config)
	// SaveConfig itself is atomic (temp file + rename), so a crash can
	// never leave a truncated config behind.
	if err := config.SaveConfig(config.GetConfigPath(), deepCopyConfig(ws.config)); err != nil {
		log.Printf("Failed to save config: %v", err)
	}
}

func deepCopyConfig(cfg *config.Config) *config.Config {
	cp := *cfg
	if cfg.PortProfiles != nil {
		cp.PortProfiles = make(map[string]*config.PortProfile, len(cfg.PortProfiles))
		for k, v := range cfg.PortProfiles {
			if v != nil {
				vv := *v
				cp.PortProfiles[k] = &vv
			}
		}
	}
	return &cp
}

// ------ Server-side Local & WebDAV Auto-Sync ------

func dedupHistory(entries []CommandEntry) []CommandEntry {
	seen := make(map[string]bool)
	var result []CommandEntry
	for _, entry := range entries {
		cmdStr := strings.TrimSpace(entry.Command)
		if cmdStr == "" || seen[cmdStr] {
			continue
		}
		seen[cmdStr] = true
		entry.Command = cmdStr
		result = append(result, entry)
	}
	return result
}

func parseHistoryBytes(data []byte) []CommandEntry {
	var objects []CommandEntry
	if err := json.Unmarshal(data, &objects); err == nil {
		for i := range objects {
			if objects[i].ID == "" {
				objects[i].ID = fmt.Sprintf("%d_%d", time.Now().UnixNano(), i)
			}
		}
		if len(objects) > 0 {
			return dedupHistory(objects)
		}
	}

	// Fallback to older string array
	var stringsArr []string
	if err := json.Unmarshal(data, &stringsArr); err == nil {
		for i, s := range stringsArr {
			objects = append(objects, CommandEntry{
				ID:        fmt.Sprintf("%d_%d", time.Now().UnixNano(), i),
				Command:   s,
				Timestamp: time.Now().Unix(),
			})
		}
		return dedupHistory(objects)
	}
	return nil
}

func getHistoryPath() string {
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".config", "go-serial-over-socket", "history.json")
	}
	return "config/history.json"
}

func (ws *WebServer) loadLocalHistory() {
	if data, err := os.ReadFile(getHistoryPath()); err == nil {
		if localStore := parseHistoryBytes(data); localStore != nil {
			ws.historyMu.Lock()
			// A manually restored old file must not resurrect
			// deletions either (tombstones load before this).
			ws.commandHistory = dedupHistory(filterTombstonedEntries(localStore, ws.historyTombstoneSet))
			ws.historyMu.Unlock()
			log.Printf("Loaded %d command history items from local cache", len(ws.commandHistory))
			ws.saveLocalHistory()
		}
	}
}

func (ws *WebServer) saveLocalHistory() {
	ws.historyMu.RLock()
	data, err := json.MarshalIndent(ws.commandHistory, "", "  ")
	ws.historyMu.RUnlock()
	if err == nil {
		path := getHistoryPath()
		os.MkdirAll(filepath.Dir(path), 0755)
		// 0600: history can contain credentials typed at the console.
		os.WriteFile(path, data, 0600)
	}
}

// ------ Server-side Auto Backup Loop & WebDAV Sync ------

// tryHistoryUpload runs one interval upload attempt: it atomically
// takes the dirty flag, uploads, and restores the flag if the
// upload failed. A change made during the upload sets the flag
// again, so it survives both outcomes: on success it drives the
// next tick's upload, on failure the restore is a no-op over it.
// Without the swap, a change made during an upload would be
// cleared by the unconditional Store(false) after it, and a
// failed upload would be cleared the same way. It returns
// whether an upload was attempted. backupSoonLoop never touches
// the flag: it is best-effort acceleration only, and the
// interval loop remains the flag's single owner.
func (ws *WebServer) tryHistoryUpload(upload func() error) bool {
	if !ws.historyChanged.Swap(false) {
		return false
	}
	if err := upload(); err != nil {
		ws.historyChanged.Store(true)
	}
	return true
}

func (ws *WebServer) autoBackupLoop() {
	// Startup sync: grab the latest history backup from server
	ws.loadHistoryFromWebDAV()

	for {
		wd := ws.getWebDAV()
		interval := wd.Interval
		if interval <= 0 {
			interval = 30 // default check interval
		}
		time.Sleep(time.Duration(interval) * time.Minute)

		wd = ws.getWebDAV()
		if wd.URL != "" {
			ws.tryHistoryUpload(ws.saveHistoryToWebDAV)
		}
	}
}

// parseBackupTimestamp parses the timestamp embedded in a history
// backup filename. Both formats are accepted: the second-precision
// history_backup_YYYYMMDDHHMMSS.json written before millisecond
// names, and the current history_backup_YYYYMMDDHHMMSS.mmm.json.
// The second return value reports whether the name parses at all,
// the third whether it carries milliseconds. String order is NOT
// chronological across formats (a millisecond name's '.' sorts
// below the second name's 'j' at the first divergence), so every
// newest/prune decision must go through parsed time, never through
// raw name comparison.
func parseBackupTimestamp(name string) (time.Time, bool, bool) {
	if !strings.HasPrefix(name, "history_backup_") || !strings.HasSuffix(name, ".json") {
		return time.Time{}, false, false
	}
	ts := strings.TrimSuffix(strings.TrimPrefix(name, "history_backup_"), ".json")
	if strings.Contains(ts, ".") {
		if t, err := time.Parse("20060102150405.000", ts); err == nil {
			return t, true, true
		}
		return time.Time{}, false, false
	}
	if t, err := time.Parse("20060102150405", ts); err == nil {
		return t, true, false
	}
	return time.Time{}, false, false
}

// compareBackupFiles orders two backup filenames newest-first:
// parseable beats unparseable, later timestamp beats earlier, and
// on equal timestamps a millisecond name beats a second name (the
// millisecond upload is the newer writer — a same-second old file
// parses to exactly .000). The filename itself is the final
// deterministic tie-break. It returns a negative value when a
// sorts before b.
func compareBackupFiles(a, b string) int {
	ta, oka, msa := parseBackupTimestamp(a)
	tb, okb, msb := parseBackupTimestamp(b)
	if oka && !okb {
		return -1
	}
	if !oka && okb {
		return 1
	}
	if oka && okb {
		if !ta.Equal(tb) {
			if ta.After(tb) {
				return -1
			}
			return 1
		}
		if msa != msb {
			if msa {
				return -1
			}
			return 1
		}
	}
	if a == b {
		return 0
	}
	if a > b {
		return -1
	}
	return 1
}

// latestBackupFile returns the newest name, or "" when none parses:
// an unparseable name can never be proven newest, so it is skipped,
// never selected.
func latestBackupFile(names []string) string {
	latest := ""
	for _, n := range names {
		if _, ok, _ := parseBackupTimestamp(n); !ok {
			continue
		}
		if latest == "" || compareBackupFiles(n, latest) < 0 {
			latest = n
		}
	}
	return latest
}

// sortBackupFilesNewestFirst sorts in place, newest first.
// Unparseable names sink to the end, so pruning over the limit
// deletes them before any parseable backup.
func sortBackupFilesNewestFirst(names []string) {
	sort.Slice(names, func(i, j int) bool { return compareBackupFiles(names[i], names[j]) < 0 })
}

func (ws *WebServer) loadHistoryFromWebDAV() {
	wd := ws.getWebDAV()
	if wd.URL == "" {
		return
	}

	// PROPFIND to list files
	propfindBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`

	resp, err := doWebDAV("PROPFIND", wd.URL, wd.Username, wd.Password,
		map[string]string{"Depth": "1", "Content-Type": "application/xml"},
		strings.NewReader(propfindBody))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		return
	}

	bodyXml, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var ms davMultistatus
	if err := xml.Unmarshal(bodyXml, &ms); err != nil {
		return
	}

	var names []string

	for _, r := range ms.Responses {
		parts := strings.Split(strings.TrimRight(r.Href, "/"), "/")
		name := parts[len(parts)-1]
		if strings.HasPrefix(name, "history_backup_") && strings.HasSuffix(name, ".json") {
			names = append(names, name)
		}
	}
	// Parsed-time newest, not lexicographic max: a same-second old
	// (second-precision) name sorts above its millisecond sibling
	// as a raw string, which would pick the stale backup.
	latestFile := latestBackupFile(names)

	if latestFile == "" {
		return
	}

	url := strings.TrimRight(wd.URL, "/") + "/" + latestFile
	dlResp, err := doWebDAV("GET", url, wd.Username, wd.Password, nil, nil)
	if err != nil || dlResp.StatusCode >= 400 {
		if dlResp != nil {
			dlResp.Body.Close()
		}
		return
	}
	defer dlResp.Body.Close()

	bodyBytes, err := io.ReadAll(dlResp.Body)
	if err != nil {
		return
	}

	// Handle raw or wrapped JSON
	var rawData interface{}
	json.Unmarshal(bodyBytes, &rawData)

	var arrayBytes []byte
	m, ok := rawData.(map[string]interface{})
	if ok && m["commands"] != nil {
		arrayBytes, _ = json.Marshal(m["commands"])
	} else {
		arrayBytes = bodyBytes
	}

	if remoteHistory := parseHistoryBytes(arrayBytes); remoteHistory != nil {
		ws.historyMu.Lock()
		// Startup (and post-settings-save) merges never
		// resurrect deletions: skip tombstoned ids even if
		// the newest cloud backup still contains them.
		remoteHistory = filterTombstonedEntries(remoteHistory, ws.historyTombstoneSet)
		merged := append(ws.commandHistory, remoteHistory...)
		ws.commandHistory = dedupHistory(merged)
		if len(ws.commandHistory) > 1000 {
			ws.commandHistory = ws.commandHistory[:1000]
		}
		// Broadcast the stored (deduped + truncated) history, not the raw
		// merged slice — previously clients received a list that didn't
		// match what the server actually stored.
		broadcast := make([]CommandEntry, len(ws.commandHistory))
		copy(broadcast, ws.commandHistory)
		ws.historyMu.Unlock()

		ws.saveLocalHistory()

		// Broadcast to any clients that connected while we were fetching
		ws.broadcastJSON(map[string]interface{}{
			"type":    "history_update",
			"history": broadcast,
		})
	}
}

// saveHistoryToWebDAV uploads the current history as a new
// timestamped backup and prunes to MaxBackups. It returns the PUT
// error (pruning stays best-effort, as before); callers that ignore
// the return (the interval loop) behave exactly as before.
func (ws *WebServer) saveHistoryToWebDAV() error {
	// The whole upload — snapshot, PUT, prune — runs under one
	// hold of uploadMu. All three upload entries (interval loop,
	// manual backup, backup-soon) funnel through this function,
	// so uploads are naturally serialized and a later snapshot
	// can never be overwritten by an earlier, slower upload
	// finishing last.
	ws.uploadMu.Lock()
	defer ws.uploadMu.Unlock()

	wd := ws.getWebDAV()
	if wd.URL == "" {
		return fmt.Errorf("WebDAV not configured on server")
	}

	ws.historyMu.RLock()
	dataArr := make([]CommandEntry, len(ws.commandHistory))
	copy(dataArr, ws.commandHistory)
	ws.historyMu.RUnlock()

	exportData := map[string]interface{}{
		"version":    "1.0",
		"exportDate": time.Now().Format(time.RFC3339),
		"commands":   dataArr,
	}
	data, err := json.MarshalIndent(exportData, "", "  ")
	if err != nil {
		return err
	}

	// Backup filenames embed a fixed-width millisecond timestamp,
	// so lexicographic order is chronological order (prune's
	// newest-first sort and loadHistoryFromWebDAV's max-filename
	// rule both rely on it). The monotonic guard keeps names
	// unique even when two serialized uploads land in the same
	// millisecond: the later one takes the previous timestamp
	// plus one millisecond rather than reusing the name.
	ms := time.Now().UnixMilli()
	if ms <= ws.lastBackupMillis {
		ms = ws.lastBackupMillis + 1
	}
	ws.lastBackupMillis = ms
	filename := fmt.Sprintf("history_backup_%s.json", time.UnixMilli(ms).Format("20060102150405.000"))
	url := strings.TrimRight(wd.URL, "/") + "/" + filename

	resp, err := doWebDAV("PUT", url, wd.Username, wd.Password, nil, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("WebDAV PUT returned %d", resp.StatusCode)
	}

	// Pruning
	maxBackups := wd.MaxBackups
	if maxBackups <= 0 {
		maxBackups = 10
	}

	propfindBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/></d:prop></d:propfind>`
	listResp, err := doWebDAV("PROPFIND", wd.URL, wd.Username, wd.Password, map[string]string{"Depth": "1", "Content-Type": "application/xml"}, strings.NewReader(propfindBody))
	if err == nil && (listResp.StatusCode == 200 || listResp.StatusCode == 207) {
		defer listResp.Body.Close()
		lsBytes, _ := io.ReadAll(listResp.Body)
		var ms davMultistatus
		if xml.Unmarshal(lsBytes, &ms) == nil {
			var files []string
			for _, r := range ms.Responses {
				parts := strings.Split(strings.TrimRight(r.Href, "/"), "/")
				name := parts[len(parts)-1]
				if strings.HasPrefix(name, "history_backup_") {
					files = append(files, name)
				}
			}
			// Newest first by parsed timestamp (both the old
			// second-precision and the current millisecond
			// formats; unparseable names sink to the end, so
			// they are pruned before any parseable backup).
			// Raw string order is wrong across formats: a
			// same-second old name sorts above its
			// millisecond sibling, which with MaxBackups=1
			// would delete the just-uploaded new backup.
			sortBackupFilesNewestFirst(files)
			if len(files) > maxBackups {
				for _, delFile := range files[maxBackups:] {
					delUrl := strings.TrimRight(wd.URL, "/") + "/" + delFile
					dResp, dErr := doWebDAV("DELETE", delUrl, wd.Username, wd.Password, nil, nil)
					if dErr == nil {
						dResp.Body.Close()
					}
				}
			}
		}
	}
	return nil
}

// Broadcast to all connected and ready clients
func (ws *WebServer) broadcastJSON(message map[string]interface{}) {
	for _, client := range ws.snapshotClients() {
		if client.isReady() {
			ws.sendJSON(client, message)
		}
	}
}

// Broadcast port list to all connected clients
func (ws *WebServer) broadcastPortList() {
	ports := ws.getPortsWithStatus()
	message := map[string]interface{}{
		"type":  "ports",
		"ports": ports,
	}

	for _, client := range ws.snapshotClients() {
		if client.isReady() {
			ws.sendJSON(client, message)
		}
	}
}

// 启动清理goroutine
func (ws *WebServer) StartCleanup() {
	go ws.cleanupClients()
}

// handlePortStatusChange handles port status change notifications from the serial manager
func (ws *WebServer) handlePortStatusChange(portID string, status serial.PortStatus, err error) {
	log.Printf("Port status changed: %s, status: %d, error: %v", portID, status, err)

	// Broadcast updated port list to all connected clients
	ws.broadcastPortList()
}
