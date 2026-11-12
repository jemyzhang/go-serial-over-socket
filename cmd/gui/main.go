// sos-gui is a fully-native GUI client for
// go-serial-over-socket: MyGo native UI plus the terminal plugin
// (Ghostty's libghostty-vt), no webview. It speaks the same WebSocket
// protocol as the WebUI, so the server is untouched.
//
// Startup mirrors the previous webview client's flag semantics, minus the saved
// preferences: -remote connects to an external sos-server,
// otherwise an embedded server is started on loopback (Linux only;
// other platforms are remote-only clients). The tray (see tray.go)
// matches the previous client: closing the window parks the app in the tray
// unless SOS_GUI_NO_TRAY=1 or the platform has no tray.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/jemyzhang/serial-over-socket-go/pkg/version"
)

var (
	remote      = flag.String("remote", "", "external sos-server URL, e.g. http://192.168.1.10:2025 (implies remote mode)")
	localPort   = flag.Int("port", 0, "port of the embedded server in local mode (default 2025)")
	showVersion = flag.Bool("v", false, "Show version")
)

// waitReady polls base until it answers 200 or the timeout passes.
func waitReady(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(base)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server %s did not become ready within %s", base, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startLocalChecked starts the embedded server on a concrete port,
// failing fast and clearly when the port is already taken (the server
// itself only reports bind errors asynchronously).
func startLocalChecked(port int) (localServer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("local port %d is already in use (is another sos-server running?)", port)
	}
	ln.Close()

	srv, err := startLocalServer(port)
	if err != nil {
		return nil, err
	}
	if err := waitReady(srv.URL(), 5*time.Second); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return nil, err
	}
	return srv, nil
}

// windowOptions builds the main window's options. DevTools is off
// unconditionally: this is a shipped app, not a dev session, and
// with it on, the framework's Ctrl+Shift+C element picker
// preempts the terminal plugin's copy shortcut — the engine
// checks inspectKey before delivering keys to the focused
// element — so terminal copy would be dead. PageOptions.DevTools
// controls the native UI inspector for Content windows too, per
// its own comment; the Content field's "Page are ignored" doc is
// about the web page itself, and the code path (platformOptions
// reads o.Page.DevTools for every window) honors the flag.
func (a *nativeApp) windowOptions() mygo.WindowOptions {
	return mygo.WindowOptions{
		Title:  "sos-gui",
		Width:  1200,
		Height: 750,
		Page:   mygo.PageOptions{DevTools: mygo.DevToolsDisabled},
		Content: ui.View(func(c *ui.Context) {
			a.render(c)
		}),
	}
}

func main() {
	flag.Parse()

	if *showVersion {
		version.Print("Serial Over Socket GUI (Go Version)")
		return
	}

	var base string
	var embedded localServer
	switch {
	case *remote != "":
		u, err := normalizeBaseURL(*remote)
		if err != nil {
			log.Fatal(err)
		}
		base = u
	default:
		if !localModeSupported {
			log.Fatal("local mode is only available on Linux; use -remote to connect to an sos-server")
		}
		port := *localPort
		if port == 0 {
			port = 2025
		}
		srv, err := startLocalChecked(port)
		if err != nil {
			log.Fatal(err)
		}
		embedded = srv
		base = srv.URL()
	}

	app := newNativeApp(embedded)
	app.setBase(base)
	client, err := dialWS(base, wsCallbacks{
		onPorts:        app.onPorts,
		onStatus:       app.onStatus,
		onData:         app.onData,
		onError:        app.onError,
		onHistory:      app.onHistory,
		onWebDAV:       app.onWebDAV,
		onBackupResult: app.backupHistoryResult,
		onDisconnect:   app.onWSDisconnect,
	})
	if err != nil {
		app.shutdown()
		log.Fatalf("connect to %s: %v", base, err)
	}
	app.attachClient(client)
	client.start()
	// Mirror the WebUI's welcome flow: ports + history + WebDAV
	// config up front. Recordings are local files and need no restore query:
	// they survive WS disconnects and reconnects by design.
	_ = client.GetPorts()
	_ = client.GetHistory()
	_ = client.GetWebDAVConfig()

	mygo.App.WhenReady(func() {
		app.win = mygo.NewWindow(app.windowOptions())
		// Offer the desktop-entry install once the window is
		// up (Linux only; elsewhere a no-op): a missing entry
		// prompts once, a stale Exec is refreshed silently
		// instead (see desktop_linux.go). The dialog blocks,
		// so it runs off the UI thread.
		go app.maybePromptDesktopInstall()
		// The toolbar's connected dots flash on this ticker;
		// it stops on shutdown.
		app.startBreathing()
		// Tray last, like the previous client: the tray's appindicator/D-Bus
		// work should not precede the window. With a tray,
		// closing the window parks the app there (see
		// onWindowClose); without one, OnClosed quits as before.
		// shutdown() is never called on a park: the WS link, the
		// embedded server and the sessions keep running.
		app.setupTray()
		app.win.OnClose(app.onWindowClose)
		app.win.OnClosed(func() { app.shutdown() })
	})
	// A real quit sets quitting first, so the close handler lets
	// the window actually close and OnClosed runs shutdown.
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) { app.quitting = true })
	mygo.App.OnQuit(func() { app.shutdown() })

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
