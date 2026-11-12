//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	resources "github.com/jemyzhang/serial-over-socket-go"
)

// Freedesktop integration: a per-user .desktop entry so sos-gui can
// be launched from the desktop's application menu. The GUI offers to
// install it once (see maybePromptDesktopInstall); the same content
// ships in the repo as packaging/sos-gui.desktop for system installs.
// This is a restore, not new: the old webview client had it (PR #4,
// 8db353d) and the native rewrite (PR #17) dropped it; the logic is
// the old one, adapted from guiPrefs to the native Settings.

func xdgDataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share")
}

func desktopEntryPath() string {
	return filepath.Join(xdgDataHome(), "applications", "sos-gui.desktop")
}

func desktopIconPath() string {
	return filepath.Join(xdgDataHome(), "icons", "hicolor", "192x192", "apps", "sos-gui.png")
}

// desktopEntryContents renders the .desktop file pointing at the
// given executable.
func desktopEntryContents(execPath string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Version=1.0\n" +
		"Name=sos gui\n" +
		"Comment=Serial over Socket 桌面客户端\n" +
		"Exec=" + strconv.Quote(execPath) + "\n" +
		"Icon=sos-gui\n" +
		"Terminal=false\n" +
		"Categories=Utility;Development;\n" +
		"StartupNotify=true\n"
}

// desktopEntryStatus reports whether the entry is installed and
// whether its Exec still points at the running binary (it goes stale
// when the binary is moved or rebuilt elsewhere).
func desktopEntryStatus() (installed, current bool) {
	data, err := os.ReadFile(desktopEntryPath())
	if err != nil {
		return false, false
	}
	exe, err := os.Executable()
	if err != nil {
		return true, true // cannot tell; leave it alone
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	want := "Exec=" + strconv.Quote(exe)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == want {
			return true, true
		}
	}
	return true, false
}

// installDesktopEntry writes the entry and the icon for the running
// binary into the user's XDG data dirs.
func installDesktopEntry() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	icon, err := fs.ReadFile(resources.StaticFS, "web/static/icons/icon-192x192.png")
	if err != nil {
		return fmt.Errorf("load icon: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(desktopIconPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(desktopIconPath(), icon, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(desktopEntryPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(desktopEntryPath(), []byte(desktopEntryContents(exe)), 0o644); err != nil {
		return err
	}
	// Best effort: refresh the desktop database so the entry shows
	// up without waiting for the desktop to rescan on its own.
	if path, err := exec.LookPath("update-desktop-database"); err == nil {
		_ = exec.Command(path, filepath.Dir(desktopEntryPath())).Run()
	}
	return nil
}

// desktopDialog is the native question dialog, injectable in tests.
// Nil means the real mygo.Dialog.Message; tests override it to
// answer without showing a window (same pattern as saveDialog).
var desktopDialog func(mygo.MessageOptions) (mygo.MessageResult, error)

// maybePromptDesktopInstall offers (once per run, from a goroutine:
// Dialog.Message blocks) to install the desktop entry. An entry that
// exists but points at a moved binary is refreshed silently instead.
// Called once the window is up (see main.go); never on every start:
// installed-and-current and dismissed both return quietly.
func (a *nativeApp) maybePromptDesktopInstall() {
	if !a.desktopPrompted.CompareAndSwap(false, true) {
		return
	}
	a.mu.Lock()
	dismissed := a.settings.DesktopDismissed
	a.mu.Unlock()
	if dismissed {
		return
	}
	installed, current := desktopEntryStatus()
	switch {
	case installed && current:
		return
	case installed: // stale Exec: fix quietly, no prompt
		if err := installDesktopEntry(); err != nil {
			log.Printf("sos-gui: refresh desktop entry: %v", err)
		}
		return
	}
	ask := desktopDialog
	if ask == nil {
		ask = mygo.Dialog.Message
	}
	res, err := ask(mygo.MessageOptions{
		Type:            mygo.MessageQuestion,
		Title:           "把 sos gui 加到应用菜单？",
		Message:         "安装桌面启动项后，可以从应用菜单直接启动 sos gui。",
		Detail:          "写入 " + desktopEntryPath(),
		Buttons:         []string{"安装", "以后再说"},
		CheckboxLabel:   "不再提示",
		CheckboxChecked: false,
	})
	if err != nil {
		return
	}
	if res.CheckboxChecked {
		a.mu.Lock()
		a.settings.DesktopDismissed = true
		s := a.settings
		a.mu.Unlock()
		if err := s.save(); err != nil {
			log.Printf("sos-gui: save desktop dismissal: %v", err)
		}
	}
	if res.Button == 0 {
		if err := installDesktopEntry(); err != nil {
			log.Printf("sos-gui: install desktop entry: %v", err)
		}
	}
}
