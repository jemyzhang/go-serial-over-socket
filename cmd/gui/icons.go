package main

import (
	_ "embed"

	"github.com/egoist/mygo/ui"
)

// SVG icon set — Jemy's own icons, 2026-11-06. The mixed text-glyph
// set (⚙ ⌕ ⤓ × │ ─ and the settings-section 🔌 🕘 ☁ ◐) landed
// 2026-11-06 and still looked bad on the real device, so Jemy handed
// over SVG artwork directly: close, plus, download, search, settings,
// serial, history, appearance and WebDAV paths came from him verbatim
// (see /tmp/svg-source-2145.txt — the 21:45 message transcript), the
// two split icons came later as exact SVGs (21:51, both stroke 1.5
// per his 21:53 call). Search and the two Lottie icons (history,
// appearance) are the only reconstructions: search is the Lottie
// circle + handle re-scaled from its 320 viewBox to 24 (stroke 3,
// deliberately heavier than the other strokes — at 16px the 1.5
// strokes read thin on that small geometry), history and appearance
// are the Lottie posters' visible stroke paths only (display:none
// groups, the black background rect and the invisible duplicate SVG
// dropped; masks not needed — the visible result is just the
// outlines), re-scaled 320 -> 24 with the stroke unified to 1.5 to
// match the split icons. Download is the plain-arrow version, not
// the tray variant Jemy also offered. Power and the status dot
// followed on 2026-11-09, also Jemy's artwork: power is his exact
// SVG (24 viewBox, stroke 1.5); the dot is his ring (a 16x16
// circle, r=1, stroke 2, fixed #111827) re-scaled into the 24
// viewBox with the stroke changed to currentColor — the file's
// dead color would mislead, ui.Icon ignores SVG colors and
// paints in the text color anyway. The dot's geometry (r=4.5,
// stroke 9, outer 18/24 of the box) was sized so its rendered
// diameter matches the old ● text glyph at the same FontSize
// (measured headless, old vs new side by side — see the
// power-dot preview): Jemy's numbers scaled proportionally,
// keeping his stroke = 2× radius, which closes the ring's hole
// — his original is a solid disc (stroke 2 on r=1 spans radius
// 0..2), not a hollow ring, so a thinner stroke would have
// opened a hole the artwork never had. A plain proportional
// scale of his 16px box would have stayed tiny; the size fix is
// the outer 18/24, i.e. the ●'s ~0.75em. Record, day and night
// followed the same day, also Jemy's artwork verbatim (fill
// currentColor, 24 viewBox): record is the scroll/log icon for
// the pill's record button (the recording strip's amber dot
// stays the status dot), day is the sun shown in dark mode and
// night the moon shown in light mode — the theme button shows
// the TARGET theme, as the old ●/☼ text did. The icons are parsed once at
// startup (MustParseSVG panics on a bad file — these ship with the
// program, so a broken one is a build-time bug, not a runtime
// condition) and drawn with ui.Icon, which paints the SVG's shapes
// in the current text color: the artwork's own colors are ignored,
// so the same icon works in both themes. What stays a text glyph:
// the find-bar ↑/↓ and the local pane's >_.

//go:embed icons/close.svg
var iconCloseSVG []byte

//go:embed icons/plus.svg
var iconPlusSVG []byte

//go:embed icons/download.svg
var iconDownloadSVG []byte

//go:embed icons/search.svg
var iconSearchSVG []byte

//go:embed icons/settings.svg
var iconSettingsSVG []byte

//go:embed icons/serial.svg
var iconSerialSVG []byte

//go:embed icons/history.svg
var iconHistorySVG []byte

//go:embed icons/appearance.svg
var iconAppearanceSVG []byte

//go:embed icons/webdav.svg
var iconWebDAVSVG []byte

//go:embed icons/split-right.svg
var iconSplitRightSVG []byte

//go:embed icons/split-below.svg
var iconSplitBelowSVG []byte

//go:embed icons/power.svg
var iconPowerSVG []byte

//go:embed icons/dot.svg
var iconDotSVG []byte

//go:embed icons/record.svg
var iconRecordSVG []byte

//go:embed icons/day.svg
var iconDaySVG []byte

//go:embed icons/night.svg
var iconNightSVG []byte

var (
	iconClose      = ui.MustParseSVG(iconCloseSVG)
	iconPlus       = ui.MustParseSVG(iconPlusSVG)
	iconDownload   = ui.MustParseSVG(iconDownloadSVG)
	iconSearch     = ui.MustParseSVG(iconSearchSVG)
	iconSettings   = ui.MustParseSVG(iconSettingsSVG)
	iconSerial     = ui.MustParseSVG(iconSerialSVG)
	iconHistory    = ui.MustParseSVG(iconHistorySVG)
	iconAppearance = ui.MustParseSVG(iconAppearanceSVG)
	iconWebDAV     = ui.MustParseSVG(iconWebDAVSVG)
	iconSplitRight = ui.MustParseSVG(iconSplitRightSVG)
	iconSplitBelow = ui.MustParseSVG(iconSplitBelowSVG)
	iconPower      = ui.MustParseSVG(iconPowerSVG)
	iconDot        = ui.MustParseSVG(iconDotSVG)
	iconRecord     = ui.MustParseSVG(iconRecordSVG)
	iconDay        = ui.MustParseSVG(iconDaySVG)
	iconNight      = ui.MustParseSVG(iconNightSVG)
)

// iconButton is quietButton's SVG sibling: the same plain button
// (transparent background, no border, theme padding and radius,
// hover fill only), but its content is a parsed SVG icon instead
// of a text glyph. It is built on ui.Button with an empty label
// plus an Icon child — exactly how the theme styles a button —
// so callers chain Padding/Height/Tooltip/Clicked the same way
// they do with quietButton. The icon is as tall as the button
// font size (the theme default, 13), so it sits where the glyph
// it replaces sat; do not shrink the font size or the icon
// shrinks with it. An SVG icon's ink is centered in its own
// box, so no vertical nudge is applied — the old ⏻ text glyph
// needed one because its fallback-font ink rode high; the SVG
// power icon replaced it and the nudge went away with the glyph.
// If an icon ever looks off, measure it from a render before
// adding one.
func iconButton(c *ui.Context, key string, s *ui.SVG, p palette, color string) ui.Element {
	btn := ui.Button(c.Key(key), "").Background(ui.RGBA(0, 0, 0, 0)).Border(0, ui.RGBA(0, 0, 0, 0)).TextColor(p.hex(color))
	btn.Children(func() {
		ui.Icon(c, s)
	})
	if btn.Hovered() {
		btn = btn.Background(p.hex(p.hover))
	}
	return btn
}

// flatIconButton is flatButton's SVG sibling: the same flat fill
// (hover color, no border, border color while hovered) with an
// SVG icon instead of a text label. No TextColor is set, so the
// icon takes the theme's default text color, exactly as the
// glyph it replaces did. Only the appearance-theme button uses
// it; the rest of flatButton's callers keep text labels.
func flatIconButton(c *ui.Context, key string, s *ui.SVG, p palette) ui.Element {
	btn := ui.Button(c.Key(key), "").Background(p.hex(p.hover)).Border(0, ui.RGBA(0, 0, 0, 0))
	btn.Children(func() {
		ui.Icon(c, s)
	})
	if btn.Hovered() {
		btn = btn.Background(p.hex(p.border))
	}
	return btn
}

// dotIcon renders the status dot (lamp, record, command-bar,
// port row): the one shared SVG, colored by the caller, at the
// ambient font size — it replaces ui.Text "●" one-for-one at
// every site without resizing its row. FontSize stays unset
// here: callers that need 12px (the recording strip) set it
// themselves, like the Text did.
func dotIcon(c *ui.Context, color ui.Color) ui.Element {
	return ui.Icon(c, iconDot).TextColor(color)
}
