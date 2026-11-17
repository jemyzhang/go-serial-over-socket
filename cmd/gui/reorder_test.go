package main

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Pill reorder (see toolbarView): a pill is a Drag source for its
// port id, the strip is the single Drop target, and the dragged
// port lands where the pointer released — before the first pill
// whose center is right of it, else at the end. The order is this
// session's only (ports come and go); broadcasts keep it (see
// mergePortOrder) and new ports append at the end.

// reorderTester builds a three-port app with real terminals, no
// WS client: dragging needs no server. Baud rates differ per
// port so each pill's baud text is distinct.
func reorderTester(t *testing.T) (*nativeApp, *ui.Tester, func()) {
	t.Helper()
	terms := map[string]*termSession{}
	ports := map[string]PortInfo{}
	ids := []string{"ttyUSB0", "ttyUSB1", "ttyUSB2"}
	bauds := []int{9600, 115200, 57600}
	for i, id := range ids {
		conn := newPortConn(&fakeSender{}, id)
		term, err := terminal.New(terminal.Options{Conn: conn})
		if err != nil {
			t.Fatalf("terminal.New: %v", err)
		}
		terms[id] = &termSession{term: term, conn: conn}
		ports[id] = PortInfo{ID: id, Status: "connected", Connected: true, Baudrate: bauds[i], Databits: 8, Stopbit: 1, Parity: "N"}
	}
	app := &nativeApp{
		ports:      ports,
		portIDs:    []string{"ttyUSB0", "ttyUSB1", "ttyUSB2"},
		selected:   "ttyUSB0",
		terms:      terms,
		settings:   defaultSettings(),
		drafts:     map[string]*portDraft{},
		cmdHistIdx: -1,
		histNavIdx: -1,
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	cleanup := func() {
		for _, sess := range terms {
			_ = sess.term.Close()
		}
	}
	return app, tt, cleanup
}

// pillNameCenter returns the center of the pill showing name,
// from the rendered toolbar: the y check keeps it from matching
// the command bar's label for the same port at the bottom.
func pillNameCenter(t *testing.T, tt *ui.Tester, name string) (float32, float32) {
	t.Helper()
	r, ok := tt.Find(name)
	if !ok {
		t.Fatalf("pill %q not found; texts = %v", name, tt.Texts())
	}
	if r.Y > 100 {
		t.Fatalf("pill %q found at y=%v, want the toolbar pill (top)", name, r.Y)
	}
	return r.X + r.W/2, r.Y + r.H/2
}

// dragPill presses on the pill showing from, moves in steps to
// (toX, toY), and releases there — the mygo drag helper's shape.
func dragPill(tt *ui.Tester, fromX, fromY, toX, toY float32) {
	tt.Press(fromX, fromY)
	for k := 1; k <= 4; k++ {
		f := float32(k) / 4
		tt.Move(fromX+(toX-fromX)*f, fromY+(toY-fromY)*f)
	}
	tt.Release(toX, toY)
	tt.Frame()
}

func TestMovePortTo(t *testing.T) {
	app := &nativeApp{portIDs: []string{"a", "b", "c"}}
	app.movePortTo("a", 2)
	if got := app.portIDs; !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Errorf("move a to 2: %v", got)
	}
	app.movePortTo("c", 0)
	if got := app.portIDs; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("move c to 0: %v", got)
	}
	app.movePortTo("b", 1)
	if got := app.portIDs; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("move b to 1 (no-op): %v", got)
	}
	app.movePortTo("x", 0)
	if got := app.portIDs; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("move unknown id: %v", got)
	}
	app.movePortTo("a", 99)
	if got := app.portIDs; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("move a to 99 (clamp to end): %v", got)
	}
	app.movePortTo("c", -5)
	if got := app.portIDs; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Errorf("move c to -5 (clamp to front): %v", got)
	}
}

func TestMergePortOrder(t *testing.T) {
	ports := map[string]PortInfo{
		"ttyUSB0": {ID: "ttyUSB0"},
		"ttyUSB1": {ID: "ttyUSB1"},
		"ttyUSB2": {ID: "ttyUSB2"},
		"ttyUSB3": {ID: "ttyUSB3"},
	}
	// A dragged order survives a broadcast; the new port
	// appends in sorted order at the end.
	if got := mergePortOrder([]string{"ttyUSB2", "ttyUSB0", "ttyUSB1"}, ports); !slices.Equal(got, []string{"ttyUSB2", "ttyUSB0", "ttyUSB1", "ttyUSB3"}) {
		t.Errorf("merge dragged order: %v", got)
	}
	// First broadcast: just the sorted list.
	if got := mergePortOrder(nil, ports); !slices.Equal(got, []string{"ttyUSB0", "ttyUSB1", "ttyUSB2", "ttyUSB3"}) {
		t.Errorf("merge from empty: %v", got)
	}
	// A closed id drops out.
	if got := mergePortOrder([]string{"ttyUSB0", "ttyUSB1", "ttyUSB2"}, map[string]PortInfo{
		"ttyUSB0": {ID: "ttyUSB0"},
		"ttyUSB2": {ID: "ttyUSB2"},
	}); !slices.Equal(got, []string{"ttyUSB0", "ttyUSB2"}) {
		t.Errorf("merge with a closed id: %v", got)
	}
}

func TestPillDragReorders(t *testing.T) {
	app, tt, cleanup := reorderTester(t)
	defer cleanup()
	// Drag ttyUSB0 to the end: release well right of ttyUSB2's
	// pill center (its name center + 120 is inside the pill's
	// right half with room on both sides).
	ax, ay := pillNameCenter(t, tt, "ttyUSB0")
	cx, cy := pillNameCenter(t, tt, "ttyUSB2")
	dragPill(tt, ax, ay, cx+120, cy)
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB1", "ttyUSB2", "ttyUSB0"}) {
		t.Fatalf("after drag to end: portIDs = %v", got)
	}
	// Drag ttyUSB0 (now last) to the front: release left of
	// ttyUSB1's pill center (its name center - 10 is inside
	// the pill's left half with room on both sides).
	ax, ay = pillNameCenter(t, tt, "ttyUSB0")
	bx, by := pillNameCenter(t, tt, "ttyUSB1")
	dragPill(tt, ax, ay, bx-10, by)
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB0", "ttyUSB1", "ttyUSB2"}) {
		t.Fatalf("after drag to front: portIDs = %v", got)
	}
	// The selection follows the port, not the slot.
	if app.selected != "ttyUSB0" {
		t.Errorf("selected = %q, want ttyUSB0", app.selected)
	}
}

func TestPillClickStillSelects(t *testing.T) {
	app, tt, cleanup := reorderTester(t)
	defer cleanup()
	x, y := pillNameCenter(t, tt, "ttyUSB1")
	tt.ClickAt(x, y)
	tt.Frame()
	if app.selected != "ttyUSB1" {
		t.Errorf("selected = %q, want ttyUSB1", app.selected)
	}
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB0", "ttyUSB1", "ttyUSB2"}) {
		t.Errorf("a click reordered the pills: %v", got)
	}
}

func TestPillDragOutsideStripDoesNothing(t *testing.T) {
	app, tt, cleanup := reorderTester(t)
	defer cleanup()
	ax, ay := pillNameCenter(t, tt, "ttyUSB0")
	// Release over the terminal, off the strip: no drop target.
	dragPill(tt, ax, ay, ax+200, 400)
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB0", "ttyUSB1", "ttyUSB2"}) {
		t.Errorf("drag off the strip reordered the pills: %v", got)
	}
}

func TestPillDragSurvivesBroadcast(t *testing.T) {
	app, tt, cleanup := reorderTester(t)
	defer cleanup()
	ax, ay := pillNameCenter(t, tt, "ttyUSB0")
	cx, cy := pillNameCenter(t, tt, "ttyUSB2")
	dragPill(tt, ax, ay, cx+120, cy)
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB1", "ttyUSB2", "ttyUSB0"}) {
		t.Fatalf("after drag: portIDs = %v", got)
	}
	// A broadcast with the same ports — plus a new one — keeps
	// the dragged order and appends the new port at the end.
	connected := func(id string, baud int) PortInfo {
		return PortInfo{ID: id, Status: "connected", Connected: true, Baudrate: baud, Databits: 8, Stopbit: 1, Parity: "N"}
	}
	app.onPorts(map[string]PortInfo{
		"ttyUSB0": connected("ttyUSB0", 9600),
		"ttyUSB1": connected("ttyUSB1", 115200),
		"ttyUSB2": connected("ttyUSB2", 57600),
		"ttyUSB3": connected("ttyUSB3", 9600),
	})
	if got := app.portIDs; !slices.Equal(got, []string{"ttyUSB1", "ttyUSB2", "ttyUSB0", "ttyUSB3"}) {
		t.Errorf("after broadcast: portIDs = %v", got)
	}
}
