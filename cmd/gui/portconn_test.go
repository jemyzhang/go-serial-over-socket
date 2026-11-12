package main

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
)

// fakeSender records write_data calls for the Conn tests.
type fakeSender struct {
	mu    sync.Mutex
	calls []writeCall
}

type writeCall struct {
	portID string
	data   string
}

func (f *fakeSender) WriteData(portID, data string) error {
	f.mu.Lock()
	f.calls = append(f.calls, writeCall{portID, data})
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) last() (writeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return writeCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func readAll(t *testing.T, c *portConn, want int) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 64)
	deadline := time.Now().Add(2 * time.Second)
	for sb.Len() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out reading from portConn: got %q, want %d bytes", sb.String(), want)
		}
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		sb.Write(buf[:n])
	}
	return sb.String()
}

func TestPortConnReadWrite(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")

	conn.deliver([]byte("hello "))
	conn.deliver([]byte("world"))
	if got := readAll(t, conn, 11); got != "hello world" {
		t.Fatalf("Read = %q, want %q", got, "hello world")
	}

	if n, err := conn.Write([]byte("AT\r")); err != nil || n != 3 {
		t.Fatalf("Write = (%d, %v), want (3, nil)", n, err)
	}
	call, ok := fake.last()
	if !ok || call != (writeCall{portID: "ttyUSB0", data: "AT\r"}) {
		t.Fatalf("sender got %+v, want write_data ttyUSB0 AT\\r", call)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := conn.Read(make([]byte, 16)); err != io.EOF {
		t.Fatalf("Read after Close = %v, want io.EOF", err)
	}
}

// TestTerminalViaConn is the headless end-to-end check of the data
// path: bytes delivered to the Conn show on the plugin terminal's
// screen (ANSI parsed), and input sent through the terminal reaches
// the WS sender as write_data. No window is involved.
func TestTerminalViaConn(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()

	conn.deliver([]byte("hello \x1b[31mRED\x1b[0m world\r\nsecond line"))

	deadline := time.Now().Add(3 * time.Second)
	for {
		text := term.Text()
		if strings.Contains(text, "hello RED world") && strings.Contains(text, "second line") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal screen did not show expected text, got:\n%s", text)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Typed input must travel terminal -> Conn.Write -> write_data.
	term.Send([]byte("typed"))
	deadline = time.Now().Add(2 * time.Second)
	for {
		if call, ok := fake.last(); ok && call == (writeCall{portID: "ttyUSB0", data: "typed"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sender did not receive typed input, calls: %+v", fake.calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAppOnDataFiltersByPort checks the app routes binary frames only
// to the selected port's Conn.
func TestAppOnDataFiltersByPort(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	app := &nativeApp{selected: "ttyUSB0", conn: conn}

	app.onData("ttyUSB1", []byte("wrong port"))
	app.onData("ttyUSB0", []byte("right"))

	buf := make([]byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		n, err := conn.Read(buf)
		if err != nil || string(buf[:n]) != "right" {
			t.Errorf("Read = (%q, %v), want (\"right\", nil)", buf[:n], err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading filtered data")
	}
}
