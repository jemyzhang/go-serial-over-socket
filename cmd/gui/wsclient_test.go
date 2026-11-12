package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNormalizeBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"192.168.1.10:2025":         "http://192.168.1.10:2025",
		"http://192.168.1.10:2025/": "http://192.168.1.10:2025",
		"https://example.com:2025":  "https://example.com:2025",
	} {
		got, err := normalizeBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("normalizeBaseURL(%q) = (%q, %v), want (%q, nil)", raw, got, err, want)
		}
	}
	if _, err := normalizeBaseURL("  "); err == nil {
		t.Error("normalizeBaseURL(blank) should fail")
	}
}

func TestWSURLFromBase(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:2025":    "ws://127.0.0.1:2025/ws",
		"https://example.com:2025": "wss://example.com:2025/ws",
	} {
		got, err := wsURLFromBase(base)
		if err != nil || got != want {
			t.Errorf("wsURLFromBase(%q) = (%q, %v), want (%q, nil)", base, got, err, want)
		}
	}
}

// loopbackServer is a scripted fake sos-server: it speaks just enough
// of the protocol to drive the client (welcome/ports, select, connect
// with a port_status plus a delayed ports broadcast, write_data, and
// one binary frame).
type loopbackServer struct {
	mu       sync.Mutex
	commands []map[string]any
	writes   []string
	// connectDelay holds the port_status reply for connect_port
	// (in-flight tests need the request outstanding for a
	// while). connectMode changes the connect_port reply: ""
	// is the normal port_status plus delayed ports broadcast,
	// "portsOnly" skips the port_status (the broadcast alone
	// reports connected), "error" replies with an error message
	// instead of any success.
	connectDelay time.Duration
	connectMode  string
	// suppressBackupReply, when true, swallows the backup_history_now
	// reply: the request is recorded but no result ever arrives, so
	// a test can leave a backup in flight across a reconnect.
	suppressBackupReply bool
	// portsConnected, when true, reports ttyUSB0 as connected in
	// the welcome and get_ports replies instead of disconnected.
	// A port a test records is connected on the server; without
	// this, a reconnect's fresh disconnected list would make the
	// onPorts safety net legitimately stop the recording.
	portsConnected bool
}

// setPortsConnected flips the welcome/get_ports port report. It
// takes the server mutex, so a test can flip it while the mock's
// handler goroutines are alive without racing them.
func (s *loopbackServer) setPortsConnected(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portsConnected = v
}

// portsReply builds the welcome/get_ports port map from the
// current portsConnected flag.
func (s *loopbackServer) portsReply() map[string]any {
	s.mu.Lock()
	connected := s.portsConnected
	s.mu.Unlock()
	status := "disconnected"
	if connected {
		status = "connected"
	}
	return map[string]any{
		"ttyUSB0": map[string]any{"status": status, "connected": connected, "baudrate": 115200},
	}
}

func (s *loopbackServer) handler(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	writeJSON := func(v any) {
		b, _ := json.Marshal(v)
		_ = conn.WriteMessage(websocket.TextMessage, b)
	}
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		s.mu.Lock()
		s.commands = append(s.commands, msg)
		s.mu.Unlock()
		switch msg["type"] {
		case "terminal_ready":
			writeJSON(map[string]any{
				"type":  "welcome",
				"ports": s.portsReply(),
			})
		case "get_ports":
			writeJSON(map[string]any{
				"type":  "ports",
				"ports": s.portsReply(),
			})
		case "select_port":
			writeJSON(map[string]any{"type": "response", "status": "success"})
		case "connect_port":
			switch s.connectMode {
			case "error":
				writeJSON(map[string]any{"type": "error", "message": "device or resource busy"})
			default:
				if s.connectDelay > 0 {
					time.Sleep(s.connectDelay)
				}
				if s.connectMode != "portsOnly" {
					writeJSON(map[string]any{"type": "port_status", "port": "ttyUSB0", "status": "connected"})
				}
				go func() {
					time.Sleep(50 * time.Millisecond)
					writeJSON(map[string]any{
						"type": "ports",
						"ports": map[string]any{
							"ttyUSB0": map[string]any{"status": "connected", "connected": true, "baudrate": 115200},
						},
					})
					// A binary frame, plus a malformed one that must not
					// crash the client.
					id := "ttyUSB0"
					frame := append([]byte{byte(len(id))}, append([]byte(id), []byte("hi")...)...)
					_ = conn.WriteMessage(websocket.BinaryMessage, frame)
					_ = conn.WriteMessage(websocket.BinaryMessage, []byte{5, 'a', 'b'})
				}()
			}
		case "write_data":
			s.mu.Lock()
			s.writes = append(s.writes, msg["data"].(string))
			s.mu.Unlock()
		case "set_webdav_config":
			// Echo the _ack token in the response, as the real
			// server does, so ack-wait callers can proceed.
			resp := map[string]any{"type": "response", "status": "success"}
			if ack, ok := msg["_ack"].(string); ok {
				resp["_ack"] = ack
			}
			writeJSON(resp)
		case "backup_history_now":
			s.mu.Lock()
			suppress := s.suppressBackupReply
			s.mu.Unlock()
			if !suppress {
				writeJSON(map[string]any{"type": "backup_history_result", "ok": true})
			}
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWSClientLoopback(t *testing.T) {
	srv := &loopbackServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer httpSrv.Close()

	var mu sync.Mutex
	var ports map[string]PortInfo
	var statuses []string
	var data []byte
	client, err := dialWS(httpSrv.URL, wsCallbacks{
		onPorts: func(p map[string]PortInfo) {
			mu.Lock()
			ports = p
			mu.Unlock()
		},
		onStatus: func(portID, status string) {
			mu.Lock()
			statuses = append(statuses, portID+":"+status)
			mu.Unlock()
		},
		onData: func(portID string, d []byte) {
			mu.Lock()
			data = append(data, d...)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer client.Close()
	client.start()

	// Welcome carries the first ports list.
	waitFor(t, "welcome ports", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ports != nil
	})
	if p := ports["ttyUSB0"]; p.Status != "disconnected" || p.Baudrate != 115200 {
		t.Fatalf("welcome ports = %+v, want ttyUSB0 disconnected/115200", ports)
	}

	if err := client.GetPorts(); err != nil {
		t.Fatalf("GetPorts: %v", err)
	}
	if err := client.SelectPort("ttyUSB0"); err != nil {
		t.Fatalf("SelectPort: %v", err)
	}
	if err := client.ConnectPort("ttyUSB0", 0); err != nil {
		t.Fatalf("ConnectPort: %v", err)
	}
	waitFor(t, "connected status", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range statuses {
			if s == "ttyUSB0:connected" {
				return true
			}
		}
		return false
	})
	waitFor(t, "ports broadcast connected", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ports != nil && ports["ttyUSB0"].Connected
	})
	waitFor(t, "binary frame data", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return string(data) == "hi"
	})

	if err := client.WriteData("ttyUSB0", "AT\r"); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	waitFor(t, "server saw write_data", func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		for _, w := range srv.writes {
			if w == "AT\r" {
				return true
			}
		}
		return false
	})
}

// loopbackCommands returns a copy of the commands the fake
// server has received so far.
func loopbackCommands(srv *loopbackServer) []map[string]any {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := make([]map[string]any, len(srv.commands))
	copy(out, srv.commands)
	return out
}

// dialAutoConnectApp wires a nativeApp to a loopback server and
// waits for the welcome ports list, so auto-connect tests drive
// the real sendSerial path (not a fake client).
func dialAutoConnectApp(t *testing.T, srv *loopbackServer) (*nativeApp, *wsClient) {
	t.Helper()
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	t.Cleanup(httpSrv.Close)
	app := &nativeApp{selected: "ttyUSB0", settings: defaultSettings(), base: httpSrv.URL}
	client, err := dialWS(httpSrv.URL, wsCallbacks{
		onPorts:  app.onPorts,
		onStatus: app.onStatus,
		onError:  app.onError,
	})
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	app.client = client
	client.start()
	waitFor(t, "welcome ports", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return len(app.ports) > 0
	})
	return app, client
}

func loopbackCommandCount(srv *loopbackServer, typ string) int {
	n := 0
	for _, cmd := range loopbackCommands(srv) {
		if cmd["type"] == typ {
			n++
		}
	}
	return n
}

func loopbackWrites(srv *loopbackServer) []string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := make([]string, len(srv.writes))
	copy(out, srv.writes)
	return out
}

// A write to a disconnected port issues exactly one connect_port
// and flushes the buffered bytes in order once connected; later
// writes go out immediately.
func TestAutoConnectOnWrite(t *testing.T) {
	srv := &loopbackServer{}
	app, _ := dialAutoConnectApp(t, srv)
	if err := app.sendSerial("ttyUSB0", "AT\r"); err != nil {
		t.Fatalf("sendSerial: %v", err)
	}
	waitFor(t, "flushed write_data", func() bool {
		for _, w := range loopbackWrites(srv) {
			if w == "AT\r" {
				return true
			}
		}
		return false
	})
	if n := loopbackCommandCount(srv, "connect_port"); n != 1 {
		t.Errorf("connect_port commands = %d, want 1", n)
	}
	if err := app.sendSerial("ttyUSB0", "X"); err != nil {
		t.Fatalf("sendSerial after connect: %v", err)
	}
	waitFor(t, "immediate write_data", func() bool {
		for _, w := range loopbackWrites(srv) {
			if w == "X" {
				return true
			}
		}
		return false
	})
}

// While a connect is outstanding, further writes only append to
// the buffer — no second connect_port — and the flush carries
// everything in order.
func TestAutoConnectInFlight(t *testing.T) {
	srv := &loopbackServer{connectDelay: 400 * time.Millisecond}
	app, _ := dialAutoConnectApp(t, srv)
	if err := app.sendSerial("ttyUSB0", "A"); err != nil {
		t.Fatalf("sendSerial A: %v", err)
	}
	if err := app.sendSerial("ttyUSB0", "B"); err != nil {
		t.Fatalf("sendSerial B: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := loopbackCommandCount(srv, "connect_port"); n != 1 {
		t.Fatalf("connect_port commands during flight = %d, want 1", n)
	}
	waitFor(t, "flushed AB in order", func() bool {
		for _, w := range loopbackWrites(srv) {
			if w == "AB" {
				return true
			}
		}
		return false
	})
}

// A failed connect sends nothing, leaves the server's real reason
// in the status line and the port "error", and the next write
// retries with a second connect_port.
func TestAutoConnectFailure(t *testing.T) {
	srv := &loopbackServer{connectMode: "error"}
	app, _ := dialAutoConnectApp(t, srv)
	if err := app.sendSerial("ttyUSB0", "AT\r"); err != nil {
		t.Fatalf("sendSerial: %v", err)
	}
	waitFor(t, "failure status line", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.status == "服务器错误: device or resource busy"
	})
	app.mu.Lock()
	portStatus := app.ports["ttyUSB0"].Status
	app.mu.Unlock()
	if portStatus != "error" {
		t.Errorf("port status = %q, want error", portStatus)
	}
	if got := loopbackWrites(srv); len(got) != 0 {
		t.Errorf("writes after failed connect = %q, want none", got)
	}
	if err := app.sendSerial("ttyUSB0", "B"); err != nil {
		t.Fatalf("sendSerial retry: %v", err)
	}
	waitFor(t, "second connect_port on retry", func() bool {
		return loopbackCommandCount(srv, "connect_port") == 2
	})
}

// When no port_status arrives, a ports broadcast showing the
// port connected still flushes the buffer.
func TestAutoConnectPortsBroadcastFlush(t *testing.T) {
	srv := &loopbackServer{connectMode: "portsOnly"}
	app, _ := dialAutoConnectApp(t, srv)
	if err := app.sendSerial("ttyUSB0", "AT\r"); err != nil {
		t.Fatalf("sendSerial: %v", err)
	}
	waitFor(t, "broadcast flushed write_data", func() bool {
		for _, w := range loopbackWrites(srv) {
			if w == "AT\r" {
				return true
			}
		}
		return false
	})
}

// TestWebDAVConfigAckAndBackupResult drives the two WebDAV
// round trips the sync tab depends on: a set_webdav_config with an
// ack (the test button's push-then-probe ordering) and a
// backup_history_now with its backup_history_result callback.
func TestWebDAVConfigAckAndBackupResult(t *testing.T) {
	srv := &loopbackServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer httpSrv.Close()

	var mu sync.Mutex
	var backupOk *bool
	var backupErr string
	client, err := dialWS(httpSrv.URL, wsCallbacks{
		onBackupResult: func(ok bool, errMsg string) {
			mu.Lock()
			backupOk = &ok
			backupErr = errMsg
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer client.Close()
	client.start()

	ackCh, err := client.SetWebDAVConfigWithAck(WebDAVConfig{
		URL: "http://127.0.0.1:8080/dav", Username: "u",
		Interval: 30, MaxBackups: 10,
	}, "", false)
	if err != nil {
		t.Fatalf("SetWebDAVConfigWithAck: %v", err)
	}
	select {
	case <-ackCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ack channel never closed (server echo not routed)")
	}

	if err := client.BackupHistoryNow(); err != nil {
		t.Fatalf("BackupHistoryNow: %v", err)
	}
	waitFor(t, "backup_history_result", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return backupOk != nil
	})
	mu.Lock()
	defer mu.Unlock()
	if !*backupOk || backupErr != "" {
		t.Fatalf("backup result = ok %v err %q, want ok true err empty", *backupOk, backupErr)
	}
}

// A manual backup issued after a reconnect must report: reconnect()
// once forgot to wire onBackupResult, so the result was dropped,
// the uploading flag never cleared, and the next backup was blocked
// by the double-send guard.
func TestReconnectRestoresBackupResult(t *testing.T) {
	srv := &loopbackServer{}
	app, _ := dialAutoConnectApp(t, srv)
	app.reconnect()
	newClient := app.client
	t.Cleanup(func() { newClient.Close() })

	app.backupWebDAVNow()
	waitFor(t, "backup completed after reconnect", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.status == "WebDAV 备份已完成" && !app.wdBackingUp
	})
	// The next backup must no longer be blocked by the guard.
	app.backupWebDAVNow()
	waitFor(t, "second backup sent after reconnect", func() bool {
		return loopbackCommandCount(srv, "backup_history_now") == 2
	})
	waitFor(t, "second backup completed", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return !app.wdBackingUp
	})
}

// A backup in flight when the connection is swapped can never
// report: the old connection is closed and the new one carries no
// pending request. Reconnect must clear the guard, say the result
// is unknown (never success, never failure), invalidate the backup
// list so the next visit refetches the truth, and leave the button
// usable; a late real result must still be accepted afterwards.
func TestReconnectClearsInFlightBackup(t *testing.T) {
	srv := &loopbackServer{suppressBackupReply: true}
	app, _ := dialAutoConnectApp(t, srv)
	app.mu.Lock()
	app.wdListLoaded = true
	app.mu.Unlock()
	app.backupWebDAVNow()
	waitFor(t, "backup in flight", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.wdBackingUp && app.status == "正在上传备份…"
	})
	app.reconnect()
	newClient := app.client
	t.Cleanup(func() { newClient.Close() })
	app.mu.Lock()
	flag, status, listLoaded := app.wdBackingUp, app.status, app.wdListLoaded
	app.mu.Unlock()
	if flag {
		t.Error("wdBackingUp still set after reconnect swapped the client")
	}
	if !strings.Contains(status, "结果未知") {
		t.Errorf("status = %q, want it to say the result is unknown", status)
	}
	if strings.Contains(status, "已完成") || strings.Contains(status, "失败") {
		t.Errorf("status = %q, must claim neither success nor failure", status)
	}
	if listLoaded {
		t.Error("backup list cache not invalidated after unknown-result recovery")
	}
	// A late real result is still accepted, not gated by recovery.
	app.backupHistoryResult(true, "")
	app.mu.Lock()
	status = app.status
	app.mu.Unlock()
	if status != "WebDAV 备份已完成" {
		t.Errorf("late backup result status = %q, want completed", status)
	}
	// And the button works again on the new connection.
	srv.mu.Lock()
	srv.suppressBackupReply = false
	srv.mu.Unlock()
	app.backupWebDAVNow()
	waitFor(t, "backup sent after recovery", func() bool {
		return loopbackCommandCount(srv, "backup_history_now") == 2
	})
	waitFor(t, "backup completed after recovery", func() bool {
		app.mu.Lock()
		defer app.mu.Unlock()
		return !app.wdBackingUp
	})
}
