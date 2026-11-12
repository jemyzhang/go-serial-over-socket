// Tray support for sos-gui, mirroring the previous client's tray behavior
// in a deliberately narrower shape. With a tray, closing the window
// only parks the app in the tray: the embedded server, the WS
// connection and the serial sessions keep running, and bringing the
// window back goes through the tray menu's "显示主窗口" item — plus,
// on Linux only, a left-click on the tray icon itself. A real quit
// comes through App.Quit (the tray menu's "退出" item), which sets
// quitting first via OnBeforeQuit so the close handler lets the
// window actually close and shutdown runs.
//
// Two tray backends: Linux uses a self-implemented StatusNotifierItem
// (see sni_tray_linux.go), because MyGo's Linux tray goes through
// libayatana-appindicator, which shows a GTK menu on click and
// exposes no click signal — the host's left-click Activate call has
// nowhere to go, so left-clicking the icon does nothing there.
// macOS/Windows keep the MyGo tray, where OnClick fires, so the
// icon click path in setupTray below still applies there.
//
// The tray menu is only three sections on purpose: a disabled
// status line, "显示主窗口", and "退出". There is no mode switch
// and no connection settings here — native has no saved
// preferences and no mode-switching mechanism; those are the previous
// client's start-page companions, not something to re-implement for a menu.
//
// Without a tray (SOS_GUI_NO_TRAY=1, or the platform has none for
// us) the app degrades to the old behavior: closing the window
// quits, via the existing OnClosed -> shutdown path.

package main

import (
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/egoist/mygo"
	resources "github.com/jemyzhang/serial-over-socket-go"
)

// trayEnabled is the SOS_GUI_NO_TRAY gate, keeping the previous
// client's variable name.
func trayEnabled() bool {
	return os.Getenv("SOS_GUI_NO_TRAY") != "1"
}

// trayStatusLine renders the tray menu's disabled status line from
// native's own startup mode: local mode shows the loopback host
// (the base URL without its scheme), remote mode shows the base
// URL as-is.
func trayStatusLine(local bool, base string) string {
	if !local {
		return "远程模式 · " + base
	}
	host := strings.TrimPrefix(base, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		return "本地模式"
	}
	return "本地模式 · " + host
}

// showMainWindow brings the main window forward. The win == nil
// branch is defensive only: the window is created once in
// WhenReady and never recreated, so a tray click before that (or
// after a quit started) must log, never panic.
func (a *nativeApp) showMainWindow() {
	if a.win == nil {
		log.Printf("sos-gui: no main window to show")
		return
	}
	a.win.Show()
	a.win.Focus()
}

// sniTrayHandle is the Linux StatusNotifierItem tray's stop hook,
// kept behind an interface so non-Linux builds (which never
// construct one) don't need the Linux-only type. isRegistered
// reports whether the item is currently registered with a
// watcher (see shouldParkOnClose).
type sniTrayHandle interface {
	stop()
	isRegistered() bool
}

// setupTray creates the tray icon, or leaves both tray handles nil
// and logs (degraded mode) when the tray is disabled or
// unavailable. Call only after the app is ready: mygo panics on
// NewTray before that.
func (a *nativeApp) setupTray() {
	if !trayEnabled() {
		log.Printf("sos-gui: tray disabled via SOS_GUI_NO_TRAY")
		return
	}
	// Linux first: the SNI tray (sni_tray_linux.go) reports
	// handled even when it degrades to no tray, so setup never
	// falls through to the MyGo AppIndicator path, whose
	// left-click is dead on Linux. Other platforms report not
	// handled and take the MyGo path below.
	if a.setupSNITray() {
		return
	}
	// StaticFS is rooted at the repo root, so the icon path is
	// the same one the previous client read: web/static/icons/icon-192x192.png.
	icon, err := fs.ReadFile(resources.StaticFS, "web/static/icons/icon-192x192.png")
	if err != nil {
		log.Printf("sos-gui: tray icon not loaded: %v", err)
	}
	tray, err := mygo.NewTray(mygo.TrayOptions{
		Icon:    icon,
		ToolTip: "sos gui",
	})
	if err != nil {
		// Degraded mode (e.g. Linux without
		// libayatana-appindicator): no tray, closing the last
		// window quits the app as usual.
		log.Printf("sos-gui: tray unavailable, continuing without: %v", err)
		return
	}
	a.tray = tray
	tray.OnClick(func() { a.showMainWindow() })
	// A tray app must not die when its last window closes.
	mygo.App.OnWindowAllClosed(func() {})
	a.refreshTrayMenu()
}

// refreshTrayMenu rebuilds the tray menu from the current startup
// mode. It is a no-op without a tray, and a no-op on the Linux SNI
// tray, whose menu is static (built once at setup).
func (a *nativeApp) refreshTrayMenu() {
	if a.sniTray != nil {
		return
	}
	if a.tray == nil {
		return
	}
	a.tray.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Label: trayStatusLine(a.embedded != nil, a.base), Disabled: true},
		{Label: "显示主窗口", Click: func(*mygo.MenuItem, *mygo.Window) { a.showMainWindow() }},
		mygo.Separator(),
		{Label: "退出", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
	}))
}

// shouldParkOnClose reports whether closing the window should park
// it in the tray instead of quitting: a real tray must exist and a
// real quit must not be in progress. An SNI tray counts only once
// it has actually registered with a watcher — a setup that failed
// to register (or a watcher that went away) leaves no icon to
// restore the window from, so closing must quit instead of hiding
// the window into nothing.
func (a *nativeApp) shouldParkOnClose() bool {
	if a.quitting {
		return false
	}
	if a.tray != nil {
		return true
	}
	return a.sniTray != nil && a.sniTray.isRegistered()
}

// onWindowClose parks the window in the tray instead of closing
// it, unless there is no tray or a real quit is already in
// progress (quitting is set by OnBeforeQuit). Without a tray this
// does nothing and the existing OnClosed -> shutdown path quits.
func (a *nativeApp) onWindowClose(e *mygo.CloseEvent) {
	if a.shouldParkOnClose() {
		e.PreventDefault()
		a.win.Hide()
	}
}
