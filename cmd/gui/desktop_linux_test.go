//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo"
)

// Desktop-entry tests run against a scratch HOME (t.Setenv +
// t.TempDir); the real ~/.local is never touched. Terminals are
// created BEFORE any HOME redirect: terminal.New loads
// libghostty-vt from the user cache, and a temp HOME would make
// it try to download the library instead — but these tests need
// no terminal at all.

func scratchHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	return home
}

// The install flow against a scratch HOME: the entry and icon land
// in the XDG data dirs, the entry points at the running binary, and
// the status check then reports installed + current.
func TestDesktopEntryInstall(t *testing.T) {
	home := scratchHome(t)

	if installed, _ := desktopEntryStatus(); installed {
		t.Fatal("fresh HOME must not report an installed entry")
	}
	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".local", "share", "applications", "sos-gui.desktop"))
	if err != nil {
		t.Fatalf("entry not written: %v", err)
	}
	if !strings.Contains(string(data), "Name=sos gui") || !strings.Contains(string(data), "Exec=") {
		t.Fatalf("entry content off:\n%s", data)
	}
	icon, err := os.ReadFile(filepath.Join(home, ".local", "share", "icons", "hicolor", "192x192", "apps", "sos-gui.png"))
	if err != nil || len(icon) == 0 {
		t.Fatalf("icon not written: %v", err)
	}
	if installed, current := desktopEntryStatus(); !installed || !current {
		t.Fatalf("status after install = (%v, %v), want (true, true)", installed, current)
	}
}

// A stale entry (Exec pointing elsewhere) reports installed but
// not current, and reinstalling refreshes it to the running binary
// — the silent-refresh path the prompt takes instead of asking.
func TestDesktopEntryStaleRefresh(t *testing.T) {
	home := scratchHome(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry: %v", err)
	}
	path := filepath.Join(home, ".local", "share", "applications", "sos-gui.desktop")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Exec=") {
			lines[i] = `Exec="/somewhere/else/sos-gui"`
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if installed, current := desktopEntryStatus(); !installed || current {
		t.Fatalf("stale entry status = (%v, %v), want (true, false)", installed, current)
	}
	if err := installDesktopEntry(); err != nil {
		t.Fatalf("refresh installDesktopEntry: %v", err)
	}
	if installed, current := desktopEntryStatus(); !installed || !current {
		t.Fatalf("status after refresh = (%v, %v), want (true, true)", installed, current)
	}
}

// The prompt, with the dialog stubbed: "以后再说" installs nothing,
// "不再提示" persists, and a dismissed run never prompts again.
func TestDesktopPromptDismiss(t *testing.T) {
	scratchHome(t)

	calls := 0
	oldDialog := desktopDialog
	desktopDialog = func(opts mygo.MessageOptions) (mygo.MessageResult, error) {
		calls++
		if len(opts.Buttons) != 2 || opts.CheckboxLabel != "不再提示" {
			t.Errorf("dialog options off: buttons=%v checkbox=%q", opts.Buttons, opts.CheckboxLabel)
		}
		return mygo.MessageResult{Button: 1, CheckboxChecked: true}, nil
	}
	t.Cleanup(func() { desktopDialog = oldDialog })

	app := &nativeApp{settings: defaultSettings()}
	app.maybePromptDesktopInstall()
	if calls != 1 {
		t.Fatalf("dialog called %d times, want 1", calls)
	}
	if installed, _ := desktopEntryStatus(); installed {
		t.Error("dismissing must not install the entry")
	}
	if got := loadSettings(); !got.DesktopDismissed {
		t.Error("desktop_dismissed must be saved after checking 不再提示")
	}
	// Next run: dismissed, so no prompt at all.
	app2 := &nativeApp{settings: loadSettings()}
	app2.maybePromptDesktopInstall()
	if calls != 1 {
		t.Errorf("dialog called %d times after dismissal, want still 1", calls)
	}
}

// The prompt, with the dialog stubbed: "安装" installs the entry,
// and an installed + current entry never prompts on the next run.
func TestDesktopPromptInstallAndSkip(t *testing.T) {
	scratchHome(t)

	calls := 0
	oldDialog := desktopDialog
	desktopDialog = func(opts mygo.MessageOptions) (mygo.MessageResult, error) {
		calls++
		return mygo.MessageResult{Button: 0}, nil // "安装"
	}
	t.Cleanup(func() { desktopDialog = oldDialog })

	app := &nativeApp{settings: defaultSettings()}
	app.maybePromptDesktopInstall()
	if calls != 1 {
		t.Fatalf("dialog called %d times, want 1", calls)
	}
	if installed, current := desktopEntryStatus(); !installed || !current {
		t.Fatalf("status after install = (%v, %v), want (true, true)", installed, current)
	}
	app2 := &nativeApp{settings: defaultSettings()}
	app2.maybePromptDesktopInstall()
	if calls != 1 {
		t.Errorf("dialog called %d times with current entry, want still 1", calls)
	}
}

// A stale entry is refreshed silently by the prompt path: no
// dialog, entry current afterwards.
func TestDesktopPromptStaleRefreshSilent(t *testing.T) {
	home := scratchHome(t)

	if err := installDesktopEntry(); err != nil {
		t.Fatalf("installDesktopEntry: %v", err)
	}
	path := filepath.Join(home, ".local", "share", "applications", "sos-gui.desktop")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Exec=") {
			lines[i] = `Exec="/somewhere/else/sos-gui"`
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	oldDialog := desktopDialog
	desktopDialog = func(opts mygo.MessageOptions) (mygo.MessageResult, error) {
		calls++
		return mygo.MessageResult{Button: 1}, nil
	}
	t.Cleanup(func() { desktopDialog = oldDialog })

	app := &nativeApp{settings: defaultSettings()}
	app.maybePromptDesktopInstall()
	if calls != 0 {
		t.Errorf("stale entry must refresh silently, dialog called %d times", calls)
	}
	if installed, current := desktopEntryStatus(); !installed || !current {
		t.Fatalf("status after silent refresh = (%v, %v), want (true, true)", installed, current)
	}
}
