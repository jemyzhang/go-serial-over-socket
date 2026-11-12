package main

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Local terminal tests (see localterm.go). Terminals are created
// BEFORE any HOME redirect: terminal.New loads libghostty-vt from
// the user cache, and a temp HOME would make it try to download
// the library instead.

// stubTerminalConstructor replaces newTerminalFunc with one that
// records the options it was called with and returns a Conn-mode
// terminal (no real shell). It returns the recorded calls; the
// constructor is restored on test cleanup.
func stubTerminalConstructor(t *testing.T) *[]terminal.Options {
	t.Helper()
	old := newTerminalFunc
	var got []terminal.Options
	newTerminalFunc = func(o terminal.Options) (*terminal.Terminal, error) {
		got = append(got, o)
		return terminal.New(terminal.Options{Conn: newPortConn(&fakeSender{}, "local")})
	}
	t.Cleanup(func() { newTerminalFunc = old })
	return &got
}

// stubLookPathFound makes localTerminalCommand see tmux installed,
// regardless of the machine running the tests.
func stubLookPathFound(t *testing.T) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/tmux", nil }
	t.Cleanup(func() { lookPath = old })
}

// stubLookPathMissing makes localTerminalCommand see tmux absent.
func stubLookPathMissing(t *testing.T) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) {
		return "", errors.New("exec: \"tmux\": executable file not found in $PATH")
	}
	t.Cleanup(func() { lookPath = old })
}

// localTestApp builds the minimal app the local-terminal tests
// need: two serial ports, settings, staging mirroring settings.
func localTestApp() *nativeApp {
	return &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
			"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Alias: "probe", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		},
		portIDs:  []string{"ttyUSB0", "ttyUSB1"},
		selected: "ttyUSB0",
		terms:    map[string]*termSession{},
		settings: defaultSettings(),
		status:   "服务连接成功",
		drafts:   map[string]*portDraft{},
		// Staging mirrors settings, as newNativeApp sets it.
		setScrollF:      10000,
		setFontSizeF:    14,
		setTermTheme:    "dark",
		setCopyOnSelect: true,
		setMultiplexer:  multiplexerTmux,
		setTmuxSession:  defaultTmuxSession,
		// Split state mirrors settings, as newNativeApp sets it.
		splitLayout:  splitLayoutRight,
		splitPercent: defaultSplitPercent,
	}
}

// TestLocalSettingsDefaults mirrors TestCopyOnSelectDefaultsOn for
// the multiplexer pair: fresh install and old files without the
// keys get tmux + serial-debug; explicit values load.
func TestLocalSettingsDefaults(t *testing.T) {
	if got := defaultSettings(); got.Multiplexer != multiplexerTmux || got.TmuxSession != defaultTmuxSession {
		t.Errorf("defaultSettings() multiplexer = %q/%q, want tmux/serial-debug", got.Multiplexer, got.TmuxSession)
	}
	data, err := json.Marshal(defaultSettings())
	if err != nil {
		t.Fatalf("marshal default settings: %v", err)
	}
	if !strings.Contains(string(data), `"multiplexer":"tmux"`) || !strings.Contains(string(data), `"tmuxSession":"serial-debug"`) {
		t.Errorf("saved defaults must contain multiplexer tmux + serial-debug, got %s", data)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Fresh install: no file at all.
	if got := loadSettings(); got.Multiplexer != multiplexerTmux || got.TmuxSession != defaultTmuxSession {
		t.Errorf("fresh install multiplexer = %q/%q, want tmux/serial-debug", got.Multiplexer, got.TmuxSession)
	}
	// Old file without the keys: same defaults, other keys load.
	writeSettingsFile(t, home, `{"dark":false,"fontSize":16}`)
	if got := loadSettings(); got.Multiplexer != multiplexerTmux || got.TmuxSession != defaultTmuxSession {
		t.Errorf("old file without the keys multiplexer = %q/%q, want tmux/serial-debug", got.Multiplexer, got.TmuxSession)
	}
	if got := loadSettings(); got.Dark || got.FontSize != 16 {
		t.Errorf("old file's explicit dark:false/fontSize:16 must still load, got dark=%v fontSize=%d", got.Dark, got.FontSize)
	}
	// Explicit values load, and an explicit empty session name
	// reads as the default (empty means "use the default").
	writeSettingsFile(t, home, `{"multiplexer":"无","tmuxSession":"debug-2"}`)
	if got := loadSettings(); got.Multiplexer != multiplexerNone || got.TmuxSession != "debug-2" {
		t.Errorf("explicit multiplexer = %q/%q, want 无/debug-2", got.Multiplexer, got.TmuxSession)
	}
	writeSettingsFile(t, home, `{"tmuxSession":""}`)
	if got := loadSettings(); got.TmuxSession != defaultTmuxSession {
		t.Errorf("explicit empty tmuxSession must read as the default, got %q", got.TmuxSession)
	}
	// Save/load round-trip keeps explicit values.
	if err := (Settings{Multiplexer: multiplexerNone, TmuxSession: "x"}).save(); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	// The save above wrote a full struct (zero values elsewhere);
	// load must keep the multiplexer pair and fall back to
	// defaults for the zeroed string keys.
	if got := loadSettings(); got.Multiplexer != multiplexerNone || got.TmuxSession != "x" {
		t.Errorf("saved multiplexer = %q/%q, want 无/x", got.Multiplexer, got.TmuxSession)
	}
}

func TestNormalizeTmuxSession(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", defaultTmuxSession, true},
		{"   ", defaultTmuxSession, true},
		{"serial-debug", "serial-debug", true},
		{"  my-sess_1.2  ", "my-sess_1.2", true},
		{"a:b", "", false}, // colon is tmux target syntax
		{"a b", "", false}, // space breaks target parsing
		{"a/b", "", false}, // slash is not a session-name char
		{"会话", "", false},  // non-ASCII rejected
		{"a;b", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeTmuxSession(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("normalizeTmuxSession(%q) = %q/%v, want %q/%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLocalTerminalCommand(t *testing.T) {
	stubLookPathFound(t)
	wantTmux := []string{"tmux", "new-session", "-A", "-s", "serial-debug"}
	if cmd, fb := localTerminalCommand(multiplexerTmux, "serial-debug"); !slices.Equal(cmd, wantTmux) || fb != "" {
		t.Errorf("tmux mode = %v/%q, want %v/\"\"", cmd, fb, wantTmux)
	}
	// Empty session name becomes the default.
	if cmd, fb := localTerminalCommand(multiplexerTmux, "  "); !slices.Equal(cmd, wantTmux) || fb != "" {
		t.Errorf("tmux mode, blank name = %v/%q, want %v/\"\"", cmd, fb, wantTmux)
	}
	// Invalid name falls back to a raw shell with a reason.
	if cmd, fb := localTerminalCommand(multiplexerTmux, "bad:name"); cmd != nil || fb == "" {
		t.Errorf("tmux mode, bad name = %v/%q, want nil + a fallback reason", cmd, fb)
	}
	// "无" is a raw shell by choice: no command, no fallback note.
	if cmd, fb := localTerminalCommand(multiplexerNone, "serial-debug"); cmd != nil || fb != "" {
		t.Errorf("raw mode = %v/%q, want nil/\"\"", cmd, fb)
	}

	stubLookPathMissing(t)
	// tmux absent: raw shell with a reason, even with a valid name.
	if cmd, fb := localTerminalCommand(multiplexerTmux, "serial-debug"); cmd != nil || fb == "" {
		t.Errorf("tmux missing = %v/%q, want nil + a fallback reason", cmd, fb)
	}
}

// TestLocalTerminalNoPorts: with no serial ports at all, the +
// button is a permanent toolbar item (not inside the pill row),
// so a local terminal can still open; the pane renders
// full-width without a serial session.
func TestLocalTerminalNoPorts(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := &nativeApp{
		ports:    map[string]PortInfo{},
		portIDs:  []string{},
		terms:    map[string]*termSession{},
		settings: defaultSettings(),
		status:   "服务连接成功",
		drafts:   map[string]*portDraft{},
		// Staging mirrors settings, as newNativeApp sets it.
		setScrollF:      10000,
		setFontSizeF:    14,
		setTermTheme:    "dark",
		setCopyOnSelect: true,
		setMultiplexer:  multiplexerTmux,
		setTmuxSession:  defaultTmuxSession,
	}
	// Empty state renders headlessly (toolbar with + included).
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	// + opens the local terminal even with no ports; it renders
	// full-width without a serial session.
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal with no ports")
	}
	defer app.closeLocalTerminal()
	tt.Frame()
	if app.snapshot().localTerm == nil {
		t.Error("snapshot must carry the local terminal with no ports")
	}
}

// TestLocalTerminalOpenClose drives the split state machine: +
// creates the pane (once), switching serial ports keeps it, ×
// closes it and re-enables +.
func TestLocalTerminalOpenClose(t *testing.T) {
	stubLookPathFound(t)
	calls := stubTerminalConstructor(t)
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	if lt.name != "tmux · serial-debug" || !lt.tmux {
		t.Errorf("local terminal name = %q tmux=%v, want tmux · serial-debug/true", lt.name, lt.tmux)
	}
	if !app.localFocusPending {
		t.Error("open must request focus for the local terminal")
	}
	if len(*calls) != 1 {
		t.Fatalf("constructor calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if want := []string{"tmux", "new-session", "-A", "-s", "serial-debug"}; !slices.Equal(got.Command, want) {
		t.Errorf("constructor Command = %v, want %v", got.Command, want)
	}
	// No OnKey: the local terminal needs none of the serial
	// interceptions — Ctrl+S reaches the shell as XOFF, and
	// Ctrl+, is handled by the frame-level Shortcut.
	if got.OnKey != nil {
		t.Error("local terminal must not set OnKey (no serial interception)")
	}
	if app.snapshot().localTerm != lt {
		t.Error("snapshot must carry the open local terminal")
	}

	// Second open is a no-op: still one pane, one constructor call.
	app.openLocalTerminal(false)
	if app.localTerm != lt || len(*calls) != 1 {
		t.Errorf("second open must be a no-op (localTerm same=%v, calls=%d)", app.localTerm == lt, len(*calls))
	}

	// The split renders headlessly without crashing.
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	// Switching serial ports keeps the local terminal.
	app.selectPort("ttyUSB1")
	if app.selected != "ttyUSB1" {
		t.Fatalf("selectPort did not switch, selected = %q", app.selected)
	}
	if app.localTerm != lt {
		t.Error("switching serial ports must keep the local terminal")
	}
	if sess2 := app.terms["ttyUSB1"]; sess2 != nil {
		defer sess2.term.Close()
	}

	// × closes the pane and returns focus to the serial terminal.
	app.closeLocalTerminal()
	if app.localTerm != nil {
		t.Error("close must clear the local terminal")
	}
	if !app.termFocusPending {
		t.Error("close must request focus back to the serial terminal")
	}
	if app.snapshot().localTerm != nil {
		t.Error("snapshot must drop the closed local terminal")
	}
	// Closing again is a no-op, not a panic.
	app.closeLocalTerminal()
	// + works again after close.
	app.openLocalTerminal(false)
	if app.localTerm == nil || app.localTerm == lt {
		t.Error("open after close must create a fresh local terminal")
	}
	app.closeLocalTerminal()
}

// TestLocalTerminalExitClosesPane calls the exit handler directly:
// a genuine exit closes the pane with a status note, an exit from
// another terminal is ignored, and a tmux client that never
// started (code -1) retries once as a raw shell.
func TestLocalTerminalExitClosesPane(t *testing.T) {
	stubLookPathFound(t)
	calls := stubTerminalConstructor(t)
	app := localTestApp()
	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer func() {
		// The retry test below creates a second terminal; close
		// whatever is still open (Close is idempotent).
		if cur := app.localTerm; cur != nil {
			_ = cur.term.Close()
		}
	}()

	// An exit from a different terminal is ignored.
	other, err := terminal.New(terminal.Options{Conn: newPortConn(&fakeSender{}, "other")})
	if err != nil {
		t.Fatalf("create other terminal: %v", err)
	}
	defer other.Close()
	app.handleLocalExit(other, 0)
	if app.localTerm != lt {
		t.Error("exit from another terminal must be ignored")
	}
	// No local terminal open: no-op, no panic.
	app.closeLocalTerminal()
	app.handleLocalExit(lt.term, 0)
	if app.localTerm != nil {
		t.Error("exit with no local terminal must be a no-op")
	}

	// Genuine exit (user typed exit): pane closes with a note.
	app.openLocalTerminal(false)
	lt = app.localTerm
	app.handleLocalExit(lt.term, 0)
	if app.localTerm != nil {
		t.Error("exit code 0 must close the pane")
	}
	if app.status != "本地终端已退出" {
		t.Errorf("status after exit = %q, want 本地终端已退出", app.status)
	}

	// tmux client that never started (code -1): one retry as a
	// raw shell, then the raw shell's own -1 just closes.
	app.openLocalTerminal(false)
	lt = app.localTerm
	if !lt.tmux {
		t.Fatal("setup: local terminal must be in tmux mode")
	}
	callsBefore := len(*calls)
	app.handleLocalExit(lt.term, -1)
	retried := app.localTerm
	if retried == nil || retried == lt {
		t.Fatal("tmux start failure must retry with a fresh terminal")
	}
	if retried.tmux {
		t.Error("retry must be a raw shell, not tmux again")
	}
	if len(*calls) != callsBefore+1 {
		t.Errorf("retry must call the constructor once more (calls=%d, want %d)", len(*calls), callsBefore+1)
	}
	if got := (*calls)[len(*calls)-1]; got.Command != nil {
		t.Errorf("retry Command = %v, want nil (raw shell)", got.Command)
	}
	if app.status != "tmux 启动失败，已退回原始 shell" {
		t.Errorf("status after tmux failure = %q, want the fallback note", app.status)
	}
	app.handleLocalExit(retried.term, -1)
	if app.localTerm != nil {
		t.Error("raw shell start failure must close the pane, not retry")
	}
}

// TestLocalTerminalRawMode opens with the multiplexer set to 无:
// raw shell, shell basename as the header name, no fallback note.
func TestLocalTerminalRawMode(t *testing.T) {
	stubLookPathFound(t)
	calls := stubTerminalConstructor(t)
	app := localTestApp()
	app.settings.Multiplexer = multiplexerNone
	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	if lt.tmux {
		t.Error("raw mode must not be tmux mode")
	}
	if lt.name == "" || strings.HasPrefix(lt.name, "tmux") {
		t.Errorf("raw mode name = %q, want a shell basename", lt.name)
	}
	if got := (*calls)[0]; got.Command != nil {
		t.Errorf("raw mode Command = %v, want nil", got.Command)
	}
	if app.status != "服务连接成功" {
		t.Errorf("raw mode must not set a fallback status, got %q", app.status)
	}
}

// TestLocalTerminalAppearanceSave checks the multiplexer pair
// saves with the appearance section and the running local
// terminal survives the save (font/theme/copy-on-select are
// applied to it by the same loop; the live copy gate itself is
// covered for serial terminals by TestCopyOnSelectLiveToggle).
func TestLocalTerminalAppearanceSave(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := localTestApp()
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	// Redirect HOME only now: the terminal is already created,
	// and the save below must land in a temp dir.
	t.Setenv("HOME", t.TempDir())
	app.setMultiplexer = multiplexerNone
	app.setTmuxSession = "debug-9"
	app.setCopyOnSelect = false
	app.applyAppearanceSettings()
	if app.localTerm == nil {
		t.Error("appearance save must not drop the local terminal")
	}
	if got := loadSettings(); got.Multiplexer != multiplexerNone || got.TmuxSession != "debug-9" {
		t.Errorf("saved multiplexer = %q/%q, want 无/debug-9", got.Multiplexer, got.TmuxSession)
	}
	// Staging reload drops the un-saved edit path back to saved.
	app.setMultiplexer = multiplexerTmux
	app.reloadSettingsStaging()
	if app.setMultiplexer != multiplexerNone || app.setTmuxSession != "debug-9" {
		t.Errorf("reload staging = %q/%q, want 无/debug-9", app.setMultiplexer, app.setTmuxSession)
	}
	app.closeLocalTerminal()
}
