package main

import (
	"testing"

	"github.com/egoist/mygo"
)

func TestTrayStatusLine(t *testing.T) {
	cases := []struct {
		local bool
		base  string
		want  string
	}{
		{true, "http://127.0.0.1:2025", "本地模式 · 127.0.0.1:2025"},
		{true, "http://127.0.0.1:2026/", "本地模式 · 127.0.0.1:2026"},
		{true, "", "本地模式"},
		{false, "http://192.168.1.10:2025", "远程模式 · http://192.168.1.10:2025"},
		{false, "", "远程模式 · "},
	}
	for _, c := range cases {
		if got := trayStatusLine(c.local, c.base); got != c.want {
			t.Errorf("trayStatusLine(%v, %q) = %q, want %q", c.local, c.base, got, c.want)
		}
	}
}

func TestTrayEnabled(t *testing.T) {
	t.Setenv("SOS_GUI_NO_TRAY", "1")
	if trayEnabled() {
		t.Errorf("trayEnabled() with SOS_GUI_NO_TRAY=1 = true, want false")
	}
	t.Setenv("SOS_GUI_NO_TRAY", "0")
	if !trayEnabled() {
		t.Errorf("trayEnabled() with SOS_GUI_NO_TRAY=0 = false, want true")
	}
	t.Setenv("SOS_GUI_NO_TRAY", "")
	if !trayEnabled() {
		t.Errorf("trayEnabled() with SOS_GUI_NO_TRAY unset = false, want true")
	}
}

// showMainWindow and refreshTrayMenu must be safe before any
// window or tray exists: a tray click racing startup, and a menu
// refresh without a tray, must log (or no-op), never panic.
func TestTrayNilSafe(t *testing.T) {
	a := &nativeApp{}
	a.showMainWindow()
	a.refreshTrayMenu()
}

// fakeSNIHandle is a test stand-in for the Linux SNI tray.
type fakeSNIHandle struct{ registered bool }

func (f *fakeSNIHandle) stop()              {}
func (f *fakeSNIHandle) isRegistered() bool { return f.registered }

func TestShouldParkOnClose(t *testing.T) {
	cases := []struct {
		name     string
		tray     *mygo.Tray
		sni      sniTrayHandle
		quitting bool
		want     bool
	}{
		{"no tray", nil, nil, false, false},
		{"mygo tray", &mygo.Tray{}, nil, false, true},
		{"mygo tray quitting", &mygo.Tray{}, nil, true, false},
		{"sni unregistered", nil, &fakeSNIHandle{registered: false}, false, false},
		{"sni registered", nil, &fakeSNIHandle{registered: true}, false, true},
		{"sni registered quitting", nil, &fakeSNIHandle{registered: true}, true, false},
	}
	for _, c := range cases {
		a := &nativeApp{tray: c.tray, sniTray: c.sni, quitting: c.quitting}
		if got := a.shouldParkOnClose(); got != c.want {
			t.Errorf("%s: shouldParkOnClose() = %v, want %v", c.name, got, c.want)
		}
	}
}
