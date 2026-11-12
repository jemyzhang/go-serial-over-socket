package main

import (
	"fmt"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// The find bar (Ghostty-style) floats at the top-right inside a
// terminal pane: a rounded pill with a search icon, an input, a
// "Selected of Total" count, ↑/↓ chevrons and a close button.
// The magnifier and close are Jemy's SVGs (see icons.go); the
// ↑/↓ chevrons stay text glyphs.
// Each pane (serial, local) has its own bar: visible, query and
// search status are stored separately, so the panes do not
// affect each other. The bar is not persisted.

// findPane identifies which terminal pane a find bar belongs to.
const (
	findPaneSerial = iota
	findPaneLocal
)

// findState is one pane's find bar. term is the terminal the bar
// belongs to: when the pane shows another terminal (port switch,
// local terminal replaced), the bar closes (see findBarOverlay).
// UI thread only: OnSearch runs on the main thread, and
// everything else touches it from render and UI event handlers.
type findState struct {
	visible  bool
	query    string
	searched string // query last passed to term.Search ("": none)
	status   terminal.SearchStatus
	term     *terminal.Terminal
	focusIn  bool // focus the input on the next frame
}

func (a *nativeApp) findStateFor(pane int) *findState {
	if pane == findPaneLocal {
		return &a.findLocal
	}
	return &a.findSerial
}

func findPaneName(pane int) string {
	if pane == findPaneLocal {
		return "local"
	}
	return "serial"
}

// findTerm returns the terminal a pane currently shows, or nil.
// It copies the pointer under mu; the caller must not hold mu
// while using it.
func (a *nativeApp) findTerm(pane int) *terminal.Terminal {
	a.mu.Lock()
	defer a.mu.Unlock()
	if pane == findPaneLocal {
		if a.localTerm == nil {
			return nil
		}
		return a.localTerm.term
	}
	if sess, ok := a.terms[a.selected]; ok && a.selected != "" {
		return sess.term
	}
	return nil
}

// openFindBar opens the find bar on the focused pane (the last
// pane to have the keyboard focus; the other pane when the
// focused one has no terminal). Opening an already-open bar
// only focuses its input. The query is kept across closes, and
// the search re-runs on the next frame (searched is cleared).
// UI thread only: called from the frame Shortcut and the
// toolbar button.
func (a *nativeApp) openFindBar() {
	pane := a.findPane
	term := a.findTerm(pane)
	if term == nil {
		pane = 1 - pane
		term = a.findTerm(pane)
		if term == nil {
			return
		}
	}
	st := a.findStateFor(pane)
	st.visible = true
	st.term = term
	st.focusIn = true
	st.searched = ""
	a.invalidate()
}

// hideFindBar ends the pane's search and hides its bar, keeping
// the query for the next open. It does not move the focus; see
// closeFindBar. UI thread only.
func (a *nativeApp) hideFindBar(pane int) {
	st := a.findStateFor(pane)
	if !st.visible {
		return
	}
	if st.term != nil {
		st.term.EndSearch()
	}
	st.visible = false
	st.searched = ""
	a.invalidate()
}

// closeFindBar hides the pane's bar and returns the focus to its
// terminal. UI thread only: called from the bar's × button and
// its input's Escape.
func (a *nativeApp) closeFindBar(pane int) {
	if !a.findStateFor(pane).visible {
		return
	}
	a.hideFindBar(pane)
	if pane == findPaneLocal {
		a.localFocusPending = true
	} else {
		a.termFocusPending = true
	}
}

// findEnter moves the selection: Enter toward older output,
// Shift+Enter toward newer. It is the OnTextKey adapter's
// target, so tests can drive it without simulating keys.
func (a *nativeApp) findEnter(pane int, shift bool) {
	st := a.findStateFor(pane)
	if st.term == nil {
		return
	}
	if shift {
		st.term.SearchPrevious()
	} else {
		st.term.SearchNext()
	}
}

// recordFindStatus stores a search status for the pane's bar,
// ignoring it when the bar is hidden or the status belongs to a
// query the bar no longer shows. Called from OnSearch, which
// runs on the main thread.
func (a *nativeApp) recordFindStatus(pane int, s terminal.SearchStatus) {
	st := a.findStateFor(pane)
	if !st.visible || (s.Text != "" && s.Text != st.query) {
		return
	}
	st.status = s
	a.invalidate()
}

// findBarOverlay builds the pane's find bar, floating at the
// top-right inside the terminal pane (ui.Overlay anchored to
// the terminal view). It is called from the pane views right
// after the terminal view is built, so the anchor is in scope.
// term is the pane's current terminal (from the snapshot);
// when the bar belongs to another terminal (port switch, local
// terminal replaced), the bar closes instead of building: the
// close runs here, on the UI thread, because selectPort can
// also run on the WS goroutine (auto-reselect), where the
// UI-thread-only find state must not be touched.
func (a *nativeApp) findBarOverlay(c *ui.Context, p palette, pane int, term *terminal.Terminal, anchor ui.Element) {
	st := a.findStateFor(pane)
	if !st.visible {
		return
	}
	if st.term != term {
		// The pane shows another terminal now: end the old
		// search and hide the bar (it belongs to the terminal
		// it was opened on).
		if st.term != nil {
			st.term.EndSearch()
		}
		st.visible = false
		st.searched = ""
		return
	}
	// Drive the search from the input: a changed query searches
	// (empty ends it); a terminal that lost its search (it was
	// recreated after the port came back) runs it again.
	if st.query != st.searched {
		if st.query == "" {
			term.EndSearch()
		} else {
			term.Search(st.query)
		}
		st.searched = st.query
	} else if st.query != "" && term.SearchStatus().Text == "" {
		term.Search(st.query)
	}
	ui.Overlay(c, func() {
		// The 8px gap from the pane's right edge is a right
		// MARGIN, not a .Right(8) inset — and the distinction
		// matters. AttachTo's alongTarget flips the bar to the
		// anchor's other side when its natural spot overflows
		// the window past the 4px windowMargin (mygo v0.7.4
		// ui/layout.go): .Right(8) is an inset applied AFTER
		// the flip check, so with the margin at 0 the natural
		// spot sat flush on the window edge and flipped —
		// in a right split the local bar landed at the pane's
		// left edge, and in a below split both bars did.
		// A margin feeds the flip math (off = left - right =
		// -8), so the natural spot already stands 8px off the
		// edge and never flips. Do not "simplify" this back
		// to .Right(8): TestFindBarSplitRightStaysTopRight
		// guards it.
		ui.Row(c.Key("find-bar-"+findPaneName(pane))).Gap(6).Padding(4, 10).Radius(16).Background(p.hex(p.elevated)).Border(1, p.hex(p.border)).AttachTo(anchor, ui.AnchorTopRight, ui.AnchorTopRight).Top(8).Margin(0, 8, 0, 0).Children(func() {
			ui.Icon(c, iconSearch).FontSize(12).TextColor(p.hex(p.muted))
			in := ui.TextInput(c.Key("find-input-"+findPaneName(pane)), &st.query).Width(180).Placeholder("Find in scrollback")
			if st.focusIn {
				in.Focus()
				st.focusIn = false
			}
			// Enter moves the selection (Shift+Enter the other
			// way). OnTextKey is the only way to tell them
			// apart: Submitted() reports Enter but not its
			// modifiers (there is no public accessor for them),
			// and Shortcut(0, KeyEnter) never fires (the
			// editor consumes Enter).
			in.OnTextKey(func(k ui.TextKey) bool {
				if k.Key != ui.KeyEnter || (k.Mods != 0 && k.Mods != ui.Shift) {
					return false
				}
				a.findEnter(pane, k.Mods == ui.Shift)
				return true
			})
			if st.query != "" {
				if st.status.Total == 0 {
					ui.Text(c, "No matches").FontSize(12).TextColor(p.hex(p.muted))
				} else {
					ui.Text(c, fmt.Sprintf("%d of %d", st.status.Selected, st.status.Total)).FontSize(12).TextColor(p.hex(p.muted))
				}
			}
			// ↑ goes toward older output (up, into the
			// scrollback), ↓ toward newer output: the plugin's
			// SearchNext / SearchPrevious, not the other way.
			if quietButton(c, "find-prev-"+findPaneName(pane), "↑", p, p.muted).Tooltip("上一个匹配").Clicked() {
				term.SearchNext()
			}
			if quietButton(c, "find-next-"+findPaneName(pane), "↓", p, p.muted).Tooltip("下一个匹配").Clicked() {
				term.SearchPrevious()
			}
			if iconButton(c, "find-close-"+findPaneName(pane), iconClose, p, p.muted).Tooltip("关闭查找").Clicked() {
				a.closeFindBar(pane)
			}
			// Escape closes the bar and returns the focus to
			// the terminal. The input's own Shortcut, not the
			// overlay's: the overlay is built before the
			// command bar, so an OverlayShortcut Escape would
			// consume the key before the command input's own
			// Escape handler (close the dropdown) ever sees
			// it; and when the terminal has the focus, Escape
			// is the program's (vim) anyway.
			if in.Shortcut(0, ui.KeyEscape) {
				a.closeFindBar(pane)
			}
		})
	})
}
