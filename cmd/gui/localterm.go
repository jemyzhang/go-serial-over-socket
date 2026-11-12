package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
)

// The local terminal is the single global split pane beside the
// serial terminal (Jemy's call, v1: at most one). It is a resident
// shell — tmux attach-or-create by default, raw login shell
// otherwise — that survives serial port switches, so an ssh
// session is not lost by switching ports. It is deliberately NOT
// in a.terms: recording, the command bar, WebDAV backup and the
// serial paths never see it, and Ctrl+S (save output) stays
// serial-only. The plugin owns the PTY, the resize and the exit
// path (Options.Command runs the program; an empty Command runs
// the login shell); no PTY code lives here.

// localTerminal is one open split pane.
type localTerminal struct {
	term *terminal.Terminal
	name string // header name: "tmux · <session>" or shell basename
	dir  string // starting directory (header fallback when Dir() is "")
	tmux bool   // started under tmux (for the exit fallback)
}

// Multiplexer choices for the local terminal (settings).
const (
	multiplexerTmux = "tmux" // attach-or-create a tmux session
	multiplexerNone = "无"    // raw login shell, no multiplexer
)

var multiplexerOptions = []string{multiplexerTmux, multiplexerNone}

// defaultTmuxSession is the tmux session a new local terminal
// attaches to (or creates) when no name is configured.
const defaultTmuxSession = "serial-debug"

// lookPath is exec.LookPath, injectable in tests so the tmux
// fallback does not depend on the machine running the tests.
var lookPath = exec.LookPath

// newTerminalFunc builds terminals; terminal.New in production,
// replaced in tests so creating a local terminal does not spawn a
// real shell.
var newTerminalFunc = terminal.New

// normalizeTmuxSession trims a session name: empty becomes the
// default. Only [A-Za-z0-9_.-] is accepted — tmux target syntax
// (session:window) and shell quoting make anything else
// unreliable in a name. Invalid returns "".
func normalizeTmuxSession(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultTmuxSession, true
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			continue
		}
		return "", false
	}
	return name, true
}

// localTerminalCommand builds the program for a new local
// terminal. tmux mode runs `tmux new-session -A -s <name>`
// (attach or create); anything else is a raw shell (nil command:
// the plugin runs the login shell). fallback explains a tmux
// request that fell back to a raw shell, "" when the raw shell
// was chosen or tmux is running.
func localTerminalCommand(multiplexer, session string) (cmd []string, fallback string) {
	if multiplexer != multiplexerTmux {
		return nil, ""
	}
	if _, err := lookPath("tmux"); err != nil {
		return nil, "未找到 tmux，已用原始 shell"
	}
	name, ok := normalizeTmuxSession(session)
	if !ok {
		return nil, "tmux 会话名无效，已用原始 shell"
	}
	return []string{"tmux", "new-session", "-A", "-s", name}, ""
}

// shellBaseName is the login shell's basename (zsh, bash) for the
// raw-mode header name, mirroring the plugin's resolution: $SHELL,
// else /bin/sh (macOS /bin/zsh, PowerShell on Windows).
func shellBaseName() string {
	if runtime.GOOS == "windows" {
		return "PowerShell"
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		} else {
			shell = "/bin/sh"
		}
	}
	return filepath.Base(shell)
}

// localDisplayName is the header name: "tmux · <session>" in tmux
// mode, the login shell's basename in raw mode.
func localDisplayName(tmuxMode bool, session string) string {
	if tmuxMode {
		name, _ := normalizeTmuxSession(session)
		return "tmux · " + name
	}
	return shellBaseName()
}

// shortenHome replaces the home prefix with ~ for the header
// directory (~/proj instead of /home/user/proj).
func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || path == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// openLocalTerminal splits the current serial terminal with the
// single global local terminal. The + button calls it with
// forceRaw=false (settings mode); the exit handler retries a
// tmux client that never started with forceRaw=true. No-op while
// a pane is already open (the + button greys out). UI thread
// only: it is called from button clicks and RunOnMain.
func (a *nativeApp) openLocalTerminal(forceRaw bool) {
	a.mu.Lock()
	if a.localTerm != nil {
		a.mu.Unlock()
		return
	}
	s := a.settings
	// The local pane has no size report of its own; the serial
	// width is the window-width proxy. It over-provisions the
	// local pane (about half the window in a right split), which
	// only ever retains more than the setting, never less.
	cols := a.lastSizeCols
	a.mu.Unlock()
	multiplexer := s.Multiplexer
	if forceRaw {
		multiplexer = multiplexerNone
	}
	cmd, fallback := localTerminalCommand(multiplexer, s.TmuxSession)
	if fallback != "" {
		a.setStatus("%s", fallback)
	}
	startDir, _ := os.UserHomeDir()
	// No OnKey: the local terminal needs none of the serial
	// interceptions — Ctrl+S goes to the shell as XOFF (the
	// save-output path is serial-only), and Ctrl+, is handled by
	// the frame-level Shortcut regardless of focus. Copy, paste
	// and scrolling are plugin-level.
	var term *terminal.Terminal
	term, err := newTerminalFunc(terminal.Options{
		Command:      cmd,
		Dir:          startDir,
		Font:         terminal.Font{Family: s.FontFamily, Size: float32(s.FontSize)},
		Theme:        terminalTheme(s.TermTheme),
		Scrollback:   s.scrollbackBytes(cols),
		CopyOnSelect: copyOnSelectMode(s.CopyOnSelect),
		OnExit: func(code int) {
			a.onLocalExit(term, code)
		},
		// OnSearch feeds the local find bar's count (see
		// recordFindStatus): it runs on the main thread, from
		// the view's frame drive. Only the current local
		// terminal's statuses count: a search ending on a
		// replaced terminal must not overwrite the new one's.
		OnSearch: func(s terminal.SearchStatus) {
			a.mu.Lock()
			cur := a.localTerm
			a.mu.Unlock()
			if cur == nil || cur.term != term {
				return
			}
			a.recordFindStatus(findPaneLocal, s)
		},
	})
	if err != nil {
		a.setStatus("本地终端启动失败: %v", err)
		return
	}
	a.mu.Lock()
	if a.localTerm != nil {
		// Opened concurrently (defensive: the UI thread is
		// single-threaded, so this should not happen) — do not
		// leak the new terminal.
		a.mu.Unlock()
		_ = term.Close()
		return
	}
	a.localTerm = &localTerminal{
		term: term,
		name: localDisplayName(cmd != nil, s.TmuxSession),
		dir:  startDir,
		tmux: cmd != nil,
	}
	a.localFocusPending = true
	a.mu.Unlock()
	a.invalidate()
}

// syncSplitSettings copies the live split state into
// a.settings, so the next settings.save() writes the dragged
// position instead of the stale saved value. It only updates
// the struct; the file is written by the caller's save (or the
// layout switch's own save). UI thread only.
func (a *nativeApp) syncSplitSettings() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settings.SplitLayout = a.splitLayout
	a.settings.SplitPercent = a.splitPercent
}

// setSplitLayout switches the split layout ("right" | "below")
// and saves it at once (a discrete choice, not staging). The
// dragged percent is synced first, so this save does not
// clobber it. UI thread only: called from the header buttons.
func (a *nativeApp) setSplitLayout(layout string) {
	if layout != splitLayoutRight && layout != splitLayoutBelow {
		return
	}
	if a.splitLayout == layout {
		return
	}
	a.splitLayout = layout
	a.syncSplitSettings()
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	if err := s.save(); err != nil {
		a.setStatus("保存设置失败: %v", err)
		return
	}
	a.invalidate()
}

// closeLocalTerminal closes the split pane: the shell (or tmux
// client) is killed, the tmux session itself stays on the server
// for the next open to re-attach. Focus returns to the serial
// terminal via the existing termFocusPending mechanism, and +
// becomes usable again. Idempotent: safe from the exit handler
// (pane already closed) and from shutdown. UI thread only.
func (a *nativeApp) closeLocalTerminal() {
	// Sync the live split state first: the pane is going away,
	// but the dragged position must survive in the settings
	// struct for the next save (and the next open).
	a.syncSplitSettings()
	// The local find bar belongs to this terminal: end its
	// search and hide it (the next local terminal starts
	// without one).
	a.hideFindBar(findPaneLocal)
	a.mu.Lock()
	lt := a.localTerm
	if lt == nil {
		a.mu.Unlock()
		return
	}
	a.localTerm = nil
	a.termFocusPending = true
	a.mu.Unlock()
	// The field is cleared before Close, so an OnExit this Close
	// triggers sees no local terminal and ignores it.
	_ = lt.term.Close()
	a.invalidate()
}

// onLocalExit runs on a terminal goroutine when the local shell
// exits: the pane closes straight back to the single serial view
// (no [Process exited] screen). All UI state changes go through
// RunOnMain to the main thread.
func (a *nativeApp) onLocalExit(term *terminal.Terminal, code int) {
	mygo.RunOnMain(func() { a.handleLocalExit(term, code) })
}

// handleLocalExit is the main-thread half of onLocalExit.
func (a *nativeApp) handleLocalExit(term *terminal.Terminal, code int) {
	a.mu.Lock()
	lt := a.localTerm
	if lt == nil || lt.term != term {
		a.mu.Unlock()
		return // already closed by × / shutdown — nothing to do
	}
	tmuxMode := lt.tmux
	a.mu.Unlock()
	// Exit code -1 means the program never started (the plugin's
	// start error; Close() before start stores the same code, but
	// that path clears the field first, so it never reaches
	// here). A tmux client that never started gets one retry as a
	// raw shell instead of dropping the pane; a raw shell that
	// never started just closes — tmuxMode is false for the
	// retry, so there is no second retry.
	if code == -1 && tmuxMode {
		a.closeLocalTerminal()
		a.openLocalTerminal(true)
		a.setStatus("tmux 启动失败，已退回原始 shell")
		return
	}
	a.closeLocalTerminal()
	a.setStatus("本地终端已退出")
}
