package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// Split layout tests (see settings.go / view.go). Terminals are
// created BEFORE any HOME redirect: terminal.New loads
// libghostty-vt from the user cache, and a temp HOME would make
// it try to download the library instead.

// TestSplitSettingsDefaults mirrors TestLocalSettingsDefaults for
// the split pair: fresh install and old files without the keys get
// right/50; explicit values load; an unknown layout or a percent
// outside (0,100] falls back to right/50.
func TestSplitSettingsDefaults(t *testing.T) {
	if got := defaultSettings(); got.SplitLayout != splitLayoutRight || got.SplitPercent != defaultSplitPercent {
		t.Errorf("defaultSettings() split = %q/%v, want right/50", got.SplitLayout, got.SplitPercent)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Fresh install: no file at all.
	if got := loadSettings(); got.SplitLayout != splitLayoutRight || got.SplitPercent != defaultSplitPercent {
		t.Errorf("fresh install split = %q/%v, want right/50", got.SplitLayout, got.SplitPercent)
	}
	// Old file without the keys: same defaults, other keys load.
	writeSettingsFile(t, home, `{"dark":false,"fontSize":16}`)
	if got := loadSettings(); got.SplitLayout != splitLayoutRight || got.SplitPercent != defaultSplitPercent {
		t.Errorf("old file without the keys split = %q/%v, want right/50", got.SplitLayout, got.SplitPercent)
	}
	if got := loadSettings(); got.Dark || got.FontSize != 16 {
		t.Errorf("old file's explicit dark:false/fontSize:16 must still load, got dark=%v fontSize=%d", got.Dark, got.FontSize)
	}
	// Explicit values load.
	writeSettingsFile(t, home, `{"splitLayout":"below","splitPercent":30}`)
	if got := loadSettings(); got.SplitLayout != splitLayoutBelow || got.SplitPercent != 30 {
		t.Errorf("explicit split = %q/%v, want below/30", got.SplitLayout, got.SplitPercent)
	}
	// Unknown layout string falls back to right.
	writeSettingsFile(t, home, `{"splitLayout":"diagonal"}`)
	if got := loadSettings(); got.SplitLayout != splitLayoutRight {
		t.Errorf("unknown layout must read as right, got %q", got.SplitLayout)
	}
	// Percents outside (0,100] fall back to 50 (0 is also what a
	// missing key reads as).
	for _, bad := range []string{`{"splitPercent":0}`, `{"splitPercent":-10}`, `{"splitPercent":200}`} {
		writeSettingsFile(t, home, bad)
		if got := loadSettings(); got.SplitPercent != defaultSplitPercent {
			t.Errorf("splitPercent in %s must read as 50, got %v", bad, got.SplitPercent)
		}
	}
}

// TestSplitLayoutSwitch drives setSplitLayout: switching layouts
// updates the live state and saves it at once (percent included:
// a drag is synced into the save, not clobbered by it). The
// settings file is only touched after the terminal exists (see
// the file header).
func TestSplitLayoutSwitch(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := localTestApp()
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	// Redirect HOME only now: the terminal is already created,
	// and the save below must land in a temp dir.
	t.Setenv("HOME", t.TempDir())

	// A drag (live percent) survives the layout switch's save.
	app.splitPercent = 62
	app.setSplitLayout(splitLayoutBelow)
	if app.splitLayout != splitLayoutBelow {
		t.Errorf("layout after switch = %q, want below", app.splitLayout)
	}
	if got := loadSettings(); got.SplitLayout != splitLayoutBelow || got.SplitPercent != 62 {
		t.Errorf("saved split = %q/%v, want below/62", got.SplitLayout, got.SplitPercent)
	}
	// And back to right.
	app.setSplitLayout(splitLayoutRight)
	if app.splitLayout != splitLayoutRight {
		t.Errorf("layout after switch back = %q, want right", app.splitLayout)
	}
	if got := loadSettings(); got.SplitLayout != splitLayoutRight || got.SplitPercent != 62 {
		t.Errorf("saved split after switch back = %q/%v, want right/62", got.SplitLayout, got.SplitPercent)
	}
	// Same layout again and unknown layouts are no-ops (no save,
	// no state change).
	app.setSplitLayout(splitLayoutRight)
	app.setSplitLayout("diagonal")
	if app.splitLayout != splitLayoutRight {
		t.Errorf("unknown layout must be a no-op, got %q", app.splitLayout)
	}
}

// TestSplitPercentSyncOnClose checks the struct sync: closing
// the pane copies the dragged percent into a.settings, so the
// next save (appearance, theme toggle, layout switch) writes
// the dragged value instead of the stale saved one.
func TestSplitPercentSyncOnClose(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := localTestApp()
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	app.splitPercent = 37
	app.closeLocalTerminal()
	if app.settings.SplitPercent != 37 {
		t.Errorf("settings.SplitPercent after close = %v, want 37", app.settings.SplitPercent)
	}
	if app.settings.SplitLayout != splitLayoutRight {
		t.Errorf("settings.SplitLayout after close = %q, want right", app.settings.SplitLayout)
	}
}

// TestSplitRenderBothLayouts renders the split headlessly in
// both layouts and after direct percent changes; a zero percent
// (e.g. a test-built app) is normalized to 50 by the view.
func TestSplitRenderBothLayouts(t *testing.T) {
	stubLookPathFound(t)
	stubTerminalConstructor(t)
	app := localTestApp()
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	app.openLocalTerminal(false)
	if app.localTerm == nil {
		t.Fatal("openLocalTerminal must create the local terminal")
	}
	defer app.closeLocalTerminal()
	// Redirect HOME only now: the terminals are already created,
	// and the layout switches below save the settings file.
	t.Setenv("HOME", t.TempDir())

	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	// Side by side (default).
	tt.Frame()
	// Stacked.
	app.setSplitLayout(splitLayoutBelow)
	tt.Frame()
	// A direct percent change renders (30% serial on top).
	app.splitPercent = 30
	tt.Frame()
	// Out-of-range percent is normalized, not crashed on.
	app.splitPercent = 0
	tt.Frame()
	if app.splitPercent != defaultSplitPercent {
		t.Errorf("splitPercent after render with 0 = %v, want 50", app.splitPercent)
	}
	// Back to side by side.
	app.setSplitLayout(splitLayoutRight)
	tt.Frame()
}
