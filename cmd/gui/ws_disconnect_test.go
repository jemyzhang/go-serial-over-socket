package main

import (
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A WS disconnect ends the server connection, not the recording:
// the file stays open and keeps appending once a new connection
// delivers frames (a gap during the outage is silent by design).
func TestServerDisconnectKeepsRecording(t *testing.T) {
	release := make(chan struct{})
	upgraded := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		close(upgraded)
		<-release
	}))
	defer srv.Close()
	app := &nativeApp{ports: map[string]PortInfo{"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true}}, wdBackingUp: true, wdListLoaded: true}
	client, err := dialWS(srv.URL, wsCallbacks{onDisconnect: app.onWSDisconnect})
	if err != nil {
		t.Fatal(err)
	}
	app.attachClient(client)
	<-upgraded
	path := filepath.Join(t.TempDir(), "record.log")
	if err := app.startLocalRecording("ttyUSB0", path); err != nil {
		t.Fatal(err)
	}
	app.writeRecording("ttyUSB0", []byte("before disconnect"))
	client.start()
	close(release)
	waitFor(t, "disconnect cleanup", func() bool { app.mu.Lock(); defer app.mu.Unlock(); return !app.wsConnected })
	// The recording entry survives the disconnect: the file is
	// still open and later frames keep appending to it.
	if !app.isRecording("ttyUSB0") {
		t.Fatal("recording stopped on WS disconnect")
	}
	app.mu.Lock()
	port, status, backup, loaded := app.ports["ttyUSB0"], app.status, app.wdBackingUp, app.wdListLoaded
	app.mu.Unlock()
	if port.Connected || port.Status != "unknown" {
		t.Fatalf("stale port: %+v", port)
	}
	if status != "服务连接已断开，请重连；备份结果未知，请查看备份列表" {
		t.Fatalf("status=%q", status)
	}
	if backup || loaded {
		t.Fatalf("backup flag/cache not cleared: %v %v", backup, loaded)
	}
	// Frames after the drop still append; stopping flushes both chunks.
	app.writeRecording("ttyUSB0", []byte("after disconnect"))
	if _, _, ok, flushErr := app.stopLocalRecording("ttyUSB0"); !ok || flushErr != nil {
		t.Fatalf("stop after disconnect: ok=%v err=%v", ok, flushErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before disconnectafter disconnect" {
		t.Fatalf("recorded bytes=%q", got)
	}
}

func TestStaleDisconnectCannotStopNewClientRecording(t *testing.T) {
	old, current := &wsClient{}, &wsClient{}
	app := &nativeApp{client: current, wsConnected: true, status: "新连接", ports: map[string]PortInfo{"p": {Connected: true, Status: "connected"}}}
	path := filepath.Join(t.TempDir(), "rec.log")
	if err := app.startLocalRecording("p", path); err != nil {
		t.Fatal(err)
	}
	defer app.closeAllRecordings()
	app.onWSDisconnect(old)
	if !app.wsConnected || !app.isRecording("p") || app.status != "新连接" || !app.ports["p"].Connected {
		t.Fatalf("stale close changed replacement state")
	}
}

func TestDisconnectDuringSaveDialogDoesNotCreateRecording(t *testing.T) {
	current := &wsClient{}
	app := &nativeApp{client: current, wsConnected: true}
	path := filepath.Join(t.TempDir(), "not-created.log")
	app.saveDialog = func(string) (string, error) { app.onWSDisconnect(current); return path, nil }
	app.toggleLoggingFor("p")
	if app.isRecording("p") {
		t.Fatal("recording started after disconnect")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file was created: %v", err)
	}
}

// Disconnect no longer closes recording files, so it cannot report
// a flush error either: the file stays open until a port disconnect,
// a manual stop, or quit closes it (quit still reports /dev/full).
func TestDisconnectDoesNotCloseRecordingFile(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full unavailable")
	}
	current := &wsClient{}
	app := &nativeApp{client: current, wsConnected: true}
	if err := app.startLocalRecording("p", "/dev/full"); err != nil {
		t.Fatal(err)
	}
	app.writeRecording("p", []byte("buffered"))
	app.onWSDisconnect(current)
	if !app.isRecording("p") {
		t.Fatal("recording closed on WS disconnect")
	}
	app.mu.Lock()
	status := app.status
	app.mu.Unlock()
	if !strings.Contains(status, "断开") || strings.Contains(status, "刷盘或关闭失败") {
		t.Fatalf("status=%q", status)
	}
	// The file is still open: further frames buffer, and only the
	// quit-path close reports the /dev/full flush failure.
	app.writeRecording("p", []byte("more"))
	if !app.isRecording("p") {
		t.Fatal("recording closed after post-disconnect write")
	}
	if errs := app.closeAllRecordings(); len(errs) == 0 {
		t.Fatal("quit-path close reported no error for an open /dev/full file")
	}
}

// A real drop followed by a reconnect: the recording survives both,
// the backup-unknown note survives the reconnect status rewrite,
// and frames from the new connection append to the same file.
func TestDisconnectKeepsRecordingAcrossReconnect(t *testing.T) {
	srv := &loopbackServer{}
	app, old := dialAutoConnectApp(t, srv)
	// The recorded port is connected on the server: the reconnect
	// flow's welcome/get_ports must say so, or the onPorts safety
	// net would legitimately stop the recording on a fresh
	// disconnected list. Set after the initial handshake so the
	// welcome it waited on is untouched.
	srv.setPortsConnected(true)
	app.attachClient(old)
	app.mu.Lock()
	app.wdBackingUp = true
	app.wdListLoaded = true
	app.mu.Unlock()
	path := filepath.Join(t.TempDir(), "rec.log")
	if err := app.startLocalRecording("ttyUSB0", path); err != nil {
		t.Fatal(err)
	}
	// writeRecording is onData's first action and has no connection
	// gating; using it here keeps the test on recording survival
	// instead of terminal-session creation (ensureTerm).
	app.writeRecording("ttyUSB0", []byte("before"))
	// Simulate the read loop firing on a real drop.
	app.onWSDisconnect(app.client)
	if !app.isRecording("ttyUSB0") {
		t.Fatal("recording stopped on WS disconnect")
	}
	app.mu.Lock()
	status := app.status
	app.mu.Unlock()
	if status != "服务连接已断开，请重连；备份结果未知，请查看备份列表" {
		t.Fatalf("drop status=%q", status)
	}
	app.reconnect()
	app.mu.Lock()
	client, status, connected := app.client, app.status, app.wsConnected
	app.mu.Unlock()
	t.Cleanup(func() { client.Close() })
	if client == old || !connected {
		t.Fatal("new client was not attached")
	}
	if !app.isRecording("ttyUSB0") {
		t.Fatal("recording stopped across reconnect")
	}
	if status != "服务已重连；备份结果未知，请查看备份列表" {
		t.Fatalf("reconnect status=%q", status)
	}
	app.writeRecording("ttyUSB0", []byte("after"))
	if _, _, ok, flushErr := app.stopLocalRecording("ttyUSB0"); !ok || flushErr != nil {
		t.Fatalf("stop after reconnect: ok=%v err=%v", ok, flushErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "beforeafter" {
		t.Fatalf("recorded=%q, want beforeafter", got)
	}
}

// A healthy manual reconnect keeps the recording too: no
// "recording stopped" note, and the status returns to connected.
func TestReconnectKeepsRecording(t *testing.T) {
	srv := &loopbackServer{}
	app, old := dialAutoConnectApp(t, srv)
	// Same connected-port report as
	// TestDisconnectKeepsRecordingAcrossReconnect: without it the
	// reconnect's own get_ports reply (disconnected) would trip
	// the onPorts safety net and stop the recording.
	srv.setPortsConnected(true)
	app.attachClient(old)
	path := filepath.Join(t.TempDir(), "rec.log")
	if err := app.startLocalRecording("ttyUSB0", path); err != nil {
		t.Fatal(err)
	}
	app.writeRecording("ttyUSB0", []byte("before"))
	app.reconnect()
	app.mu.Lock()
	client, status, connected := app.client, app.status, app.wsConnected
	app.mu.Unlock()
	t.Cleanup(func() { client.Close() })
	if client == old || !connected {
		t.Fatal("new client was not attached")
	}
	if !app.isRecording("ttyUSB0") {
		t.Fatal("recording stopped across manual reconnect")
	}
	if status != "服务连接成功" {
		t.Fatalf("status=%q", status)
	}
	app.writeRecording("ttyUSB0", []byte("after"))
	if _, _, ok, flushErr := app.stopLocalRecording("ttyUSB0"); !ok || flushErr != nil {
		t.Fatalf("stop after reconnect: ok=%v err=%v", ok, flushErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "beforeafter" {
		t.Fatalf("recorded=%q, want beforeafter", got)
	}
	app.onWSDisconnect(old)
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.wsConnected {
		t.Fatal("old client marked replacement disconnected")
	}
}
