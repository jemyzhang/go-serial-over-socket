//go:build linux

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestPNGToARGB(t *testing.T) {
	// 2x2: opaque red, opaque green, transparent, opaque white.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(1, 0, color.RGBA{0, 255, 0, 255})
	img.Set(0, 1, color.RGBA{0, 0, 0, 0})
	img.Set(1, 1, color.RGBA{255, 255, 255, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	w, h, data, err := pngToARGB(buf.Bytes())
	if err != nil {
		t.Fatalf("pngToARGB: %v", err)
	}
	if w != 2 || h != 2 {
		t.Fatalf("size = %dx%d, want 2x2", w, h)
	}
	// ARGB32 order per pixel: alpha, red, green, blue.
	want := []byte{
		0xff, 0xff, 0x00, 0x00,
		0xff, 0x00, 0xff, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0xff, 0xff, 0xff, 0xff,
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want %v", data, want)
	}

	// Semi-transparent red through NRGBA: premultiplied on the
	// draw.Draw conversion, so (128, 0, 0, 128).
	nimg := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	nimg.Set(0, 0, color.NRGBA{255, 0, 0, 128})
	buf.Reset()
	if err := png.Encode(&buf, nimg); err != nil {
		t.Fatalf("png encode nrgba: %v", err)
	}
	w, h, data, err = pngToARGB(buf.Bytes())
	if err != nil {
		t.Fatalf("pngToARGB nrgba: %v", err)
	}
	if w != 1 || h != 1 || !bytes.Equal(data, []byte{0x80, 0x80, 0x00, 0x00}) {
		t.Fatalf("semi = %dx%d %v, want 1x1 [0x80 0x80 0x00 0x00]", w, h, data)
	}

	if _, _, _, err := pngToARGB([]byte("not a png")); err == nil {
		t.Fatalf("pngToARGB accepted garbage")
	}
}

func mustSNILayout(t *testing.T, srv *sniMenuServer, id int32) sniLayout {
	t.Helper()
	_, layout, err := srv.GetLayout(id, -1, nil)
	if err != nil {
		t.Fatalf("GetLayout(%d): %v", id, err)
	}
	return layout
}

func TestSNIMenuLayout(t *testing.T) {
	status := trayStatusLine(true, "http://127.0.0.1:2025")
	if status != "本地模式 · 127.0.0.1:2025" {
		t.Fatalf("status line = %q", status)
	}
	srv := &sniMenuServer{t: &sniTray{menu: newSNIMenu(status)}}

	root := mustSNILayout(t, srv, 0)
	if root.ID != 0 || len(root.Children) != 4 {
		t.Fatalf("root = id %d with %d children, want id 0 with 4", root.ID, len(root.Children))
	}
	wantIDs := []int32{1, 2, 3, 4}
	wantLabels := []string{status, "显示主窗口", "", "退出"}
	wantEnabled := []bool{false, true, true, true}
	for i, childVar := range root.Children {
		child, ok := childVar.Value().(*sniLayout)
		if !ok {
			t.Fatalf("child %d: type %T, want *sniLayout", i, childVar.Value())
		}
		if child.ID != wantIDs[i] {
			t.Fatalf("child %d: id %d, want %d", i, child.ID, wantIDs[i])
		}
		label, _ := child.Props["label"].Value().(string)
		if label != wantLabels[i] {
			t.Fatalf("child %d: label %q, want %q", i, label, wantLabels[i])
		}
		enabled, _ := child.Props["enabled"].Value().(bool)
		if enabled != wantEnabled[i] {
			t.Fatalf("child %d: enabled %v, want %v", i, enabled, wantEnabled[i])
		}
		if i == 2 {
			typ, _ := child.Props["type"].Value().(string)
			if typ != "separator" {
				t.Fatalf("child 2: type %q, want separator", typ)
			}
		} else if _, hasType := child.Props["type"]; hasType {
			t.Fatalf("child %d: unexpected type prop", i)
		}
	}

	show := mustSNILayout(t, srv, 2)
	if show.ID != 2 || len(show.Children) != 0 {
		t.Fatalf("show node = id %d with %d children, want id 2 flat", show.ID, len(show.Children))
	}
	unknown := mustSNILayout(t, srv, 99)
	if len(unknown.Children) != 0 || len(unknown.Props) != 0 {
		t.Fatalf("unknown id: got non-empty node %+v", unknown)
	}

	props, err := srv.GetGroupProperties([]int32{1, 2, 3, 4, 99}, nil)
	if err != nil {
		t.Fatalf("GetGroupProperties: %v", err)
	}
	if len(props) != 4 {
		t.Fatalf("GetGroupProperties returned %d entries, want 4 (unknown skipped)", len(props))
	}
	v, err := srv.GetProperty(2, "label")
	if err != nil {
		t.Fatalf("GetProperty(2, label): %v", err)
	}
	if v.Value().(string) != "显示主窗口" {
		t.Fatalf("GetProperty(2, label) = %q", v.Value().(string))
	}
	if _, err := srv.GetProperty(99, "label"); err == nil {
		t.Fatalf("GetProperty(99, label) unexpectedly succeeded")
	}
	if _, err := srv.GetProperty(2, "nope"); err == nil {
		t.Fatalf("GetProperty(2, nope) unexpectedly succeeded")
	}
}

func TestSNIHandlers(t *testing.T) {
	var shows, quits int
	tray := &sniTray{
		menu:   newSNIMenu("本地模式"),
		onShow: func() { shows++ },
		onQuit: func() { quits++ },
	}
	item := &sniItemServer{t: tray}
	menu := &sniMenuServer{t: tray}

	if err := item.Activate(0, 0); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if shows != 1 || quits != 0 {
		t.Fatalf("after Activate: shows=%d quits=%d, want 1/0", shows, quits)
	}
	for name, call := range map[string]func() *dbus.Error{
		"ContextMenu":       func() *dbus.Error { return item.ContextMenu(0, 0) },
		"SecondaryActivate": func() *dbus.Error { return item.SecondaryActivate(0, 0) },
		"Scroll":            func() *dbus.Error { return item.Scroll(1, "vertical") },
	} {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if shows != 1 || quits != 0 {
		t.Fatalf("after no-op methods: shows=%d quits=%d, want 1/0", shows, quits)
	}

	click := func(id int32) {
		if err := menu.Event(id, "clicked", dbus.Variant{}, 0); err != nil {
			t.Fatalf("Event(%d, clicked): %v", id, err)
		}
	}
	click(2)
	click(4)
	click(1)
	click(3)
	click(0)
	if shows != 2 || quits != 1 {
		t.Fatalf("after menu clicks: shows=%d quits=%d, want 2/1", shows, quits)
	}
	if err := menu.Event(2, "opened", dbus.Variant{}, 0); err != nil {
		t.Fatalf("Event opened: %v", err)
	}
	if shows != 2 {
		t.Fatalf("opened fired click: shows=%d", shows)
	}
	idErrs, err := menu.EventGroup([]sniEventEntry{
		{ID: 2, EventID: "clicked"},
		{ID: 4, EventID: "clicked"},
	})
	if err != nil {
		t.Fatalf("EventGroup: %v", err)
	}
	if len(idErrs) != 0 || shows != 3 || quits != 2 {
		t.Fatalf("after EventGroup: shows=%d quits=%d idErrs=%v, want 3/2/[]", shows, quits, idErrs)
	}
	if need, err := menu.AboutToShow(0); err != nil || need {
		t.Fatalf("AboutToShow = %v, %v, want false, nil", need, err)
	}
	if upd, errs, err := menu.AboutToShowGroup([]int32{0}); err != nil || len(upd) != 0 || len(errs) != 0 {
		t.Fatalf("AboutToShowGroup = %v, %v, %v", upd, errs, err)
	}
}

func TestSetupSNIDegradedNoBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/sos-test-bus")
	app := &nativeApp{}
	if handled := app.setupSNITray(); !handled {
		t.Fatalf("setupSNITray reported not handled on Linux")
	}
	if app.sniTray != nil {
		t.Fatalf("sniTray set despite no bus")
	}
}

func TestSNIWellKnownName(t *testing.T) {
	if got := sniWellKnownName(123); got != "org.kde.StatusNotifierItem-123-1" {
		t.Errorf("sniWellKnownName(123) = %q, want %q", got, "org.kde.StatusNotifierItem-123-1")
	}
	if got := sniWellKnownName(1); got != "org.kde.StatusNotifierItem-1-1" {
		t.Errorf("sniWellKnownName(1) = %q, want %q", got, "org.kde.StatusNotifierItem-1-1")
	}
}
