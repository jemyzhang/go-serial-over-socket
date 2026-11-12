package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// palette is the app chrome's color set, taken from GoRex's
// style.go (darkColors / lightColors) rather than the WebUI's CSS
// :root: the window is a vertical gradient, cards float on it
// translucent with a soft shadow, and status is green/amber dots.
// ui.Hex parses #rrggbbaa, so the translucent values are used
// as-is, not flattened. Terminal colors are separate
// (terminalTheme below) and unchanged.
type palette struct {
	// Window gradient stops (GoRex paints four stops + a right
	// tint with a custom painter; ui.LinearGradient blends two
	// colors, so the chrome uses top -> bottom only — see README).
	// The Tern-style layout no longer paints a gradient: the
	// terminal background is the window, and section is the side
	// panel's section-number color. The settings panel has no
	// background of its own: it takes the window background
	// (termBg) directly — it used to have one (dark 17171b /
	// light f5f3ef, GoRex's paper tone), but Jemy decided
	// 2026-11-09 that the panel matches the main background in
	// both themes, so the field was removed rather than kept as
	// a second source of truth.
	bgTop, bgBottom                                            string
	bg, surface, card, elevated, hover, active                 string
	text, muted, dim, accent, success, danger, warning, border string
	termBg, shadow, section                                    string
}

var darkPalette = palette{
	bgTop: "2c2131", bgBottom: "2a2619",
	bg: "231e27", surface: "242429f0", card: "242429f0", elevated: "26262af7",
	hover: "ffffff14", active: "ffffff24",
	text: "f2f2f7", muted: "aeaeb2", dim: "8e8e93", accent: "f2f2f7",
	success: "4ade80", danger: "f87171", warning: "fbbf24",
	border: "ffffff24", termBg: "0b0d10",
	shadow:  "00000066",
	section: "7dd3fc",
}

var lightPalette = palette{
	bgTop: "f6e7f6", bgBottom: "ece1bd",
	bg: "efe1e6", surface: "fffffff2", card: "fffffff2", elevated: "fcfcfdf7",
	hover: "0000000e", active: "fffffff7",
	text: "1d1d1f", muted: "636366", dim: "8e8e93", accent: "1d1d1f",
	success: "34a853", danger: "e5484d", warning: "f59e0b",
	border: "00000014", termBg: "ffffff",
	shadow:  "3c28321f",
	section: "0369a1",
}

func (p palette) hex(s string) ui.Color { return ui.Hex("#" + s) }

// widgetTheme returns the mygo widget theme for the app's dark/light
// mode, with the tooltip (and toast) inverse colors set so tooltips
// follow the theme instead of running the wrong way round: dark
// mode gets a semi-transparent black fill with white text, light
// mode a semi-transparent white fill with black text. mygo fills
// tooltips AND toasts from Theme.Inverse / InverseText (inverse()),
// with no separate tooltip knob — left zero they fall back to
// Text on Background, i.e. a light box in dark mode and a dark
// box in light mode. The app never shows toasts (no Toast calls),
// so the shared fill only affects tooltips in practice. The 0.85
// alpha is real: node backgrounds composite it (the palette
// already relies on translucent fills), so the tooltip lets a
// little of what's beneath show through instead of a flat slab.
func widgetTheme(dark bool) *ui.Theme {
	th := ui.LightTheme()
	if dark {
		th = ui.DarkTheme()
	}
	if dark {
		th.Inverse = ui.RGBA(0, 0, 0, 0.85)
		th.InverseText = ui.RGBA(255, 255, 255, 1)
	} else {
		th.Inverse = ui.RGBA(255, 255, 255, 0.85)
		th.InverseText = ui.RGBA(0, 0, 0, 1)
	}
	return th
}

// terminalTheme maps the WebUI TERMINAL_THEMES names (app.js) onto a
// plugin Theme. The palette comes from the plugin's own dark/light
// theme so program colors stay sane; only fg/bg/cursor/selection are
// overridden to the WebUI values. Unlike the app chrome theme, the
// terminal theme is independent (as in the WebUI).
func terminalTheme(name string) *terminal.Theme {
	var base *terminal.Theme
	if name == "light" {
		base = terminal.LightTheme()
	} else {
		base = terminal.DarkTheme()
	}
	switch name {
	case "light":
		base.Foreground = ui.Hex("#0f172a")
		base.Background = ui.Hex("#ffffff")
		base.Cursor = ui.Hex("#ea6645")
		base.Selection = ui.RGBA(234, 102, 69, 0.2)
	case "blue":
		base.Foreground = ui.Hex("#93c5fd")
		base.Background = ui.Hex("#0b1329")
		base.Cursor = ui.Hex("#38bdf8")
		base.Selection = ui.RGBA(56, 189, 248, 0.25)
	case "green":
		base.Foreground = ui.Hex("#86efac")
		base.Background = ui.Hex("#0a1a0f")
		base.Cursor = ui.Hex("#4ade80")
		base.Selection = ui.RGBA(74, 222, 128, 0.25)
	default: // "dark" and anything unknown
		base.Foreground = ui.Hex("#f1f5f9")
		base.Background = ui.Hex("#0a0e17")
		base.Cursor = ui.Hex("#ea6645")
		base.Selection = ui.RGBA(234, 102, 69, 0.35)
	}
	return base
}

// statusColor is the lamp's color, shared by the pill dot, the
// panel row dots, and the command bar dot: green = connected,
// amber = connecting, red = error or disconnected (a port that
// is not connected is a stop red, not a muted gray — the old
// gray "anything else" hid a disconnected port among idle UI).
// Flashing is NOT part of this mapping: only the pill dot
// flashes, and only while its port records (see lampDotColor);
// every other dot renders this color at full opacity.
func statusColor(p palette, info PortInfo) string {
	if info.Connected {
		return p.success
	}
	if info.Status == "connecting" {
		return p.warning
	}
	if info.Status == "error" {
		return p.danger
	}
	return p.danger
}

func displayName(info PortInfo) string {
	if info.Alias != "" {
		return info.Alias
	}
	return info.ID
}

// frameFormat renders a port's frame format as in the WebUI cards
// (8N1): databits + parity + stopbit. Empty when any part is
// unknown — a guessed default would be worse than no frame.
func frameFormat(info PortInfo) string {
	if info.Databits <= 0 || info.Parity == "" || info.Stopbit <= 0 {
		return ""
	}
	return fmt.Sprintf("%d%s%s", info.Databits, info.Parity, strconv.FormatFloat(info.Stopbit, 'f', -1, 64))
}

// portPillTexts returns the toolbar pill tab's texts in order:
// the name (alias when set, else the port id) then the short
// config (baudrate only). The pill never carries the full
// detail — path, frame and vendor live in the pill's tooltip
// (see portTooltip) — so a strip of many ports stays short
// enough to compress into one row without a scrollbar.
func portPillTexts(info PortInfo) []string {
	name := info.Alias
	if name == "" {
		name = info.ID
	}
	if info.Baudrate > 0 {
		return []string{name, "· " + strconv.Itoa(info.Baudrate)}
	}
	return []string{name}
}

// portTooltip returns the pill's full device info for the native
// hover tooltip, one fact per line: name, path, baud + frame,
// vendor description when present. mygo's text layout breaks at
// newlines, so a "\n"-joined string renders as stacked lines
// through the plain Element.Tooltip — no custom tooltip layer.
// A line is omitted when its facts are all unknown, never filled
// with a guessed default. No status text — the dot carries the
// state by color alone.
func portTooltip(info PortInfo) string {
	name := info.Alias
	if name == "" {
		name = info.ID
	}
	lines := []string{name, "/dev/" + info.ID}
	if cfg := baudFrame(info); cfg != "" {
		lines = append(lines, cfg)
	}
	if info.Description != "" {
		lines = append(lines, info.Description)
	}
	return strings.Join(lines, "\n")
}

// baudFrame renders "baud frame" (e.g. "115200 8N1"), omitting
// each unknown part; empty when both are unknown.
func baudFrame(info PortInfo) string {
	parts := []string{}
	if info.Baudrate > 0 {
		parts = append(parts, strconv.Itoa(info.Baudrate))
	}
	if f := frameFormat(info); f != "" {
		parts = append(parts, f)
	}
	return strings.Join(parts, " ")
}

// nextBreathTarget flips the dot-flash target between dim and
// full: the toolbar dot takes each target directly (step flash,
// 1.8s cycle). The flip itself is driven by a ticker (see
// startBreathing); the view never interpolates toward the
// target, so no animation runs between ticks.
func nextBreathTarget(cur float32) float32 {
	if cur >= 1.0 {
		return 0.45
	}
	return 1.0
}

// lampDotColor returns the pill lamp's color. Flashing means
// recording, nothing else: a connected port that records flashes
// green at the stepped snapshot alpha; every other state renders
// its status color at full opacity — connected green still,
// connecting amber still, disconnected and error stop red still.
// Panel row dots and the command bar dot never call this: they
// render statusColor directly, so they share the colors but
// never flash.
func lampDotColor(p palette, info PortInfo, recording bool, breath float32) ui.Color {
	if info.Connected && recording {
		r, g, b := parseHex6(p.success)
		return ui.RGBA(r, g, b, breath)
	}
	return p.hex(statusColor(p, info))
}

// pillConnectButton maps a port's connection state to its pill
// power button: color, hover tip, the action ("connect" /
// "disconnect", "" when disabled), and whether a click does
// nothing. Connected green disconnects, disconnected muted
// connects, error red reconnects, connecting amber is disabled —
// a click on a connecting port must not fire a second connect.
// Disabled states are action-gated in the view, not
// native-Disabled: a natively disabled element's click
// fall-through to the pill Row is unproven, while a gated Button
// provably consumes the click (the record-dot pattern), so the
// pill never switches on a dead-button click.
func pillConnectButton(p palette, info PortInfo) (color, tip, action string, disabled bool) {
	if info.Connected {
		return p.success, "断开", "disconnect", false
	}
	if info.Status == "connecting" {
		return p.warning, "连接中", "", true
	}
	if info.Status == "error" {
		return p.danger, "连接", "connect", false
	}
	return p.muted, "连接", "connect", false
}

// pillRecordButton maps a port's recording state to its pill ●
// button: color, hover tip, and whether a click does nothing.
// Recording amber stops, a live idle port muted starts, and any
// port without a live connection is dimmed and disabled —
// recording needs a connected port (a connecting port is not
// one yet). The disabled tip is just the plain action name
// ("记录"), not an explanation of why it is disabled.
func pillRecordButton(p palette, info PortInfo, recording bool) (color, tip string, disabled bool) {
	if recording {
		return p.warning, "停止记录", false
	}
	if !info.Connected {
		return p.dim, "记录", true
	}
	return p.muted, "开始记录", false
}

// parseHex6 parses a 6-digit hex color (no '#') into bytes; any
// malformed value falls back to the dark theme's success green
// rather than crashing the toolbar render.
func parseHex6(s string) (uint8, uint8, uint8) {
	if len(s) != 6 {
		return 74, 222, 128
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 74, 222, 128
	}
	return uint8(v >> 16), uint8(v >> 8), uint8(v)
}

// portDetailLabel is the muted subtitle inside the panel's port
// row: path · baudrate + frame format. No vendor name here (the
// panel row appends it); no status text anywhere — the dot in
// front of each port carries the state by color alone (see
// statusColor). The pill tooltip no longer uses it: it stacks
// the same facts as separate lines (see portTooltip).
func portDetailLabel(info PortInfo) string {
	detail := "/dev/" + info.ID
	if info.Baudrate > 0 {
		detail += " · " + strconv.Itoa(info.Baudrate)
	}
	if f := frameFormat(info); f != "" {
		detail += " " + f
	}
	return detail
}

// NOTE: the AutoScroll follow-lock veto helpers used to live
// here (wheel + Shift+PageUp/Down swallowed while the switch was
// ON). They are deleted, not disabled: with the switch defaulting
// ON, the veto froze the viewport and made wheel and keyboard
// scrolling silently dead, and the plugin exposes no
// scroll-to-bottom call that could implement "force back to
// bottom on new data" honestly — so the switch itself is gone
// too (see README). Wheel and Shift+PageUp/Down now reach the
// plugin's native scrollback scrolling untouched.
