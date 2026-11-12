package web

import (
	"errors"
	"testing"
)

var errTestUpload = errors.New("test upload failure")

// tryHistoryUpload flag discipline: the flag is taken atomically,
// restored on failure, and never loses a change made mid-upload.
// The loop body itself (sleep + WebDAV I/O) is not unit-tested;
// this covers the extracted flag logic it now delegates to.
func TestTryHistoryUpload(t *testing.T) {
	// Clean flag: no upload attempted.
	ws := &WebServer{}
	called := false
	if ws.tryHistoryUpload(func() error { called = true; return nil }) {
		t.Errorf("clean flag attempted an upload")
	}
	if called {
		t.Errorf("clean flag called upload")
	}

	// Dirty + success: flag cleared.
	ws.historyChanged.Store(true)
	if !ws.tryHistoryUpload(func() error { return nil }) {
		t.Errorf("dirty flag attempted no upload")
	}
	if ws.historyChanged.Load() {
		t.Errorf("successful upload left the flag set")
	}

	// Dirty + failure: flag restored for the next tick.
	ws.historyChanged.Store(true)
	if !ws.tryHistoryUpload(func() error { return errTestUpload }) {
		t.Errorf("dirty flag attempted no upload")
	}
	if !ws.historyChanged.Load() {
		t.Errorf("failed upload cleared the flag, want it restored")
	}

	// Change during a successful upload: still set afterwards.
	ws.historyChanged.Store(true)
	if !ws.tryHistoryUpload(func() error {
		ws.historyChanged.Store(true) // a concurrent handler's change
		return nil
	}) {
		t.Errorf("dirty flag attempted no upload")
	}
	if !ws.historyChanged.Load() {
		t.Errorf("change made during upload was lost")
	}

	// Change during a failed upload: the restore is a no-op
	// over it, and the flag stays set either way.
	ws.historyChanged.Store(true)
	if !ws.tryHistoryUpload(func() error {
		ws.historyChanged.Store(true)
		return errTestUpload
	}) {
		t.Errorf("dirty flag attempted no upload")
	}
	if !ws.historyChanged.Load() {
		t.Errorf("flag not set after failed upload with concurrent change")
	}
}
