package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Keyboard shortcuts tests (see shortcuts.go): Ctrl+Shift+Enter
// toggles the local terminal, Ctrl+Shift+L zooms the focused
// pane, Ctrl+Shift+K clears the focused pane's screen, and
// Ctrl+Shift+? opens the help overlay. Terminals are created
// BEFORE any HOME redirect, as in the other terminal tests.

// splitShortcutApp builds a loopback-backed app with a resident
// serial terminal and a stubbed local-terminal constructor,
// ready to render.
func splitShortcutApp(t *testing.T, srv *loopbackServer) (*nativeApp, *termSession) {
	t.Helper()
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app, sess := resizeTestApp(t, srv)
	return app, sess
}

// TestToggleLocalTerminalShortcut drives Ctrl+Shift+Enter: it
// opens the local terminal on the saved (right) layout and
// focuses it, and a second press closes it and returns the
// focus to the serial terminal.
func TestToggleLocalTerminalShortcut(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	colsFull, _ := sess.term.Size()
	_, rowsFull := sess.term.Size()
	waitReadySize(t, srv, rowsFull, colsFull)

	if app.localTerm != nil {
		t.Fatal("local terminal must start closed")
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyEnter)
	tt.Frame()
	if app.localTerm == nil {
		t.Fatal("Ctrl+Shift+Enter must open the local terminal")
	}
	if app.splitLayout != splitLayoutRight {
		t.Errorf("splitLayout = %q, want right (the saved layout)", app.splitLayout)
	}
	if !tt.HasText("tmux · serial-debug") {
		t.Errorf("local pane header missing; texts = %v", tt.Texts())
	}
	if app.findPane != findPaneLocal {
		t.Errorf("findPane = %d, want local (focus must follow the open)", app.findPane)
	}
	colsSplit, _ := sess.term.Size()
	if colsSplit >= colsFull {
		t.Errorf("split must narrow the serial terminal, cols %d -> %d", colsFull, colsSplit)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyEnter)
	tt.Frame()
	if app.localTerm != nil {
		t.Error("second Ctrl+Shift+Enter must close the local terminal")
	}
	if tt.HasText("tmux · serial-debug") {
		t.Errorf("local pane header must be gone; texts = %v", tt.Texts())
	}
	if app.findPane != findPaneSerial {
		t.Errorf("findPane = %d, want serial (focus must return)", app.findPane)
	}
	cols, _ := sess.term.Size()
	if cols != colsFull {
		t.Errorf("serial cols after close = %d, want %d (full width back)", cols, colsFull)
	}
}

// TestToggleLocalTerminalNoLeak covers the two leak paths of
// Ctrl+Shift+Enter: with the serial terminal focused it must
// not send an Enter to the device, and with the command bar
// focused it must not send the command.
func TestToggleLocalTerminalNoLeak(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	// Serial terminal focused (AutoFocus on first appearance):
	// the chord opens the local terminal and sends nothing.
	writesBefore := len(loopbackWrites(srv))
	connectsBefore := loopbackCommandCount(srv, "connect_port")
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyEnter)
	tt.Frame()
	if app.localTerm == nil {
		t.Fatal("Ctrl+Shift+Enter must open the local terminal")
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes after Ctrl+Shift+Enter = %d, want %d (no Enter to the device)", got, writesBefore)
	}
	if got := loopbackCommandCount(srv, "connect_port"); got != connectsBefore {
		t.Errorf("connect_port count = %d, want %d (no auto-connect from the chord)", got, connectsBefore)
	}
	// Closing again sends nothing either.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyEnter)
	tt.Frame()
	if app.localTerm != nil {
		t.Error("second Ctrl+Shift+Enter must close the local terminal")
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes after closing = %d, want %d", got, writesBefore)
	}

	// Command bar focused, with a command typed: the chord
	// opens the local terminal and leaves the command unsent.
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(600, sendRect.Y+sendRect.H/2)
	tt.Type("AT")
	if app.cmdInput != "AT" {
		t.Fatalf("cmdInput = %q, want AT", app.cmdInput)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyEnter)
	tt.Frame()
	if app.localTerm == nil {
		t.Error("Ctrl+Shift+Enter from the command bar must open the local terminal")
	}
	if app.cmdInput != "AT" {
		t.Errorf("cmdInput = %q, want AT (the command must stay unsent)", app.cmdInput)
	}
	if got := loopbackWrites(srv); len(got) != writesBefore {
		t.Errorf("writes = %v, want none (the command must not send)", got)
	}
}

// TestZoomSerialPane drives Ctrl+Shift+L on the focused serial
// pane: the local pane hides, the serial terminal grows to full
// width and reports the new size, and a second press restores
// the split and the size.
func TestZoomSerialPane(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	colsFull, rowsFull := sess.term.Size()
	waitReadySize(t, srv, rowsFull, colsFull)

	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	tt.Frame()
	waitFor(t, "terminal_ready after opening the split", func() bool {
		return len(readySizes(srv)) > 1
	})
	time.Sleep(100 * time.Millisecond)
	colsSplit, rowsSplit := sess.term.Size()
	if colsSplit >= colsFull {
		t.Fatalf("split must narrow the serial terminal, cols %d -> %d", colsFull, colsSplit)
	}
	// Focus the serial pane (opening the split focused local).
	tt.ClickAt(200, 300)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Fatalf("findPane = %d, want serial after clicking its pane", app.findPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if !app.paneZoomed || app.zoomedPane != findPaneSerial {
		t.Errorf("paneZoomed=%v zoomedPane=%d, want true/serial", app.paneZoomed, app.zoomedPane)
	}
	if tt.HasText("tmux · serial-debug") {
		t.Errorf("local pane header must hide while zoomed; texts = %v", tt.Texts())
	}
	cols, rows := sess.term.Size()
	if cols != colsFull || rows != rowsFull {
		t.Errorf("zoomed serial Size() = %dx%d, want %dx%d (full width)", cols, rows, colsFull, rowsFull)
	}
	waitReadySize(t, srv, rows, cols)
	if got := readySizes(srv); got[len(got)-1] != [2]int{rows, cols} {
		t.Errorf("last terminal_ready = %v, want [%d %d]", got[len(got)-1], rows, cols)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if app.paneZoomed {
		t.Error("second Ctrl+Shift+L must restore the split")
	}
	if !tt.HasText("tmux · serial-debug") {
		t.Errorf("local pane header must be back; texts = %v", tt.Texts())
	}
	cols, rows = sess.term.Size()
	if cols != colsSplit || rows != rowsSplit {
		t.Errorf("restored serial Size() = %dx%d, want %dx%d", cols, rows, colsSplit, rowsSplit)
	}
	waitReadySize(t, srv, rows, cols)
}

// TestZoomLocalPane drives Ctrl+Shift+L with the local pane
// focused: the local terminal grows to full width, the serial
// terminal keeps its split size (and sends no new report), and
// a second press restores the split.
func TestZoomLocalPane(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	colsFull, _ := sess.term.Size()
	_, rowsFull := sess.term.Size()
	waitReadySize(t, srv, rowsFull, colsFull)

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	waitFor(t, "terminal_ready after opening the split", func() bool {
		return len(readySizes(srv)) > 1
	})
	time.Sleep(100 * time.Millisecond)
	colsSplit, _ := sess.term.Size()
	localColsSplit, _ := lt.term.Size()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local (opening focuses it)", app.findPane)
	}
	readyBefore := len(readySizes(srv))

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if !app.paneZoomed || app.zoomedPane != findPaneLocal {
		t.Errorf("paneZoomed=%v zoomedPane=%d, want true/local", app.paneZoomed, app.zoomedPane)
	}
	cols, _ := lt.term.Size()
	if cols != colsFull {
		t.Errorf("zoomed local cols = %d, want %d (full width)", cols, colsFull)
	}
	cols, _ = sess.term.Size()
	if cols != colsSplit {
		t.Errorf("serial cols while local zoomed = %d, want %d (unchanged)", cols, colsSplit)
	}
	time.Sleep(300 * time.Millisecond)
	if got := len(readySizes(srv)); got != readyBefore {
		t.Errorf("terminal_ready count = %d, want %d (serial size did not change)", got, readyBefore)
	}
	if localColsSplit >= colsFull {
		t.Errorf("split local cols %d must be under full width %d", localColsSplit, colsFull)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if app.paneZoomed {
		t.Error("second Ctrl+Shift+L must restore the split")
	}
	cols, _ = lt.term.Size()
	if cols != localColsSplit {
		t.Errorf("restored local cols = %d, want %d", cols, localColsSplit)
	}
}

// TestZoomSinglePaneNoop covers the no-split cases: with no
// local terminal, and with the local terminal full-width (no
// serial pane), Ctrl+Shift+L is a silent no-op.
func TestZoomSinglePaneNoop(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if app.paneZoomed {
		t.Error("Ctrl+Shift+L with a single pane must be a no-op")
	}

	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.localTerm.term.Close()
	// No serial pane: the local terminal is full-width.
	app.selected = ""
	tt.Frame()
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if app.paneZoomed {
		t.Error("Ctrl+Shift+L with only the local pane must be a no-op")
	}
}

// TestClearFocusedPane drives Ctrl+Shift+K on the serial pane:
// the screen clears, the scrollback stays, and nothing is sent
// to the device.
func TestClearFocusedPane(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	// A few lines, no scrolling: the screen clears to blank.
	app.onData("ttyUSB0", []byte("aaa\r\nbbb\r\nccc\r\n"))
	waitFor(t, "output to land", func() bool {
		return strings.Contains(sess.term.Text(), "ccc")
	})
	writesBefore := len(loopbackWrites(srv))
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyK)
	tt.Frame()
	if got := strings.TrimSpace(sess.term.Text()); got != "" {
		t.Errorf("Text() after clear = %q, want blank", got)
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes after clear = %d, want %d (clear sends nothing)", got, writesBefore)
	}

	// Enough lines to scroll: the screen clears but the
	// scrolled-off lines stay in the scrollback.
	var sb strings.Builder
	for i := 1; i <= 60; i++ {
		sb.WriteString(strings.Repeat("x", 10))
		sb.WriteString("line")
		if i < 10 {
			sb.WriteString("0")
		}
		sb.WriteString(strconv.Itoa(i) + "\r\n")
	}
	app.onData("ttyUSB0", []byte(sb.String()))
	waitFor(t, "scrolled output to land", func() bool {
		return strings.Contains(sess.term.Text(), "line60")
	})
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyK)
	tt.Frame()
	text := sess.term.Text()
	if !strings.Contains(text, "line01") {
		t.Errorf("Text() after clear must keep the scrollback (line01); got %q", text)
	}
	if strings.Contains(text, "line60") {
		t.Errorf("Text() after clear must not keep the screen lines (line60); got %q", text)
	}
}

// TestClearFocusedPaneAltScreen covers the full-screen guard:
// with a program on the alternate screen, Ctrl+Shift+K leaves
// it alone; back on the normal screen it clears.
func TestClearFocusedPaneAltScreen(t *testing.T) {
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.onData("ttyUSB0", []byte("aaa\r\nbbb\r\nccc\r\n"))
	waitFor(t, "output to land", func() bool {
		return strings.Contains(sess.term.Text(), "ccc")
	})
	sess.term.Feed([]byte("\x1b[?1049h" + "alt-line\r\n"))
	waitFor(t, "alt-screen output to land", func() bool {
		return strings.Contains(sess.term.Text(), "alt-line")
	})
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyK)
	tt.Frame()
	if !strings.Contains(sess.term.Text(), "alt-line") {
		t.Error("clear on the alternate screen must leave it alone")
	}
	sess.term.Feed([]byte("\x1b[?1049l"))
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyK)
	tt.Frame()
	if got := strings.TrimSpace(sess.term.Text()); got != "" {
		t.Errorf("Text() after clear on the normal screen = %q, want blank", got)
	}
}

// TestClearLocalPane drives Ctrl+Shift+K with the local pane
// focused: the local screen clears, its scrollback stays.
func TestClearLocalPane(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local (opening focuses it)", app.findPane)
	}
	lt.term.Feed([]byte("aaa\r\nbbb\r\nccc\r\n"))
	waitFor(t, "local output to land", func() bool {
		return strings.Contains(lt.term.Text(), "ccc")
	})
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyK)
	tt.Frame()
	if got := strings.TrimSpace(lt.term.Text()); got != "" {
		t.Errorf("local Text() after clear = %q, want blank", got)
	}
}

// TestHelpOverlay drives Ctrl+Shift+? end to end: the overlay
// opens with the full list, any key closes it without leaking
// into the terminal or the command bar, Esc closes it, a
// registered shortcut closes it without acting, and a click
// does not close it.
func TestHelpOverlay(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.localTerm.term.Close()
	tt.Frame()

	// Open: the overlay lists the four new shortcuts (and the
	// existing ones), and the opening chord does not close it
	// again on the same frame.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must open the help overlay")
	}
	for _, want := range []string{"快捷键", "Ctrl+Shift+Enter", "Ctrl+Shift+L", "Ctrl+Shift+K", "Ctrl+Shift+?", "Ctrl+,", "Ctrl+Shift+F", "按任意键关闭"} {
		if !tt.HasText(want) {
			t.Errorf("help must list %q; texts = %v", want, tt.Texts())
		}
	}

	// A plain key closes it and leaks nowhere: the command
	// input keeps its text, the terminal keeps its output, and
	// nothing is sent.
	app.cmdInput = "AT"
	app.onData("ttyUSB0", []byte("marker\r\n"))
	waitFor(t, "marker to land", func() bool {
		return strings.Contains(sess.term.Text(), "marker")
	})
	writesBefore := len(loopbackWrites(srv))
	tt.Key(0, ui.KeyX)
	tt.Frame()
	if app.helpOpen {
		t.Error("a plain key must close the help overlay")
	}
	if app.cmdInput != "AT" {
		t.Errorf("cmdInput = %q, want AT (the key must not reach the command bar)", app.cmdInput)
	}
	if !strings.Contains(sess.term.Text(), "marker") {
		t.Error("the closing key must not reach the terminal")
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes = %d, want %d (the closing key must send nothing)", got, writesBefore)
	}
	// Focus returns to the terminal that had it.
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane = %d, want local (focus must return to the opener)", app.findPane)
	}

	// Esc closes it too.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must reopen the help overlay")
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if app.helpOpen {
		t.Error("Esc must close the help overlay")
	}

	// A registered shortcut closes it without acting: L must
	// not zoom the split open behind it.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must reopen the help overlay")
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if app.helpOpen {
		t.Error("Ctrl+Shift+L must close the help overlay")
	}
	if app.paneZoomed {
		t.Error("Ctrl+Shift+L while help is open must not zoom")
	}

	// A click does not close it; a key after the click still does.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must reopen the help overlay")
	}
	tt.ClickAt(50, 50)
	tt.Frame()
	if !app.helpOpen {
		t.Error("a click must not close the help overlay")
	}
	tt.Key(0, ui.KeyX)
	tt.Frame()
	if app.helpOpen {
		t.Error("a key after a click must still close the help overlay")
	}
}

// splitFocusApp builds a loopback-backed app like
// splitShortcutApp, but keeps the local terminal's fake sender
// so a test can prove a chord sent nothing to the shell (the
// shared stub in stubTerminalConstructor discards its sender).
func splitFocusApp(t *testing.T, srv *loopbackServer) (*nativeApp, *termSession, *fakeSender) {
	t.Helper()
	stubLookPathFound(t)
	sender := &fakeSender{}
	localConn := newPortConn(sender, "local")
	old := newTerminalFunc
	newTerminalFunc = func(o terminal.Options) (*terminal.Terminal, error) {
		return terminal.New(terminal.Options{Conn: localConn, OnSearch: o.OnSearch})
	}
	t.Cleanup(func() { newTerminalFunc = old })
	app, sess := resizeTestApp(t, srv)
	return app, sess, sender
}

// TestFocusPaneJump drives Ctrl+Shift+[ / ]: with the split
// open, [ moves the focus to the first (serial) pane and ]
// back to the second (local) pane.
func TestFocusPaneJump(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local (opening focuses it)", app.findPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after [ = %d, want serial", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] = %d, want local", app.findPane)
	}
}

// TestFocusPaneJumpNoop covers the silent no-ops: already on
// the target pane, a single pane, and only the local pane.
func TestFocusPaneJumpNoop(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	// Single pane (no local terminal): neither key does
	// anything — no local terminal appears, focus stays.
	if app.findPane != findPaneSerial {
		t.Fatalf("findPane = %d, want serial initially", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.localTerm != nil {
		t.Error("] with a single pane must not open the local terminal")
	}
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after ] = %d, want serial (unchanged)", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after [ = %d, want serial (unchanged)", app.findPane)
	}

	// Already on the target pane: ] on local, [ on serial.
	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local (opening focuses it)", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] on local = %d, want local (unchanged)", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Fatalf("findPane after [ = %d, want serial", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after [ on serial = %d, want serial (unchanged)", app.findPane)
	}

	// Only the local pane (no serial port selected): [ has no
	// target, ] is already there.
	app.selected = ""
	tt.Frame()
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after [ with no serial pane = %d, want local (unchanged)", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] with no serial pane = %d, want local (unchanged)", app.findPane)
	}
}

// TestFocusPaneJumpBelowLayout drives Ctrl+Shift+[ / ] in the
// stacked layout: [ is the pane above (serial), ] the one
// below (local), and the jump does not change the layout.
func TestFocusPaneJumpBelowLayout(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	app.setSplitLayout(splitLayoutBelow)
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local (opening focuses it)", app.findPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after [ = %d, want serial (above)", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] = %d, want local (below)", app.findPane)
	}
	if app.splitLayout != splitLayoutBelow {
		t.Errorf("splitLayout = %q, want below (the jump must not change it)", app.splitLayout)
	}
}

// TestFocusPaneJumpNoLeak covers the three leak paths of
// Ctrl+Shift+[ / ]: with the serial terminal focused, ] must
// not send to the device or trigger a connect; with the local
// terminal focused, [ must not send to the shell; with the
// command bar focused, the chord must not send the command.
func TestFocusPaneJumpNoLeak(t *testing.T) {
	srv := &loopbackServer{}
	app, sess, sender := splitFocusApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()

	// Serial focused: ] jumps to local, sends nothing.
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Fatalf("findPane = %d, want serial before the leak check", app.findPane)
	}
	writesBefore := len(loopbackWrites(srv))
	connectsBefore := loopbackCommandCount(srv, "connect_port")
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] = %d, want local", app.findPane)
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes after ] = %d, want %d (nothing to the device)", got, writesBefore)
	}
	if got := loopbackCommandCount(srv, "connect_port"); got != connectsBefore {
		t.Errorf("connect_port count = %d, want %d (no auto-connect)", got, connectsBefore)
	}

	// Local focused: [ jumps to serial, sends nothing to the
	// shell. The baseline is taken after setup, in case the
	// split itself reported anything through the sender.
	localBefore := fakeWrites(sender)
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Errorf("findPane after [ = %d, want serial", app.findPane)
	}
	if got := fakeWrites(sender); got != localBefore {
		t.Errorf("local sender writes after [ = %q, want %q (nothing to the shell)", got, localBefore)
	}

	// Command bar focused, with a command typed: ] jumps the
	// focus away and leaves the command unsent.
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(600, sendRect.Y+sendRect.H/2)
	tt.Type("AT")
	if app.cmdInput != "AT" {
		t.Fatalf("cmdInput = %q, want AT", app.cmdInput)
	}
	writesBefore = len(loopbackWrites(srv))
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] from the command bar = %d, want local", app.findPane)
	}
	if app.cmdInput != "AT" {
		t.Errorf("cmdInput = %q, want AT (the command must stay unsent)", app.cmdInput)
	}
	if got := len(loopbackWrites(srv)); got != writesBefore {
		t.Errorf("writes = %d, want %d (the command must not send)", got, writesBefore)
	}
}

// TestFocusPaneJumpHelpOpen drives Ctrl+Shift+[ / ] with the
// help overlay open: each closes the overlay without jumping
// the focus.
func TestFocusPaneJumpHelpOpen(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Fatalf("findPane = %d, want local before help", app.findPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must open the help overlay")
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketLeft)
	tt.Frame()
	if app.helpOpen {
		t.Error("[ must close the help overlay")
	}
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after [ = %d, want local (no jump)", app.findPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeySlash)
	tt.Frame()
	if !app.helpOpen {
		t.Fatal("Ctrl+Shift+? must reopen the help overlay")
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.helpOpen {
		t.Error("] must close the help overlay")
	}
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] = %d, want local (no jump)", app.findPane)
	}
}

// TestFocusPaneJumpUnzooms covers the zoomed split: jumping
// to the hidden pane restores the split and moves the focus
// there, instead of dropping the focus request on a pane
// that is not rendered.
func TestFocusPaneJumpUnzooms(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := splitShortcutApp(t, srv)
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer lt.term.Close()
	tt.Frame()
	tt.ClickAt(200, 300)
	tt.Frame()
	if app.findPane != findPaneSerial {
		t.Fatalf("findPane = %d, want serial after clicking its pane", app.findPane)
	}
	tt.Key(ui.Ctrl|ui.Shift, ui.KeyL)
	tt.Frame()
	if !app.paneZoomed || app.zoomedPane != findPaneSerial {
		t.Fatalf("paneZoomed=%v zoomedPane=%d, want true/serial", app.paneZoomed, app.zoomedPane)
	}

	tt.Key(ui.Ctrl|ui.Shift, ui.KeyBracketRight)
	tt.Frame()
	if app.paneZoomed {
		t.Error("] to the hidden pane must restore the split")
	}
	if !tt.HasText("tmux · serial-debug") {
		t.Errorf("local pane header must be back; texts = %v", tt.Texts())
	}
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after ] = %d, want local", app.findPane)
	}
}
