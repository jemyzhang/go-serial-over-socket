package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings mirrors the WebUI settings view (index.html + app.js
// loadTerminalSettings/updateTerminalSettings), persisted to
// ~/.config/go-serial-over-socket/gui-native.json instead of
// localStorage. Defaults match the WebUI defaults.
type Settings struct {
	Dark         bool   `json:"dark"` // app chrome theme; WebUI data-theme, default dark
	TermTheme    string `json:"termTheme"`
	FontFamily   string `json:"fontFamily"`
	FontSize     int    `json:"fontSize"`
	Scrollback   int    `json:"scrollback"`   // lines, as in the WebUI
	CopyOnSelect bool   `json:"copyOnSelect"` // mouse selection copies to clipboard on release; default on
	// Local terminal multiplexer ("tmux" | "无") and the tmux
	// session a new local terminal attaches to (or creates).
	// Default tmux + serial-debug.
	Multiplexer string `json:"multiplexer"`
	TmuxSession string `json:"tmuxSession"`
	// Local terminal split: layout ("right" | "below") and the
	// serial pane's share in percent (0,100]. Default right/50.
	SplitLayout  string  `json:"splitLayout"`
	SplitPercent float32 `json:"splitPercent"`
	// DesktopDismissed: the user checked "不再提示" on the
	// install-desktop-entry prompt (Linux only). Default false,
	// so no presence check is needed on load: a missing key
	// reads false, same as the default.
	DesktopDismissed bool `json:"desktop_dismissed,omitempty"`
}

// Split layout choices for the local terminal split pane.
const (
	splitLayoutRight = "right" // serial left, local right
	splitLayoutBelow = "below" // serial above, local below
)

// defaultSplitPercent is the serial pane's share of the split
// when no value is saved (or the saved one is out of range).
const defaultSplitPercent float32 = 50

func defaultSettings() Settings {
	return Settings{
		Dark:         true,
		TermTheme:    "dark",
		FontFamily:   "",
		FontSize:     14,
		Scrollback:   10000,
		CopyOnSelect: true,
		Multiplexer:  multiplexerTmux,
		TmuxSession:  defaultTmuxSession,
		SplitLayout:  splitLayoutRight,
		SplitPercent: defaultSplitPercent,
	}
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "go-serial-over-socket", "gui-native.json"), nil
}

func loadSettings() Settings {
	s := defaultSettings()
	path, err := settingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var saved Settings
	if err := json.Unmarshal(data, &saved); err != nil {
		return s
	}
	// Zero values from an old/partial file must not wipe defaults.
	if saved.TermTheme != "" {
		s.TermTheme = saved.TermTheme
	}
	if saved.FontFamily != "" {
		s.FontFamily = saved.FontFamily
	}
	if saved.FontSize > 0 {
		s.FontSize = saved.FontSize
	}
	if saved.Scrollback > 0 {
		s.Scrollback = saved.Scrollback
	}
	// Strings: an old file missing the keys reads "", which must
	// not wipe the defaults (tmux + serial-debug).
	if saved.Multiplexer != "" {
		s.Multiplexer = saved.Multiplexer
	}
	if saved.TmuxSession != "" {
		s.TmuxSession = saved.TmuxSession
	}
	// Split: an unknown layout string or a percent outside
	// (0,100] (including 0, which is what a missing key reads
	// as) falls back to right/50.
	if saved.SplitLayout == splitLayoutRight || saved.SplitLayout == splitLayoutBelow {
		s.SplitLayout = saved.SplitLayout
	}
	if saved.SplitPercent > 0 && saved.SplitPercent <= 100 {
		s.SplitPercent = saved.SplitPercent
	}
	// DesktopDismissed defaults false, so an old file without
	// the key loads false with no presence check (unlike dark
	// and copyOnSelect above, whose defaults are true).
	s.DesktopDismissed = saved.DesktopDismissed
	// Bools need presence checks: a missing key must not flip a
	// default (dark=true).
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) == nil {
		if _, ok := raw["dark"]; ok {
			s.Dark = saved.Dark
		}
		// CopyOnSelect defaults on, so a file written before
		// the setting existed (key absent) must keep it on;
		// only an explicit false turns it off. Same
		// presence-check pattern as dark above.
		if _, ok := raw["copyOnSelect"]; ok {
			s.CopyOnSelect = saved.CopyOnSelect
		}
		// No autoScroll presence check: the setting was deleted
		// (its follow-lock veto froze wheel and keyboard
		// scrolling, and the plugin has no scroll-to-bottom call
		// to implement it honestly), and an old file's key is
		// ignored as an unknown field on load.
	}
	return s
}

func (s Settings) save() error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 0600: same config directory as the server's config.json, which
	// holds credentials; keep GUI settings equally private.
	return os.WriteFile(path, data, 0o600)
}

// scrollbackBytes converts the WebUI's scrollback-lines setting
// to the plugin's Scrollback, which is in bytes. The plugin's
// budget counts stored cells, not output bytes — measured headless
// (scrollback_test.go), a full row costs about 8-9 bytes per cell
// at steady state, and pruning overshoots when the feed is near
// capacity, so the conversion uses 16 bytes per cell, roughly
// twice the steady-state cost. cols is the width the terminal is
// (or will be) shown at; it is floored at 160 because the budget
// is fixed at creation while the window can widen later, and a
// narrow creation width must not starve a later wide one. The
// total is capped at 256 MiB: that still covers the UI maximum
// (100000 lines at 160 columns), and the budget is a cap, not an
// allocation — only rows actually fed are stored.
const (
	scrollbackBytesPerCell = 16
	scrollbackMinCols      = 160
	scrollbackMaxBytes     = 256 << 20
)

func (s Settings) scrollbackBytes(cols int) int {
	if s.Scrollback <= 0 {
		return 10 << 20
	}
	if cols < scrollbackMinCols {
		cols = scrollbackMinCols
	}
	if b := s.Scrollback * cols * scrollbackBytesPerCell; b > scrollbackMaxBytes {
		return scrollbackMaxBytes
	} else {
		return b
	}
}

// defaultLineEnding is the fixed line ending appended by Send.
// The line-ending setting was removed (it was the same item as
// "发送换行符"): sending always appends CRLF, the old default.
const defaultLineEnding = "\r\n"
