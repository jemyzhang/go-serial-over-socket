package main

import (
	"io"
	"sync"
)

// wsSender is the slice of wsClient the port Conn needs. Kept as an
// interface so tests can substitute a recording fake.
type wsSender interface {
	WriteData(portID, data string) error
}

// portConn adapts one selected serial port to the io.ReadWriteCloser
// the terminal plugin wants (Options.Conn):
//
//   - Read returns bytes the server pushed for this port (binary WS
//     frames, delivered via deliver), blocking until data arrives.
//   - Write sends keystrokes to the port as write_data messages.
//   - Resize reports the terminal's new size to the server through
//     onResize (a terminal_ready resend): the server only ever
//     learns the size this way — the dial's terminal_ready carries
//     a fixed 80x24, and the WebUI never reports its size at all.
//     onResize is nil in tests that do not wire it.
//   - Close unblocks Read with io.EOF; the plugin calls it when its
//     terminal is closed, which is also how a port switch retires the
//     old terminal's Conn.
type portConn struct {
	sender wsSender
	portID string
	// onResize, when set, is called with the terminal's new size
	// (cols, rows) on the plugin's tellConn goroutine: it must be
	// thread-safe and must not touch UI state.
	onResize func(cols, rows int)

	mu      sync.Mutex
	pending []byte
	// inbound wakes Read when deliver adds bytes. Buffered so a burst
	// does not block the WS read loop; deliver drops only when the
	// buffer is full (1024 pending chunks, far above line-rate serial
	// traffic).
	inbound chan struct{}
	closed  bool
}

func newPortConn(sender wsSender, portID string) *portConn {
	return &portConn{
		sender:  sender,
		portID:  portID,
		inbound: make(chan struct{}, 1024),
	}
}

// deliver hands server output for this port to the Conn. Called by the
// app's onData handler, which already filtered by port.
func (p *portConn) deliver(data []byte) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	p.pending = append(p.pending, data...)
	p.mu.Unlock()
	select {
	case p.inbound <- struct{}{}:
	default:
		// Buffer full: the reader is behind. The bytes are already in
		// pending; the next Read still sees them, it just is not
		// woken for this chunk.
	}
}

func (p *portConn) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if len(p.pending) > 0 {
			n := copy(b, p.pending)
			p.pending = p.pending[n:]
			p.mu.Unlock()
			return n, nil
		}
		if p.closed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		p.mu.Unlock()
		<-p.inbound
	}
}

func (p *portConn) Write(b []byte) (int, error) {
	if err := p.sender.WriteData(p.portID, string(b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize satisfies the terminal plugin's optional sizer interface:
// it forwards the new size to onResize (when set), which reports
// it to the server. The plugin only calls it when the size
// changes, from its tellConn goroutine.
func (p *portConn) Resize(cols, rows int) error {
	if p.onResize != nil {
		p.onResize(cols, rows)
	}
	return nil
}

func (p *portConn) Close() error {
	// The channel is never closed: deliver signals on it, and a send
	// on a closed channel panics. Close only sets the flag and wakes
	// a blocked Read, which then sees closed and returns io.EOF (after
	// draining any pending bytes).
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	select {
	case p.inbound <- struct{}{}:
	default:
		// A blocked reader cannot exist with a full buffer: it would
		// already hold an unread wake signal and see closed next.
	}
	return nil
}
