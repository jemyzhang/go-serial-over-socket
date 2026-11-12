// Package main implements sos-gui, a fully-native
// GUI client for go-serial-over-socket: MyGo native UI plus the terminal
// plugin (Ghostty's libghostty-vt), no webview. It speaks the same
// WebSocket protocol as the WebUI (see internal/web/web_server.go), so
// the server is untouched and this client can share sessions with the
// TUI and the WebUI.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// PortInfo is one entry of the server's ports map (see
// getPortsWithStatus in internal/web/web_server.go).
type PortInfo struct {
	ID          string
	Status      string // "connected", "disconnected", "connecting", "error", "unknown"
	Connected   bool
	Alias       string
	Baudrate    int
	Databits    int
	Stopbit     float64
	Parity      string
	Description string
}

// HistoryEntry is one server-side command-history record
// (CommandEntry in internal/web/web_server.go).
type HistoryEntry struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Timestamp int64  `json:"timestamp"`
}

// WebDAVConfig mirrors the server's webdav_config payload. The
// server never sends the password back, only whether one is set.
type WebDAVConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
	Interval    int    `json:"interval"`
	MaxBackups  int    `json:"max_backups"`
}

// normalizeBaseURL tidies a user-provided server address into a base
// URL: scheme added when missing, trailing slash removed.
func normalizeBaseURL(raw string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if u == "" {
		return "", fmt.Errorf("empty server address")
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	return u, nil
}

// wsURLFromBase converts an http(s) base URL into the server's
// WebSocket endpoint.
func wsURLFromBase(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// wsCallbacks are the events the WS read loop delivers. All run on the
// read loop's goroutine; handlers must be goroutine-safe.
type wsCallbacks struct {
	onPorts      func(ports map[string]PortInfo)
	onStatus     func(portID, status string)
	onData       func(portID string, data []byte)
	onError      func(msg string)
	onHistory    func(entries []HistoryEntry)
	onWebDAV     func(cfg WebDAVConfig)
	onDisconnect func(client *wsClient)
	// onBackupResult reports a backup_history_now round trip
	// (server-side cloud upload): ok and, on failure, the reason.
	onBackupResult func(ok bool, errMsg string)
}

// wsClient is a minimal client for the sos WebSocket protocol. One
// goroutine reads; all writes go through writeMu (gorilla connections
// allow one concurrent reader and one concurrent writer).
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	cb      wsCallbacks

	// _ack waiters: the server echoes a request's _ack token in its
	// response (see set_webdav_config), so a caller that must order
	// a second step after a config push can wait for the push to
	// land instead of racing it. Waiters are one-shot channels,
	// resolved in handleText before the type dispatch.
	ackMu      sync.Mutex
	ackWaiters map[string]chan struct{}
	ackSeq     uint64

	closeOnce sync.Once
	closed    chan struct{}
}

// dialWS connects to the server at base (http(s) URL) and announces
// the client as ready: without terminal_ready the server sends no
// broadcasts at all (see handleWebSocket/isReady in web_server.go).
func dialWS(base string, cb wsCallbacks) (*wsClient, error) {
	wsURL, err := wsURLFromBase(base)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	c := &wsClient{conn: conn, cb: cb, closed: make(chan struct{})}
	if err := c.sendCommand(map[string]any{
		"type": "terminal_ready",
		"rows": 24,
		"cols": 80,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	// The read loop starts only on start(): the caller attaches the
	// client to the app first, so callbacks never see a half-wired app.
	return c, nil
}

// start begins the read loop.
func (c *wsClient) start() { go c.readLoop() }

func (c *wsClient) sendCommand(msg map[string]any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(msg)
}

// sendTerminalReady re-announces the client's terminal size (a
// terminal_ready resend): the server's case is a setReady
// overwrite, so no server change is needed. Used when the
// terminal is resized after the initial ready (split toggle,
// window resize, font change) — the dial's terminal_ready
// carries a fixed 80x24, which is only the ready gate.
func (c *wsClient) sendTerminalReady(rows, cols int) error {
	return c.sendCommand(map[string]any{
		"type": "terminal_ready",
		"rows": rows,
		"cols": cols,
	})
}

// sendCommandWithAck sends msg with a fresh _ack token and returns a
// channel closed when the server's response carrying that token
// arrives. On send failure the channel is nil and the waiter is
// already removed. Callers wait on the channel with their own
// timeout; a timeout does not cancel the server-side effect.
func (c *wsClient) sendCommandWithAck(msg map[string]any) (<-chan struct{}, error) {
	ack := fmt.Sprintf("native-%d", atomic.AddUint64(&c.ackSeq, 1))
	msg["_ack"] = ack
	ch := make(chan struct{})
	c.ackMu.Lock()
	if c.ackWaiters == nil {
		c.ackWaiters = map[string]chan struct{}{}
	}
	c.ackWaiters[ack] = ch
	c.ackMu.Unlock()
	if err := c.sendCommand(msg); err != nil {
		c.ackMu.Lock()
		delete(c.ackWaiters, ack)
		c.ackMu.Unlock()
		return nil, err
	}
	return ch, nil
}

func (c *wsClient) resolveAck(ack string) {
	c.ackMu.Lock()
	ch, ok := c.ackWaiters[ack]
	if ok {
		delete(c.ackWaiters, ack)
	}
	c.ackMu.Unlock()
	if ok {
		close(ch)
	}
}

// GetPorts asks the server for a fresh ports list (it answers with a
// "ports" message, same as the broadcasts).
func (c *wsClient) GetPorts() error {
	return c.sendCommand(map[string]any{"type": "get_ports"})
}

// SelectPort binds this connection to a port for write_data without an
// explicit port_id, and for server-side logging.
func (c *wsClient) SelectPort(portID string) error {
	return c.sendCommand(map[string]any{"type": "select_port", "port_id": portID})
}

// ConnectPort connects a serial port, optionally at a baudrate.
func (c *wsClient) ConnectPort(portID string, baudrate int) error {
	msg := map[string]any{"type": "connect_port", "port_id": portID}
	if baudrate > 0 {
		msg["baudrate"] = baudrate
	}
	return c.sendCommand(msg)
}

// DisconnectPort disconnects a serial port.
func (c *wsClient) DisconnectPort(portID string) error {
	return c.sendCommand(map[string]any{"type": "disconnect_port", "port_id": portID})
}

// ConfigurePort applies a serial config (v1: baudrate only; the server
// defaults databits 8 / stopbit 1 / parity N / timeout 10 and only
// overrides the fields sent, see the configure_port branch in
// web_server.go).
func (c *wsClient) ConfigurePort(portID string, baudrate int) error {
	return c.sendCommand(map[string]any{
		"type":     "configure_port",
		"port_id":  portID,
		"baudrate": baudrate,
	})
}

// ConfigurePortFull applies the full serial config the WebUI port
// cards send (baudrate/databits/stopbit/parity, timeout always 10).
func (c *wsClient) ConfigurePortFull(portID string, baudrate, databits int, stopbit float64, parity string) error {
	return c.sendCommand(map[string]any{
		"type":     "configure_port",
		"port_id":  portID,
		"baudrate": baudrate,
		"databits": databits,
		"stopbit":  stopbit,
		"parity":   parity,
		"timeout":  10,
	})
}

// ConnectPortFull connects with the full config, as the WebUI's
// connectCurrentPort does.
func (c *wsClient) ConnectPortFull(portID string, baudrate, databits int, stopbit float64, parity string) error {
	return c.sendCommand(map[string]any{
		"type":     "connect_port",
		"port_id":  portID,
		"baudrate": baudrate,
		"databits": databits,
		"stopbit":  stopbit,
		"parity":   parity,
		"timeout":  10,
	})
}

// SetPortAlias sets (or, with an empty alias, clears) a port's alias.
func (c *wsClient) SetPortAlias(portID, alias string) error {
	return c.sendCommand(map[string]any{
		"type":    "set_port_alias",
		"port_id": portID,
		"alias":   alias,
	})
}

// AddPort adds a custom port path, as the WebUI's add-port modal does
// (server creates it disconnected with the 115200/8/1/N defaults).
func (c *wsClient) AddPort(portID, alias string) error {
	if err := c.sendCommand(map[string]any{
		"type":    "add_port",
		"port_id": portID,
	}); err != nil {
		return err
	}
	if alias != "" {
		return c.SetPortAlias(portID, alias)
	}
	return nil
}

// GetHistory asks for the server-side command history (answered with
// a "history" message; later changes arrive as "history_update").
func (c *wsClient) GetHistory() error {
	return c.sendCommand(map[string]any{"type": "get_history"})
}

// DeleteCommand removes one history entry by id.
func (c *wsClient) DeleteCommand(id string) error {
	return c.sendCommand(map[string]any{"type": "delete_command", "id": id})
}

// EditCommand replaces one history entry's text.
func (c *wsClient) EditCommand(id, command string) error {
	return c.sendCommand(map[string]any{
		"type":    "edit_command",
		"id":      id,
		"command": command,
	})
}

// MergeHistory merges commands into the server history (import path).
// Commands are plain strings; the server accepts string arrays.
func (c *wsClient) MergeHistory(commands []string) error {
	return c.sendCommand(map[string]any{"type": "merge_history", "commands": commands})
}

// ReplaceHistory replaces the server history with commands.
func (c *wsClient) ReplaceHistory(commands []string) error {
	return c.sendCommand(map[string]any{"type": "replace_history", "commands": commands})
}

// GetWebDAVConfig asks for the stored WebDAV config.
func (c *wsClient) GetWebDAVConfig() error {
	return c.sendCommand(map[string]any{"type": "get_webdav_config"})
}

// SetWebDAVConfig saves the WebDAV config. An empty password keeps
// the stored one; clearPassword wipes it (server semantics).
func (c *wsClient) SetWebDAVConfig(cfg WebDAVConfig, password string, clearPassword bool) error {
	return c.sendCommand(map[string]any{
		"type":           "set_webdav_config",
		"url":            cfg.URL,
		"username":       cfg.Username,
		"password":       password,
		"clear_password": clearPassword,
		"interval":       cfg.Interval,
		"maxBackups":     cfg.MaxBackups,
	})
}

// SetWebDAVConfigWithAck is SetWebDAVConfig with a one-shot ack
// channel: the WebDAV test path pushes the form values, waits for
// this ack (the push to land), and only then tests the saved
// configuration — never testing while the push may still be
// in flight (see testWebDAV in app.go).
func (c *wsClient) SetWebDAVConfigWithAck(cfg WebDAVConfig, password string, clearPassword bool) (<-chan struct{}, error) {
	return c.sendCommandWithAck(map[string]any{
		"type":           "set_webdav_config",
		"url":            cfg.URL,
		"username":       cfg.Username,
		"password":       password,
		"clear_password": clearPassword,
		"interval":       cfg.Interval,
		"maxBackups":     cfg.MaxBackups,
	})
}

// BackupHistoryNow asks the server to upload the current history
// as a new cloud backup now; the result arrives as a
// backup_history_result message (see onBackupResult).
func (c *wsClient) BackupHistoryNow() error {
	return c.sendCommand(map[string]any{"type": "backup_history_now"})
}

// MergeHistoryEntries merges full history entries (with ids) into
// the server history. Unlike MergeHistory (plain strings, which the
// server re-ids), this preserves ids — so the server's deletion
// tombstones can match and skip entries deleted locally. Used for
// cloud-backup imports, where the file's ids are the tombstone keys.
func (c *wsClient) MergeHistoryEntries(entries []HistoryEntry) error {
	return c.sendCommand(map[string]any{"type": "merge_history", "commands": entries})
}

// ReplaceHistoryEntries replaces the server history with full
// entries (ids preserved). The server also clears the tombstones
// of the restored ids, so a restore is a deliberate un-delete.
func (c *wsClient) ReplaceHistoryEntries(entries []HistoryEntry) error {
	return c.sendCommand(map[string]any{"type": "replace_history", "commands": entries})
}

// WriteData sends bytes to a port. The protocol carries data as a JSON
// string, so this is for text input; the server writes it to the port
// verbatim (see the write_data branch in web_server.go).
func (c *wsClient) WriteData(portID, data string) error {
	return c.sendCommand(map[string]any{
		"type":    "write_data",
		"port_id": portID,
		"data":    data,
	})
}

// AddCommand records a command in the server-side history.
func (c *wsClient) AddCommand(portID, command string) error {
	return c.sendCommand(map[string]any{
		"type":    "add_command",
		"port_id": portID,
		"command": command,
	})
}

func (c *wsClient) readLoop() {
	defer c.Close()
	defer func() {
		if c.cb.onDisconnect != nil {
			c.cb.onDisconnect(c)
		}
	}()
	for {
		msgType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		switch msgType {
		case websocket.TextMessage:
			c.handleText(payload)
		case websocket.BinaryMessage:
			c.handleBinary(payload)
		}
	}
}

func (c *wsClient) handleText(payload []byte) {
	var msg struct {
		Type    string              `json:"type"`
		Ports   map[string]portJSON `json:"ports"`
		Port    string              `json:"port"`
		Status  string              `json:"status"`
		Message string              `json:"message"`
		History []HistoryEntry      `json:"history"`
		Config  *WebDAVConfig       `json:"config"`
		Ok      *bool               `json:"ok"`
		Error   string              `json:"error"`
		Ack     string              `json:"_ack"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	// _ack resolves before the type dispatch: the echo rides on
	// the response whatever its type (notably "response" for
	// set_webdav_config, which has no other handler here).
	if msg.Ack != "" {
		c.resolveAck(msg.Ack)
	}
	switch msg.Type {
	case "welcome", "ports":
		if c.cb.onPorts != nil && msg.Ports != nil {
			ports := make(map[string]PortInfo, len(msg.Ports))
			for id, p := range msg.Ports {
				ports[id] = PortInfo{
					ID:          id,
					Status:      p.Status,
					Connected:   p.Connected,
					Alias:       p.Alias,
					Baudrate:    p.Baudrate,
					Databits:    p.Databits,
					Stopbit:     p.Stopbit,
					Parity:      p.Parity,
					Description: p.Description,
				}
			}
			c.cb.onPorts(ports)
		}
	case "port_status":
		if c.cb.onStatus != nil && msg.Port != "" {
			c.cb.onStatus(msg.Port, msg.Status)
		}
	case "error":
		if c.cb.onError != nil {
			c.cb.onError(msg.Message)
		}
	case "history", "history_update":
		if c.cb.onHistory != nil && msg.History != nil {
			c.cb.onHistory(msg.History)
		}
	case "webdav_config":
		if c.cb.onWebDAV != nil && msg.Config != nil {
			c.cb.onWebDAV(*msg.Config)
		}
	case "backup_history_result":
		if c.cb.onBackupResult != nil {
			ok := msg.Ok != nil && *msg.Ok
			c.cb.onBackupResult(ok, msg.Error)
		}
	}
}

// portJSON mirrors one entry of the server's ports map. Numbers decode
// as int where the server sends them (baudrate); unknown fields are
// ignored.
type portJSON struct {
	Status      string  `json:"status"`
	Connected   bool    `json:"connected"`
	Alias       string  `json:"alias"`
	Baudrate    int     `json:"baudrate"`
	Databits    int     `json:"databits"`
	Stopbit     float64 `json:"stopbit"`
	Parity      string  `json:"parity"`
	Description string  `json:"description"`
}

// handleBinary parses the server's serial-data frame:
// [1 byte portID length][portID UTF-8][raw serial bytes]
// (see HandleSerialData in web_server.go). Frames are broadcast to all
// ready clients, so the app filters by selected port.
func (c *wsClient) handleBinary(payload []byte) {
	if len(payload) < 1 {
		return
	}
	n := int(payload[0])
	if len(payload) < 1+n {
		return
	}
	portID := string(payload[1 : 1+n])
	data := payload[1+n:]
	if c.cb.onData != nil {
		c.cb.onData(portID, data)
	}
}

// sortedPortIDs returns the port IDs in stable order for the list UI.
func sortedPortIDs(ports map[string]PortInfo) []string {
	ids := make([]string, 0, len(ports))
	for id := range ports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Close closes the connection; safe to call more than once.
func (c *wsClient) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.conn.Close()
	})
}
