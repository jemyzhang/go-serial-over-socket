//go:build !linux

// Non-Linux platforms keep the MyGo tray: setupSNITray reports
// "not handled" so setupTray falls through to the mygo.NewTray
// path, where OnClick fires.

package main

// setupSNITray is a no-op off Linux: the StatusNotifierItem tray
// exists only for Linux (see sni_tray_linux.go).
func (a *nativeApp) setupSNITray() bool { return false }
