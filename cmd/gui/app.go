package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"github.com/jemyzhang/serial-over-socket-go/pkg/version"
)

// settingsTab values: the settings panel's numbered sections, in
// Tern style: a fixed-width full-height side panel, not a modal.
// The layout has no navigation views at all — the terminal is
// always the window, and ports/history/WebDAV/settings live only
// inside this panel. panelSection "" means "show all sections";
// otherwise only that section is shown (the category icons toggle
// between one section and all).
const (
	settingsTabPorts      = "ports"
	settingsTabHistory    = "history"
	settingsTabSync       = "sync"
	settingsTabAppearance = "appearance"
)

// sectionDef is one numbered settings-panel section: the number
// is fixed per category (not its position in a filtered list),
// icon is the section's parsed SVG (see icons.go), and desc
// carries the searchable terms for the panel's search box.
type sectionDef struct {
	id, label, num, title, desc string
	icon                        *ui.SVG
}

// Icon set — Jemy's SVGs (see icons.go for the full story):
// close, plus, download (plain arrow), search, settings, serial,
// history, appearance, WebDAV, the two split icons, power, the
// status dot, record, day and night are all parsed SVGs drawn
// with ui.Icon in the text color. What stays a text glyph:
// the find-bar ↑/↓ and the local pane's >_. History: the all-herdr geometric set landed
// 2026-11-06 and looked bad on the real device, then the mixed
// text-glyph set (⚙ ⌕ ⤓ × │ ─ 🔌 🕘 ☁ ◐) also looked bad, so
// Jemy handed over SVG artwork — do not "unify" these back to
// text glyphs. Still out: 🔍/⬇/✕/◫/◪/☀/☾.
var settingsSections = []sectionDef{
	{settingsTabPorts, "端口", "01", "端口与连接", "管理串口设备、连接状态、别名与波特率配置。点一行切换当前终端。", iconSerial},
	{settingsTabHistory, "历史", "02", "命令历史", "已保存的命令记录，可直接发送到串口、行内编辑、删除、导出或导入。", iconHistory},
	{settingsTabSync, "同步", "03", "WebDAV 同步", "WebDAV 同步配置与连接测试。", iconWebDAV},
	{settingsTabAppearance, "外观", "04", "外观与终端行为", "界面深浅主题、终端配色、系统字体与字号，以及回滚行数、选中即复制与本地终端复用器。", iconAppearance},
}

// matchSectionText is the panel search predicate: empty query
// matches everything, otherwise a case-insensitive substring
// match over the section's number + label + title + description.
func matchSectionText(query, haystack string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), q)
}

// visiblePanelSections applies the category filter ("" = all) and
// then the search query, in that order.
func visiblePanelSections(query, filter string) []sectionDef {
	var out []sectionDef
	for _, s := range settingsSections {
		if filter != "" && s.id != filter {
			continue
		}
		if !matchSectionText(query, s.num+" "+s.label+" "+s.title+" "+s.desc) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// termSession is one port's resident terminal: created on first
// select (or first data for that port) and kept in the map, so
// switching tabs never rebuilds it and never loses its screen —
// the WebUI's per-port xterm model.
type termSession struct {
	term *terminal.Terminal
	conn *portConn
}

// portDraft holds one port card's editable fields. The WebUI cards
// read their selects straight from the DOM; here the drafts live in
// the app and are initialized from the port's reported config.
type portDraft struct {
	alias, baud, databits, stopbit, parity string
}

func draftFromInfo(info PortInfo) *portDraft {
	baud := info.Baudrate
	if baud == 0 {
		baud = 115200
	}
	databits := info.Databits
	if databits == 0 {
		databits = 8
	}
	stopbit := info.Stopbit
	if stopbit == 0 {
		stopbit = 1
	}
	parity := info.Parity
	if parity == "" {
		parity = "N"
	}
	return &portDraft{
		alias:    info.Alias,
		baud:     strconv.Itoa(baud),
		databits: strconv.Itoa(databits),
		stopbit:  strconv.FormatFloat(stopbit, 'f', -1, 64),
		parity:   parity,
	}
}

// nativeApp holds the whole native GUI state. Methods run on either
// the UI thread (view function, button clicks) or the WS read loop's
// goroutine (ports/data/status/history/webdav/log callbacks); all
// shared state is behind mu, and win.Invalidate asks the UI thread
// for a new frame.
type nativeApp struct {
	mu       sync.Mutex
	client   *wsClient
	embedded localServer

	wsConnected bool
	ports       map[string]PortInfo
	portIDs     []string
	selected    string
	terms       map[string]*termSession
	conn        *portConn // selected session's conn (onData fallback)

	settings Settings
	fontFams []string

	status string

	// Terminal view.
	// termFocusPending asks the next terminal render to give
	// the terminal the keyboard focus (set by a pill-body
	// click, by closing a find bar, and by Enter in the
	// command bar; consumed by the terminal section). UI
	// thread only: both endpoints run inside render, except
	// the command bar's Enter, which sets it from OnTextKey
	// — still on the main thread, between frames.
	termFocusPending bool
	// localTerm is the single global split-pane local terminal
	// (see localterm.go): it is not in terms, so no serial path
	// sees it. localFocusPending asks the next terminal render
	// to focus it (set on open; consumed by the terminal
	// section), mirroring termFocusPending. UI thread only.
	localTerm         *localTerminal
	localFocusPending bool
	// splitLayout / splitPercent are the live split state: the
	// layout ("right" | "below") and the serial pane's share in
	// percent (0,100]. UI thread only, like termFocusPending:
	// the view binds &a.splitPercent to the Split, so dragging
	// the divider writes here directly; a.settings only mirrors
	// them for saving (see syncSplitSettings).
	splitLayout  string
	splitPercent float32
	// lastSizeCols / lastSizeRows are the last serial size
	// reported to the server (0,0 = none yet). mu-guarded:
	// onSerialResize runs on the plugin's tellConn goroutine.
	lastSizeCols, lastSizeRows int
	// findPane is the terminal pane that last had the keyboard
	// focus (findPaneSerial | findPaneLocal): the find bar opens
	// on it. findSerial / findLocal are the panes' find bars
	// (see findbar.go). UI thread only.
	findPane     int
	findSerial   findState
	findLocal    findState
	cmdInput     string
	cmdHistIdx   int  // index into cmdList, -1 = none
	histNavIdx   int  // index into full history for ↑/↓ without dropdown, -1 = none
	cmdHistHide  bool // Esc hid the dropdown; cleared on the next input edit
	prevCmdInput string
	// cmdList is the dropdown's match list, frozen while the user
	// navigates it (see cmdBarView): the input is re-filled from
	// it on every ↑/↓, so recomputing the list from the filled
	// text would move the rows under the selection.
	cmdList []string
	// cmdCaretEnd asks the next command-bar render to put the
	// caret at the end of the input (set by a history fill in
	// cmdBarKey, consumed by cmdBarView). k.SetText only clamps
	// the caret, it does not move it, so a fill from offset 0
	// would otherwise leave the caret at 0. UI thread only.
	cmdCaretEnd bool

	// History view.
	history      []HistoryEntry
	histFilter   string
	histEditID   string // row under inline edit, "" = none
	histEditText string
	clearArmed   bool

	// WebDAV view staging fields (UI thread only; onWebDAV copies
	// into them under mu). Binding inputs straight to a.webdav
	// would race the WS goroutine's whole-struct assignment.
	// wdLast* keeps the last server-pushed config so closing the
	// panel (or switching sections) without saving can reload it.
	wdURL         string
	wdUser        string
	webdavPass    string
	webdavIntF    float64
	webdavMaxF    float64
	wdPassSet     bool
	wdLastURL     string
	wdLastUser    string
	wdLastIntF    float64
	wdLastMaxF    float64
	wdLastPassSet bool

	// Cloud backup list (sync tab), via the server's
	// /api/webdav/list proxy endpoint. Loading/loaded/err are the
	// list's states: the list is fetched once when the sync tab
	// first renders, and again on refresh or after an upload,
	// delete, or import (refreshWebDAVBackupList resets loaded;
	// the next sync-tab render refetches). wdDelArmed is the
	// two-step delete confirm: first click arms that file's
	// button ("确认删除？"), second executes. wdBackingUp guards
	// the 立即备份 button against double sends.
	wdBackups     []webdavBackupFile
	wdListLoading bool
	wdListLoaded  bool
	wdListErr     string
	wdDelArmed    string
	wdBackingUp   bool

	// Client-side recording, per port: a port's entry exists
	// while that port is recording (local file + bytes so far).
	// Guarded by recMu, never by mu: see recording.go for the
	// lock ordering rule.
	recMu      sync.Mutex
	recordings map[string]*localRecording

	// saveDialog is the native save dialog, injectable in tests.
	// Nil means the real mygo.Dialog.Save; tests override it to
	// return a temp path (success) or "" (user cancelled).
	saveDialog func(defPath string) (string, error)

	// desktopPrompted guards the desktop-entry install prompt:
	// once per run (see desktop_linux.go).
	desktopPrompted atomic.Bool

	// pendingSend buffers bytes addressed to a port while it is
	// not connected, and autoConnect marks ports with a
	// connect_port request outstanding (see sendSerial). Both
	// are lazily created under mu: tests build nativeApp
	// literals without them, and a nil map is only ever read
	// outside sendSerial's locked init.
	pendingSend map[string][]byte
	autoConnect map[string]bool

	// Add-port modal.
	addOpen  bool
	addPath  string
	addAlias string
	drafts   map[string]*portDraft

	// Settings panel (Ctrl+,): fixed-width side panel, "" filter =
	// show all sections, otherwise only that section.
	settingsOpen bool
	panelSection string
	panelSearch  string
	// portsTab expansion: at most one config row and one alias row
	// open at a time (empty = none).
	portConfigOpen string
	portAliasOpen  string

	// Settings view staging fields: inputs bind here (UI thread
	// only); the appearance section's own Save button copies them
	// into settings under mu. Binding inputs straight to settings
	// would race the WS goroutine's settings reads.
	setScrollF      float64
	setFontSizeF    float64
	setFontFamily   string
	setTermTheme    string
	setCopyOnSelect bool
	setMultiplexer  string // local terminal multiplexer ("tmux" | "无")
	setTmuxSession  string // tmux session name for a new local terminal

	// Reconnect target (set by main once the base URL is known).
	base string

	win      *mygo.Window
	shutOnce sync.Once

	// Tray state (see tray.go): tray is nil when the tray is
	// disabled or unavailable, and quitting is set by
	// OnBeforeQuit so the close handler lets a real quit through.
	// Both are touched only on the UI thread (setupTray, the
	// OnClose handler, OnBeforeQuit), like the previous client's.
	// sniTray is the Linux StatusNotifierItem tray (see
	// sni_tray_linux.go): non-nil only on Linux when the SNI tray
	// registered, same UI-thread-only rule; it is stopped in
	// shutdown.
	tray     *mygo.Tray
	sniTray  sniTrayHandle
	quitting bool

	// Dot flash driver: a ticker flips breathTarget between dim
	// and full every 900ms; the toolbar dot takes the snapshot
	// target directly (step flash, 1.8s cycle) instead of a smooth
	// Animate interpolation — the interpolation's duration used
	// to match the ticker period, so it never settled and mygo
	// redrew the whole window at display refresh rate while any
	// port was connected. Started once from main; stopped on
	// shutdown. Render reads it through snapshot, never live.
	breathTarget float32
	breathOnce   sync.Once
	breathStop   chan struct{}
}

func newNativeApp(embedded localServer) *nativeApp {
	s := loadSettings()
	a := &nativeApp{
		embedded:     embedded,
		ports:        map[string]PortInfo{},
		terms:        map[string]*termSession{},
		drafts:       map[string]*portDraft{},
		settings:     s,
		panelSection: "",
		fontFams:     listMonoFontFamilies(),
		status:       "就绪",
		cmdHistIdx:   -1,
		histNavIdx:   -1,
		setScrollF:   float64(s.Scrollback),
		setFontSizeF: float64(s.FontSize),
	}
	a.setFontFamily = s.FontFamily
	a.setTermTheme = s.TermTheme
	a.setCopyOnSelect = s.CopyOnSelect
	a.setMultiplexer = s.Multiplexer
	a.setTmuxSession = s.TmuxSession
	a.splitLayout = s.SplitLayout
	a.splitPercent = s.SplitPercent
	a.breathTarget = 1.0
	a.breathStop = make(chan struct{})
	a.webdavIntF = 60
	a.webdavMaxF = 10
	return a
}

// setBase records the server base URL for reconnect.
func (a *nativeApp) setBase(base string) {
	a.mu.Lock()
	a.base = base
	a.mu.Unlock()
}

// appSender routes terminal keystrokes through the app's current WS
// client, so a reconnect (client swap) does not strand the resident
// terminals on a dead connection.
type appSender struct{ a *nativeApp }

func (s *appSender) WriteData(portID, data string) error {
	return s.a.sendSerial(portID, data)
}

// reconnect re-dials the server and swaps the client, mirroring the
// WebUI sidebar's reconnect. Terminals keep working: their Conns
// write through appSender, which follows the new client.
func (a *nativeApp) reconnect() {
	a.mu.Lock()
	base, old := a.base, a.client
	a.mu.Unlock()
	if base == "" {
		a.setStatus("重连需要服务地址（本地模式请重启应用）")
		return
	}
	// Callbacks mirror main.go's wiring exactly: every field must be
	// set here too, or the reconnected client silently drops that
	// round trip (onBackupResult was once missed this way, leaving
	// a manual backup stuck "uploading" forever).
	client, err := dialWS(base, wsCallbacks{
		onPorts:        a.onPorts,
		onStatus:       a.onStatus,
		onData:         a.onData,
		onError:        a.onError,
		onHistory:      a.onHistory,
		onWebDAV:       a.onWebDAV,
		onBackupResult: a.backupHistoryResult,
		onDisconnect:   a.onWSDisconnect,
	})
	if err != nil {
		a.setStatus("重连失败: %v", err)
		return
	}
	// The swap finalizes the old connection's state (ports unknown,
	// any in-flight backup cleared with an unknown-result note) so
	// the post-swap status below can preserve the backup note.
	// Recordings are deliberately NOT touched here: they bind to
	// the GUI process and survive the swap, and frames from the
	// new connection keep appending to the same files.
	a.onWSDisconnect(old)
	a.mu.Lock()
	previousStatus := a.status
	a.mu.Unlock()
	a.attachClient(client)
	if old != nil {
		old.Close()
	}
	if strings.Contains(previousStatus, "备份结果未知") {
		a.setStatus("服务已重连%s", strings.TrimPrefix(previousStatus, "服务连接已断开，请重连"))
	}
	client.start()
	_ = client.GetPorts()
	_ = client.GetHistory()
	_ = client.GetWebDAVConfig()
	// A backup in flight on the old client can never report now:
	// the old connection is closed and the new one carries no
	// pending request. Drop the guard so the button works again,
	// and say the result is unknown — never success, never
	// failure. The server-side upload may still have completed;
	// the list invalidation below makes the next sync-tab visit
	// refetch, which is the source of truth. A late result from
	// the old connection, if one still arrives, is still accepted
	// by backupHistoryResult (it only clears the flag and reports
	// what actually happened). On a failed dial above, the old
	// client stays attached, so its late result may still arrive
	// and the flag is deliberately left alone here.
	a.mu.Lock()
	backingUp := a.wdBackingUp
	a.wdBackingUp = false
	a.mu.Unlock()
	if backingUp {
		a.setStatus("上次 WebDAV 备份结果未知（重连前发出），请以备份列表为准")
		a.refreshWebDAVBackupList()
	}
}

// attachClient wires the WS client after dialing and before the read
// loop starts, so no callback can observe a nil client.
// A stale connection cannot mark its replacement disconnected.
func (a *nativeApp) attachClient(client *wsClient) {
	a.mu.Lock()
	a.client = client
	a.wsConnected = true
	a.mu.Unlock()
	a.setStatus("服务连接成功")
}

// onWSDisconnect handles only the current client. Recordings are
// deliberately untouched here: they bind to the GUI process, not to
// the server connection, so a live recording survives a WS
// disconnect and keeps appending once a new connection delivers
// frames (a gap during the outage is silent by design). A stale
// connection cannot mark its replacement disconnected.
func (a *nativeApp) onWSDisconnect(client *wsClient) {
	a.mu.Lock()
	if client == nil || a.client != client || !a.wsConnected {
		a.mu.Unlock()
		return
	}
	a.wsConnected = false
	for id, port := range a.ports {
		port.Connected = false
		port.Status = "unknown"
		a.ports[id] = port
	}
	backup := a.wdBackingUp
	if backup {
		a.wdBackingUp = false
		a.wdListLoaded = false
		a.wdListErr = ""
		a.wdBackups = nil
	}
	status := "服务连接已断开，请重连"
	if backup {
		status += "；备份结果未知，请查看备份列表"
	}
	a.status = status
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) invalidate() {
	if a.win != nil {
		a.win.Invalidate()
	}
}

func (a *nativeApp) setStatus(format string, args ...any) {
	a.mu.Lock()
	a.status = fmt.Sprintf(format, args...)
	a.mu.Unlock()
	a.invalidate()
}

// replaceStaleStatus swaps the status line from an in-progress line
// for one port to its result line, but only when the line is still
// exactly that in-progress text. The status line is sticky: it
// shows the last message until another replaces it, so without
// this a completed connect/disconnect would leave its
// "正在连接/断开 …" line up forever. Anything newer — a later
// operation, another port's message, an error reason, the
// disconnect auto-stop note — never matches, so it is never
// clobbered.
func (a *nativeApp) replaceStaleStatus(old, new string) {
	a.mu.Lock()
	changed := false
	if a.status == old {
		a.status = new
		changed = true
	}
	a.mu.Unlock()
	if changed {
		a.invalidate()
	}
}

func (a *nativeApp) palette() palette {
	a.mu.Lock()
	dark := a.settings.Dark
	a.mu.Unlock()
	if dark {
		return darkPalette
	}
	return lightPalette
}

// --- WS callbacks (read loop goroutine) ---------------------------

func (a *nativeApp) onPorts(ports map[string]PortInfo) {
	a.mu.Lock()
	a.ports = ports
	a.portIDs = sortedPortIDs(ports)
	var gone []*termSession
	for id, sess := range a.terms {
		if _, ok := ports[id]; !ok {
			gone = append(gone, sess)
			delete(a.terms, id)
			delete(a.drafts, id)
		}
	}
	_, stillThere := ports[a.selected]
	needReselect := a.selected == "" || !stillThere
	next := ""
	if needReselect && len(a.portIDs) > 0 {
		next = a.portIDs[0]
	}
	if !stillThere {
		a.selected = ""
		a.conn = nil
	}
	a.mu.Unlock()
	// Ports the fresh list shows as gone, disconnected, or
	// error stop recording (see stopRecordingOnDisconnect):
	// this is the safety net when a port_status reply was
	// missed. Collect under the lock, act outside it; the
	// helper is a no-op for ports that never recorded.
	var stopRec []string
	a.recMu.Lock()
	recIDs := make([]string, 0, len(a.recordings))
	for id := range a.recordings {
		recIDs = append(recIDs, id)
	}
	a.recMu.Unlock()
	a.mu.Lock()
	for _, id := range recIDs {
		if info, ok := a.ports[id]; !ok || info.Status == "disconnected" || info.Status == "error" {
			stopRec = append(stopRec, id)
		}
	}
	a.mu.Unlock()
	for _, id := range stopRec {
		a.stopRecordingOnDisconnect(id)
	}
	// Settle stale in-progress lines against the fresh list
	// (broadcast safety net, mirroring onStatus): a
	// connected port replaces its connect line, a
	// disconnected/errored port its disconnect line. The
	// exact-match guard keeps anything newer untouched, and
	// this runs after the auto-stop above so a recording
	// port keeps its auto-stop note. Collected under the
	// lock, applied outside it.
	var staleConnect, staleDisconnect []string
	a.mu.Lock()
	for id, info := range a.ports {
		if info.Connected {
			staleConnect = append(staleConnect, id)
		} else if info.Status == "disconnected" || info.Status == "error" {
			staleDisconnect = append(staleDisconnect, id)
		}
	}
	a.mu.Unlock()
	for _, id := range staleConnect {
		a.replaceStaleStatus(fmt.Sprintf("正在连接 %s…", id), fmt.Sprintf("%s 连接成功", id))
	}
	for _, id := range staleDisconnect {
		a.replaceStaleStatus(fmt.Sprintf("正在断开 %s…", id), fmt.Sprintf("%s 已断开", id))
	}
	for _, sess := range gone {
		_ = sess.term.Close()
	}
	if needReselect && next != "" {
		a.selectPort(next)
	}
	// Settle auto-connect buffers against the fresh list: a
	// broadcast is the safety net when a port_status reply was
	// missed (e.g. across a reconnect). A port that vanished
	// drops its buffer; a connected one flushes; an error or
	// disconnected one only clears the outstanding flag, so the
	// next write retries (see sendSerial).
	flush := map[string][]byte{}
	a.mu.Lock()
	for id, buf := range a.pendingSend {
		info, ok := a.ports[id]
		if !ok {
			delete(a.pendingSend, id)
			delete(a.autoConnect, id)
			continue
		}
		switch {
		case info.Connected:
			delete(a.pendingSend, id)
			delete(a.autoConnect, id)
			flush[id] = buf
		case info.Status == "error" || info.Status == "disconnected":
			delete(a.autoConnect, id)
		}
	}
	client := a.client
	a.mu.Unlock()
	for id, buf := range flush {
		if client != nil && len(buf) > 0 {
			_ = client.WriteData(id, string(buf))
		}
	}
	a.invalidate()
}

func (a *nativeApp) onStatus(portID, status string) {
	a.mu.Lock()
	if info, ok := a.ports[portID]; ok {
		info.Status = status
		info.Connected = status == "connected"
		a.ports[portID] = info
	}
	a.mu.Unlock()
	// Settle auto-connect state (see sendSerial): a connect
	// flushes its buffered bytes in order; any other terminal
	// state ends the outstanding request but keeps the buffer,
	// so the next write retries the connect.
	switch status {
	case "connected":
		if buf := a.takePending(portID); len(buf) > 0 {
			a.mu.Lock()
			client := a.client
			a.mu.Unlock()
			if client != nil {
				_ = client.WriteData(portID, string(buf))
			}
			// Without a server the bytes have nowhere to go;
			// takePending already dropped them.
		}
		// The connect is done, so its in-progress line must
		// not linger: replace it with the result — but only
		// if nothing newer has taken the line since (see
		// replaceStaleStatus). This covers both the manual
		// connect button and the sendSerial auto-connect,
		// which both announce with the same line.
		a.replaceStaleStatus(fmt.Sprintf("正在连接 %s…", portID), fmt.Sprintf("%s 连接成功", portID))
	case "error", "disconnected":
		a.mu.Lock()
		delete(a.autoConnect, portID)
		a.mu.Unlock()
		// A port that went away stops recording too (see
		// stopRecordingOnDisconnect); the heuristic error
		// marks in onError deliberately do NOT stop anything
		// — they carry no port, and the port may still be
		// alive, so only this authoritative status does.
		a.stopRecordingOnDisconnect(portID)
		// Replace the disconnect in-progress line after the
		// auto-stop above, so a recording port keeps its
		// more informative auto-stop note (the guard then
		// finds no match and leaves it alone), while a
		// non-recording port gets its plain result line.
		// An error status only ever replaces a disconnect
		// line, never a connect line: failure reasons arrive
		// via onError and must not be overwritten here.
		a.replaceStaleStatus(fmt.Sprintf("正在断开 %s…", portID), fmt.Sprintf("%s 已断开", portID))
	}
	a.invalidate()
}

// onData routes a binary frame to its own port's terminal, whether
// or not that port is the visible tab — this is what keeps
// background tabs' history. The conn fallback preserves the old
// single-terminal shape for callers without a terms entry.
func (a *nativeApp) onData(portID string, data []byte) {
	// Client-side recording follows the port's data stream, not
	// the visible tab: background ports record exactly like the
	// selected one, and the write happens even when no terminal
	// session exists yet for the port.
	a.writeRecording(portID, data)
	a.mu.Lock()
	sess, ok := a.terms[portID]
	conn, selected := a.conn, a.selected
	a.mu.Unlock()
	if ok {
		sess.conn.deliver(data)
		return
	}
	if conn != nil && portID == selected {
		conn.deliver(data)
		return
	}
	// First data for an unseen but known port: give it a resident
	// terminal so its output accumulates before the user ever opens
	// its tab. Frames for ports no longer listed are dropped.
	a.mu.Lock()
	_, known := a.ports[portID]
	a.mu.Unlock()
	if !known {
		return
	}
	a.ensureTerm(portID)
	a.mu.Lock()
	sess, ok = a.terms[portID]
	a.mu.Unlock()
	if ok {
		sess.conn.deliver(data)
	}
}

func (a *nativeApp) onError(msg string) {
	a.setStatus("服务器错误: %s", msg)
	// A failed connect_port arrives as a bare error message with
	// no port attached, so attribute it to the outstanding
	// auto-connects, if any: mark each "error" (red dot by the
	// existing rule) and clear its flag, so the next write
	// retries. The reason itself stays in the status line above
	// — no dialog, no toast. With no auto-connect outstanding
	// (e.g. an explicit button connect failed), the status line
	// alone carries the reason and nothing is re-marked. The
	// attribution is heuristic — an unrelated error landing in
	// the same window mis-marks at most a transient red dot,
	// corrected by the next port_status or ports broadcast —
	// but it is what keeps a failed auto-connect retryable:
	// without it, the flag would stick and no later keystroke
	// would ever re-initiate.
	a.mu.Lock()
	for id := range a.autoConnect {
		delete(a.autoConnect, id)
		if info, ok := a.ports[id]; ok {
			info.Status = "error"
			a.ports[id] = info
		}
	}
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) onHistory(entries []HistoryEntry) {
	a.mu.Lock()
	a.history = entries
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) onWebDAV(cfg WebDAVConfig) {
	a.mu.Lock()
	a.wdURL = cfg.URL
	a.wdUser = cfg.Username
	a.wdPassSet = cfg.PasswordSet
	if cfg.Interval > 0 {
		a.webdavIntF = float64(cfg.Interval)
	}
	if cfg.MaxBackups > 0 {
		a.webdavMaxF = float64(cfg.MaxBackups)
	}
	a.webdavPass = ""
	a.wdLastURL = cfg.URL
	a.wdLastUser = cfg.Username
	a.wdLastPassSet = cfg.PasswordSet
	if cfg.Interval > 0 {
		a.wdLastIntF = float64(cfg.Interval)
	}
	if cfg.MaxBackups > 0 {
		a.wdLastMaxF = float64(cfg.MaxBackups)
	}
	a.mu.Unlock()
	a.invalidate()
}

// portLogState is one port's recording indicator for the
// snapshot: the local file path and how many bytes it holds so
// far. The live state (handle, buffered writer) lives in
// localRecording under recMu; the snapshot copies only these two
// display fields.
type portLogState struct {
	filename string
	size     int64
}

// stopRecordingOnDisconnect ends one port's local recording
// because the port went away: an explicit disconnect, a
// port_status disconnected/error broadcast, or a ports broadcast
// showing it gone. It is a no-op when the port is not recording,
// so a broadcast arriving twice (or after a manual stop) does
// nothing. The local file is flushed and closed, so it never
// resumes appending on reconnect — reconnect therefore never
// resumes a recording. The status line names the port, so a
// multi-port user knows which recording ended. A flush failure
// reports as an error (naming port, path, and the incomplete-file
// risk) instead of the success sentence; the file stays on disk.
func (a *nativeApp) stopRecordingOnDisconnect(portID string) {
	a.mu.Lock()
	display := portID
	if info, ok := a.ports[portID]; ok && info.Alias != "" {
		display = info.Alias
	}
	a.mu.Unlock()
	path, _, ok, flushErr := a.stopLocalRecording(portID)
	if !ok {
		return
	}
	if flushErr != nil {
		// A flush failure must not wear the success sentence:
		// the file stays on disk, but it may be missing data,
		// and the user needs to know that instead of reading
		// "记录已自动停止" as a clean stop.
		a.setStatus("%s 已断开，记录刷盘失败，文件可能缺数据：%s：%v", display, path, flushErr)
	} else {
		a.setStatus("%s 已断开，记录已自动停止", display)
	}
	a.invalidate()
}

// --- terminal lifecycle --------------------------------------------

// copyOnSelectMode maps the CopyOnSelect setting to the plugin's
// CopyOnSelect value: Clipboard when on, None when off.
func copyOnSelectMode(on bool) terminal.CopyOnSelect {
	if on {
		return terminal.CopyOnSelectClipboard
	}
	return terminal.CopyOnSelectNone
}

// ensureTerm returns the resident terminal for a port, creating it
// (with the current font/theme settings) on first use. Creation
// happens outside mu: terminal.New may load the native library.
// The Conn writes through appSender, so it survives reconnects.
func (a *nativeApp) ensureTerm(portID string) *termSession {
	a.mu.Lock()
	if sess, ok := a.terms[portID]; ok {
		a.mu.Unlock()
		return sess
	}
	settings := a.settings
	cols := a.lastSizeCols
	a.mu.Unlock()
	conn := newPortConn(&appSender{a}, portID)
	// Resize reports go to the server (see onSerialResize): the
	// plugin tells Conn the size on every paint-driven change,
	// and the server only ever learns it this way (the dial's
	// terminal_ready carries a fixed 80x24). Set before
	// terminal.New: the plugin cannot report before the first
	// paint, which happens after ensureTerm returns.
	conn.onResize = func(cols, rows int) { a.onSerialResize(portID, cols, rows) }
	term, err := terminal.New(terminal.Options{
		Conn:       conn,
		Font:       terminal.Font{Family: settings.FontFamily, Size: float32(settings.FontSize)},
		Theme:      terminalTheme(settings.TermTheme),
		Scrollback: settings.scrollbackBytes(cols),
		// CopyOnSelect follows the appearance section's switch
		// (default on): a mouse selection is written to the
		// clipboard as it ends, as Ghostty's copy-on-select =
		// clipboard; Select All copies too. The cost is that
		// every mouse selection overwrites the clipboard.
		CopyOnSelect: copyOnSelectMode(settings.CopyOnSelect),
		// No ClipboardRead on purpose: its zero value denies
		// program clipboard reads (OSC 52 "?", OSC 5522), which
		// is the behavior v0.4.8 already had — that version
		// wired no read callback into libghostty-vt at all, so
		// reads went unanswered. Opting into ClipboardAllow
		// would let any program on the serial line read the
		// user's clipboard, which this app never asked for.
		// No OnPaste either: with it unset the plugin pastes
		// the clipboard text to the program by default.
		// No OnScroll callback on purpose: the plugin scrolls the
		// scrollback natively on wheel and Shift+PageUp/Down
		// (stay when scrolled up, follow at the bottom). A previous
		// follow-lock implementation vetoed both here via OnScroll/
		// OnKey, which froze the viewport and made the wheel and
		// keyboard scrolling silently dead while the switch
		// defaulted ON — that veto is deleted, not disabled, and
		// the AutoScroll checkbox with it (see README). The
		// plugin exposes no scroll-to-bottom call, so a "force
		// back to bottom on new data" switch cannot be
		// implemented honestly and is not kept as a dead switch.
		OnKey: func(mods ui.Modifiers, key ui.Key) bool {
			return a.terminalKeyHandled(portID, mods, key)
		},
		// OnSearch feeds the find bar's count (see
		// recordFindStatus): it runs on the main thread, from
		// the view's frame drive. Only the selected port's bar
		// takes statuses: a search ending on a port the user
		// has left must not overwrite the current port's count.
		OnSearch: func(s terminal.SearchStatus) {
			a.mu.Lock()
			selected := a.selected
			a.mu.Unlock()
			if portID != selected {
				return
			}
			a.recordFindStatus(findPaneSerial, s)
		},
	})
	if err != nil {
		a.setStatus("创建终端失败: %v", err)
		return nil
	}
	sess := &termSession{term: term, conn: conn}
	a.mu.Lock()
	if old, ok := a.terms[portID]; ok {
		a.mu.Unlock()
		_ = term.Close()
		return old
	}
	a.terms[portID] = sess
	a.mu.Unlock()
	return sess
}

// onSerialResize reports a serial terminal's new size to the
// server as a terminal_ready resend (the server's case is a
// setReady overwrite). It runs on the plugin's tellConn
// goroutine, so it only touches mu-guarded state and the
// client's writeMu (via sendTerminalReady): no UI state, no
// invalidate. Only the selected port's size is reported (the
// visible pane; every serial pane shares the pane and the font,
// so the size is the same for all of them), and only when it
// differs from the last reported size. The local terminal never
// reports: the server only knows serial ports. The check and
// the record happen atomically under mu, before the send, so a
// failed send is the only way the record and the server can
// diverge — and then the connection is broken anyway (the
// reconnect path re-announces 80x24).
func (a *nativeApp) onSerialResize(portID string, cols, rows int) {
	a.mu.Lock()
	client := a.client
	if portID != a.selected || client == nil || (cols == a.lastSizeCols && rows == a.lastSizeRows) {
		a.mu.Unlock()
		return
	}
	a.lastSizeCols, a.lastSizeRows = cols, rows
	a.mu.Unlock()
	_ = client.sendTerminalReady(rows, cols)
}

// selectPort switches the visible tab: it binds the WS connection
// (select_port, as the WebUI's switchToPort does) and shows the
// port's resident terminal. It never rebuilds the terminal.
func (a *nativeApp) selectPort(portID string) {
	a.mu.Lock()
	if portID == a.selected {
		a.mu.Unlock()
		return
	}
	client := a.client
	a.mu.Unlock()
	if client != nil {
		if err := client.SelectPort(portID); err != nil {
			a.setStatus("选择串口失败: %v", err)
			return
		}
	}
	sess := a.ensureTerm(portID)
	a.mu.Lock()
	a.selected = portID
	if sess != nil {
		a.conn = sess.conn
	} else {
		a.conn = nil
	}
	a.mu.Unlock()
	a.invalidate()
	if a.win != nil {
		a.win.SetTitle("sos-gui — " + portID)
	}
}

// selectAdjacentPort switches the visible terminal to the previous
// (dir < 0) or next (dir > 0) port in pill order, wrapping around:
// Ctrl+Shift+Left/Right, the keyboard equivalent of clicking a
// pill. With fewer than two ports there is nothing to switch to;
// with no port selected, right takes the first and left the last.
// It mirrors a pill click exactly (selectPort plus the focus
// request and the command-dropdown hide), so the new terminal
// takes the keyboard focus. Note the tradeoff, shared with Ctrl+S
// and Ctrl+,: this chord no longer reaches the device as an arrow
// escape sequence.
func (a *nativeApp) selectAdjacentPort(dir int) {
	a.mu.Lock()
	ids := a.portIDs
	cur := a.selected
	a.mu.Unlock()
	if len(ids) < 2 {
		return
	}
	idx := -1
	for i, id := range ids {
		if id == cur {
			idx = i
			break
		}
	}
	var target string
	if idx < 0 {
		if dir < 0 {
			target = ids[len(ids)-1]
		} else {
			target = ids[0]
		}
	} else {
		target = ids[(idx+dir+len(ids))%len(ids)]
	}
	a.selectPort(target)
	a.termFocusPending = true
	a.cmdHistHide = true
}

func (a *nativeApp) closeAllTerms() {
	a.mu.Lock()
	sessions := make([]*termSession, 0, len(a.terms))
	for _, sess := range a.terms {
		sessions = append(sessions, sess)
	}
	a.terms = map[string]*termSession{}
	a.conn = nil
	a.mu.Unlock()
	for _, sess := range sessions {
		_ = sess.term.Close()
	}
}

// terminalKeyHandled decides the app-level terminal key
// interceptions, shared by every resident terminal's OnKey
// callback. Ctrl+S runs the same merged save action as the
// toolbar's "保存输出" button and never reaches the device as
// XOFF: the same rule the TUI and WebUI follow (see PR #14),
// enforced here at the key level, per terminal so it saves the
// focused port's output. Ctrl+, toggles settings even when the
// terminal has the focus and would otherwise swallow the key
// before the frame-level Context.Shortcut in render sees it.
// Ctrl+Shift+Left/Right switches the visible terminal to the
// adjacent port in pill order (the keyboard equivalent of
// clicking a pill) and never reaches the device as an arrow
// escape sequence. Everything else returns false so the plugin
// sees it — notably Shift+PageUp/Down, which the plugin scrolls
// natively (a previous follow-lock veto swallowed those here,
// which made wheel and keyboard scrolling silently dead; see
// README). Ctrl+Shift+C is deliberately NOT intercepted here:
// with DevTools off, the framework no longer preempts it, so it
// reaches the plugin's own copy handler.
func (a *nativeApp) terminalKeyHandled(portID string, mods ui.Modifiers, key ui.Key) bool {
	if mods&ui.Ctrl != 0 && key == ui.KeyS {
		a.saveOutputFor(portID)
		return true
	}
	if mods&ui.Ctrl != 0 && key == ui.KeyComma {
		a.toggleSettings()
		return true
	}
	if mods&ui.Ctrl != 0 && mods&ui.Shift != 0 {
		switch key {
		case ui.KeyLeft:
			a.selectAdjacentPort(-1)
			return true
		case ui.KeyRight:
			a.selectAdjacentPort(1)
			return true
		}
	}
	return false
}

// --- actions (UI thread) -------------------------------------------

// requestConnect sends a single connect_port request for id with
// the port's resolved config. It reports only the request error;
// the connect's outcome arrives asynchronously as a port_status
// message or an error message (see onStatus/onError).
func (a *nativeApp) requestConnect(id string) error {
	a.mu.Lock()
	client := a.client
	draft := a.drafts[id]
	info := a.ports[id]
	a.mu.Unlock()
	if client == nil || id == "" {
		return fmt.Errorf("not connected to server")
	}
	baud, databits, stopbit, parity := configFromDraft(draft, info)
	return client.ConnectPortFull(id, baud, databits, stopbit, parity)
}

func (a *nativeApp) connectPort(id string) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || id == "" {
		return
	}
	if err := a.requestConnect(id); err != nil {
		a.setStatus("连接失败: %v", err)
		return
	}
	a.setStatus("正在连接 %s…", id)
}

// pendingSendCap bounds the per-port auto-connect buffer: input
// typed while a port stays unconnected is low-rate, and anything
// past the cap is dropped rather than growing without bound.
const pendingSendCap = 64 * 1024

// sendSerial writes data to a port, auto-connecting it on first
// use. Terminal keystrokes (via appSender) and command-bar sends
// (via deliverCommand) share it, so typing into a disconnected
// terminal never hits a "[Not connected]" dead end:
//
//   - connected: the bytes go out immediately, as before.
//   - disconnected: the bytes are buffered and one connect_port
//     request is issued. While that request is outstanding —
//     the port's status is "connecting", or this app's
//     autoConnect flag is set — further writes only append to
//     the buffer, never re-initiate.
//   - the buffer flushes in order when onStatus reports the port
//     connected (or a ports broadcast shows it connected).
//   - a failed connect leaves no dialog and no toast: the
//     server's error message already lands in the status line
//     via onError, which also clears the outstanding flag and
//     marks the port "error" (red dot by the existing rule).
//     The buffered bytes are kept, so the next write retries
//     the connect and, on success, flushes everything in order.
//
// A nil return means "accepted": sent now, or buffered for the
// flush. Only a missing server connection is an error. Keystrokes
// only ever arrive from the focused terminal's Conn, so this
// auto-connect is inherently focused-port-only; the explicit
// connect/disconnect buttons and menus are untouched.
func (a *nativeApp) sendSerial(id, data string) error {
	a.mu.Lock()
	client := a.client
	info, known := a.ports[id]
	if client == nil {
		a.mu.Unlock()
		return fmt.Errorf("not connected to server")
	}
	if known && info.Connected {
		a.mu.Unlock()
		return client.WriteData(id, data)
	}
	if a.pendingSend == nil {
		a.pendingSend = map[string][]byte{}
	}
	if a.autoConnect == nil {
		a.autoConnect = map[string]bool{}
	}
	buf := append(a.pendingSend[id], data...)
	if len(buf) > pendingSendCap {
		buf = buf[:pendingSendCap]
	}
	a.pendingSend[id] = buf
	initiate := !a.autoConnect[id] && !(known && info.Status == "connecting")
	if initiate {
		a.autoConnect[id] = true
	}
	a.mu.Unlock()
	if !initiate {
		return nil
	}
	if err := a.requestConnect(id); err != nil {
		// The request itself failed (e.g. the socket broke):
		// clear the flag so the next write retries, and leave
		// the real reason in the status line.
		a.mu.Lock()
		delete(a.autoConnect, id)
		a.mu.Unlock()
		a.setStatus("连接失败: %v", err)
		return nil
	}
	a.setStatus("正在连接 %s…", id)
	return nil
}

// takePending removes and returns a port's buffered bytes, and
// clears its auto-connect flag. Callers flush outside the lock.
func (a *nativeApp) takePending(id string) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	buf := a.pendingSend[id]
	delete(a.pendingSend, id)
	delete(a.autoConnect, id)
	return buf
}

func (a *nativeApp) disconnectPort(id string) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || id == "" {
		return
	}
	if err := client.DisconnectPort(id); err != nil {
		a.setStatus("断开失败: %v", err)
		return
	}
	a.setStatus("正在断开 %s…", id)
}

func (a *nativeApp) connectSelected() { a.connectPort(a.selectedID()) }
func (a *nativeApp) disconnectSelected() {
	a.disconnectPort(a.selectedID())
}

func (a *nativeApp) selectedID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selected
}

// configFromDraft resolves a port's connect/config parameters: the
// card draft wins when present, else the port's reported config,
// else the WebUI card defaults (115200/8/1/N).
func configFromDraft(draft *portDraft, info PortInfo) (baud, databits int, stopbit float64, parity string) {
	baud, databits, stopbit, parity = info.Baudrate, info.Databits, info.Stopbit, info.Parity
	if baud == 0 {
		baud = 115200
	}
	if databits == 0 {
		databits = 8
	}
	if stopbit == 0 {
		stopbit = 1
	}
	if parity == "" {
		parity = "N"
	}
	if draft == nil {
		return
	}
	if v, err := strconv.Atoi(draft.baud); err == nil && v > 0 {
		baud = v
	}
	if v, err := strconv.Atoi(draft.databits); err == nil && v > 0 {
		databits = v
	}
	if v, err := strconv.ParseFloat(draft.stopbit, 64); err == nil && v > 0 {
		stopbit = v
	}
	if draft.parity != "" {
		parity = draft.parity
	}
	return
}

func (a *nativeApp) saveCardConfig(id string) {
	a.mu.Lock()
	client := a.client
	draft := a.drafts[id]
	info := a.ports[id]
	a.mu.Unlock()
	if client == nil || id == "" {
		return
	}
	baud, databits, stopbit, parity := configFromDraft(draft, info)
	if err := client.ConfigurePortFull(id, baud, databits, stopbit, parity); err != nil {
		a.setStatus("保存配置失败: %v", err)
		return
	}
	a.setStatus("端口 %s 配置已保存", id)
}

func (a *nativeApp) saveCardAlias(id string) {
	a.mu.Lock()
	client := a.client
	draft := a.drafts[id]
	a.mu.Unlock()
	if client == nil || id == "" || draft == nil {
		return
	}
	alias := strings.TrimSpace(draft.alias)
	if err := client.SetPortAlias(id, alias); err != nil {
		a.setStatus("保存别名失败: %v", err)
		return
	}
	if alias == "" {
		a.setStatus("端口 %s 别名已清除", id)
	} else {
		a.setStatus("端口 %s 别名已设为 %s", id, alias)
	}
}

// deliverCommand records a command in the server-side history and
// sends it to a port (plus the fixed CRLF line ending), like the
// WebUI's sendTerminalInput (which records history even when the
// port is not connected). The send goes through sendSerial, so
// an unconnected port auto-connects with the command buffered
// instead of the old "[Not connected]" in-terminal dead end.
// It never touches the command bar's input: the bar and the
// history Send button share it, and only the bar clears its own
// input after sending.
func (a *nativeApp) deliverCommand(id, text string) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if id == "" || strings.TrimSpace(text) == "" {
		return
	}
	if client != nil {
		_ = client.AddCommand(id, text)
	}
	// commandWire, not the raw draft: a multi-line draft (or a
	// multi-line history entry) carries "\n" between lines, and
	// the device expects CRLF per line.
	if err := a.sendSerial(id, commandWire(text)+defaultLineEnding); err != nil {
		a.setStatus("发送失败: %v", err)
	}
}

// commandWire converts a command draft to what goes on the wire:
// every line ends with CRLF, never a bare LF. Normalize any CRLF
// already in the text first, so a history entry recorded with
// CRLF does not double it, then "\n" to CRLF. The history
// record keeps the draft as typed.
func commandWire(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
}

// sendCommandBar sends the command bar's line and clears it.
func (a *nativeApp) sendCommandBar() {
	a.mu.Lock()
	id, text := a.selected, a.cmdInput
	a.mu.Unlock()
	a.deliverCommand(id, text)
	a.mu.Lock()
	a.cmdInput = ""
	a.cmdHistIdx = -1
	a.histNavIdx = -1
	a.mu.Unlock()
	// As in the Enter path: the input is empty now, so a fill's
	// caret request is moot — and applying it would yank the
	// focus back to the input.
	a.cmdCaretEnd = false
	a.invalidate()
}

// sendHistoryDirect sends a history entry straight to the selected
// port's serial line — one click, no detour through the command
// bar. The bar's input is left untouched.
func (a *nativeApp) sendHistoryDirect(cmd string) {
	a.deliverCommand(a.selectedID(), cmd)
	a.invalidate()
}

// historyMatches implements the WebUI dropdown rule exactly:
// commands containing the query (case-insensitive), last 10 of the
// matches, reversed.
func historyMatches(query string, entries []HistoryEntry) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Command), q) {
			matches = append(matches, e.Command)
		}
	}
	if len(matches) > 10 {
		matches = matches[len(matches)-10:]
	}
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	return matches
}

// saveOutputFor is the single merged save action behind both the
// toolbar's "保存输出" button and Ctrl+S: it saves the visible
// terminal's screen + scrollback text (the plugin's Text(), the
// native equivalent of the WebUI's selectAll/getSelection). It
// always shows the native save dialog (default file
// serial_<port>_<timestamp>.txt in the home directory); if the
// user cancels or closes the dialog, NOTHING is written — the old
// export fallback that wrote the default path anyway is gone.
// The old Ctrl+S raw-snapshot save is gone with it: the merged
// action saves rendered text only.
func (a *nativeApp) saveOutputFor(portID string) {
	a.mu.Lock()
	sess := a.terms[portID]
	saveDialog := a.saveDialog
	a.mu.Unlock()
	if sess == nil || portID == "" {
		a.setStatus("没有选中的终端，无法保存")
		return
	}
	content := sess.term.Text()
	if content == "" {
		a.setStatus("终端为空，无需保存")
		return
	}
	name := fmt.Sprintf("serial_%s_%s.txt", portID, time.Now().Format("20060102_150405"))
	home, herr := os.UserHomeDir()
	if herr != nil {
		a.setStatus("保存失败: %v", herr)
		return
	}
	defPath := filepath.Join(home, name)
	var path string
	var err error
	if saveDialog != nil {
		path, err = saveDialog(defPath)
	} else {
		path, err = mygo.Dialog.Save(mygo.SaveDialogOptions{DefaultPath: defPath})
	}
	if err != nil || path == "" {
		a.setStatus("已取消保存")
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		a.setStatus("保存失败: %v", err)
		return
	}
	a.setStatus("输出已保存: %s", path)
}

// toggleLoggingFor flips client-side logging for ONE port:
// raw frames are appended to a local file as they arrive, in
// local and remote mode alike (the file is always local now —
// remote mode no longer has a server-side path). Other ports'
// recordings are untouched. Starting pops the native save
// dialog first (default serial_<port>_<timestamp>.log in the
// home directory); cancelling the dialog does NOT start
// recording — there is no fallback here, the program must not
// decide on its own. Stopping never prompts and reports the
// local path and size. Recording binds to the GUI process:
// it survives WS disconnects and reconnects, ends on port
// disconnect, and is flushed and closed on quit; it does not
// resume after a GUI restart.
// Status notes carry the port's display name, so a note
// about one port is never mistaken for another.
func (a *nativeApp) toggleLoggingFor(portID string) {
	if portID == "" {
		return
	}
	a.mu.Lock()
	saveDialog := a.saveDialog
	display := portID
	if info, ok := a.ports[portID]; ok && info.Alias != "" {
		display = info.Alias
	}
	a.mu.Unlock()
	if a.isRecording(portID) {
		path, size, _, flushErr := a.stopLocalRecording(portID)
		if flushErr != nil {
			a.setStatus("%s 停止记录失败：%v", display, flushErr)
		} else {
			a.setStatus("%s 停止记录：%s（%s）", display, path, formatBytes(size))
		}
		a.invalidate()
		return
	}
	name := fmt.Sprintf("serial_%s_%s.log", portID, time.Now().Format("20060102_150405"))
	home, herr := os.UserHomeDir()
	if herr != nil {
		a.setStatus("记录失败: %v", herr)
		return
	}
	defPath := filepath.Join(home, name)
	var path string
	var err error
	if saveDialog != nil {
		path, err = saveDialog(defPath)
	} else {
		path, err = mygo.Dialog.Save(mygo.SaveDialogOptions{DefaultPath: defPath})
	}
	if err != nil || path == "" {
		a.setStatus("已取消记录 %s", display)
		return
	}
	a.mu.Lock()
	if !a.wsConnected {
		a.mu.Unlock()
		a.setStatus("服务已断开，无法开始记录；请先重连")
		return
	}
	err = a.startLocalRecording(portID, path)
	a.mu.Unlock()
	if err != nil {
		a.setStatus("记录操作失败: %v", err)
		return
	}
	a.invalidate()
}

// --- history view actions -----------------------------------------

// startInlineHistoryEdit turns one history row into its inline
// editor (no modal): the row's text becomes an editable field
// with its own save/cancel buttons.
func (a *nativeApp) startInlineHistoryEdit(id, cmd string) {
	a.mu.Lock()
	a.histEditID = id
	a.histEditText = cmd
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) cancelInlineHistoryEdit() {
	a.mu.Lock()
	a.histEditID = ""
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) saveInlineHistoryEdit() {
	a.mu.Lock()
	client := a.client
	id, text := a.histEditID, a.histEditText
	a.mu.Unlock()
	if client == nil || id == "" || strings.TrimSpace(text) == "" {
		return
	}
	if err := client.EditCommand(id, text); err != nil {
		a.setStatus("编辑失败: %v", err)
		return
	}
	a.mu.Lock()
	a.histEditID = ""
	a.mu.Unlock()
	a.invalidate()
}

func (a *nativeApp) deleteHistory(id string) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil {
		return
	}
	if err := client.DeleteCommand(id); err != nil {
		a.setStatus("删除失败: %v", err)
	}
}

// clearHistory deletes every entry one by one: the server has no
// clear-all message, and replace_history with an empty list is a
// no-op there (parseHistoryBytes returns nil for it). The WebUI's
// clear only wipes its local copy; deleting each id clears the
// server copy for real.
func (a *nativeApp) clearHistory() {
	a.mu.Lock()
	client := a.client
	entries := append([]HistoryEntry(nil), a.history...)
	a.clearArmed = false
	a.mu.Unlock()
	if client == nil {
		return
	}
	for _, e := range entries {
		_ = client.DeleteCommand(e.ID)
	}
	a.setStatus("历史已清空（%d 条）", len(entries))
}

func (a *nativeApp) exportHistory() {
	a.mu.Lock()
	entries := append([]HistoryEntry(nil), a.history...)
	a.mu.Unlock()
	if len(entries) == 0 {
		a.setStatus("暂无历史可导出")
		return
	}
	cmds := make([]string, 0, len(entries))
	for _, e := range entries {
		cmds = append(cmds, e.Command)
	}
	doc := map[string]any{
		"version":    "1.0",
		"exportDate": time.Now().Format(time.RFC3339),
		"commands":   cmds,
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	name := fmt.Sprintf("command_history_%s.json", time.Now().Format("2006-01-02"))
	home, herr := os.UserHomeDir()
	if herr != nil {
		a.setStatus("导出失败: %v", herr)
		return
	}
	defPath := filepath.Join(home, name)
	// Same injectable seam as saveOutputFor/toggleLoggingFor, so
	// tests can drive cancel and error without a real dialog.
	// Cancel or dialog error returns WITHOUT writing: falling
	// back to the default name here used to truncate whatever
	// that day's default file already held.
	var path string
	var err error
	if a.saveDialog != nil {
		path, err = a.saveDialog(defPath)
	} else {
		path, err = mygo.Dialog.Save(mygo.SaveDialogOptions{DefaultPath: defPath})
	}
	if err != nil {
		a.setStatus("导出失败: %v", err)
		return
	}
	if path == "" {
		a.setStatus("已取消导出")
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		a.setStatus("导出失败: %v", err)
		return
	}
	a.setStatus("历史已导出: %s", path)
}

func (a *nativeApp) importHistory(merge bool) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{})
	if err != nil || len(paths) == 0 {
		if err != nil {
			a.setStatus("导入失败: %v", err)
		}
		return
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		a.setStatus("导入失败: %v", err)
		return
	}
	var doc struct {
		Commands []json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Commands == nil {
		a.setStatus("导入失败: 文件格式不正确，需要 {\"commands\": [...]}")
		return
	}
	var cmds []string
	for _, raw := range doc.Commands {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			cmds = append(cmds, s)
			continue
		}
		var obj struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.Command != "" {
			cmds = append(cmds, obj.Command)
		}
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil {
		return
	}
	if merge {
		err = client.MergeHistory(cmds)
	} else {
		err = client.ReplaceHistory(cmds)
	}
	if err != nil {
		a.setStatus("导入失败: %v", err)
		return
	}
	mode := "替换"
	if merge {
		mode = "合并"
	}
	a.setStatus("已导入 %d 条历史（%s）", len(cmds), mode)
}

// --- WebDAV actions ------------------------------------------------

func (a *nativeApp) saveWebDAV() {
	a.mu.Lock()
	client := a.client
	cfg := WebDAVConfig{
		URL:        a.wdURL,
		Username:   a.wdUser,
		Interval:   int(a.webdavIntF),
		MaxBackups: int(a.webdavMaxF),
	}
	pass := a.webdavPass
	a.mu.Unlock()
	if client == nil {
		return
	}
	if err := client.SetWebDAVConfig(cfg, pass, false); err != nil {
		a.setStatus("保存 WebDAV 配置失败: %v", err)
		return
	}
	a.setStatus("WebDAV 配置已保存")
}

func (a *nativeApp) clearWebDAVPassword() {
	a.mu.Lock()
	client := a.client
	cfg := WebDAVConfig{URL: a.wdURL, Username: a.wdUser}
	a.mu.Unlock()
	if client == nil {
		return
	}
	if err := client.SetWebDAVConfig(cfg, "", true); err != nil {
		a.setStatus("清除密码失败: %v", err)
		return
	}
	a.setStatus("已保存的 WebDAV 密码已清除")
}

// testWebDAV tests the WebDAV configuration the same way the WebUI
// does: push the form values to the server first, then probe the
// server's SAVED configuration via /api/webdav/test.
//
// The old native test sent a PROPFIND straight from the GUI with
// the form's password field — but the panel never loads the saved
// password (the server never sends it back), so testing without
// re-typing the password always failed 401. Pushing first (empty
// password = keep saved, per the server) makes the test test what
// the server will actually use.
//
// Side effect, identical to the WebUI test: pushing SAVES the form
// values (URL, user, interval, max backups, password if typed).
// The ack wait orders the probe after the push: without it the
// probe could race the push and test the previous configuration.
// Runs off the UI thread; results land on the status line.
func (a *nativeApp) testWebDAV() {
	a.mu.Lock()
	cfg := WebDAVConfig{
		URL:        a.wdURL,
		Username:   a.wdUser,
		Interval:   int(a.webdavIntF),
		MaxBackups: int(a.webdavMaxF),
	}
	pass := a.webdavPass
	client := a.client
	base := a.base
	a.mu.Unlock()
	if strings.TrimSpace(cfg.URL) == "" {
		a.setStatus("请先填写 WebDAV 服务器 URL")
		return
	}
	if client == nil {
		a.setStatus("未连接服务器，无法测试")
		return
	}
	a.setStatus("正在测试 WebDAV 连接…")
	go func() {
		ackCh, err := client.SetWebDAVConfigWithAck(cfg, pass, false)
		if err != nil {
			a.setStatus("WebDAV 配置推送失败: %v", err)
			return
		}
		select {
		case <-ackCh:
		case <-time.After(5 * time.Second):
			a.setStatus("WebDAV 配置推送超时，未测试")
			return
		}
		res, err := testWebDAVEndpoint(base)
		if err != nil {
			a.setStatus("WebDAV 测试失败: %v", err)
			return
		}
		switch {
		case res.ErrMsg != "":
			a.setStatus("WebDAV 测试失败: %s", res.ErrMsg)
		case res.Ok:
			a.setStatus("WebDAV 连接正常（HTTP %d）", res.Status)
		case res.Status == 401 || res.Status == 403:
			a.setStatus("WebDAV 认证失败（HTTP %d），请检查用户名密码", res.Status)
		default:
			a.setStatus("WebDAV 测试异常（HTTP %d）", res.Status)
		}
	}()
}

// backupWebDAVNow uploads the server's current history as a new
// cloud backup right now (the server prunes to MaxBackups, so the
// button path cannot grow the backup set unboundedly). The result
// arrives asynchronously as backup_history_result (see
// backupHistoryResult); the button is guarded against double sends
// while one is in flight.
func (a *nativeApp) backupWebDAVNow() {
	a.mu.Lock()
	client := a.client
	if client == nil {
		a.mu.Unlock()
		return
	}
	if a.wdBackingUp {
		a.mu.Unlock()
		a.setStatus("正在备份，请稍候")
		return
	}
	a.wdBackingUp = true
	a.mu.Unlock()
	if err := client.BackupHistoryNow(); err != nil {
		a.mu.Lock()
		a.wdBackingUp = false
		a.mu.Unlock()
		a.setStatus("备份失败: %v", err)
		return
	}
	a.setStatus("正在上传备份…")
}

// backupHistoryResult is the backup_history_now round trip's landing
// point (wired in main.go and in reconnect). On success the backup list refreshes
// (the new file should appear in it); on failure the reason lands
// on the status line — never silent.
func (a *nativeApp) backupHistoryResult(ok bool, errMsg string) {
	a.mu.Lock()
	a.wdBackingUp = false
	a.mu.Unlock()
	if !ok {
		if errMsg == "" {
			errMsg = "未知错误"
		}
		a.setStatus("WebDAV 备份失败: %s", errMsg)
		return
	}
	a.setStatus("WebDAV 备份已完成")
	a.refreshWebDAVBackupList()
}

// refreshWebDAVBackupList invalidates the cached backup list so the
// next sync-tab render refetches it. It does not fetch directly:
// the sync tab's render trigger owns fetching (once per entry),
// and a list refresh while the user is on another tab simply
// refetches on the next visit.
func (a *nativeApp) refreshWebDAVBackupList() {
	a.mu.Lock()
	a.wdListLoaded = false
	a.wdListErr = ""
	a.wdBackups = nil
	a.mu.Unlock()
	a.invalidate()
}

// fetchWebDAVBackups loads the cloud backup list off the UI thread
// and stores it (or the failure) for the sync tab to render. The
// reason for a failure goes to the status line; the list area
// itself only shows the generic failed state, never a silent blank.
func (a *nativeApp) fetchWebDAVBackups() {
	a.mu.Lock()
	base := a.base
	a.mu.Unlock()
	files, err := fetchWebDAVBackupList(base)
	a.mu.Lock()
	a.wdListLoading = false
	if err != nil {
		a.wdListLoaded = false
		a.wdListErr = err.Error()
	} else {
		a.wdListLoaded = true
		a.wdListErr = ""
		a.wdBackups = files
	}
	a.mu.Unlock()
	if err != nil {
		a.setStatus("备份列表加载失败: %v", err)
	}
	a.invalidate()
}

// importWebDAVBackup downloads one cloud backup and imports it.
// Merge goes through merge_history with ids preserved, so the
// server's deletion tombstones skip entries deleted locally.
// Replace goes through replace_history with ids preserved, and the
// server clears the restored ids' tombstones (a deliberate
// un-delete). The list refreshes afterwards either way.
func (a *nativeApp) importWebDAVBackup(name string, merge bool) {
	a.mu.Lock()
	client := a.client
	base := a.base
	a.mu.Unlock()
	if client == nil {
		return
	}
	mode := "合并"
	if !merge {
		mode = "替换"
	}
	a.setStatus("正在导入云端备份 %s（%s）…", name, mode)
	go func() {
		entries, err := downloadWebDAVBackup(base, name)
		if err != nil {
			a.setStatus("导入失败: %v", err)
			a.refreshWebDAVBackupList()
			return
		}
		if merge {
			err = client.MergeHistoryEntries(entries)
		} else {
			err = client.ReplaceHistoryEntries(entries)
		}
		if err != nil {
			a.setStatus("导入失败: %v", err)
		} else {
			a.setStatus("已导入 %d 条历史（%s）", len(entries), mode)
		}
		a.refreshWebDAVBackupList()
	}()
}

// deleteWebDAVBackupFile deletes one cloud backup file (cloud copy
// only; local history is untouched) and refreshes the list.
func (a *nativeApp) deleteWebDAVBackupFile(name string) {
	a.mu.Lock()
	base := a.base
	a.mu.Unlock()
	a.setStatus("正在删除云端备份 %s…", name)
	go func() {
		if err := deleteWebDAVBackup(base, name); err != nil {
			a.setStatus("删除失败: %v", err)
		} else {
			a.setStatus("已删除云端备份 %s", name)
		}
		a.refreshWebDAVBackupList()
	}()
}

// --- settings actions ----------------------------------------------

// applyAppearanceSettings saves the appearance section's own
// staging fields (theme is separate: its toggle saves at once).
// It is the section's own Save button — there is no panel-wide
// save bar anymore, so every section saves where it lives.
func (a *nativeApp) applyAppearanceSettings() {
	// The dragged percent lives only in a.splitPercent until a
	// save; sync it into the settings struct first, or this
	// save would write the stale saved value over it.
	a.syncSplitSettings()
	a.mu.Lock()
	a.settings.Scrollback = int(a.setScrollF)
	a.settings.FontSize = int(a.setFontSizeF)
	a.settings.FontFamily = a.setFontFamily
	a.settings.TermTheme = a.setTermTheme
	a.settings.CopyOnSelect = a.setCopyOnSelect
	a.settings.Multiplexer = a.setMultiplexer
	a.settings.TmuxSession = a.setTmuxSession
	s := a.settings
	terms := make([]*termSession, 0, len(a.terms))
	for _, sess := range a.terms {
		terms = append(terms, sess)
	}
	// The local terminal shares font, theme and copy-on-select
	// with the serial terminals (creation and live apply); it is
	// not in a.terms, so the loop above never sees it. The
	// multiplexer and session name only affect a NEW local
	// terminal, like scrollback.
	localTerm := a.localTerm
	a.mu.Unlock()
	if err := s.save(); err != nil {
		a.setStatus("保存设置失败: %v", err)
		return
	}
	for _, sess := range terms {
		sess.term.SetFont(terminal.Font{Family: s.FontFamily, Size: float32(s.FontSize)})
		sess.term.SetTheme(terminalTheme(s.TermTheme), nil)
		sess.term.SetCopyOnSelect(copyOnSelectMode(s.CopyOnSelect))
	}
	if localTerm != nil {
		localTerm.term.SetFont(terminal.Font{Family: s.FontFamily, Size: float32(s.FontSize)})
		localTerm.term.SetTheme(terminalTheme(s.TermTheme), nil)
		localTerm.term.SetCopyOnSelect(copyOnSelectMode(s.CopyOnSelect))
	}
	a.setStatus("外观设置已保存（字号/字体/终端配色/选中即复制即时生效；回滚行数、复用器与会话名仅对新终端生效）")
	a.invalidate()
}

func (a *nativeApp) toggleAppTheme() {
	// Sync the live split state first: this save would
	// otherwise write the stale saved percent over a drag.
	a.syncSplitSettings()
	a.mu.Lock()
	a.settings.Dark = !a.settings.Dark
	s := a.settings
	a.mu.Unlock()
	_ = s.save()
	a.invalidate()
}

// --- settings panel (Ctrl+,) --------------------------------------

// toggleSettings is what Ctrl+, runs (both call sites): the
// panel closes with cancelSettings semantics — settingsOpen off
// and un-saved staging dropped, exactly like Esc/× — and opens
// with openSettings semantics, keeping the current category
// filter. openSettings itself stays open-only: the empty state's
// button, in-panel entries, and later pill buttons still use it.
func (a *nativeApp) toggleSettings() {
	a.mu.Lock()
	open := a.settingsOpen
	a.mu.Unlock()
	if open {
		a.cancelSettings()
	} else {
		a.openSettings()
	}
}

// openSettings shows the settings panel, keeping the current
// category filter. It is also the terminal OnKey fallback for
// Ctrl+,: Context.Shortcut (checked in render) does not fire when
// the terminal view itself consumed the key.
func (a *nativeApp) openSettings() {
	a.mu.Lock()
	a.settingsOpen = true
	a.mu.Unlock()
	a.invalidate()
}

// openSettingsSection shows the settings panel filtered to one
// section (the empty state's "前往端口管理" button uses ports).
func (a *nativeApp) openSettingsSection(section string) {
	a.mu.Lock()
	a.panelSection = section
	a.settingsOpen = true
	a.mu.Unlock()
	a.invalidate()
}

// openPortSettings is the pill's settings-icon button action: it opens the
// panel filtered to the ports section with that port's config
// expanded inline. It does not switch the current terminal —
// the button is about this port, not the selected one.
func (a *nativeApp) openPortSettings(portID string) {
	a.mu.Lock()
	a.panelSection = settingsTabPorts
	a.settingsOpen = true
	a.portConfigOpen = portID
	a.mu.Unlock()
	a.invalidate()
}

// reloadSettingsStaging copies the saved settings back into the
// staging fields, dropping un-saved edits.
func (a *nativeApp) reloadSettingsStaging() {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	a.mu.Lock()
	a.setScrollF = float64(s.Scrollback)
	a.setFontSizeF = float64(s.FontSize)
	a.setFontFamily = s.FontFamily
	a.setTermTheme = s.TermTheme
	a.setCopyOnSelect = s.CopyOnSelect
	a.setMultiplexer = s.Multiplexer
	a.setTmuxSession = s.TmuxSession
	a.mu.Unlock()
}

// reloadPanelStaging drops every un-saved panel edit at once:
// appearance staging, WebDAV staging (back to the last
// server-pushed config), port card drafts, expansion state, and
// any inline history edit. It runs on every panel close and on
// every category switch, so an un-saved edit never leaks into
// another section or a later open.
func (a *nativeApp) reloadPanelStaging() {
	a.reloadSettingsStaging()
	a.mu.Lock()
	a.wdURL = a.wdLastURL
	a.wdUser = a.wdLastUser
	a.webdavIntF = a.wdLastIntF
	a.webdavMaxF = a.wdLastMaxF
	a.wdPassSet = a.wdLastPassSet
	a.webdavPass = ""
	a.drafts = map[string]*portDraft{}
	a.portConfigOpen = ""
	a.portAliasOpen = ""
	a.histEditID = ""
	a.mu.Unlock()
}

// cancelSettings closes the panel and drops un-saved staging edits.
func (a *nativeApp) cancelSettings() {
	a.mu.Lock()
	a.settingsOpen = false
	a.mu.Unlock()
	a.reloadPanelStaging()
	a.invalidate()
}

func versionString() string { return version.Get() }

// --- shutdown --------------------------------------------------------

// startBreathing starts the dot-flash ticker (see the field
// comment). The view takes each flipped target directly, so a
// redraw happens once per 900ms tick — never a continuous
// animation, which is what used to keep the whole window
// redrawing at display refresh rate. Invalidate is safe from any
// goroutine.
func (a *nativeApp) startBreathing() {
	a.breathOnce.Do(func() {
		if a.breathStop == nil {
			a.breathStop = make(chan struct{})
		}
		stop := a.breathStop
		go func() {
			t := time.NewTicker(900 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					a.mu.Lock()
					a.breathTarget = nextBreathTarget(a.breathTarget)
					a.mu.Unlock()
					a.invalidate()
				}
			}
		}()
	})
}

func (a *nativeApp) shutdown() {
	a.shutOnce.Do(func() {
		if a.sniTray != nil {
			a.sniTray.stop()
			a.sniTray = nil
		}
		if a.breathStop != nil {
			close(a.breathStop)
		}
		for _, err := range a.closeAllRecordings() {
			log.Printf("recording close failed: %v", err)
		}
		a.closeAllTerms()
		// The local terminal is not in terms: close it too, so
		// its shell dies with the app. (Parking to the tray does
		// not shut down, so the shell keeps running there.)
		// Sync the split state first (closeLocalTerminal does
		// the same); the struct must hold the last dragged
		// position even though no save runs at shutdown.
		a.syncSplitSettings()
		a.closeLocalTerminal()
		a.mu.Lock()
		client, embedded := a.client, a.embedded
		a.mu.Unlock()
		if client != nil {
			client.Close()
		}
		if embedded != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = embedded.Shutdown(ctx)
		}
	})
}
