package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Find bar tests (see findbar.go). Terminals are created BEFORE
// any HOME redirect: terminal.New loads libghostty-vt from the
// user cache, and a temp HOME would make it try to download the
// library instead. The search is driven by the terminal view's
// frames, so the tests pump frames while waiting (waitFor alone
// would never advance it).

// waitFrames pumps frames until cond holds or the deadline hits.
func waitFrames(t *testing.T, tt *ui.Tester, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		tt.Frame()
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFindBarOpenClose drives the bar's open/close state: opening
// shows it and focuses the input (the focus request is consumed
// on the next frame); closing ends the search and returns the
// focus to the terminal. With no terminal at all, opening is a
// no-op.
func TestFindBarOpenClose(t *testing.T) {
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openFindBar()
	st := &app.findSerial
	if !st.visible || !st.focusIn || st.term != sess.term {
		t.Errorf("after open: visible=%v focusIn=%v term-set=%v, want true/true/true", st.visible, st.focusIn, st.term == sess.term)
	}
	// The focus request is consumed by the next frame.
	tt.Frame()
	if st.focusIn {
		t.Error("focusIn must be consumed by the next frame")
	}
	// Opening again only refocuses (no toggle, no new state).
	st.query = "hello"
	app.openFindBar()
	if !st.visible || st.query != "hello" {
		t.Errorf("reopen must keep the bar and the query, visible=%v query=%q", st.visible, st.query)
	}

	app.closeFindBar(findPaneSerial)
	if st.visible {
		t.Error("close must hide the bar")
	}
	if !app.termFocusPending {
		t.Error("close must request focus back to the serial terminal")
	}
	// Closing again is a no-op, not a panic.
	app.closeFindBar(findPaneSerial)

	// No terminal anywhere: opening is a no-op.
	empty := &nativeApp{
		ports:    map[string]PortInfo{},
		portIDs:  []string{},
		terms:    map[string]*termSession{},
		settings: defaultSettings(),
		drafts:   map[string]*portDraft{},
	}
	empty.openFindBar()
	if empty.findSerial.visible || empty.findLocal.visible {
		t.Error("open with no terminal must be a no-op")
	}
}

// TestFindBarSearch feeds a real Conn-mode terminal a few lines
// and drives a search through the bar: the count comes from
// OnSearch, Enter/chevrons move the selection, and an empty
// query clears the search.
func TestFindBarSearch(t *testing.T) {
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	app.onData("ttyUSB0", []byte("hello world\r\nfoo bar\r\nhello again\r\n"))
	waitFor(t, "output to land", func() bool {
		return strings.Contains(sess.term.Text(), "hello")
	})
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.openFindBar()
	app.findSerial.query = "hello"
	// The change detector searches on the next frame; the view
	// drives the search across frames.
	waitFrames(t, tt, "search to find 2 matches", func() bool {
		return sess.term.SearchStatus().Total == 2
	})
	if got := app.findSerial.status; got.Total != 2 || got.Selected != 1 {
		t.Errorf("OnSearch status = %+v, want Total 2 Selected 1 (newest)", got)
	}
	// Enter (SearchNext) moves toward older output, Shift+Enter
	// (SearchPrevious) back toward newer.
	app.findEnter(findPaneSerial, false)
	if got := sess.term.SearchStatus().Selected; got != 2 {
		t.Errorf("Selected after SearchNext = %d, want 2 (older match)", got)
	}
	app.findEnter(findPaneSerial, true)
	if got := sess.term.SearchStatus().Selected; got != 1 {
		t.Errorf("Selected after SearchPrevious = %d, want 1 (newest match)", got)
	}
	// No matches: the count reports it, and the status still
	// carries the query.
	app.findSerial.query = "zzz"
	waitFrames(t, tt, "search to report no matches", func() bool {
		return sess.term.SearchStatus().Total == 0 && sess.term.SearchStatus().Text == "zzz"
	})
	if got := app.findSerial.status; got.Total != 0 {
		t.Errorf("OnSearch status for no matches = %+v, want Total 0", got)
	}
	// Empty query ends the search.
	app.findSerial.query = ""
	waitFrames(t, tt, "search to end", func() bool {
		return sess.term.SearchStatus().Text == ""
	})
	waitFrames(t, tt, "OnSearch to report the end", func() bool {
		return app.findSerial.status.Text == ""
	})
	// Closing after the search ended is still clean.
	app.closeFindBar(findPaneSerial)
	if app.findSerial.visible {
		t.Error("close must hide the bar")
	}
}

// TestFindBarPerPane checks the bars are independent and that a
// port switch closes the serial bar (the render's terminal
// identity check), while the local bar is untouched.
func TestFindBarPerPane(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := localTestApp()
	sess0 := app.ensureTerm("ttyUSB0")
	if sess0 == nil {
		t.Fatal("ensureTerm ttyUSB0 returned nil")
	}
	defer sess0.term.Close()
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	// Serial bar open with a query.
	app.openFindBar()
	app.findSerial.query = "hello"
	tt.Frame()
	if !app.findSerial.visible || app.findLocal.visible {
		t.Errorf("after serial open: serial=%v local=%v, want true/false", app.findSerial.visible, app.findLocal.visible)
	}
	// Local bar opens beside it (focus moved to the local pane
	// on open); the serial bar and its query are untouched.
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	tt.Frame()
	if app.findPane != findPaneLocal {
		t.Errorf("findPane after opening the local terminal = %d, want local", app.findPane)
	}
	app.openFindBar()
	app.findLocal.query = "local-q"
	tt.Frame()
	if !app.findLocal.visible || !app.findSerial.visible {
		t.Errorf("both bars must stay open: serial=%v local=%v", app.findSerial.visible, app.findLocal.visible)
	}
	if app.findSerial.query != "hello" {
		t.Errorf("serial query = %q, want hello (untouched by the local bar)", app.findSerial.query)
	}
	// Switching ports closes the serial bar (EndSearch on the
	// old terminal) and leaves the local bar alone.
	app.selectPort("ttyUSB1")
	tt.Frame()
	if app.findSerial.visible {
		t.Error("port switch must close the serial bar")
	}
	if got := sess0.term.SearchStatus().Text; got != "" {
		t.Errorf("old terminal's search after switch = %q, want ended", got)
	}
	if !app.findLocal.visible {
		t.Error("port switch must leave the local bar alone")
	}
}

// TestFindBarSplitRightStaysTopRight is the regression test for
// the right-split find bar landing at the local pane's LEFT edge
// (Jemy's 2026-11-06 screenshot): the bar's .Right(8) inset put
// its right edge flush on the window edge, past the 4px
// windowMargin, so AttachTo's overflow flip moved it to the
// other side of its anchor. Both bars are opened with real
// searches (distinct counts), and the count texts anchor the
// assertion: the local count must sit in the window's right
// half, hard against the right edge, while the serial count
// stays in the left half, hard against the split.
func TestFindBarSplitRightStaysTopRight(t *testing.T) {
	stubLookPathFound(t)
	// Like stubTerminalConstructor, but the test keeps the local
	// terminal's Conn so it can feed it output to search: the
	// shared stub discards the passed-in options and its Conn
	// with them.
	oldNewTerminal := newTerminalFunc
	localConn := newPortConn(&fakeSender{}, "local")
	newTerminalFunc = func(o terminal.Options) (*terminal.Terminal, error) {
		return terminal.New(terminal.Options{Conn: localConn, OnSearch: o.OnSearch})
	}
	t.Cleanup(func() { newTerminalFunc = oldNewTerminal })
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	app.onData("ttyUSB0", []byte("hello world\r\nhello again\r\n"))
	waitFor(t, "serial output to land", func() bool {
		return strings.Contains(sess.term.Text(), "hello again")
	})
	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	localConn.deliver([]byte("cd ~/workspace\r\n"))
	waitFor(t, "local output to land", func() bool {
		return strings.Contains(lt.term.Text(), "workspace")
	})

	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()

	app.findPane = findPaneSerial
	app.openFindBar()
	app.findSerial.query = "hello"
	waitFrames(t, tt, "serial search to find 2 matches", func() bool {
		return sess.term.SearchStatus().Total == 2
	})
	app.findPane = findPaneLocal
	app.openFindBar()
	app.findLocal.query = "workspace"
	waitFrames(t, tt, "local search to find 1 match", func() bool {
		return lt.term.SearchStatus().Total == 1
	})
	tt.Frame()

	// Right split: the serial pane is the left half, the local
	// pane the right half; each bar hangs off its own pane's
	// top-right corner, 8px in from the pane edge.
	assertBar := func(name, count string, wantRightMin, wantRightMax float32) {
		t.Helper()
		r, ok := tt.Find(count)
		if !ok {
			t.Fatalf("%s count %q not rendered: texts = %v", name, count, tt.Texts())
		}
		if right := r.X + r.W; right < wantRightMin || right > wantRightMax {
			t.Errorf("%s count %q right edge = %.0f, want %.0f..%.0f (bar flipped or misplaced)", name, count, right, wantRightMin, wantRightMax)
		}
	}
	assertBar("serial", "1 of 2", 300, 600)
	assertBar("local", "1 of 1", 1000, 1200)

	// Below split: both panes are full-width, so both bars must
	// sit hard against the window's right edge — the same flip
	// would have hit them here too.
	app.splitLayout = splitLayoutBelow
	tt.Frame()
	assertBar("serial (below)", "1 of 2", 1000, 1200)
	assertBar("local (below)", "1 of 1", 1000, 1200)
}

// TestFindBarLocalOnSearch checks the local terminal's OnSearch
// wiring through the stub's recorded options: the callback is
// set, current statuses land in the local bar, stale ones (a
// query the bar no longer shows) and ones arriving after the
// bar closed are ignored.
func TestFindBarLocalOnSearch(t *testing.T) {
	stubLookPathFound(t)
	calls := stubTerminalConstructor(t)
	app := localTestApp()
	app.openLocalTerminal(false)
	lt := app.localTerm
	if lt == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	if len(*calls) != 1 {
		t.Fatalf("constructor calls = %d, want 1", len(*calls))
	}
	onSearch := (*calls)[0].OnSearch
	if onSearch == nil {
		t.Fatal("local terminal must set OnSearch")
	}
	st := &app.findLocal
	st.visible = true
	st.query = "x"
	onSearch(terminal.SearchStatus{Text: "x", Total: 3, Selected: 2})
	if st.status.Total != 3 || st.status.Selected != 2 {
		t.Errorf("local status after OnSearch = %+v, want Total 3 Selected 2", st.status)
	}
	// Stale: a status for another query is ignored.
	onSearch(terminal.SearchStatus{Text: "y", Total: 1, Selected: 1})
	if st.status.Total != 3 {
		t.Errorf("stale OnSearch must be ignored, status = %+v", st.status)
	}
	// Hidden: statuses are ignored until the bar opens again.
	st.visible = false
	onSearch(terminal.SearchStatus{Text: "x", Total: 5, Selected: 1})
	if st.status.Total != 3 {
		t.Errorf("OnSearch while hidden must be ignored, status = %+v", st.status)
	}
}
