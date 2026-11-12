package main

import (
	"testing"

	"github.com/egoist/mygo"
)

func TestWindowOptionsDevToolsDisabled(t *testing.T) {
	opts := (&nativeApp{}).windowOptions()
	if opts.Page.DevTools != mygo.DevToolsDisabled {
		t.Errorf("windowOptions Page.DevTools = %v, want DevToolsDisabled", opts.Page.DevTools)
	}
	if opts.Title != "sos-gui" || opts.Width != 1200 || opts.Height != 750 {
		t.Errorf("windowOptions lost the window geometry: title=%q %dx%d", opts.Title, opts.Width, opts.Height)
	}
	if opts.Content == nil {
		t.Error("windowOptions lost the Content view")
	}
}
