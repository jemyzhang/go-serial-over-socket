package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// Disconnect retention (see onPorts): a port whose hardware is
// unplugged vanishes from the server's ports broadcast, but its
// window stays — terminal, scrollback and draft kept — until the
// user closes it. A re-plugged port comes back on the same
// session, history intact, no new window.

// disconnectTester builds a two-port app with real terminals:
// ttyUSB0 (selected, connected, with fed history) and ttyUSB1
// (connected). A loopback WS client stands in for the server so
// the send path is real: writes land in loopbackWrites.
func disconnectTester(t *testing.T) (*nativeApp, *loopbackServer, *ui.Tester, func()) {
	t.Helper()
	fake := &fakeSender{}
	conn0 := newPortConn(fake, "ttyUSB0")
	term0, err := terminal.New(terminal.Options{Conn: conn0})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	conn1 := newPortConn(fake, "ttyUSB1")
	term1, err := terminal.New(terminal.Options{Conn: conn1})
	if err != nil {
		_ = term0.Close()
		t.Fatalf("terminal.New: %v", err)
	}
	term0.Feed([]byte("HELLO-HISTORY\r\n"))
	srv := &loopbackServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	client, err := dialWS(httpSrv.URL, wsCallbacks{})
	if err != nil {
		_ = term0.Close()
		_ = term1.Close()
		httpSrv.Close()
		t.Fatalf("dialWS: %v", err)
	}
	connected := func(id string) PortInfo {
		return PortInfo{ID: id, Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"}
	}
	app := &nativeApp{
		client:      client,
		wsConnected: true,
		ports: map[string]PortInfo{
			"ttyUSB0": connected("ttyUSB0"),
			"ttyUSB1": connected("ttyUSB1"),
		},
		portIDs:  []string{"ttyUSB0", "ttyUSB1"},
		selected: "ttyUSB0",
		terms: map[string]*termSession{
			"ttyUSB0": {term: term0, conn: conn0},
			"ttyUSB1": {term: term1, conn: conn1},
		},
		settings:   defaultSettings(),
		drafts:     map[string]*portDraft{},
		cmdHistIdx: -1,
		histNavIdx: -1,
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	cleanup := func() {
		client.Close()
		httpSrv.Close()
		_ = term0.Close()
		_ = term1.Close()
	}
	return app, srv, tt, cleanup
}

// unplug0 drops ttyUSB0 from the broadcast, as the server does
// when its hardware is unplugged.
func unplug0(app *nativeApp) {
	app.onPorts(map[string]PortInfo{
		"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
	})
}

func TestUnplugRetainsWindow(t *testing.T) {
	app, _, tt, cleanup := disconnectTester(t)
	defer cleanup()
	sess0 := app.terms["ttyUSB0"]
	unplug0(app)
	// The session, the pill and the selection all survive: the
	// user stays on the disconnected window.
	if app.terms["ttyUSB0"] != sess0 {
		t.Error("unplug rebuilt or dropped the ttyUSB0 session")
	}
	info := app.ports["ttyUSB0"]
	if !info.Gone || info.Status != "disconnected" || info.Connected {
		t.Errorf("retained port = %+v, want Gone + disconnected", info)
	}
	if !slices.Contains(app.portIDs, "ttyUSB0") {
		t.Errorf("portIDs = %v, want ttyUSB0 kept", app.portIDs)
	}
	if app.selected != "ttyUSB0" {
		t.Errorf("selected = %q, want ttyUSB0 (stay on the disconnected window)", app.selected)
	}
	// History survives the unplug.
	if !strings.Contains(sess0.term.Text(), "HELLO-HISTORY") {
		t.Errorf("terminal text after unplug = %q, want HELLO-HISTORY kept", sess0.term.Text())
	}
	// The window says so: the disconnected banner is visible.
	tt.Frame()
	if !tt.HasText("已断开 · 硬件已拔出，历史保留") {
		t.Errorf("no disconnected banner; texts = %v", tt.Texts())
	}
}

func TestUnplugBlocksSend(t *testing.T) {
	app, srv, _, cleanup := disconnectTester(t)
	defer cleanup()
	unplug0(app)
	// No bytes to the dead port, no auto-connect attempt.
	if err := app.sendSerial("ttyUSB0", "AT\r\n"); err == nil || !strings.Contains(err.Error(), "已断开") {
		t.Errorf("sendSerial on a gone port = %v, want a 已断开 error", err)
	}
	if got := loopbackWrites(srv); len(got) != 0 {
		t.Errorf("gone port wrote %q to the server", got)
	}
	if n := loopbackCommandCount(srv, "connect_port"); n != 0 {
		t.Errorf("gone port issued %d connect_port, want 0", n)
	}
	// The command-bar path reports the same failure in the
	// status line.
	app.deliverCommand("ttyUSB0", "AT")
	if !strings.Contains(app.status, "已断开") {
		t.Errorf("status after blocked send = %q, want a 已断开 note", app.status)
	}
}

func TestGonePortConnectIsBlocked(t *testing.T) {
	app, srv, _, cleanup := disconnectTester(t)
	defer cleanup()
	unplug0(app)
	// The pill power button is disabled for a gone port…
	if _, _, _, disabled := pillConnectButton(darkPalette, app.ports["ttyUSB0"]); !disabled {
		t.Error("pill connect button enabled for a gone port")
	}
	// …and the panel's connect path refuses without sending.
	app.connectPort("ttyUSB0")
	if n := loopbackCommandCount(srv, "connect_port"); n != 0 {
		t.Errorf("gone port issued %d connect_port, want 0", n)
	}
	if !strings.Contains(app.status, "已断开") {
		t.Errorf("status after blocked connect = %q, want a 已断开 note", app.status)
	}
}

func TestCloseRetainedPort(t *testing.T) {
	app, _, _, cleanup := disconnectTester(t)
	defer cleanup()
	unplug0(app)
	// Closing a live port is a no-op.
	app.closeRetainedPort("ttyUSB1")
	if _, ok := app.terms["ttyUSB1"]; !ok {
		t.Error("closeRetainedPort closed a live port")
	}
	// Closing the retained window removes it for good and
	// reselects the first remaining port.
	app.closeRetainedPort("ttyUSB0")
	if _, ok := app.terms["ttyUSB0"]; ok {
		t.Error("closed window's session still in terms")
	}
	if _, ok := app.ports["ttyUSB0"]; ok {
		t.Error("closed window's port still in ports")
	}
	if slices.Contains(app.portIDs, "ttyUSB0") {
		t.Errorf("portIDs = %v, want ttyUSB0 dropped", app.portIDs)
	}
	if app.selected != "ttyUSB1" {
		t.Errorf("selected = %q, want ttyUSB1 after closing", app.selected)
	}
	// A later broadcast does not resurrect it.
	unplug0(app)
	if _, ok := app.terms["ttyUSB0"]; ok {
		t.Error("closed window came back on the next broadcast")
	}
}

func TestCloseRetainedPillButton(t *testing.T) {
	app, _, tt, cleanup := disconnectTester(t)
	defer cleanup()
	unplug0(app)
	tt.Frame()
	// The banner is only a sign now; the pill's × (tooltip
	// 关闭窗口) is the only way to close the window.
	if !tt.HasText("已断开 · 硬件已拔出，历史保留") {
		t.Fatalf("no disconnected banner; texts = %v", tt.Texts())
	}
	if err := tt.Click("关闭窗口"); err != nil {
		t.Fatalf("click pill ×: %v (texts = %v)", err, tt.Texts())
	}
	tt.Frame()
	if _, ok := app.terms["ttyUSB0"]; ok {
		t.Error("pill × did not remove the window")
	}
	if app.selected != "ttyUSB1" {
		t.Errorf("selected = %q, want ttyUSB1 after pill × close", app.selected)
	}
}

func TestReplugTerminalKeysStillSend(t *testing.T) {
	app, srv, _, cleanup := disconnectTester(t)
	defer cleanup()
	sess0 := app.terms["ttyUSB0"]
	// The real wiring: a resident terminal's Conn writes through
	// appSender into sendSerial (see ensureTerm). The tester builds
	// its terminals on a recording fake; rewire this one so a
	// keystroke takes the real path.
	sess0.conn.sender = &appSender{app}
	unplug0(app)
	// A keystroke while the hardware is gone: sendSerial refuses
	// with 已断开. The plugin's input queue dies on the first
	// Write error (it closes the queue and its goroutine
	// returns), so this one keystroke must not silence the
	// terminal forever. (A focus report does the same: the
	// plugin pushes one on focus change.)
	sess0.term.Send([]byte("X"))
	time.Sleep(500 * time.Millisecond)
	// The hardware comes back, same session.
	app.onPorts(map[string]PortInfo{
		"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
	})
	// Typing in the terminal now must reach the server again,
	// and the command bar must keep working too.
	sess0.term.Send([]byte("Y"))
	app.deliverCommand("ttyUSB0", "AT")
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := strings.Join(loopbackWrites(srv), "")
		if strings.Contains(got, "Y") && strings.Contains(got, "AT") {
			break
		}
		if strings.Contains(got, "X") {
			t.Fatalf("server got %q: the gone-period keystroke leaked out", got)
		}
		if time.Now().After(deadline) {
			t.Fatalf("server got %q after re-plug, want terminal Y + command-bar AT", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReplugRestoresWindow(t *testing.T) {
	app, srv, _, cleanup := disconnectTester(t)
	defer cleanup()
	sess0 := app.terms["ttyUSB0"]
	unplug0(app)
	if !app.ports["ttyUSB0"].Gone {
		t.Fatal("unplugged port not marked Gone")
	}
	// The hardware comes back: same session, history intact,
	// no new window — and sending works again.
	app.onPorts(map[string]PortInfo{
		"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
	})
	if app.ports["ttyUSB0"].Gone {
		t.Error("re-plugged port still Gone")
	}
	if app.terms["ttyUSB0"] != sess0 {
		t.Error("re-plug rebuilt the session instead of reusing it")
	}
	if !strings.Contains(sess0.term.Text(), "HELLO-HISTORY") {
		t.Errorf("terminal text after re-plug = %q, want HELLO-HISTORY kept", sess0.term.Text())
	}
	if err := app.sendSerial("ttyUSB0", "AT\r\n"); err != nil {
		t.Errorf("sendSerial after re-plug = %v, want nil", err)
	}
	// The write crosses the socket asynchronously: poll for
	// its arrival, as the command-bar tests do.
	deadline := time.Now().Add(3 * time.Second)
	for len(loopbackWrites(srv)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := loopbackWrites(srv); len(got) == 0 {
		t.Error("no bytes reached the server after re-plug")
	}
}
