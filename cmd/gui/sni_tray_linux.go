//go:build linux

// Linux tray via a self-implemented StatusNotifierItem (SNI) over D-Bus.
//
// MyGo's Linux tray goes through libayatana-appindicator, which shows
// a GTK menu on click and exposes no click signal: the host's
// left-click Activate call has nowhere to go, so left-clicking the
// icon does nothing while right-click opens the menu. This file
// replaces that path on Linux only (macOS/Windows keep the MyGo tray,
// where OnClick fires): the item implements org.kde.StatusNotifierItem
// directly, so a primary click arrives as Activate and a secondary
// click as ContextMenu, and the menu is served over
// com.canonical.dbusmenu.
//
// Protocol shape follows Wails v3's Linux tray (its
// systemtray_linux.go plus internal/dbus/notifier and
// internal/dbus/menu): same object paths, same
// RegisterStatusNotifierItem argument (the item object path), same
// well-known-name scheme, same dbusmenu method set. Only the
// application behavior is ours: Activate and the "显示主窗口" menu
// item restore the main window; "退出" quits via mygo.App.Quit.

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/fs"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/egoist/mygo"
	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
	resources "github.com/jemyzhang/serial-over-socket-go"
)

const (
	sniItemPath       = dbus.ObjectPath("/StatusNotifierItem")
	sniMenuPath       = dbus.ObjectPath("/StatusNotifierMenu")
	sniWatcherService = "org.kde.StatusNotifierWatcher"
	sniWatcherPath    = dbus.ObjectPath("/StatusNotifierWatcher")
	sniWatcherIface   = "org.kde.StatusNotifierWatcher"
	sniItemIface      = "org.kde.StatusNotifierItem"
	sniMenuIface      = "com.canonical.dbusmenu"
)

// Fixed dbusmenu ids. The menu is static (built once at setup):
// a disabled status line, "显示主窗口", a separator, "退出" —
// the same structure as the MyGo tray menu in tray.go.
const (
	sniMenuRoot      = int32(0)
	sniMenuStatus    = int32(1)
	sniMenuShow      = int32(2)
	sniMenuSeparator = int32(3)
	sniMenuQuit      = int32(4)
)

// sniIconPixmap is one IconPixmap entry: width, height, ARGB32 bytes
// (the a(iiay) wire shape).
type sniIconPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

// sniToolTip is the (sa(iiay)ss) ToolTip property.
type sniToolTip struct {
	IconName    string
	IconPixmaps []sniIconPixmap
	Title       string
	Description string
}

// sniLayout is one dbusmenu layout node: id, properties, children
// (the (ia{sv}av) wire shape; children wrap *sniLayout).
type sniLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

// sniPropEntry is one GetGroupProperties entry.
type sniPropEntry struct {
	ID    int32
	Props map[string]dbus.Variant
}

// sniEventEntry is one EventGroup entry.
type sniEventEntry struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

// sniMenuModel is the static dbusmenu, built once at setup. It is
// immutable afterwards, so layout calls can hand out fresh maps
// without locking (godbus serialises the reply on its own worker
// goroutine after the method returns).
type sniMenuModel struct {
	items map[int32]*sniLayout
}

func newSNIMenu(statusLine string) *sniMenuModel {
	mk := func(id int32, props map[string]dbus.Variant) *sniLayout {
		return &sniLayout{ID: id, Props: props}
	}
	status := mk(sniMenuStatus, map[string]dbus.Variant{
		"label":   dbus.MakeVariant(statusLine),
		"enabled": dbus.MakeVariant(false),
		"visible": dbus.MakeVariant(true),
	})
	show := mk(sniMenuShow, map[string]dbus.Variant{
		"label":   dbus.MakeVariant("显示主窗口"),
		"enabled": dbus.MakeVariant(true),
		"visible": dbus.MakeVariant(true),
	})
	separator := mk(sniMenuSeparator, map[string]dbus.Variant{
		"type":    dbus.MakeVariant("separator"),
		"enabled": dbus.MakeVariant(true),
		"visible": dbus.MakeVariant(true),
	})
	quit := mk(sniMenuQuit, map[string]dbus.Variant{
		"label":   dbus.MakeVariant("退出"),
		"enabled": dbus.MakeVariant(true),
		"visible": dbus.MakeVariant(true),
	})
	root := &sniLayout{ID: sniMenuRoot, Props: map[string]dbus.Variant{}}
	for _, child := range []*sniLayout{status, show, separator, quit} {
		root.Children = append(root.Children, dbus.MakeVariant(child))
	}
	return &sniMenuModel{items: map[int32]*sniLayout{
		sniMenuRoot:      root,
		sniMenuStatus:    status,
		sniMenuShow:      show,
		sniMenuSeparator: separator,
		sniMenuQuit:      quit,
	}}
}

// layout returns a fresh copy of the node for id (root includes its
// children; items are flat so they carry none). Unknown ids report
// not-found; the caller returns a zero layout in that case.
func (m *sniMenuModel) layout(id int32) (sniLayout, bool) {
	src, ok := m.items[id]
	if !ok {
		return sniLayout{}, false
	}
	out := sniLayout{ID: src.ID, Props: make(map[string]dbus.Variant, len(src.Props))}
	for k, v := range src.Props {
		out.Props[k] = v
	}
	out.Children = append(out.Children, src.Children...)
	return out, true
}

// sniTray is the Linux tray: the D-Bus connection, the watcher
// re-register loop, and the two callbacks. onShow/onQuit run on the
// UI thread already (setup wires them through mygo.RunOnMain), so
// the method handlers below never touch GTK directly.
//
// serviceName is the RegisterStatusNotifierItem argument: the
// item's service name as a string (see register). registered
// reports whether the item is currently registered with a watcher;
// it is the only field touched from both the UI thread (close
// handler) and the watcher goroutine, so it is atomic.
type sniTray struct {
	conn        *dbus.Conn
	done        chan struct{}
	once        sync.Once
	menu        *sniMenuModel
	onShow      func()
	onQuit      func()
	serviceName string
	registered  atomic.Bool
}

// isRegistered reports whether the item is currently registered
// with a watcher (see sniTrayHandle in tray.go).
func (t *sniTray) isRegistered() bool { return t.registered.Load() }

// sniWellKnownName is the per-process well-known name for the item
// (Wails' scheme), preferred as the register argument when the bus
// grants it.
func sniWellKnownName(pid int) string {
	return fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", pid)
}

// sniItemServer implements org.kde.StatusNotifierItem.
type sniItemServer struct{ t *sniTray }

// Activate is the primary (left) click: restore the main window.
func (s *sniItemServer) Activate(x, y int32) *dbus.Error {
	if s.t.onShow != nil {
		s.t.onShow()
	}
	return nil
}

// ContextMenu is the secondary (right) click: a no-op, mirroring
// Wails — the host renders the dbusmenu itself, and answering here
// would only duplicate it.
func (s *sniItemServer) ContextMenu(x, y int32) *dbus.Error { return nil }

// SecondaryActivate is the middle click: deliberately unbound (no
// sos-gui action maps to it), stated here so a future reader does
// not mistake the silence for a missing method.
func (s *sniItemServer) SecondaryActivate(x, y int32) *dbus.Error { return nil }

// Scroll is unbound for the same reason as SecondaryActivate.
func (s *sniItemServer) Scroll(delta int32, orientation string) *dbus.Error { return nil }

// sniMenuServer implements com.canonical.dbusmenu.
type sniMenuServer struct{ t *sniTray }

func (s *sniMenuServer) GetLayout(parentID int32, recursionDepth int32, propertyNames []string) (uint32, sniLayout, *dbus.Error) {
	// Static menu: depth and property filters are ignored (as in
	// Wails) — the tree is two levels and hosts re-fetch on demand.
	layout, ok := s.t.menu.layout(parentID)
	if !ok {
		return 0, sniLayout{}, nil
	}
	return 0, layout, nil
}

func (s *sniMenuServer) GetGroupProperties(ids []int32, propertyNames []string) ([]sniPropEntry, *dbus.Error) {
	var out []sniPropEntry
	for _, id := range ids {
		if node, ok := s.t.menu.items[id]; ok {
			props := make(map[string]dbus.Variant, len(node.Props))
			for k, v := range node.Props {
				props[k] = v
			}
			out = append(out, sniPropEntry{ID: id, Props: props})
		}
	}
	return out, nil
}

func (s *sniMenuServer) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	if node, ok := s.t.menu.items[id]; ok {
		if v, ok := node.Props[name]; ok {
			return v, nil
		}
	}
	return dbus.Variant{}, &dbus.Error{
		Name: "org.freedesktop.DBus.Error.InvalidArgs",
		Body: []interface{}{"unknown menu id or property"},
	}
}

func (s *sniMenuServer) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		// "opened"/"closed" only announce the menu; they must not
		// fire the click action as well.
		return nil
	}
	switch id {
	case sniMenuShow:
		if s.t.onShow != nil {
			s.t.onShow()
		}
	case sniMenuQuit:
		if s.t.onQuit != nil {
			s.t.onQuit()
		}
	}
	return nil
}

func (s *sniMenuServer) EventGroup(events []sniEventEntry) ([]int32, *dbus.Error) {
	for _, e := range events {
		s.Event(e.ID, e.EventID, e.Data, e.Timestamp)
	}
	return []int32{}, nil
}

func (s *sniMenuServer) AboutToShow(id int32) (bool, *dbus.Error) {
	return false, nil
}

func (s *sniMenuServer) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

// pngToARGB decodes a PNG and converts it to ARGB32 bytes in the
// SNI IconPixmap order (alpha, red, green, blue per pixel), the
// same conversion Wails uses: image.RGBA() returns premultiplied
// 16-bit channels, and the low byte of each is the 8-bit value.
func pngToARGB(pngBytes []byte) (int32, int32, []byte, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return 0, 0, nil, err
	}
	rgba, ok := img.(*image.RGBA)
	if !ok {
		b := img.Bounds()
		rgba = image.NewRGBA(b)
		draw.Draw(rgba, b, img, b.Min, draw.Src)
	}
	w, h := int32(rgba.Bounds().Dx()), int32(rgba.Bounds().Dy())
	data := make([]byte, w*h*4)
	i := 0
	for y := 0; y < int(h); y++ {
		for x := 0; x < int(w); x++ {
			r, g, b, a := rgba.At(x, y).RGBA()
			data[i] = byte(a)
			data[i+1] = byte(r)
			data[i+2] = byte(g)
			data[i+3] = byte(b)
			i += 4
		}
	}
	return w, h, data, nil
}

// sniIntrospectNode builds the introspection data for one object
// path: the standard Introspectable and Properties interfaces plus
// the service interface itself.
func sniIntrospectNode(path dbus.ObjectPath, svc introspect.Interface) introspect.Node {
	return introspect.Node{
		Name: string(path),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			svc,
		},
	}
}

var sniItemIntrospect = introspect.Interface{
	Name: sniItemIface,
	Methods: []introspect.Method{
		{Name: "ContextMenu", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
		{Name: "Activate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
		{Name: "SecondaryActivate", Args: []introspect.Arg{{Name: "x", Type: "i", Direction: "in"}, {Name: "y", Type: "i", Direction: "in"}}},
		{Name: "Scroll", Args: []introspect.Arg{{Name: "delta", Type: "i", Direction: "in"}, {Name: "orientation", Type: "s", Direction: "in"}}},
	},
	Properties: []introspect.Property{
		{Name: "Category", Type: "s", Access: "read"},
		{Name: "Id", Type: "s", Access: "read"},
		{Name: "Title", Type: "s", Access: "read"},
		{Name: "Status", Type: "s", Access: "read"},
		{Name: "WindowId", Type: "i", Access: "read"},
		{Name: "IconThemePath", Type: "s", Access: "read"},
		{Name: "Menu", Type: "o", Access: "read"},
		{Name: "ItemIsMenu", Type: "b", Access: "read"},
		{Name: "IconName", Type: "s", Access: "read"},
		{Name: "IconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "OverlayIconName", Type: "s", Access: "read"},
		{Name: "OverlayIconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "AttentionIconName", Type: "s", Access: "read"},
		{Name: "AttentionIconPixmap", Type: "a(iiay)", Access: "read"},
		{Name: "AttentionMovieName", Type: "s", Access: "read"},
		{Name: "ToolTip", Type: "(sa(iiay)ss)", Access: "read"},
	},
}

var sniMenuIntrospect = introspect.Interface{
	Name: sniMenuIface,
	Methods: []introspect.Method{
		{Name: "GetLayout", Args: []introspect.Arg{{Name: "parentId", Type: "i", Direction: "in"}, {Name: "recursionDepth", Type: "i", Direction: "in"}, {Name: "propertyNames", Type: "as", Direction: "in"}, {Name: "revision", Type: "u", Direction: "out"}, {Name: "layout", Type: "(ia{sv}av)", Direction: "out"}}},
		{Name: "GetGroupProperties", Args: []introspect.Arg{{Name: "ids", Type: "ai", Direction: "in"}, {Name: "propertyNames", Type: "as", Direction: "in"}, {Name: "properties", Type: "a(ia{sv})", Direction: "out"}}},
		{Name: "GetProperty", Args: []introspect.Arg{{Name: "id", Type: "i", Direction: "in"}, {Name: "name", Type: "s", Direction: "in"}, {Name: "value", Type: "v", Direction: "out"}}},
		{Name: "Event", Args: []introspect.Arg{{Name: "id", Type: "i", Direction: "in"}, {Name: "eventId", Type: "s", Direction: "in"}, {Name: "data", Type: "v", Direction: "in"}, {Name: "timestamp", Type: "u", Direction: "in"}}},
		{Name: "EventGroup", Args: []introspect.Arg{{Name: "events", Type: "a(isvu)", Direction: "in"}, {Name: "idErrors", Type: "ai", Direction: "out"}}},
		{Name: "AboutToShow", Args: []introspect.Arg{{Name: "id", Type: "i", Direction: "in"}, {Name: "needUpdate", Type: "b", Direction: "out"}}},
		{Name: "AboutToShowGroup", Args: []introspect.Arg{{Name: "ids", Type: "ai", Direction: "in"}, {Name: "updatesNeeded", Type: "ai", Direction: "out"}, {Name: "idErrors", Type: "ai", Direction: "out"}}},
	},
	Properties: []introspect.Property{
		{Name: "Version", Type: "u", Access: "read"},
		{Name: "TextDirection", Type: "s", Access: "read"},
		{Name: "Status", Type: "s", Access: "read"},
		{Name: "IconThemePath", Type: "as", Access: "read"},
	},
}

// register asks the watcher to pick up this item. The argument is
// the item's service name as a STRING (the spec's method signature
// is "s"): the well-known name when the bus granted it, else the
// connection's unique name. Passing the object path as a
// dbus.ObjectPath sends wire type "o", which a strict watcher
// rejects — Wails passes the same path value but as a plain Go
// string (wire "s"), which KDE additionally reads as
// sender-plus-path; a service name works on both. On success the
// item is registered; on failure (or when the watcher later goes
// away) it is not, and the close handler must not park the window
// into a tray that does not exist.
func (t *sniTray) register() bool {
	call := t.conn.Object(sniWatcherService, sniWatcherPath).Call(
		sniWatcherIface+".RegisterStatusNotifierItem", 0, t.serviceName)
	if call.Err != nil {
		log.Printf("sos-gui: sni register failed: %v", call.Err)
		t.registered.Store(false)
		return false
	}
	t.registered.Store(true)
	return true
}

// watchWatcher re-registers whenever a StatusNotifierWatcher gains
// an owner (host started or restarted after us). It exits when the
// tray stops or the bus connection closes (nil signal).
func (t *sniTray) watchWatcher() {
	t.register()
	if err := t.conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, sniWatcherService),
	); err != nil {
		log.Printf("sos-gui: sni watcher match failed: %v", err)
		return
	}
	ch := make(chan *dbus.Signal, 10)
	t.conn.Signal(ch)
	for {
		select {
		case <-t.done:
			return
		case sig := <-ch:
			if sig == nil {
				return
			}
			// conn.Signal delivers every signal on this
			// connection, not just the match above; guard the
			// body shape before indexing (other signals can
			// carry fewer elements).
			if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) < 3 {
				continue
			}
			name, _ := sig.Body[0].(string)
			if name != sniWatcherService {
				continue
			}
			newOwner, _ := sig.Body[2].(string)
			if newOwner != "" {
				t.register()
			} else {
				// The watcher went away: the icon is gone
				// with it, so the window must not park
				// into a tray that does not exist.
				t.registered.Store(false)
			}
		}
	}
}

// stop unexports the tray and closes the bus connection. Safe to
// call twice and safe without a tray (shutdown guards nil).
func (t *sniTray) stop() {
	t.once.Do(func() {
		t.registered.Store(false)
		close(t.done)
		if t.conn != nil {
			t.conn.Close()
		}
	})
}

// setupSNITray builds the Linux SNI tray and reports whether it
// handled setup. It always reports handled on Linux — even when
// degraded to no tray — so setupTray never falls through to the
// MyGo AppIndicator path, whose left-click is dead on Linux.
func (a *nativeApp) setupSNITray() bool {
	statusLine := trayStatusLine(a.embedded != nil, a.base)
	t := &sniTray{
		done: make(chan struct{}),
		menu: newSNIMenu(statusLine),
		// D-Bus callbacks arrive on godbus worker goroutines;
		// GTK calls must run on the main thread, and
		// mygo.RunOnMain is the public dispatch for exactly
		// that (it runs directly when already on the main
		// thread and drops the call after quit).
		onShow: func() { mygo.RunOnMain(func() { a.showMainWindow() }) },
		onQuit: func() { mygo.RunOnMain(func() { mygo.App.Quit() }) },
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		log.Printf("sos-gui: sni tray unavailable, continuing without: %v", err)
		return true
	}
	t.conn = conn
	// Icon: same file the MyGo tray reads. A decode failure falls
	// back to a stock icon name; the tray still registers.
	iconName := ""
	var pixmaps []sniIconPixmap
	if iconBytes, err := fs.ReadFile(resources.StaticFS, "web/static/icons/icon-192x192.png"); err != nil {
		log.Printf("sos-gui: tray icon not loaded: %v", err)
		iconName = "application-x-executable"
	} else if w, h, data, err := pngToARGB(iconBytes); err != nil {
		log.Printf("sos-gui: tray icon not decoded: %v", err)
		iconName = "application-x-executable"
	} else {
		pixmaps = []sniIconPixmap{{Width: w, Height: h, Data: data}}
	}
	fail := func(format string, args ...any) bool {
		log.Printf("sos-gui: sni tray unavailable, continuing without: "+format, args...)
		conn.Close()
		return true
	}
	if err := conn.Export(&sniItemServer{t: t}, sniItemPath, sniItemIface); err != nil {
		return fail("item export failed: %v", err)
	}
	if err := conn.Export(&sniMenuServer{t: t}, sniMenuPath, sniMenuIface); err != nil {
		return fail("menu export failed: %v", err)
	}
	itemProps := map[string]*prop.Prop{
		"Category":      {Value: "ApplicationStatus", Writable: false, Emit: prop.EmitTrue},
		"Id":            {Value: "sos gui", Writable: false, Emit: prop.EmitTrue},
		"Title":         {Value: "sos gui", Writable: false, Emit: prop.EmitTrue},
		"Status":        {Value: "Active", Writable: false, Emit: prop.EmitTrue},
		"WindowId":      {Value: int32(0), Writable: false, Emit: prop.EmitTrue},
		"IconThemePath": {Value: "", Writable: false, Emit: prop.EmitTrue},
		"Menu":          {Value: sniMenuPath, Writable: false, Emit: prop.EmitTrue},
		"ItemIsMenu":    {Value: false, Writable: false, Emit: prop.EmitTrue},
		"IconName":      {Value: iconName, Writable: false, Emit: prop.EmitTrue},
		"IconPixmap":    {Value: pixmaps, Writable: false, Emit: prop.EmitTrue},
		"ToolTip":       {Value: sniToolTip{Title: "sos gui"}, Writable: false, Emit: prop.EmitTrue},
	}
	if _, err := prop.Export(conn, sniItemPath, map[string]map[string]*prop.Prop{sniItemIface: itemProps}); err != nil {
		return fail("item properties export failed: %v", err)
	}
	menuProps := map[string]*prop.Prop{
		"Version":       {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
		"TextDirection": {Value: "ltr", Writable: false, Emit: prop.EmitTrue},
		"Status":        {Value: "normal", Writable: false, Emit: prop.EmitTrue},
		"IconThemePath": {Value: []string{}, Writable: false, Emit: prop.EmitTrue},
	}
	if _, err := prop.Export(conn, sniMenuPath, map[string]map[string]*prop.Prop{sniMenuIface: menuProps}); err != nil {
		return fail("menu properties export failed: %v", err)
	}
	itemNode := sniIntrospectNode(sniItemPath, sniItemIntrospect)
	if err := conn.Export(introspect.NewIntrospectable(&itemNode), sniItemPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return fail("item introspection export failed: %v", err)
	}
	menuNode := sniIntrospectNode(sniMenuPath, sniMenuIntrospect)
	if err := conn.Export(introspect.NewIntrospectable(&menuNode), sniMenuPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return fail("menu introspection export failed: %v", err)
	}
	// The register argument must be a service name string the item
	// owns: prefer the per-process well-known name (Wails'
	// scheme); if the bus refuses it (e.g. another sos-gui
	// instance owns it), fall back to this connection's unique
	// name, which Names() reports first and always provides.
	// A failure here is not fatal — register() will fail and the
	// close handler will quit instead of parking into nothing.
	t.serviceName = sniWellKnownName(os.Getpid())
	if _, err := conn.RequestName(t.serviceName, dbus.NameFlagDoNotQueue); err != nil {
		log.Printf("sos-gui: sni well-known name not acquired, using unique name: %v", err)
		if names := conn.Names(); len(names) > 0 {
			t.serviceName = names[0]
		}
	}
	// A tray app must not die when its last window closes.
	mygo.App.OnWindowAllClosed(func() {})
	a.sniTray = t
	go t.watchWatcher()
	return true
}
