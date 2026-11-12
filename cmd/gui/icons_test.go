package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestIconsParse guards the embedded SVGs: every file in icons/
// must parse (MustParseSVG at package init would panic first,
// but this names the broken file), and every icon must be a
// 24x24 viewBox — the set shares one coordinate space, so a
// stray 320 viewBox (a Lottie paste that skipped the re-scale)
// fails here instead of rendering at the wrong weight.
func TestIconsParse(t *testing.T) {
	files := map[string][]byte{
		"close":       iconCloseSVG,
		"plus":        iconPlusSVG,
		"download":    iconDownloadSVG,
		"search":      iconSearchSVG,
		"settings":    iconSettingsSVG,
		"serial":      iconSerialSVG,
		"history":     iconHistorySVG,
		"appearance":  iconAppearanceSVG,
		"webdav":      iconWebDAVSVG,
		"split-right": iconSplitRightSVG,
		"split-below": iconSplitBelowSVG,
		"power":       iconPowerSVG,
		"dot":         iconDotSVG,
		"record":      iconRecordSVG,
		"day":         iconDaySVG,
		"night":       iconNightSVG,
	}
	for name, data := range files {
		if len(data) == 0 {
			t.Errorf("icons/%s.svg: embedded file is empty", name)
			continue
		}
		s, err := ui.ParseSVG(data)
		if err != nil {
			t.Errorf("icons/%s.svg: ParseSVG: %v", name, err)
			continue
		}
		if w, h := s.Size(); w != 24 || h != 24 {
			t.Errorf("icons/%s.svg: Size = %v x %v, want 24 x 24", name, w, h)
		}
	}
	for name, s := range map[string]*ui.SVG{
		"close":       iconClose,
		"plus":        iconPlus,
		"download":    iconDownload,
		"search":      iconSearch,
		"settings":    iconSettings,
		"serial":      iconSerial,
		"history":     iconHistory,
		"appearance":  iconAppearance,
		"webdav":      iconWebDAV,
		"split-right": iconSplitRight,
		"split-below": iconSplitBelow,
		"power":       iconPower,
		"dot":         iconDot,
		"record":      iconRecord,
		"day":         iconDay,
		"night":       iconNight,
	} {
		if s == nil {
			t.Errorf("icon %s: parsed SVG is nil", name)
		}
	}
}
