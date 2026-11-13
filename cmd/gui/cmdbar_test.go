package main

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// cmdDropdownLine: one row, one line.
func TestCmdDropdownLine(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"AT+VERSION", "AT+VERSION"},
		{"AT+CMD line1\nline2 is here", "AT+CMD line1 …"},
		{"AT+CMD line1\n", "AT+CMD line1 …"},
		{"\nline2", " …"},
		{"AT+CMD line1\r\nline2", "AT+CMD line1 …"},
		{"AT+CMD line1\r", "AT+CMD line1"},
		{"", ""},
	} {
		if got := cmdDropdownLine(tc.in); got != tc.want {
			t.Errorf("cmdDropdownLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// commandWire: every line ends with CRLF, never a bare LF.
func TestCommandWire(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"AT", "AT"},
		{"line1\nline2", "line1\r\nline2"},
		{"line1\r\nline2", "line1\r\nline2"},
		{"line1\nline2\n", "line1\r\nline2\r\n"},
		{"", ""},
	} {
		if got := commandWire(tc.in); got != tc.want {
			t.Errorf("commandWire(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// cmdBarTester builds a one-port app with a real terminal, as
// TestCmdBarDropdownFlow does, optionally with a loopback WS
// client for tests that assert the bytes on the wire.
func cmdBarTester(t *testing.T, history []HistoryEntry, withClient bool) (*nativeApp, *fakeSender, *loopbackServer, *ui.Tester, func()) {
	t.Helper()
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	var srv *loopbackServer
	var client *wsClient
	cleanup := func() { _ = term.Close() }
	if withClient {
		srv = &loopbackServer{}
		httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
		termCleanup := cleanup
		cleanup = func() {
			termCleanup()
			httpSrv.Close()
		}
		var err error
		client, err = dialWS(httpSrv.URL, wsCallbacks{})
		if err != nil {
			cleanup()
			t.Fatalf("dialWS: %v", err)
		}
		prevCleanup := cleanup
		cleanup = func() {
			_ = client.conn.Close()
			prevCleanup()
		}
	}
	app := &nativeApp{
		client:      client,
		wsConnected: withClient,
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		},
		portIDs:    []string{"ttyUSB0"},
		selected:   "ttyUSB0",
		terms:      map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings:   defaultSettings(),
		history:    history,
		drafts:     map[string]*portDraft{},
		cmdHistIdx: -1,
		histNavIdx: -1,
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	return app, fake, srv, tt, cleanup
}

// clickCmdInput focuses the command input by clicking the middle
// of its row, as TestCmdBarDropdownFlow does.
func clickCmdInput(t *testing.T, tt *ui.Tester) {
	t.Helper()
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(600, sendRect.Y+sendRect.H/2)
	tt.Frame()
}

func waitLoopbackWrite(t *testing.T, srv *loopbackServer, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, w := range loopbackWrites(srv) {
			if w == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("loopback writes = %q, want %q", loopbackWrites(srv), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Enter sends a multi-line draft with every line CRLF-terminated
// (no bare LF), clears the input, and moves the focus to the
// serial terminal.
func TestCmdBarMultilineSend(t *testing.T) {
	app, fake, srv, tt, cleanup := cmdBarTester(t, nil, true)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("line1\nline2")
	tt.Frame()
	if app.cmdInput != "line1\nline2" {
		t.Fatalf("input = %q, want line1\\nline2", app.cmdInput)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("enter must clear the input, got %q", app.cmdInput)
	}
	waitLoopbackWrite(t, srv, "line1\r\nline2\r\n")
	// Focus left the input for the terminal: typing now reaches
	// the device, not the input.
	tt.Type("X")
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("typing after enter should not reach the input, got %q", app.cmdInput)
	}
	waitFakeWrites(t, fake, "X")
}

// Shift+Enter inserts a newline and sends nothing; a plain Enter
// after it sends the whole multi-line draft.
func TestCmdBarShiftEnterNewline(t *testing.T) {
	app, fake, srv, tt, cleanup := cmdBarTester(t, nil, true)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("line1")
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Type("line2")
	tt.Frame()
	if app.cmdInput != "line1\nline2" {
		t.Fatalf("shift+enter should break the line, input = %q, want line1\\nline2", app.cmdInput)
	}
	if got := loopbackWrites(srv); len(got) != 0 {
		t.Fatalf("shift+enter must not send, writes = %q", got)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("enter must clear the input, got %q", app.cmdInput)
	}
	waitLoopbackWrite(t, srv, "line1\r\nline2\r\n")
	if got := fakeWrites(fake); got != "" {
		t.Fatalf("nothing typed into the terminal yet, fake writes = %q", got)
	}
}

// A multi-line history entry shows one row of one line: the
// second line never appears, the first line carries " …", and
// the row is exactly one line tall, like a short row.
func TestCmdBarDropdownSingleLine(t *testing.T) {
	app, _, _, tt, cleanup := cmdBarTester(t, []HistoryEntry{
		{ID: "h1", Command: "AT+CMD line1\nline2 is here"},
		{ID: "h2", Command: "AT"},
	}, false)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("AT")
	tt.Frame()
	if app.cmdInput != "AT" {
		t.Fatalf("input = %q, want AT", app.cmdInput)
	}
	if tt.HasText("line2 is here") {
		t.Fatalf("second line must not show in the dropdown; texts = %v", tt.Texts())
	}
	if !tt.HasText("AT+CMD line1 …") {
		t.Fatalf("first line + marker should show; texts = %v", tt.Texts())
	}
	long, ok1 := tt.Find("AT+CMD line1 …")
	short, ok2 := tt.Find("AT")
	if !ok1 || !ok2 {
		t.Fatalf("rows not found; texts = %v", tt.Texts())
	}
	if long.H != short.H {
		t.Fatalf("multi-line row height = %v, short row = %v, want equal (one line)", long.H, short.H)
	}
}

// ↑/↓ split on the caret, the WebUI rule: in the middle of a
// multi-line draft the caret moves and no history is touched;
// at the top edge Up navigates history, at the bottom edge
// Down navigates (or does nothing when there is nowhere to go).
func TestCmdBarUpDownCaretSplit(t *testing.T) {
	app, _, _, tt, cleanup := cmdBarTester(t, []HistoryEntry{
		{ID: "h1", Command: "AT+VERSION"},
		{ID: "h2", Command: "AT+RESET"},
	}, false)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("AB\nCD")
	tt.Frame()
	// Caret to the middle of the draft (line 2, column 0).
	tt.Key(0, ui.KeyLeft)
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	// Up from the middle moves the caret, not history.
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	if app.cmdInput != "AB\nCD" || app.histNavIdx != -1 {
		t.Fatalf("up mid-draft must move the caret only, input = %q histNavIdx = %d", app.cmdInput, app.histNavIdx)
	}
	// Up again: the caret is now at offset 0, so history nav
	// takes over and fills the oldest entry.
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	if app.cmdInput != "AT+VERSION" || app.histNavIdx != 0 {
		t.Fatalf("up at offset 0 must navigate history, input = %q histNavIdx = %d", app.cmdInput, app.histNavIdx)
	}
	// The fill re-opens the dropdown for the filled text, so
	// the next Down would navigate the dropdown instead: Esc
	// closes it first, then Down walks back past the newest to
	// a blank input.
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if app.cmdInput != "" || app.histNavIdx != -1 {
		t.Fatalf("down past newest must blank the input, input = %q histNavIdx = %d", app.cmdInput, app.histNavIdx)
	}
	// Down at the end of a fresh draft with nowhere to go does
	// nothing.
	tt.Type("XY\nZW")
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if app.cmdInput != "XY\nZW" || app.histNavIdx != -1 {
		t.Fatalf("down at end with no history position must be a no-op, input = %q histNavIdx = %d", app.cmdInput, app.histNavIdx)
	}
}

// The area grows from 1 line to 5, then stops: the Send
// button's top edge moves up to the 5-line height and no
// further, however many lines are typed.
func TestCmdBarHeightCap(t *testing.T) {
	_, _, _, tt, cleanup := cmdBarTester(t, nil, false)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("one")
	tt.Frame()
	one, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.Type("\n2\n3\n4\n5")
	tt.Frame()
	five, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found after 5 lines; texts = %v", tt.Texts())
	}
	if !(five.Y < one.Y) {
		t.Fatalf("5-line bar should sit higher than 1-line: y1 = %v y5 = %v", one.Y, five.Y)
	}
	tt.Type("\n6\n7\n8\n9\n10")
	tt.Frame()
	ten, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found after 10 lines; texts = %v", tt.Texts())
	}
	if ten.Y != five.Y {
		t.Fatalf("10-line bar must stop at the 5-line height: y5 = %v y10 = %v", five.Y, ten.Y)
	}
}

// Navigating the dropdown and then sending with the button
// (not Enter) must not yank the focus back to the input: the
// fill's caret request is moot once the button clears the input.
// The dropdown is Esc-closed first, so the button click frame
// does not re-focus the input on its own (with the dropdown
// open, the click frame's Focus call leaves the focus in the
// input, as before — that is the button's old behavior).
func TestCmdBarDropdownThenButtonSend(t *testing.T) {
	app, fake, srv, tt, cleanup := cmdBarTester(t, []HistoryEntry{
		{ID: "h1", Command: "AT+VERSION"},
	}, true)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("AT")
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if app.cmdInput != "AT+VERSION" {
		t.Fatalf("down should fill the input live, got %q", app.cmdInput)
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(sendRect.X+sendRect.W/2, sendRect.Y+sendRect.H/2)
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("button send must clear the input, got %q", app.cmdInput)
	}
	waitLoopbackWrite(t, srv, "AT+VERSION\r\n")
	// Typing after a button send reaches neither the terminal
	// (focus did not move there) nor the input (the button holds
	// the focus, as before). Wait out the terminal's drain first:
	// a write that did go to the terminal arrives asynchronously.
	tt.Type("X")
	tt.Frame()
	time.Sleep(200 * time.Millisecond)
	if got := fakeWrites(fake); got != "" {
		t.Fatalf("typing after a button send must not reach the terminal, fake writes = %q", got)
	}
	if app.cmdInput != "" {
		t.Fatalf("typing after a button send must not reach the input, got %q", app.cmdInput)
	}
}

// The Send button keeps its old behavior: it sends and clears,
// but does NOT move the focus to the terminal (only Enter does).
func TestCmdBarSendButtonKeepsFocus(t *testing.T) {
	app, fake, srv, tt, cleanup := cmdBarTester(t, nil, true)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("AT")
	tt.Frame()
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(sendRect.X+sendRect.W/2, sendRect.Y+sendRect.H/2)
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("button send must clear the input, got %q", app.cmdInput)
	}
	waitLoopbackWrite(t, srv, "AT\r\n")
	// Typing after a button send reaches neither the terminal
	// (focus did not move there) nor the input (the button holds
	// the focus, as before). Wait out the terminal's drain first:
	// a write that did go to the terminal arrives asynchronously.
	tt.Type("X")
	tt.Frame()
	time.Sleep(200 * time.Millisecond)
	if got := fakeWrites(fake); got != "" {
		t.Fatalf("typing after a button send must not reach the terminal, fake writes = %q", got)
	}
	if app.cmdInput != "" {
		t.Fatalf("typing after a button send must not reach the input, got %q", app.cmdInput)
	}
}

// cmdInputBox returns the rendered box of the command input
// (left, top, right, bottom, in window pixels) by finding its
// background color: the input is the hover fill over the
// elevated column, so sample a point inside it, level with the
// Send button, and take that color's bounding box in the bottom
// half of the window.
func cmdInputBox(t *testing.T, tt *ui.Tester) (left, top, right, bottom int) {
	t.Helper()
	img := tt.Image()
	if img == nil {
		t.Fatal("no frame image")
	}
	send, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	bg := img.At(1000, int(send.Y+send.H/2))
	left, top = -1, -1
	right, bottom = -1, -1
	for y := 500; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			if sameColor(img.At(x, y), bg) {
				if left < 0 || x < left {
					left = x
				}
				if x > right {
					right = x
				}
				if top < 0 || y < top {
					top = y
				}
				if y > bottom {
					bottom = y
				}
			}
		}
	}
	if left < 0 {
		t.Fatal("input background not found in frame image")
	}
	return left, top, right, bottom
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
}

// lightPixels counts near-white (text) pixels in a rectangle of
// the last frame, for asserting which lines are visible.
func lightPixels(t *testing.T, tt *ui.Tester, x0, y0, x1, y1 int) int {
	t.Helper()
	img := tt.Image()
	if img == nil {
		t.Fatal("no frame image")
	}
	n := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r > 0x9600 && g > 0x9600 && b > 0x9600 {
				n++
			}
		}
	}
	return n
}

// Empty is one line tall, the bar grows one line at a time to 5,
// then stops: the theme's 80-DIP TextArea floor used to pin an
// empty bar at ~4 lines, and the 5-line cap is what stops the
// growth, not the window.
func TestCmdBarSingleLineDefault(t *testing.T) {
	_, _, _, tt, cleanup := cmdBarTester(t, nil, false)
	defer cleanup()
	clickCmdInput(t, tt)
	heights := map[string]int{}
	measure := func(label string) {
		t.Helper()
		tt.Frame()
		_, top, _, bottom := cmdInputBox(t, tt)
		heights[label] = bottom - top + 1
	}
	measure("empty")
	tt.Type("one")
	measure("1 line")
	tt.Type("\n2")
	measure("2 lines")
	tt.Type("\n3")
	measure("3 lines")
	tt.Type("\n4")
	measure("4 lines")
	tt.Type("\n5")
	measure("5 lines")
	tt.Type("\n6")
	measure("6 lines")
	tt.Type("\n7\n8\n9\n10")
	measure("10 lines")
	// One line is ~20px of text + the input's padding: an empty
	// bar must be that tall, not the theme's ~78px floor.
	if h := heights["empty"]; h < 28 || h > 40 {
		t.Fatalf("empty bar height = %d, want ~32 (one line)", h)
	}
	if heights["1 line"] != heights["empty"] {
		t.Fatalf("1-line bar = %d, empty = %d, want equal", heights["1 line"], heights["empty"])
	}
	for i, label := range []string{"2 lines", "3 lines", "4 lines", "5 lines"} {
		want := heights["empty"] + (i+1)*20
		if got := heights[label]; got < want-3 || got > want+3 {
			t.Fatalf("%s bar = %d, want ~%d", label, got, want)
		}
	}
	if heights["6 lines"] != heights["5 lines"] || heights["10 lines"] != heights["5 lines"] {
		t.Fatalf("bar must stop at 5 lines: 5=%d 6=%d 10=%d", heights["5 lines"], heights["6 lines"], heights["10 lines"])
	}
}

// The input's corners are Radius 10, like the panel search box:
// at the top row the fill starts a few pixels in from the box's
// left edge, not ~45px in as a Radius(999) stadium would on a
// 5-line bar.
func TestCmdBarRadius10(t *testing.T) {
	_, _, _, tt, cleanup := cmdBarTester(t, nil, false)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("one\n2\n3\n4\n5")
	tt.Frame()
	left, top, _, _ := cmdInputBox(t, tt)
	img := tt.Image()
	bg := img.At(1000, top+50)
	first := -1
	for x := left; x < left+80; x++ {
		if sameColor(img.At(x, top+1), bg) {
			first = x
			break
		}
	}
	if first < 0 {
		t.Fatalf("no input background at top row; box left=%d top=%d", left, top)
	}
	if first-left > 12 {
		t.Fatalf("top-row fill starts %dpx in from the box edge, want <= 12 (Radius 10, not a stadium)", first-left)
	}
}

// Clicking a dropdown row fills a long entry and puts the caret
// at its end, as the keyboard navigation fill and the WebUI's
// selectHistoryItem do: the area reveal-scrolls to the end, so
// the first line scrolls out and only the last 5 lines show.
func TestCmdBarClickFillScrollsToEnd(t *testing.T) {
	six := "Name=eth0\n\n[Network]\nDHCP=yes\nLinkLocalAddressing=yes\nDNS=10.58.160.222"
	app, _, _, tt, cleanup := cmdBarTester(t, []HistoryEntry{
		{ID: "h1", Command: six},
	}, false)
	defer cleanup()
	clickCmdInput(t, tt)
	tt.Type("Name")
	tt.Frame()
	row, ok := tt.Find("Name=eth0 …")
	if !ok {
		t.Fatalf("dropdown row not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(row.X+row.W/2, row.Y+row.H/2)
	tt.Frame()
	tt.Frame()
	if app.cmdInput != six {
		t.Fatalf("click must fill the input, got %q", app.cmdInput)
	}
	// The fill re-opens the dropdown for the filled text, and the
	// reopened row paints the input's own background color: close
	// it with Esc (it only hides the list) before measuring the
	// input's box, or the row merges into it.
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	left, top, _, bottom := cmdInputBox(t, tt)
	// The first line scrolled out: its glyphs are only a sliver
	// at the top of the box now, while the last line shows fully
	// at the bottom.
	topBand := lightPixels(t, tt, left+8, top+4, left+220, top+24)
	bottomBand := lightPixels(t, tt, left+8, bottom-24, left+220, bottom-4)
	if topBand > 120 {
		t.Fatalf("first line still fully visible after fill (top band light pixels = %d)", topBand)
	}
	if bottomBand < 200 {
		t.Fatalf("last line not fully visible after fill (bottom band light pixels = %d)", bottomBand)
	}
	if h := bottom - top + 1; h < 100 || h > 120 {
		t.Fatalf("filled bar height = %d, want ~112 (5 lines)", h)
	}
}
