package main

import (
	"bufio"
	"fmt"
	"os"
)

// Client-side recording: raw serial frames are appended to a local
// file as they arrive, whether or not the port is the visible tab.
// Recording binds to the GUI process, not to the server connection:
// a live recording survives WS disconnects and reconnects (frames
// from the new connection keep appending to the same file); only a
// port disconnect, manual stop, or quit ends it. A gap during a WS
// outage is silent by design (frames sent while disconnected are
// lost). It is flushed and closed on quit. It does NOT resume after
// a GUI restart: the state lives in this process only.
//
// Locking: recordings are guarded by recMu. Where both locks are
// needed (snapshot and starting a recording), the
// order is app mutex -> recMu, never the reverse. setStatus takes the app mutex and is
// therefore never called while recMu is held: stop paths remove
// the entry under recMu, then flush, close, and report outside it.
type localRecording struct {
	file *os.File
	w    *bufio.Writer
	path string
	size int64
}

// startLocalRecording creates the file and registers it. The caller
// has already picked the path (save dialog) and checked the port
// is not recording; if it is, the fresh file is closed again and
// an error is returned rather than silently replacing the entry.
func (a *nativeApp) startLocalRecording(portID, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	a.recMu.Lock()
	if _, ok := a.recordings[portID]; ok {
		a.recMu.Unlock()
		f.Close()
		return fmt.Errorf("port %s is already recording", portID)
	}
	if a.recordings == nil {
		a.recordings = make(map[string]*localRecording)
	}
	a.recordings[portID] = &localRecording{file: f, w: bufio.NewWriter(f), path: path}
	a.recMu.Unlock()
	return nil
}

// stopLocalRecording removes one port's entry and flushes/closes
// its file. ok is false when the port was not recording.
func (a *nativeApp) stopLocalRecording(portID string) (path string, size int64, ok bool, flushErr error) {
	a.recMu.Lock()
	rec, ok := a.recordings[portID]
	if !ok {
		a.recMu.Unlock()
		return "", 0, false, nil
	}
	delete(a.recordings, portID)
	a.recMu.Unlock()
	flushErr = rec.w.Flush()
	if err := rec.file.Close(); err != nil && flushErr == nil {
		flushErr = err
	}
	return rec.path, rec.size, true, flushErr
}

// isRecording reports whether a port currently has a local file.
func (a *nativeApp) isRecording(portID string) bool {
	a.recMu.Lock()
	defer a.recMu.Unlock()
	_, ok := a.recordings[portID]
	return ok
}

// writeRecording appends one raw frame to a port's local file, if
// that port is recording. Bytes are written verbatim — no ANSI
// processing, no framing — and counted as written. A write failure
// ends the recording (file closed, entry dropped) and reports once
// on the status line; later frames for the port are then dropped
// like any non-recording port's.
func (a *nativeApp) writeRecording(portID string, data []byte) {
	a.recMu.Lock()
	rec, ok := a.recordings[portID]
	if !ok {
		a.recMu.Unlock()
		return
	}
	n, err := rec.w.Write(data)
	rec.size += int64(n)
	if err == nil {
		a.recMu.Unlock()
		return
	}
	delete(a.recordings, portID)
	a.recMu.Unlock()
	_ = rec.w.Flush()
	_ = rec.file.Close()
	a.setStatus("记录写入失败 %s: %v", portID, err)
}

// closeAllRecordings flushes and closes every local record file.
// Used on quit; it returns one error per file that failed to flush
// or close, so the caller can retain them — the status line is gone
// by then, and silently dropping them would hide lost data.
func (a *nativeApp) closeAllRecordings() []error {
	a.recMu.Lock()
	recs := make([]*localRecording, 0, len(a.recordings))
	for _, rec := range a.recordings {
		recs = append(recs, rec)
	}
	a.recordings = nil
	a.recMu.Unlock()
	var errs []error
	for _, rec := range recs {
		if err := rec.w.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("recording %s flush: %w", rec.path, err))
		}
		if err := rec.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("recording %s close: %w", rec.path, err))
		}
	}
	return errs
}
