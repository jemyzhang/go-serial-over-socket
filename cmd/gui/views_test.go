package main

import (
	"image"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// scanHairlineBetween reports whether any single row y in
// (yTop, yBottom) contains a horizontal run of at least minRun
// pixels that are all the same color AND differ from the pixel
// directly above the run's end. A solid 1px line qualifies; a
// uniform background row does not (its row above is the same
// color), and text antialiasing never forms a run that long.
// The check is theme-agnostic: it never assumes a line color.
func scanHairlineBetween(img *image.RGBA, yTop, yBottom, minRun int) bool {
	b := img.Bounds()
	if yTop < b.Min.Y {
		yTop = b.Min.Y
	}
	if yBottom > b.Max.Y {
		yBottom = b.Max.Y
	}
	for y := yTop + 1; y < yBottom; y++ {
		if y <= b.Min.Y {
			continue
		}
		var runColor [4]uint8
		run := 0
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			cur := [4]uint8{c.R, c.G, c.B, c.A}
			if x == b.Min.X || cur != runColor {
				runColor = cur
				run = 1
				continue
			}
			run++
			if run >= minRun {
				above := img.RGBAAt(x, y-1)
				if [4]uint8{above.R, above.G, above.B, above.A} != runColor {
					return true
				}
			}
		}
	}
	return false
}

// TestHistoryDividerPaints renders the real history tab (panel open
// on the history section, two entries) and asserts a hairline is
// painted between the two rows. The pre-fix construction
// (Height(1).Fill()) painted nothing: Fill overwrote the 1px
// height with percent(100), which resolves to 0 against the
// auto-height Column.
func TestHistoryDividerPaints(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N", Description: "USB Serial"},
		},
		portIDs:      []string{"ttyUSB0"},
		selected:     "ttyUSB0",
		terms:        map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings:     defaultSettings(),
		settingsOpen: true,
		panelSection: settingsTabHistory,
		status:       "服务连接成功",
		history: []HistoryEntry{
			{ID: "h1", Command: "AT+VERSION"},
			{ID: "h2", Command: "STATUS?"},
		},
		drafts: map[string]*portDraft{},
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	img := tt.Image()
	r1, ok1 := tt.Find("AT+VERSION")
	r2, ok2 := tt.Find("STATUS?")
	if !ok1 || !ok2 {
		t.Fatalf("history texts not rendered: texts = %v", tt.Texts())
	}
	// Find rects are in logical units; the image may be scaled.
	scale := float64(img.Bounds().Dy()) / 750
	yTop := int((r1.Y + r1.H) * float32(scale))
	yBottom := int(r2.Y * float32(scale))
	if !scanHairlineBetween(img, yTop, yBottom, 200) {
		t.Errorf("no hairline between history rows (y %d..%d); the divider is still not painting", yTop, yBottom)
	}
}

// TestPortsDividerPaints renders the real ports tab (panel open
// on the ports section, two ports) and asserts a hairline is
// painted between the two port rows. Same Fill() defect as the
// history divider: the old construction painted nothing.
func TestPortsDividerPaints(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N", Description: "USB Serial"},
			"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected", Alias: "card", Baudrate: 921600, Databits: 8, Stopbit: 1, Parity: "N", Description: "USB Serial"},
		},
		portIDs:      []string{"ttyUSB0", "ttyUSB1"},
		selected:     "ttyUSB0",
		terms:        map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings:     defaultSettings(),
		settingsOpen: true,
		panelSection: settingsTabPorts,
		status:       "服务连接成功",
		drafts:       map[string]*portDraft{},
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	img := tt.Image()
	// Anchor on the panel rows' detail lines, not the aliases:
	// Find returns the first exact match, and the aliases also
	// appear in the toolbar pills above the panel.
	r1, ok1 := tt.Find("/dev/ttyUSB0 · 115200 8N1 · USB Serial")
	r2, ok2 := tt.Find("/dev/ttyUSB1 · 921600 8N1 · USB Serial")
	if !ok1 || !ok2 {
		t.Fatalf("port texts not rendered: texts = %v", tt.Texts())
	}
	// Find rects are in logical units; the image may be scaled.
	scale := float64(img.Bounds().Dy()) / 750
	yTop := int((r1.Y + r1.H) * float32(scale))
	yBottom := int(r2.Y * float32(scale))
	if !scanHairlineBetween(img, yTop, yBottom, 200) {
		t.Errorf("no hairline between port rows (y %d..%d); the divider is still not painting", yTop, yBottom)
	}
}

// TestHairlineConstruction isolates the Fill() defect: the old
// construction (Height(1) then Fill) and the new one
// (Height(1) + AlignSelf(Stretch) + Shrink(0)) are rendered in the
// same faithful nesting — a fixed-size root holding an auto-height
// Column, as historyTab does. The old line must NOT paint (0
// height), the new one must.
func TestHairlineConstruction(t *testing.T) {
	border := darkPalette.hex(darkPalette.border)
	render := func(old bool) (*image.RGBA, *ui.Tester) {
		tt := ui.NewTester(func(c *ui.Context) {
			ui.Column(c).Background(ui.RGBA(0, 0, 0, 255)).Children(func() {
				ui.Column(c).Gap(16).Children(func() {
					ui.Text(c, "AAA").TextColor(ui.RGBA(255, 255, 255, 255))
					if old {
						ui.Row(c).Height(1).Fill().Background(border)
					} else {
						ui.Row(c).Height(1).AlignSelf(ui.Stretch).Shrink(0).Background(border)
					}
					ui.Text(c, "BBB").TextColor(ui.RGBA(255, 255, 255, 255))
				})
			})
		}, 400, 300)
		tt.Frame()
		return tt.Image(), tt
	}
	between := func(img *image.RGBA, tt *ui.Tester) (int, int) {
		r1, ok1 := tt.Find("AAA")
		r2, ok2 := tt.Find("BBB")
		if !ok1 || !ok2 {
			t.Fatalf("marker texts not rendered: texts = %v", tt.Texts())
		}
		scale := float64(img.Bounds().Dy()) / 300
		return int((r1.Y + r1.H) * float32(scale)), int(r2.Y * float32(scale))
	}

	// New construction paints.
	imgNew, ttNew := render(false)
	yTop, yBottom := between(imgNew, ttNew)
	if !scanHairlineBetween(imgNew, yTop, yBottom, 200) {
		t.Error("new hairline construction did not paint")
	}

	// Old construction must not paint (this is the regression
	// guard: if a mygo change ever makes Fill() behave, this
	// test says so instead of silently passing).
	imgOld, ttOld := render(true)
	yTopOld, yBottomOld := between(imgOld, ttOld)
	if scanHairlineBetween(imgOld, yTopOld, yBottomOld, 200) {
		t.Error("old Fill() construction painted a line — the defect model changed; re-check before trusting the fix")
	}
}

// TestWidgetThemeTooltipColors pins the tooltip inverse colors
// (see widgetTheme): dark mode gets a semi-transparent black
// fill with white text, light mode a semi-transparent white
// fill with black text. mygo's defaults would leave Inverse
// zero, which fills a dark-mode tooltip with the light Text
// color and a light-mode one with the dark Text color — the
// wrong way round from what the app wants.
func TestWidgetThemeTooltipColors(t *testing.T) {
	th := widgetTheme(true)
	if th.Inverse != ui.RGBA(0, 0, 0, 0.85) || th.InverseText != ui.RGBA(255, 255, 255, 1) {
		t.Errorf("dark widgetTheme inverse = %v on %v, want semi-transparent black on white", th.Inverse, th.InverseText)
	}
	th = widgetTheme(false)
	if th.Inverse != ui.RGBA(255, 255, 255, 0.85) || th.InverseText != ui.RGBA(0, 0, 0, 1) {
		t.Errorf("light widgetTheme inverse = %v on %v, want semi-transparent white on black", th.Inverse, th.InverseText)
	}
}

// TestTooltipFollowsTheme renders the app in both themes, hovers
// a port pill so its native tooltip shows, and checks the
// tooltip fill and text follow the theme. The fill is sampled
// just left of the tooltip text (inside the tooltip padding);
// the text is checked by scanning the tooltip text rect for a
// near-white pixel (dark mode) or a near-black one (light mode).
// The fill assertion is deliberately loose on the exact value:
// a semi-transparent fill composites over the terminal
// background, so dark mode expects near-black rather than pure
// black, and light mode near-white rather than pure white.
func TestTooltipFollowsTheme(t *testing.T) {
	for _, dark := range []bool{true, false} {
		app := localTestApp()
		app.settings.Dark = dark
		tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
		tt.Frame()
		r, ok := tt.Find("sensor")
		if !ok {
			t.Fatalf("dark=%v: pill text not rendered: %v", dark, tt.Texts())
		}
		tt.Move(r.X+r.W/2, r.Y+r.H/2)
		tt.Frame()
		tt.Advance(2 * time.Second)
		tt.Frame()
		// The tooltip is one multi-line text element, so Find
		// needs the whole string, not one of its lines.
		tr, ok := tt.Find(portTooltip(app.ports["ttyUSB0"]))
		if !ok {
			t.Fatalf("dark=%v: tooltip did not show: %v", dark, tt.Texts())
		}
		img := tt.Image()
		scale := float64(img.Bounds().Dy()) / 750
		fx := int((tr.X - 4) * float32(scale))
		fy := int((tr.Y + tr.H/2) * float32(scale))
		fill := img.RGBAAt(fx, fy)
		x0 := int(tr.X * float32(scale))
		y0 := int(tr.Y * float32(scale))
		x1 := int((tr.X + tr.W) * float32(scale))
		y1 := int((tr.Y + tr.H) * float32(scale))
		bright, dim := uint8(0), uint8(255)
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				px := img.RGBAAt(x, y)
				mn := px.R
				if px.G < mn {
					mn = px.G
				}
				if px.B < mn {
					mn = px.B
				}
				mx := px.R
				if px.G > mx {
					mx = px.G
				}
				if px.B > mx {
					mx = px.B
				}
				if mx > bright {
					bright = mx
				}
				if mn < dim {
					dim = mn
				}
			}
		}
		if dark {
			if fill.R > 40 || fill.G > 40 || fill.B > 40 {
				t.Errorf("dark tooltip fill = %v, want near-black (semi-transparent black over the dark terminal)", fill)
			}
			if bright < 200 {
				t.Errorf("dark tooltip text brightest channel = %d, want near-white text (>= 200)", bright)
			}
		} else {
			if fill.R < 215 || fill.G < 215 || fill.B < 215 {
				t.Errorf("light tooltip fill = %v, want near-white (semi-transparent white over the light terminal)", fill)
			}
			if dim > 60 {
				t.Errorf("light tooltip text darkest channel = %d, want near-black text (<= 60)", dim)
			}
		}
		t.Logf("dark=%v tooltip fill=%v textBright=%d textDim=%d", dark, fill, bright, dim)
	}
}
