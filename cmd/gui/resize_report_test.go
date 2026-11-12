package main

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// Resize reporting tests (see onSerialResize): the plugin tells
// portConn the size on every paint-driven change, and only the
// selected port's size, when it differs from the last reported
// one, is re-announced to the server as terminal_ready. The
// fake server records every command, so the tests assert on what
// it actually received, end to end.

// readySizes returns the (rows, cols) of every terminal_ready the
// fake server has received, in order. The dial's fixed 80x24 is
// always first.
func readySizes(srv *loopbackServer) [][2]int {
	var out [][2]int
	for _, cmd := range loopbackCommands(srv) {
		if cmd["type"] != "terminal_ready" {
			continue
		}
		rows, _ := cmd["rows"].(float64)
		cols, _ := cmd["cols"].(float64)
		out = append(out, [2]int{int(rows), int(cols)})
	}
	return out
}

// resizeTestApp wires a loopback-backed app with a resident
// serial terminal, ready to render: the dial's terminal_ready
// (80x24) is already recorded by the fake server.
func resizeTestApp(t *testing.T, srv *loopbackServer) (*nativeApp, *termSession) {
	t.Helper()
	app, _ := dialAutoConnectApp(t, srv)
	// dialAutoConnectApp leaves the render fields unset; mirror
	// localTestApp for the ones render touches.
	app.terms = map[string]*termSession{}
	app.drafts = map[string]*portDraft{}
	app.status = "服务连接成功"
	app.splitLayout = splitLayoutRight
	app.splitPercent = defaultSplitPercent
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	return app, sess
}

// waitReadySize waits until the fake server's last terminal_ready
// carries rows/cols (or the deadline in waitFor fires).
func waitReadySize(t *testing.T, srv *loopbackServer, rows, cols int) {
	t.Helper()
	waitFor(t, "terminal_ready with size", func() bool {
		got := readySizes(srv)
		if len(got) == 0 {
			return false
		}
		return got[len(got)-1] == [2]int{rows, cols}
	})
}

// TestSerialResizeReport covers the decision logic end to end:
// the first paint's Resize reports the terminal's real size
// (not the dial's 80x24), the same size is not resent, an
// unselected port's Resize is ignored, and a selected port's
// Resize through portConn is reported.
func TestSerialResizeReport(t *testing.T) {
	srv := &loopbackServer{}
	app, sess := resizeTestApp(t, srv)
	// The dial's fixed 80x24 is the only terminal_ready so far.
	if got := readySizes(srv); len(got) != 1 || got[0] != [2]int{24, 80} {
		t.Fatalf("dial terminal_ready = %v, want [{24 80}]", got)
	}

	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	// The first paint's Resize reports the real size.
	cols, rows := sess.term.Size()
	waitReadySize(t, srv, rows, cols)
	if got := readySizes(srv); len(got) != 2 {
		t.Fatalf("terminal_ready count after first paint = %d (%v), want 2", len(got), got)
	}

	// Same size again: not resent.
	tt.Frame()
	tt.Frame()
	time.Sleep(300 * time.Millisecond)
	if got := readySizes(srv); len(got) != 2 {
		t.Errorf("same size must not be resent, terminal_ready = %v", got)
	}

	// An unselected port's Resize is ignored (through portConn).
	other := app.ensureTerm("ttyUSB1")
	if other == nil {
		t.Fatal("ensureTerm ttyUSB1 returned nil")
	}
	defer other.term.Close()
	if err := other.conn.Resize(100, 50); err != nil {
		t.Fatalf("unselected Resize: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := readySizes(srv); len(got) != 2 {
		t.Errorf("unselected port must not report, terminal_ready = %v", got)
	}

	// The selected port's Resize through portConn is reported,
	// once, with the values it carried.
	if err := sess.conn.Resize(cols+1, rows); err != nil {
		t.Fatalf("selected Resize: %v", err)
	}
	waitReadySize(t, srv, rows, cols+1)
	// And the same size again is not resent.
	if err := sess.conn.Resize(cols+1, rows); err != nil {
		t.Fatalf("selected Resize again: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := readySizes(srv); len(got) != 3 {
		t.Errorf("repeated size must not be resent, terminal_ready = %v", got)
	}
}

// TestSerialResizeSplitRegression is the reported bug: opening
// and closing the split, and resizing the window, must report
// the size the terminal actually has (term.Size()) each time.
func TestSerialResizeSplitRegression(t *testing.T) {
	srv := &loopbackServer{}
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app, sess := resizeTestApp(t, srv)

	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	cols, rows := sess.term.Size()
	waitReadySize(t, srv, rows, cols)
	before := len(readySizes(srv))

	// Open the split: the serial pane narrows, the report follows.
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	tt.Frame()
	waitFor(t, "terminal_ready after opening the split", func() bool {
		return len(readySizes(srv)) > before
	})
	time.Sleep(100 * time.Millisecond)
	cols, rows = sess.term.Size()
	if got := readySizes(srv); got[len(got)-1] != [2]int{rows, cols} {
		t.Errorf("last terminal_ready after opening the split = %v, want [%d %d]", got[len(got)-1], rows, cols)
	}
	if cols >= 120 {
		t.Errorf("split must narrow the serial terminal, Size() = %dx%d", cols, rows)
	}

	// Close the split: full width again, reported as such.
	before = len(readySizes(srv))
	app.closeLocalTerminal()
	tt.Frame()
	waitFor(t, "terminal_ready after closing the split", func() bool {
		return len(readySizes(srv)) > before
	})
	time.Sleep(100 * time.Millisecond)
	cols, rows = sess.term.Size()
	if got := readySizes(srv); got[len(got)-1] != [2]int{rows, cols} {
		t.Errorf("last terminal_ready after closing the split = %v, want [%d %d]", got[len(got)-1], rows, cols)
	}

	// Window resize: same treatment.
	before = len(readySizes(srv))
	tt.SetSize(800, 600)
	tt.Frame()
	waitFor(t, "terminal_ready after window resize", func() bool {
		return len(readySizes(srv)) > before
	})
	time.Sleep(100 * time.Millisecond)
	cols, rows = sess.term.Size()
	if got := readySizes(srv); got[len(got)-1] != [2]int{rows, cols} {
		t.Errorf("last terminal_ready after window resize = %v, want [%d %d]", got[len(got)-1], rows, cols)
	}
}
