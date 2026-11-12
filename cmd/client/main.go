package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
	"github.com/jemyzhang/serial-over-socket-go/config"
	"github.com/jemyzhang/serial-over-socket-go/pkg/version"
	"golang.org/x/term"
)

// Global state
var (
	wsConn       *websocket.Conn
	termMutex    sync.RWMutex
	inTerminal   bool
	backlogBuf   []byte
	ports        map[string]PortInfo
	portsReady   chan struct{}
	portUpdateMu sync.Mutex

	// Active terminal port tracking
	currentPortID string
	currentPortMu sync.RWMutex

	// Output processing state (persistent across chunks)
	outputLineBuf     []byte // buffer for current incomplete line
	outputAtLineStart = true // true when next output byte starts a new line
	outputMu          sync.Mutex

	// Rolling snapshot of processed output, dumped to a file by
	// Ctrl+S. Independent of the log file: it always holds the
	// recent past, including output produced before logging
	// was switched on.
	snapshotBuf []byte
	snapshotMu  sync.Mutex

	localCommandHistory []string
	historyMu           sync.RWMutex

	// Output filter & timestamp & logging (shared across terminal sessions)
	filterPattern string
	filterRegex   *regexp.Regexp
	filterNegate  bool // if true, hide matching lines (grep -v mode)
	filterMu      sync.RWMutex
	timestampOn   bool
	timestampMu   sync.RWMutex
	logFile       *os.File
	logFilePath   string
	logMu         sync.Mutex

	// Connection-loss handling. wsReader must NEVER call os.Exit directly:
	// the terminal may be in raw mode (ours or bubbletea's) and os.Exit
	// skips all deferred restores, leaving the user's shell broken.
	connLostCh   = make(chan struct{})
	connLostOnce sync.Once
	// rawOldState is the term.State to restore when OUR raw mode is active
	// (enterTerminalMode); guarded by termMutex.
	rawOldState *term.State
	// activeTeaProg is the currently running bubbletea program, if any.
	activeTeaProg   *tea.Program
	activeTeaProgMu sync.Mutex
)

// handleConnLost is called by wsReader when the server connection drops.
// It restores the terminal to a sane state and wakes the main loop so the
// program exits gracefully (defers run) instead of os.Exit mid-raw-mode.
func handleConnLost() {
	connLostOnce.Do(func() {
		close(connLostCh)

		termMutex.Lock()
		st := rawOldState
		rawOldState = nil
		termMutex.Unlock()
		if st != nil {
			term.Restore(int(os.Stdin.Fd()), st)
			fmt.Print("\r\n")
		}

		activeTeaProgMu.Lock()
		prog := activeTeaProg
		activeTeaProgMu.Unlock()
		if prog != nil {
			prog.Quit() // bubbletea restores its own raw mode on return
		}
	})
}

func connLost() bool {
	select {
	case <-connLostCh:
		return true
	default:
		return false
	}
}

// runTeaProgram runs a bubbletea program while registering it as the active
// one, so handleConnLost can quit it if the server connection drops.
// Programs never nest (sub-editors run after the parent returned), but the
// previous value is restored anyway for safety.
func runTeaProgram(p *tea.Program) (tea.Model, error) {
	activeTeaProgMu.Lock()
	prev := activeTeaProg
	activeTeaProg = p
	activeTeaProgMu.Unlock()
	defer func() {
		activeTeaProgMu.Lock()
		activeTeaProg = prev
		activeTeaProgMu.Unlock()
	}()
	return p.Run()
}

type PortInfo struct {
	Status      string `json:"status"`
	Connected   bool   `json:"connected"`
	Description string `json:"description"` // e.g. "Silicon Labs CP210x USB to UART Bridge Controller 0001"
	ByIDName    string `json:"by_id_name"`  // raw /dev/serial/by-id/ symlink name (stable key)
	Alias       string `json:"alias"`       // user-defined display name
	Baudrate    int    `json:"baudrate"`
	Databits    int    `json:"databits"`
	Stopbit     int    `json:"stopbit"`
	Parity      string `json:"parity"`
}

func main() {
	server := flag.String("server", "127.0.0.1:2025", "Server address (e.g., 127.0.0.1:2025)")
	showVersion := flag.Bool("v", false, "Show version info")
	showVersionLong := flag.Bool("version", false, "Show version info")
	flag.Parse()

	if *showVersion || *showVersionLong {
		version.Print("Serial Over Socket Client (Go Version)")
		os.Exit(0)
	}

	u := url.URL{Scheme: "ws", Host: *server, Path: "/ws"}
	var err error
	wsConn, _, err = websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		fmt.Printf("Cannot connect to server %s: %v\n", *server, err)
		os.Exit(1)
	}
	defer wsConn.Close()

	portsReady = make(chan struct{}, 1)
	ports = make(map[string]PortInfo)

	go wsReader()

	termWidth, termHeight, _ := term.GetSize(int(os.Stdout.Fd()))
	if termWidth == 0 {
		termWidth = 80
		termHeight = 24
	}

	wsConn.WriteJSON(map[string]interface{}{
		"type": "terminal_ready",
		"rows": termHeight,
		"cols": termWidth,
	})
	wsConn.WriteJSON(map[string]interface{}{"type": "get_history"})

	// Don't hang here forever if the server dies before the first
	// port list arrives.
	select {
	case <-portsReady:
	case <-connLostCh:
		fmt.Println("Connection to server lost before initialisation. Exiting.")
		return
	}

	var activePort string

mainLoop:
	for {
		// enterTerminalMode returns false on Ctrl+T (back to menu) but
		// also when the connection died mid-session — don't loop back
		// into the menu when the server is gone.
		if connLost() {
			fmt.Println("\r\nConnection to server lost. Exiting.")
			break mainLoop
		}
		m := initialModel(activePort)
		p := tea.NewProgram(m, tea.WithAltScreen())

		finalModel, err := runTeaProgram(p)

		if err != nil {
			fmt.Println("Error running menu:", err)
			os.Exit(1)
		}

		// The connection may have dropped while the menu was up
		// (wsReader quits the program via handleConnLost).
		if connLost() {
			fmt.Println("\r\nConnection to server lost. Exiting.")
			break mainLoop
		}

		var fm model
		if modelInst, ok := finalModel.(model); ok {
			if modelInst.quitting {
				break
			}
			fm = modelInst
		}

		if fm.action == "resume" && activePort != "" {
			portUpdateMu.Lock()
			pInfo := ports[activePort]
			portUpdateMu.Unlock()
			baud, data, stop, par := resolvePortConfig(activePort, pInfo)
			if enterTerminalMode(activePort, baud, data, stop, par, false) {
				break
			}
			wsConn.WriteJSON(map[string]interface{}{"type": "get_ports"})
			continue
		}

		if fm.action == "disconnect" && activePort != "" {
			wsConn.WriteJSON(map[string]interface{}{
				"type":    "disconnect_port",
				"port_id": activePort,
			})
			activePort = ""
			continue
		}

		// 'c' → open Port Settings for the selected/cursor port
		if fm.action == "config" && fm.configPort != "" {
			result := runPortSettings(fm.configPort)
			if result != nil {
				// Connect with the newly configured settings
				activePort = fm.configPort
				if enterTerminalMode(fm.configPort, result.Baudrate, result.Databits, result.Stopbit, result.Parity, true) {
					break
				}
				wsConn.WriteJSON(map[string]interface{}{"type": "get_ports"})
			}
			continue
		}

		if fm.selected == "" {
			break
		}

		// Direct Enter on a port: use saved profile or default
		activePort = fm.selected
		portUpdateMu.Lock()
		pInfo := ports[fm.selected]
		portUpdateMu.Unlock()
		baud, data, stop, par := resolvePortConfig(fm.selected, pInfo)
		if enterTerminalMode(fm.selected, baud, data, stop, par, true) {
			break
		}
		wsConn.WriteJSON(map[string]interface{}{"type": "get_ports"})
	}

	// Close log file if open
	logMu.Lock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
	logMu.Unlock()

	fmt.Println("Goodbye!")
}

func wsReader() {
	for {
		msgType, msg, err := wsConn.ReadMessage()
		if err != nil {
			fmt.Println("\r\nConnection to server lost.")
			handleConnLost()
			return
		}

		if msgType == websocket.BinaryMessage {
			if len(msg) < 1 {
				continue
			}
			portIDLen := int(msg[0])
			if len(msg) < 1+portIDLen {
				continue
			}
			msgPortID := string(msg[1 : 1+portIDLen])
			rawBytes := msg[1+portIDLen:]

			currentPortMu.RLock()
			activePort := currentPortID
			currentPortMu.RUnlock()

			// Only process and display bytes if they belong to current active terminal port
			if activePort != "" && msgPortID != activePort {
				continue
			}

			termMutex.RLock()
			active := inTerminal
			termMutex.RUnlock()

			processedLines := processOutput(rawBytes)

			if active {
				os.Stdout.Write(processedLines)
			} else {
				termMutex.Lock()
				backlogBuf = append(backlogBuf, processedLines...)
				// Cap the backlog: a long menu session with heavy serial
				// output must not grow memory without bound.
				const maxBacklog = 1 << 20 // 1 MiB
				if len(backlogBuf) > maxBacklog {
					backlogBuf = append(backlogBuf[:0], backlogBuf[len(backlogBuf)-maxBacklog:]...)
				}
				termMutex.Unlock()
			}

			// Write to log file
			logMu.Lock()
			if logFile != nil {
				logFile.Write(processedLines)
			}
			logMu.Unlock()

			// Tee into the rolling snapshot buffer (Ctrl+S dumps
			// it to a file on demand). Same cap as the backlog.
			snapshotMu.Lock()
			snapshotBuf = append(snapshotBuf, processedLines...)
			const maxSnapshotBuf = 1 << 20 // 1 MiB
			if len(snapshotBuf) > maxSnapshotBuf {
				snapshotBuf = append(snapshotBuf[:0], snapshotBuf[len(snapshotBuf)-maxSnapshotBuf:]...)
			}
			snapshotMu.Unlock()
		} else if msgType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(msg, &data); err == nil {
				if data["type"] == "welcome" || data["type"] == "ports" || data["type"] == "port_status" {
					if pData, ok := data["ports"].(map[string]interface{}); ok {
						// Build a fresh map so that ports removed on the server
						// are also removed on the client side.
						newPorts := make(map[string]PortInfo, len(pData))
						for k, v := range pData {
							if vm, ok := v.(map[string]interface{}); ok {
								// All assertions are comma-ok: a malformed
								// server message must not panic the client.
								pi := PortInfo{}
								if s, ok := vm["status"].(string); ok {
									pi.Status = s
								}
								if b, ok := vm["connected"].(bool); ok {
									pi.Connected = b
								}
								if s, ok := vm["description"].(string); ok {
									pi.Description = s
								}
								if s, ok := vm["by_id_name"].(string); ok {
									pi.ByIDName = s
								}
								if s, ok := vm["alias"].(string); ok {
									pi.Alias = s
								}
								if f, ok := vm["baudrate"].(float64); ok {
									pi.Baudrate = int(f)
								}
								if f, ok := vm["databits"].(float64); ok {
									pi.Databits = int(f)
								}
								if f, ok := vm["stopbit"].(float64); ok {
									pi.Stopbit = int(f)
								}
								if s, ok := vm["parity"].(string); ok {
									pi.Parity = s
								}
								newPorts[k] = pi
							}
						}
						portUpdateMu.Lock()
						ports = newPorts // replace entirely — removes stale entries
						portUpdateMu.Unlock()

						select {
						case portsReady <- struct{}{}:
						default:
						}
					}

					if data["type"] == "port_status" {
						portId, ok1 := data["port"].(string)
						status, ok2 := data["status"].(string)
						if ok1 && ok2 {
							portUpdateMu.Lock()
							pInfo := ports[portId]
							pInfo.Status = status
							pInfo.Connected = (status == "connected")
							ports[portId] = pInfo
							portUpdateMu.Unlock()

							select {
							case portsReady <- struct{}{}:
							default:
							}
						}
					}
				}

				if data["type"] == "history" || data["type"] == "history_update" {
					if hist, ok := data["history"].([]interface{}); ok {
						var newHist []string
						for _, v := range hist {
							if m, ok := v.(map[string]interface{}); ok {
								if s, ok := m["command"].(string); ok {
									newHist = append(newHist, s)
								}
							} else if s, ok := v.(string); ok {
								newHist = append(newHist, s)
							}
						}
						historyMu.Lock()
						localCommandHistory = newHist
						historyMu.Unlock()
					}
				}
			}
		}
	}
}

// processOutput applies timestamp and filter to raw terminal bytes
// processOutput applies timestamp and/or filter to a raw chunk from the serial port.
// State (partial line buffer, line-start flag) persists across chunk boundaries.
func processOutput(raw []byte) []byte {
	timestampMu.RLock()
	ts := timestampOn
	timestampMu.RUnlock()

	filterMu.RLock()
	rx := filterRegex
	neg := filterNegate
	filterMu.RUnlock()

	if !ts && rx == nil {
		return raw
	}

	outputMu.Lock()
	defer outputMu.Unlock()

	var out []byte

	for i := 0; i < len(raw); i++ {
		b := raw[i]

		if b == '\n' {
			// Line complete — decide whether to emit it
			line := string(outputLineBuf)
			outputLineBuf = outputLineBuf[:0]

			// Apply filter on the complete line (neg = grep -v)
			if rx != nil {
				matched := rx.MatchString(line)
				if neg == matched {
					// discard: either (negate && matched) or (!negate && !matched)
					outputAtLineStart = true
					continue
				}
			}

			// Prepend timestamp only at the start of a new line
			if ts && outputAtLineStart && len(strings.TrimSpace(line)) > 0 {
				now := time.Now().Format("15:04:05.000")
				tsTag := "\033[48;5;33m\033[97m " + now + " \033[0m "
				out = append(out, []byte(tsTag)...)
			}

			out = append(out, []byte(line)...)
			out = append(out, '\n')
			outputAtLineStart = true

		} else {
			// Mid-line byte
			if rx != nil {
				// Filter active: buffer until '\n' (can't know if line matches yet)
				if outputAtLineStart {
					outputAtLineStart = false
				}
				outputLineBuf = append(outputLineBuf, b)
				// Cap the line buffer: a device that never sends '\n'
				// must not grow memory without bound; emit the overlong
				// chunk unfiltered instead.
				const maxLineBuf = 1 << 20 // 1 MiB
				if len(outputLineBuf) > maxLineBuf {
					out = append(out, outputLineBuf...)
					outputLineBuf = outputLineBuf[:0]
				}
			} else {
				// No filter: pass through immediately; add timestamp at line start
				if outputAtLineStart && ts && b != '\r' {
					now := time.Now().Format("15:04:05.000")
					tsTag := "\033[48;5;33m\033[97m " + now + " \033[0m "
					out = append(out, []byte(tsTag)...)
					outputAtLineStart = false
				}
				out = append(out, b)
			}
		}
	}

	return out
}

// flushBacklog marks the terminal active again and replays any
// serial output that accumulated while a menu or sub-editor
// owned the screen.
func flushBacklog() {
	termMutex.Lock()
	inTerminal = true
	if len(backlogBuf) > 0 {
		os.Stdout.Write(backlogBuf)
		backlogBuf = nil
	}
	termMutex.Unlock()
}

func enterTerminalMode(port string, baudrate int, databits int, stopbit int, parity string, sendConfig bool) bool {
	currentPortMu.Lock()
	currentPortID = port
	currentPortMu.Unlock()
	defer func() {
		currentPortMu.Lock()
		currentPortID = ""
		currentPortMu.Unlock()
	}()

	if sendConfig {
		err := wsConn.WriteJSON(map[string]interface{}{
			"type":     "connect_port",
			"port_id":  port,
			"baudrate": baudrate,
			"databits": databits,
			"stopbit":  stopbit,
			"parity":   parity,
		})
		if err != nil {
			fmt.Printf("Failed to request connection to %s\n", port)
			return false
		}
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return false
	}
	// Register for handleConnLost: if the connection drops while we're raw,
	// wsReader restores this state before waking the main loop.
	termMutex.Lock()
	rawOldState = oldState
	termMutex.Unlock()
	defer func() {
		term.Restore(int(os.Stdin.Fd()), oldState)
		termMutex.Lock()
		rawOldState = nil
		termMutex.Unlock()
	}()

	// One-time banner: the connected port and the hotkey list.
	// (Ctrl+? shows the full help at any time.)
	logMu.Lock()
	logStatus := ""
	if logFile != nil {
		logStatus = " \033[1;33m[LOG:" + logFilePath + "]\033[0m"
	}
	logMu.Unlock()

	fmt.Printf("\r\n\033[1;36m[SOS Client] Connected to %s.%s\r\n", port, logStatus)
	fmt.Printf("\033[1;36mCtrl+V Command · Ctrl+F Filter · Ctrl+L Log · Ctrl+S Save Buffer · Ctrl+\\ Timestamp · Ctrl+T Port Settings · Ctrl+Q Quit · Ctrl+? Full Help\033[0m\r\n")

	flushBacklog()

	defer func() {
		termMutex.Lock()
		inTerminal = false
		termMutex.Unlock()
	}()

	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			break
		}

		skipWrite := false
		for i := 0; i < n; i++ {
			handled := false
			switch buf[i] {
			case 0x14: // Ctrl+T → palette
				return false
			case 0x11: // Ctrl+Q → quit client
				return true
			case 0x13: // Ctrl+S → save output buffer to file.
				// Never forwarded: 0x13 is XOFF, it would freeze
				// the device's output until an XON arrived.
				body, ok := saveOutputBuffer()
				title := "💾 Buffer Saved"
				if !ok {
					title = "⚠ Save Failed"
				}
				oldState = runNoticeUI(oldState, title, body, !ok)
				flushBacklog()
				handled = true
			case 0x16: // Ctrl+V → command input
				cmdStr, newState := runLineEditor(oldState)
				oldState = newState
				flushBacklog()
				if cmdStr != "" {
					wsConn.WriteJSON(map[string]interface{}{
						"type":    "add_command",
						"command": cmdStr,
						"data":    cmdStr + "\n",
					})
				}
				handled = true
			case 0x06: // Ctrl+F → output filter
				oldState = runFilterEditor(oldState)
				flushBacklog()
				handled = true
			case 0x0C: // Ctrl+L → log to file
				logMu.Lock()
				isActive := logFile != nil
				logMu.Unlock()

				if isActive {
					logMu.Lock()
					if logFile != nil {
						logFile.Close()
						logFile = nil
						logFilePath = ""
					}
					logMu.Unlock()
				} else {
					oldState = runLogEditor(oldState)
					flushBacklog()
				}
				handled = true
			case 0x1F, 0x08: // Ctrl+? (0x1F often) or Ctrl+H (0x08) → help
				oldState = runHelpUI(oldState)
				flushBacklog()
				handled = true
			case 0x1C: // Ctrl+\ → toggle timestamp
				timestampMu.Lock()
				timestampOn = !timestampOn
				timestampMu.Unlock()
				handled = true
			}
			if handled {
				skipWrite = true
				break
			}
		}

		if n > 0 && !skipWrite {
			// If the connection is gone there's no point staying in the
			// loop: break so the deferred terminal restore runs and the
			// main loop can exit gracefully via connLostCh.
			if err := wsConn.WriteJSON(map[string]interface{}{
				"type": "write_data",
				"data": string(buf[:n]),
			}); err != nil {
				break
			}
		}
	}
	return false
}

// saveOutputBuffer writes the rolling output snapshot to
// ~/serialport_buffer_<timestamp>.txt — the same naming scheme
// as the Ctrl+L log files — and returns a message describing
// the outcome for the notice popup (ok=false on failure).
func saveOutputBuffer() (string, bool) {
	snapshotMu.Lock()
	data := append([]byte(nil), snapshotBuf...)
	snapshotMu.Unlock()

	home, _ := os.UserHomeDir()
	path := filepath.Join(home, fmt.Sprintf("serialport_buffer_%s.txt", time.Now().Format("20060102_150405")))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Sprintf("Failed to save buffer: %v", err), false
	}
	return fmt.Sprintf("%s\n%d bytes", path, len(data)), true
}

// ------ Transient notice popup ------

type noticeModel struct {
	title  string
	body   string
	isErr  bool
	width  int
	height int
}

type noticeTimeoutMsg struct{}

func (m noticeModel) Init() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return noticeTimeoutMsg{} })
}

func (m noticeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		return m, tea.Quit
	case noticeTimeoutMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m noticeModel) View() string {
	bg := "#27864d"
	if m.isErr {
		bg = "#c0392b"
	}
	title := lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("#FAFAFA")).
		Background(lipgloss.Color(bg)).
		Padding(0, 1).Render(m.title)
	popup := popupStyle.Render(title + "\n\n" + m.body + "\n")
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

// runNoticeUI shows a transient popup that dismisses itself
// after 3 seconds (or on any keypress), following the same
// suspend/restore choreography as the other editors.
func runNoticeUI(oldState *term.State, title, body string, isErr bool) *term.State {
	term.Restore(int(os.Stdin.Fd()), oldState)
	termMutex.Lock()
	inTerminal = false
	termMutex.Unlock()

	p := tea.NewProgram(noticeModel{title: title, body: body, isErr: isErr}, tea.WithAltScreen())
	runTeaProgram(p)

	newState, _ := term.MakeRaw(int(os.Stdin.Fd()))
	return newState
}

// ------ BubbleTea Menu Implementation ------

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#ea6645")).
			Padding(0, 1).
			MarginBottom(1)

	popupStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#ea6645")).
			Padding(1, 3).
			Align(lipgloss.Center)

	itemStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	selectedItemStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#10b981")).
				Bold(true)

	statusConnectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#10b981"))

	statusDisconnectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#f43f5e"))

	descStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("33")).
			PaddingLeft(4)

	configSummaryStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("245")).
				PaddingLeft(4)

	baudRates    = []int{9600, 14400, 19200, 38400, 57600, 115200, 230400, 460800, 921600}
	dataBitsList = []int{5, 6, 7, 8}
	stopBitsList = []int{1, 2}
	parities     = []string{"N", "O", "E", "M", "S"}
)

type portsMsg map[string]PortInfo

type model struct {
	portList   []string
	cursor     int
	selected   string
	quitting   bool
	action     string
	activePort string
	configPort string // port to open settings for ('c' key)

	width  int
	height int
}

func initialModel(activePort string) model {
	portUpdateMu.Lock()
	ptList := make([]string, 0, len(ports))
	for k := range ports {
		ptList = append(ptList, k)
	}
	portUpdateMu.Unlock()

	sort.Strings(ptList)

	// Position cursor on the currently active port so the user sees it
	// highlighted immediately when the palette opens.
	cursor := 0
	for i, p := range ptList {
		if p == activePort {
			cursor = i
			break
		}
	}

	return model{
		activePort: activePort,
		portList:   ptList,
		cursor:     cursor,
	}
}

// resolvePortConfig returns the best serial config for a port.
// Look-up priority:
//  1. Saved PortProfile keyed by device description (e.g. "Silicon Labs CP2102 …")
//  2. Saved PortProfile keyed by portID node name (e.g. "ttyUSB0") – fallback
//     when the device has no entry in /dev/serial/by-id/.
//  3. Server-reported live config (current baudrate/etc from the serial manager).
//  4. Hard default (115200 8N1 N).
func resolvePortConfig(portID string, pi PortInfo) (baudrate, databits, stopbit int, parity string) {
	cfg, cfgErr := config.LoadConfig(config.GetConfigPath())

	if cfgErr == nil && cfg.PortProfiles != nil {
		// 1. By description
		if pi.Description != "" {
			if profile, ok := cfg.PortProfiles[pi.Description]; ok && profile != nil {
				return profile.Baudrate, profile.Databits, profile.Stopbit, profile.Parity
			}
		}
		// 2. By port node ID
		if portID != "" {
			if profile, ok := cfg.PortProfiles[portID]; ok && profile != nil {
				return profile.Baudrate, profile.Databits, profile.Stopbit, profile.Parity
			}
		}
	}

	// 3. Server-reported live config
	if pi.Baudrate > 0 {
		return pi.Baudrate, pi.Databits, pi.Stopbit, pi.Parity
	}

	// 4. Hard default
	return 115200, 8, 1, "N"
}

func updatePortsLoop() tea.Cmd {
	return func() tea.Msg {
		<-portsReady
		portUpdateMu.Lock()
		snapshot := make(map[string]PortInfo)
		for k, v := range ports {
			snapshot[k] = v
		}
		portUpdateMu.Unlock()
		return portsMsg(snapshot)
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(updatePortsLoop())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit

		case "esc":
			if m.activePort != "" {
				m.action = "resume"
				return m, tea.Quit
			}
			m.quitting = true
			return m, tea.Quit

		case "d":
			if m.activePort != "" {
				m.action = "disconnect"
				return m, tea.Quit
			}

		case "c":
			// Open Port Settings for the currently highlighted port
			if m.cursor < len(m.portList) {
				m.configPort = m.portList[m.cursor]
				m.action = "config"
				return m, tea.Quit
			}

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(m.portList) - 1
			}

		case "down", "j":
			maxCursor := len(m.portList) - 1
			if maxCursor < 0 {
				maxCursor = 0
			}
			if m.cursor < maxCursor {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "enter":
			if m.cursor < len(m.portList) {
				m.selected = m.portList[m.cursor]
			}
			if m.selected != "" {
				return m, tea.Quit
			}

		case "r":
			if wsConn != nil {
				wsConn.WriteJSON(map[string]interface{}{"type": "get_ports"})
			}
		}

	case portsMsg:
		ptList := make([]string, 0, len(msg))
		for k := range msg {
			ptList = append(ptList, k)
		}
		sort.Strings(ptList)

		m.portList = ptList
		if m.cursor >= len(m.portList) && len(m.portList) > 0 {
			m.cursor = len(m.portList) - 1
		}

		return m, updatePortsLoop()
	}

	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	str := titleStyle.Render(" ⚡ Select Port ") + "\n\n"

	if m.activePort != "" {
		str += statusConnectedStyle.Render(fmt.Sprintf("● Active Session: %s", m.activePort)) + "\n"
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("Press [Esc] to resume, [d] to disconnect") + "\n\n"
	}

	if len(m.portList) == 0 {
		str += "  No serial ports available.\n"
	}

	for i, port := range m.portList {
		portUpdateMu.Lock()
		st := ports[port]
		portUpdateMu.Unlock()

		cursor := "  "

		// Status badge
		var statusText string
		if st.Connected {
			statusText = statusConnectedStyle.Render(" ● Connected")
		} else {
			statusText = statusDisconnectedStyle.Render(" ● " + st.Status)
		}

		// Port display name: alias (portID) or just portID
		portDisplayName := port
		if st.Alias != "" {
			portDisplayName = st.Alias + " (" + port + ")"
		}

		// Port node name + status
		portLine := portDisplayName + statusText
		if m.cursor == i {
			cursor = "> "
			str += fmt.Sprintf("%s%s\n", cursor, selectedItemStyle.Render(portLine))
		} else {
			str += fmt.Sprintf("%s%s\n", cursor, itemStyle.Render(portLine))
		}

		// Device description (sub-line)
		if st.Description != "" {
			str += descStyle.Render("⬡ "+st.Description) + "\n"
		}

		// Current config summary
		baud, data, stop, par := resolvePortConfig(port, st)
		cfgStr := fmt.Sprintf("%d baud  %dN%d  parity:%s", baud, data, stop, par)
		if m.cursor == i {
			str += lipgloss.NewStyle().Foreground(lipgloss.Color("245")).PaddingLeft(4).Render(cfgStr) + "\n"
		} else {
			str += configSummaryStyle.Render(cfgStr) + "\n"
		}
		str += "\n"
	}

	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	str += hintStyle.Render("↑k/↓j: navigate • Enter: connect • c: settings • r: refresh • q: quit") + "\n"
	if m.activePort != "" {
		str += hintStyle.Render("Esc: resume session • d: disconnect") + "\n"
	}

	popup := popupStyle.Render(str)

	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

// ------ Enhanced Line Editor with Tab switching & history list ------

type editorFocus int

const (
	focusInput editorFocus = iota
	focusList
)

type lineEditorModel struct {
	textArea     textarea.Model
	suggestions  []string
	submitted    string
	quitting     bool
	historyIndex int // 0 = not navigating
	typingBuffer string
	focus        editorFocus
	listCursor   int

	width  int
	height int
}

func initialLineEditorModel() lineEditorModel {
	ta := textarea.New()
	ta.Placeholder = "Enter command...\n(Allows multiple lines)"
	ta.Focus()
	ta.CharLimit = 1000
	ta.SetWidth(60)
	ta.SetHeight(6)
	ta.ShowLineNumbers = true

	historyMu.RLock()
	hist := make([]string, len(localCommandHistory))
	copy(hist, localCommandHistory)
	historyMu.RUnlock()

	return lineEditorModel{
		textArea:    ta,
		suggestions: hist,
		focus:       focusInput,
	}
}

func (m lineEditorModel) getFilteredSuggestions(prefix string) []string {
	var matched []string
	if strings.TrimSpace(prefix) != "" {
		for _, s := range m.suggestions {
			if strings.Contains(strings.ToUpper(s), strings.ToUpper(prefix)) {
				matched = append(matched, s)
			}
		}
	} else {
		matched = m.suggestions
	}

	seen := make(map[string]bool)
	var uniqueMatched []string
	for _, s := range matched {
		if !seen[s] {
			seen[s] = true
			uniqueMatched = append(uniqueMatched, s)
		}
	}
	return uniqueMatched
}

func (m lineEditorModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m lineEditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			return m, tea.Quit

		case tea.KeyTab:
			// Switch focus between input and list
			if m.focus == focusInput {
				m.focus = focusList
				m.textArea.Blur()
				// Save current text as typing buffer for filtering
				m.typingBuffer = m.textArea.Value()
				// Initialize list cursor to top and immediately preview first item
				m.listCursor = 0
				filtered := m.getFilteredSuggestions(m.typingBuffer)
				if len(filtered) > 0 {
					m.textArea.SetValue(filtered[0])
					m.textArea.CursorEnd()
				}
			} else {
				// Switching back to input: restore the original typed content
				m.textArea.SetValue(m.typingBuffer)
				m.textArea.CursorEnd()
				m.focus = focusInput
				m.textArea.Focus()
			}
			return m, nil
		}

		if m.focus == focusInput {
			// Enter = send command; Alt+Enter = insert newline
			if !msg.Alt && msg.Type == tea.KeyEnter {
				m.submitted = m.textArea.Value()
				m.quitting = true
				return m, tea.Quit
			}
			// Alt+Enter → insert a literal newline into the textarea
			if msg.Alt && msg.Type == tea.KeyEnter {
				// Forward a plain Enter (no Alt) to the textarea so it inserts a newline
				newlineMsg := tea.KeyMsg{Type: tea.KeyEnter}
				m.textArea, cmd = m.textArea.Update(newlineMsg)
				return m, cmd
			}

			switch msg.String() {
			case "ctrl+n":
				if m.historyIndex == 0 {
					m.typingBuffer = m.textArea.Value()
				}
				filtered := m.getFilteredSuggestions(m.typingBuffer)
				if len(filtered) > 0 && m.historyIndex < len(filtered) {
					m.historyIndex++
					m.textArea.SetValue(filtered[m.historyIndex-1])
					m.textArea.CursorEnd()
				}
				return m, nil

			case "ctrl+p":
				filtered := m.getFilteredSuggestions(m.typingBuffer)
				if m.historyIndex > 1 {
					m.historyIndex--
					m.textArea.SetValue(filtered[m.historyIndex-1])
					m.textArea.CursorEnd()
				} else if m.historyIndex == 1 {
					m.historyIndex = 0
					m.textArea.SetValue(m.typingBuffer)
					m.textArea.CursorEnd()
				}
				return m, nil
			}

		} else {
			// focus == focusList
			filtered := m.getFilteredSuggestions(m.typingBuffer)

			// Enter in list mode → send selected directly
			if !msg.Alt && msg.Type == tea.KeyEnter {
				if len(filtered) > 0 && m.listCursor < len(filtered) {
					m.submitted = filtered[m.listCursor]
					m.quitting = true
					return m, tea.Quit
				}
				return m, nil
			}

			switch msg.String() {
			case "up", "k":
				if m.listCursor > 0 {
					m.listCursor--
				}
				// Preview selected item in textarea
				if len(filtered) > 0 && m.listCursor < len(filtered) {
					m.textArea.SetValue(filtered[m.listCursor])
					m.textArea.CursorEnd()
				}
				return m, nil

			case "down", "j":
				if m.listCursor < len(filtered)-1 {
					m.listCursor++
				}
				// Preview selected item in textarea
				if len(filtered) > 0 && m.listCursor < len(filtered) {
					m.textArea.SetValue(filtered[m.listCursor])
					m.textArea.CursorEnd()
				}
				return m, nil

			case "alt+enter":
				// Alt+Enter in list mode: confirm selection and switch back to input
				if len(filtered) > 0 && m.listCursor < len(filtered) {
					m.textArea.SetValue(filtered[m.listCursor])
					m.textArea.CursorEnd()
				}
				m.focus = focusInput
				m.textArea.Focus()
				return m, nil
			}
		}
	}

	// Only forward key events to textarea when focused on input
	if m.focus == focusInput {
		m.textArea, cmd = m.textArea.Update(msg)
	}

	return m, cmd
}

func (m lineEditorModel) View() string {
	if m.quitting {
		return ""
	}

	val := m.textArea.Value()
	filterPrefix := val
	if m.focus == focusList {
		filterPrefix = m.typingBuffer
	}
	filtered := m.getFilteredSuggestions(filterPrefix)

	// Styles
	activeTabStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FAFAFA")).
		Background(lipgloss.Color("#ea6645")).
		Padding(0, 1)

	inactiveTabStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Padding(0, 1)

	inputLabel := "Input"
	listLabel := "History"
	if m.focus == focusInput {
		inputLabel = activeTabStyle.Render("▶ Input")
		listLabel = inactiveTabStyle.Render("  History")
	} else {
		inputLabel = inactiveTabStyle.Render("  Input")
		listLabel = activeTabStyle.Render("▶ History")
	}

	str := titleStyle.Render("Input Commands") + "\n"
	str += inputLabel + "  " + listLabel + "\n\n"

	// Input box
	inputBorderColor := lipgloss.Color("238")
	if m.focus == focusInput {
		inputBorderColor = lipgloss.Color("#ea6645")
	}
	inputBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(inputBorderColor).
		Padding(0, 1).
		Render(m.textArea.View())
	str += inputBox + "\n\n"

	// History list
	listHeight := 8
	start := 0
	if m.focus == focusList && m.listCursor > listHeight-1 {
		start = m.listCursor - listHeight + 1
	}

	if len(filtered) > 0 {
		headerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
		if m.focus == focusList {
			str += headerStyle.Render(fmt.Sprintf("History (%d matches) — ↑k/↓j Enter=send Alt+Enter=use:", len(filtered))) + "\n"
		} else {
			str += headerStyle.Render(fmt.Sprintf("Suggestions (%d) — Tab to switch, Ctrl+P/N to navigate:", len(filtered))) + "\n"
		}

		shown := 0
		for i := start; i < len(filtered) && shown < listHeight; i++ {
			s := filtered[i]
			line := strings.Split(s, "\n")[0] // first line only
			if len(line) > 55 {
				line = line[:55] + "…"
			}

			if m.focus == focusList && i == m.listCursor {
				str += lipgloss.NewStyle().
					Foreground(lipgloss.Color("#10b981")).
					Bold(true).
					Render("  ▶ "+line) + "\n"
			} else {
				str += lipgloss.NewStyle().
					Foreground(lipgloss.Color("36")).
					Render("    "+line) + "\n"
			}
			shown++
		}

		if len(filtered) > listHeight {
			str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).
				Render(fmt.Sprintf("    ... %d more", len(filtered)-listHeight)) + "\n"
		}
	} else {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("  No matching history") + "\n"
	}

	str += "\n"

	if m.focus == focusInput {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).
			Render("  [Enter] SEND • [Alt+Enter] New Line • [Tab] Switch to History • [Esc] CANCEL") + "\n"
	} else {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).
			Render("  [Enter] SEND Direct • [Alt+Enter] Select to Input • [Tab] Back to Input • [Esc] CANCEL") + "\n"
	}

	popup := popupStyle.Render(str)

	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

func runLineEditor(oldState *term.State) (string, *term.State) {
	term.Restore(int(os.Stdin.Fd()), oldState)

	termMutex.Lock()
	inTerminal = false
	termMutex.Unlock()

	p := tea.NewProgram(initialLineEditorModel(), tea.WithAltScreen())
	m, _ := runTeaProgram(p)

	newState, _ := term.MakeRaw(int(os.Stdin.Fd()))

	if lm, ok := m.(lineEditorModel); ok {
		return strings.TrimRight(lm.submitted, "\r\n"), newState
	}
	return "", newState
}

// ------ Output Filter (Regex) ------
// filterModel is used for setting/clearing the output filter

type filterModel struct {
	input    textinput.Model
	quitting bool
	err      string
	width    int
	height   int
}

func initialFilterModel(current string) filterModel {
	ti := textinput.New()
	ti.Placeholder = "ERROR|WARN  or  !DEBUG (prefix ! to hide matches)"
	ti.SetValue(current)
	ti.Focus()
	ti.Width = 50

	return filterModel{input: ti}
}

func (m filterModel) Init() tea.Cmd { return textinput.Blink }

func (m filterModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyCtrlC:
			m.input.SetValue("__CANCEL__")
			m.quitting = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m filterModel) View() string {
	if m.quitting {
		return ""
	}
	str := titleStyle.Render("Output Filter") + "\n\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(
		"Regex  →  show only matching lines\n"+
			"!Regex →  hide matching lines (grep -v)") + "\n\n"
	str += m.input.View() + "\n\n"
	if m.err != "" {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("red")).Render("Error: "+m.err) + "\n\n"
	}
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("[Enter] Apply • [Esc] Cancel") + "\n"
	popup := popupStyle.Render(str)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

func runFilterEditor(oldState *term.State) *term.State {
	filterMu.RLock()
	current := filterPattern
	filterMu.RUnlock()

	term.Restore(int(os.Stdin.Fd()), oldState)
	termMutex.Lock()
	inTerminal = false
	termMutex.Unlock()

	p := tea.NewProgram(initialFilterModel(current), tea.WithAltScreen())
	m, _ := runTeaProgram(p)

	newState, _ := term.MakeRaw(int(os.Stdin.Fd()))

	if fm, ok := m.(filterModel); ok {
		val := fm.input.Value()
		if val == "__CANCEL__" {
			return newState
		}
		filterMu.Lock()
		filterPattern = val
		negate := strings.HasPrefix(val, "!")
		pattern := strings.TrimPrefix(val, "!")
		if val == "" || val == "!" {
			filterRegex = nil
			filterNegate = false
		} else {
			rx, err := regexp.Compile(pattern)
			if err == nil {
				filterRegex = rx
				filterNegate = negate
			}
		}
		filterMu.Unlock()
	}

	return newState
}

// ------ Log to File ------

type logPathModel struct {
	input    textinput.Model
	quitting bool
	width    int
	height   int
	current  string
}

func initialLogPathModel() logPathModel {
	ti := textinput.New()
	logMu.Lock()
	cur := logFilePath
	logMu.Unlock()

	if cur != "" {
		ti.SetValue(cur)
	} else {
		home, _ := os.UserHomeDir()
		ti.SetValue(filepath.Join(home, fmt.Sprintf("serialport_log_%s.txt", time.Now().Format("20060102_150405"))))
	}
	ti.Focus()
	ti.Width = 60
	return logPathModel{input: ti, current: cur}
}

func (m logPathModel) Init() tea.Cmd { return textinput.Blink }

func (m logPathModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEnter:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyCtrlC:
			m.input.SetValue("__CANCEL__")
			m.quitting = true
			return m, tea.Quit
		case tea.KeyTab:
			// Tab-complete: expand ~ and list directory
			val := m.input.Value()
			if strings.HasPrefix(val, "~") {
				home, _ := os.UserHomeDir()
				val = home + val[1:]
				m.input.SetValue(val)
			}
			dir := filepath.Dir(val)
			base := filepath.Base(val)
			entries, err := os.ReadDir(dir)
			if err == nil {
				var matches []string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), base) {
						matches = append(matches, filepath.Join(dir, e.Name()))
					}
				}
				if len(matches) == 1 {
					m.input.SetValue(matches[0])
					// move cursor to end
					m.input.CursorEnd()
				} else if len(matches) > 1 {
					// Find longest common prefix
					common := matches[0]
					for _, match := range matches[1:] {
						for !strings.HasPrefix(match, common) {
							common = common[:len(common)-1]
						}
					}
					m.input.SetValue(common)
					m.input.CursorEnd()
				}
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m logPathModel) View() string {
	if m.quitting {
		return ""
	}
	str := titleStyle.Render("Log to File") + "\n\n"
	logMu.Lock()
	active := logFile != nil
	curPath := logFilePath
	logMu.Unlock()

	if active {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981")).
			Render("● Currently logging to: "+curPath) + "\n"
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).
			Render("(Enter empty path to stop logging)") + "\n\n"
	}
	str += "Log file path:\n"
	str += m.input.View() + "\n\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("[Tab] Complete • [Enter] Apply • [Esc] Cancel") + "\n"
	popup := popupStyle.Render(str)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

type helpModel struct {
	width  int
	height int
}

func initialHelpModel() helpModel {
	return helpModel{}
}

func (m helpModel) Init() tea.Cmd {
	return nil
}

func (m helpModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q", "enter", "ctrl+c", "ctrl+h":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m helpModel) View() string {
	str := titleStyle.Render(" ❓ Keyboard Shortcuts ") + "\n\n"

	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+T") + "  : Open Port Palette\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+V") + "  : Command Input (Enter=Send, Alt+Enter=New Line)\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+F") + "  : Output Filter (regex, !negate)\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+L") + "  : Toggle Log to File (Start/Stop)\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+S") + "  : Save Output Buffer to File\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+\\") + "  : Toggle Timestamp\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+?") + " / " + lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+H") + " : Show this Help\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("Ctrl+Q") + "  : Quit Client\n"
	str += "\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("Port Menu (Ctrl+T):") + "\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("  c") + "  : Port Settings (baud/parity/alias)\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("  a") + "  : Rename port alias (inside Port Settings)\n"
	str += lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Render("  s") + "  : Save serial config (inside Port Settings)\n"
	str += "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("Press any key to close...")

	helpPopupStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#ea6645")).
		Padding(1, 3).
		Align(lipgloss.Left)

	popup := helpPopupStyle.Render(str)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

func runHelpUI(oldState *term.State) *term.State {
	term.Restore(int(os.Stdin.Fd()), oldState)
	termMutex.Lock()
	inTerminal = false
	termMutex.Unlock()

	p := tea.NewProgram(initialHelpModel(), tea.WithAltScreen())
	runTeaProgram(p)

	newState, _ := term.MakeRaw(int(os.Stdin.Fd()))

	return newState
}

func runLogEditor(oldState *term.State) *term.State {
	term.Restore(int(os.Stdin.Fd()), oldState)
	termMutex.Lock()
	inTerminal = false
	termMutex.Unlock()

	p := tea.NewProgram(initialLogPathModel(), tea.WithAltScreen())
	m, _ := runTeaProgram(p)

	newState, _ := term.MakeRaw(int(os.Stdin.Fd()))

	if lm, ok := m.(logPathModel); ok {
		val := lm.input.Value()
		if val == "__CANCEL__" {
			return newState
		}

		logMu.Lock()
		// Close existing log
		if logFile != nil {
			logFile.Close()
			logFile = nil
			logFilePath = ""
		}
		if val != "" {
			// Expand ~
			if strings.HasPrefix(val, "~") {
				home, _ := os.UserHomeDir()
				val = home + val[1:]
			}
			os.MkdirAll(filepath.Dir(val), 0755)
			f, err := os.OpenFile(val, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if err == nil {
				logFile = f
				logFilePath = val
				// Write log header
				bw := bufio.NewWriter(f)
				bw.WriteString(fmt.Sprintf("=== SOS Log started at %s ===\n", time.Now().Format(time.RFC3339)))
				bw.Flush()
			}
		}
		logMu.Unlock()
	}

	return newState
}

// ══════════════════════════════════════════════════════════════
//  Port Settings Interface
// ══════════════════════════════════════════════════════════════

// portSettingsResult is returned when the user confirms settings.
// nil means the user cancelled without connecting.
type portSettingsResult struct {
	Baudrate int
	Databits int
	Stopbit  int
	Parity   string
}

type portSettingsModel struct {
	portID      string
	description string // e.g. "Silicon Labs CP210x ..."
	byIDName    string // raw /dev/serial/by-id/ symlink name

	baudIdx   int
	dataIdx   int
	stopIdx   int
	parityIdx int
	cursor    int // 0=baud 1=data 2=stop 3=parity

	// Alias editing
	aliasInput   textinput.Model
	editingAlias bool
	currentAlias string
	aliasSaved   bool

	saved    bool // true after 's' pressed
	quitting bool
	result   *portSettingsResult

	width  int
	height int
}

func newPortSettingsModel(portID string) portSettingsModel {
	portUpdateMu.Lock()
	pi := ports[portID]
	portUpdateMu.Unlock()

	// Resolve initial values: saved profile → server config → default
	baud, data, stop, par := resolvePortConfig(portID, pi)

	// Find indices
	baudIdx := 5 // default → 115200
	for i, v := range baudRates {
		if v == baud {
			baudIdx = i
			break
		}
	}
	dataIdx := 3 // default → 8
	for i, v := range dataBitsList {
		if v == data {
			dataIdx = i
			break
		}
	}
	stopIdx := 0
	for i, v := range stopBitsList {
		if v == stop {
			stopIdx = i
			break
		}
	}
	parIdx := 0
	for i, v := range parities {
		if v == par {
			parIdx = i
			break
		}
	}

	// Setup alias text input
	ti := textinput.New()
	ti.Placeholder = "e.g. Mainboard / ESP32 Dev"
	ti.SetValue(pi.Alias)
	ti.Width = 30
	ti.CharLimit = 50

	return portSettingsModel{
		portID:       portID,
		description:  pi.Description,
		byIDName:     pi.ByIDName,
		baudIdx:      baudIdx,
		dataIdx:      dataIdx,
		stopIdx:      stopIdx,
		parityIdx:    parIdx,
		aliasInput:   ti,
		currentAlias: pi.Alias,
	}
}

func (m portSettingsModel) Init() tea.Cmd { return nil }

func (m portSettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		// When editing alias, forward keys to the text input
		if m.editingAlias {
			switch msg.Type {
			case tea.KeyEnter:
				// Confirm alias
				m.currentAlias = strings.TrimSpace(m.aliasInput.Value())
				m.editingAlias = false
				// Send set_port_alias to server
				if wsConn != nil {
					wsConn.WriteJSON(map[string]interface{}{
						"type":    "set_port_alias",
						"port_id": m.portID,
						"alias":   m.currentAlias,
					})
				}
				m.aliasSaved = true
				return m, nil
			case tea.KeyEsc, tea.KeyCtrlC:
				m.editingAlias = false
				m.aliasInput.SetValue(m.currentAlias) // restore
				return m, nil
			}
			m.aliasInput, cmd = m.aliasInput.Update(msg)
			return m, cmd
		}

		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = 3
			}

		case "down", "j":
			if m.cursor < 3 {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "left", "h":
			switch m.cursor {
			case 0:
				if m.baudIdx > 0 {
					m.baudIdx--
				}
			case 1:
				if m.dataIdx > 0 {
					m.dataIdx--
				}
			case 2:
				if m.stopIdx > 0 {
					m.stopIdx--
				}
			case 3:
				if m.parityIdx > 0 {
					m.parityIdx--
				}
			}

		case "right", "l":
			switch m.cursor {
			case 0:
				if m.baudIdx < len(baudRates)-1 {
					m.baudIdx++
				}
			case 1:
				if m.dataIdx < len(dataBitsList)-1 {
					m.dataIdx++
				}
			case 2:
				if m.stopIdx < len(stopBitsList)-1 {
					m.stopIdx++
				}
			case 3:
				if m.parityIdx < len(parities)-1 {
					m.parityIdx++
				}
			}

		case "a":
			// Enter alias editing mode
			m.editingAlias = true
			m.aliasSaved = false
			m.aliasInput.Focus()
			return m, textinput.Blink

		case "s":
			// Save serial profile (baud/data/stop/parity) via server
			if wsConn != nil {
				wsConn.WriteJSON(map[string]interface{}{
					"type":     "configure_port",
					"port_id":  m.portID,
					"baudrate": baudRates[m.baudIdx],
					"databits": dataBitsList[m.dataIdx],
					"stopbit":  stopBitsList[m.stopIdx],
					"parity":   parities[m.parityIdx],
				})
			}
			// Also persist locally as fallback
			m.saved = savePortProfile(m.byIDName, m.description, m.portID,
				baudRates[m.baudIdx], dataBitsList[m.dataIdx],
				stopBitsList[m.stopIdx], parities[m.parityIdx])

		case "enter":
			m.result = &portSettingsResult{
				Baudrate: baudRates[m.baudIdx],
				Databits: dataBitsList[m.dataIdx],
				Stopbit:  stopBitsList[m.stopIdx],
				Parity:   parities[m.parityIdx],
			}
			m.quitting = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m portSettingsModel) View() string {
	if m.quitting {
		return ""
	}

	settingRowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).PaddingLeft(4)
	selectedRowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981")).Bold(true).PaddingLeft(2)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	str := titleStyle.Render(" ⚙  Port Settings ") + "\n\n"

	// Port identity
	str += lipgloss.NewStyle().Bold(true).Render("  Port: ") +
		lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981")).Render(m.portID) + "\n"
	if m.description != "" {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("33")).PaddingLeft(2).Render("⬡ "+m.description) + "\n"
	}
	str += "\n"

	// Alias row
	if m.editingAlias {
		str += lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ea6645")).Render("  ✎ Alias: ") +
			m.aliasInput.View() + "\n"
		str += dimStyle.Render("  [Enter] Confirm • [Esc] Cancel") + "\n"
	} else {
		aliasDisplay := m.currentAlias
		if aliasDisplay == "" {
			aliasDisplay = dimStyle.Render("(none)")
		} else {
			aliasDisplay = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700")).Bold(true).Render(aliasDisplay)
		}
		str += lipgloss.NewStyle().Bold(true).Render("  Alias: ") + aliasDisplay +
			dimStyle.Render("  [a] edit") + "\n"
		if m.aliasSaved {
			str += lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981")).PaddingLeft(2).Render("✓ Alias saved") + "\n"
		}
	}
	str += "\n"

	if m.saved {
		str += lipgloss.NewStyle().Foreground(lipgloss.Color("#10b981")).Render("  ✓ Config saved") + "\n\n"
	}

	rows := []struct{ label, value string }{
		{"Baud Rate", fmt.Sprintf("< %d >", baudRates[m.baudIdx])},
		{"Data Bits", fmt.Sprintf("< %d >", dataBitsList[m.dataIdx])},
		{"Stop Bits", fmt.Sprintf("< %d >", stopBitsList[m.stopIdx])},
		{"Parity   ", fmt.Sprintf("< %s >", parities[m.parityIdx])},
	}

	for i, row := range rows {
		label := fmt.Sprintf("%-10s  %s", row.label, row.value)
		if m.cursor == i {
			str += fmt.Sprintf("> %s\n", selectedRowStyle.Render(label))
		} else {
			str += fmt.Sprintf("  %s\n", settingRowStyle.Render(label))
		}
	}

	str += "\n"
	str += dimStyle.Render("  ↑k/↓j: field • ←h/→l: value • a: rename alias • s: save config • Enter: connect • Esc: back") + "\n"

	popup := popupStyle.Render(str)
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, popup)
	}
	return "\n\n" + popup
}

// savePortProfile persists baud/data/stop/parity to the local config file as a fallback.
// Keys are tried in priority order: byIDName → description → portID.
// IMPORTANT: Alias is only carried by stable hardware keys (byIDName, description).
// The portID key (e.g. "ttyUSB0") stores serial params without alias, preventing
// future devices at the same node from inheriting this alias.
func savePortProfile(byIDName, description, portID string, baudrate, databits, stopbit int, parity string) bool {
	cfgPath := config.GetConfigPath()
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		cfg = config.DefaultConfig()
	}
	if cfg.PortProfiles == nil {
		cfg.PortProfiles = make(map[string]*config.PortProfile)
	}

	profile := &config.PortProfile{
		Baudrate: baudrate,
		Databits: databits,
		Stopbit:  stopbit,
		Parity:   parity,
	}

	// Preserve existing alias from stable hardware keys ONLY (skip portID)
	for _, key := range []string{byIDName, description} {
		if key == "" {
			continue
		}
		if existing, ok := cfg.PortProfiles[key]; ok && existing != nil && existing.Alias != "" {
			profile.Alias = existing.Alias
			break
		}
	}

	// Write under stable hardware keys (with alias)
	for _, key := range []string{byIDName, description} {
		if key != "" {
			cfg.PortProfiles[key] = profile
		}
	}

	// Write under portID key WITHOUT alias (portID is not hardware-specific)
	if portID != "" {
		noAlias := *profile
		noAlias.Alias = ""
		cfg.PortProfiles[portID] = &noAlias
	}

	return config.SaveConfig(cfgPath, cfg) == nil
}

// runPortSettings opens the Port Settings UI and returns chosen settings (or nil on cancel).
func runPortSettings(portID string) *portSettingsResult {
	p := tea.NewProgram(newPortSettingsModel(portID), tea.WithAltScreen())
	m, _ := runTeaProgram(p)
	if psm, ok := m.(portSettingsModel); ok {
		return psm.result
	}
	return nil
}
