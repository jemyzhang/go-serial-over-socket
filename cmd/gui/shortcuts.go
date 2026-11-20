package main

import (
	"bytes"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// The window-wide keyboard shortcuts (render registers them in
// render(); whoever queries a delivered key first consumes it, so
// the render layer runs before the command bar and the panels
// are built, and a shortcut never leaks into a terminal or the
// serial line — the same pattern as Ctrl+Shift+F).

// toggleLocalTerminal opens the local terminal when closed and
// closes it when open (Ctrl+Shift+Enter): the same open/close as
// the toolbar + button, on the saved split layout (right/below),
// with the same focus handoff (open focuses the local pane,
// close the serial one). UI thread only.
func (a *nativeApp) toggleLocalTerminal() {
	a.mu.Lock()
	open := a.localTerm != nil
	a.mu.Unlock()
	if open {
		a.closeLocalTerminal()
	} else {
		a.openLocalTerminal(false)
	}
}

// toggleZoom grows the focused terminal pane to fill the
// terminal area (Ctrl+Shift+L), hiding the other pane, and
// restores the split on the second press. Only when a split is
// actually showing: with a single pane there is nothing to
// grow, and the key is a silent no-op. The settings panel is
// untouched — only the terminal area changes — and the size
// change reports to the server through the existing resize path
// (the plugin tells portConn on every paint-driven change, as a
// split open/close does). Which pane is zoomed is the focused
// one (a.findPane; nothing focused yet means the serial pane).
// UI thread only.
func (a *nativeApp) toggleZoom() {
	a.mu.Lock()
	hasLocal := a.localTerm != nil
	_, hasSerial := a.terms[a.selected]
	hasSerial = hasSerial && a.selected != ""
	pane := a.findPane
	a.mu.Unlock()
	if !hasLocal || !hasSerial {
		return
	}
	if a.paneZoomed {
		a.paneZoomed = false
		if a.zoomedPane == findPaneLocal {
			a.localFocusPending = true
		} else {
			a.termFocusPending = true
		}
		a.invalidate()
		return
	}
	if pane != findPaneLocal {
		pane = findPaneSerial
	}
	a.zoomedPane = pane
	a.paneZoomed = true
	if pane == findPaneLocal {
		a.localFocusPending = true
	} else {
		a.termFocusPending = true
	}
	a.invalidate()
}

// focusPane moves the keyboard focus to the given terminal
// pane (Ctrl+Shift+[ for the first pane, Ctrl+Shift+] for
// the second): left / right in the side-by-side layout,
// above / below in the stacked layout. Only the pending
// flags move — the pane views consume them on the next
// render, the same mechanism as opening the local terminal
// or restoring a zoom. Already on the target pane, or with
// no such pane (a single pane, or no serial port at all),
// the key is a silent no-op. A zoomed split restores first
// when the target is the hidden pane: focus cannot land on
// a pane that is not rendered. UI thread only.
func (a *nativeApp) focusPane(pane int) {
	a.mu.Lock()
	hasLocal := a.localTerm != nil
	_, hasSerial := a.terms[a.selected]
	hasSerial = hasSerial && a.selected != ""
	cur := a.findPane
	zoomed := a.paneZoomed
	zoomedPane := a.zoomedPane
	a.mu.Unlock()
	if pane == findPaneLocal {
		if !hasLocal {
			return
		}
	} else {
		if !hasSerial {
			return
		}
	}
	if zoomed && zoomedPane != pane {
		a.paneZoomed = false
	} else if cur == pane {
		return
	}
	if pane == findPaneLocal {
		a.localFocusPending = true
	} else {
		a.termFocusPending = true
	}
	a.invalidate()
}

// clearScreenSeq is the terminfo clear sequence (ESC[H ESC[2J):
// erase the whole visible screen and home the cursor. It is
// display-only — fed to the terminal, never sent to the program —
// so nothing reaches the serial device or the local shell.
const clearScreenSeq = "\x1b[H\x1b[2J"

// termAltScreen reports whether the terminal shows a full-screen
// program (alternate screen). The plugin exposes no accessor,
// so it reads the VT dump Snapshot returns, which carries the
// modes the program set: DECSET 1049/1047/47 set means alt
// screen (probe-verified headless: 1049h present after 1049h,
// gone after 1049l). Snapshot dumps the scrollback too, so this
// costs one dump per press — fine for a human-rare key.
func termAltScreen(term *terminal.Terminal) bool {
	snap := term.Snapshot()
	return bytes.Contains(snap, []byte("\x1b[?1049h")) ||
		bytes.Contains(snap, []byte("\x1b[?1047h")) ||
		bytes.Contains(snap, []byte("\x1b[?47h"))
}

// clearFocusedPane clears the focused terminal pane's screen
// (Ctrl+Shift+K), like the shell's clear: the screen goes blank
// and the scrollback stays. That is why it feeds the clear
// sequence to the display instead of calling ClearScreen, which
// also erases the scrollback (ESC[3J) and deletes the rows above
// the cursor. A full-screen program's screen is left alone, as
// ClearScreen/Ghostty do — its program owns that screen, and an
// external clear would blank it without the program knowing.
// One wart the plugin cannot fix: there is no public
// scroll-to-bottom, so a pane scrolled up in its scrollback
// stays scrolled up after the clear until new output or a wheel
// scroll brings it back. UI thread only.
func (a *nativeApp) clearFocusedPane() {
	term := a.findTerm(a.findPane)
	if term == nil || termAltScreen(term) {
		return
	}
	term.Feed([]byte(clearScreenSeq))
}

// helpRow is one row of the shortcut help overlay.
type helpRow struct {
	keys string
	desc string
}

// helpRows is the whole app-level shortcut list, in the order the
// overlay shows it. README's shortcut section lists the same
// rows in the same words — keep the two in sync.
var helpRows = []helpRow{
	{"Ctrl+Shift+Enter", "开关本地终端"},
	{"Ctrl+Shift+L", "放大 / 恢复当前终端"},
	{"Ctrl+Shift+K", "清空当前终端屏幕"},
	{"Ctrl+Shift+?", "打开快捷键帮助"},
	{"Ctrl+Shift+[ / ]", "焦点移到左 / 右边的终端"},
	{"Ctrl+,", "打开 / 关闭设置"},
	{"Ctrl+Shift+←/→", "切换到上一个 / 下一个串口"},
	{"Ctrl+Shift+F", "在当前终端中查找"},
	{"Ctrl+S", "保存当前串口输出"},
	{"Ctrl+Shift+C", "复制选中内容"},
	{"Esc", "关闭设置面板 / 查找栏 / 帮助"},
}

// openHelp opens the shortcut help overlay (Ctrl+Shift+?).
// UI thread only.
func (a *nativeApp) openHelp() {
	if a.helpOpen {
		return
	}
	a.helpOpen = true
	a.helpFocusPending = true
	a.invalidate()
}

// closeHelp closes the shortcut help overlay. UI thread only.
func (a *nativeApp) closeHelp() {
	if !a.helpOpen {
		return
	}
	a.helpOpen = false
	a.invalidate()
}

// helpOverlay renders the shortcut help as a centered card over
// a dimmed window (ui.Overlay): not a full-screen page, not a
// dock panel. The backdrop takes the focus when the overlay
// opens and swallows every key (HandleInput): any key closes the
// overlay and never reaches the terminal or the command bar —
// the opening chord itself cannot close it again, because it was
// already consumed by the frame Shortcut before the overlay was
// built. Clicks do not close it: the backdrop is only a focus
// and key sink (it is Focusable, so a click on it keeps the
// focus here instead of dropping it to the terminal behind).
// The focus returns to the element that had it when the overlay
// goes away (the overlay's own restore).
func (a *nativeApp) helpOverlay(c *ui.Context, p palette) {
	ui.Overlay(c, func() {
		back := ui.Box(c.Key("help-backdrop")).Absolute().Left(0).Top(0).Right(0).Bottom(0).Center().Focusable().Background(ui.RGBA(0, 0, 0, 0.4))
		if a.helpFocusPending {
			a.helpFocusPending = false
			back.Focus()
		}
		back.HandleInput(func(ev ui.InputEvent) bool {
			if ev.Kind == ui.InputKeyDown {
				a.closeHelp()
				return true
			}
			return false
		})
		back.Children(func() {
			ui.Column(c.Key("help-card")).Padding(20, 24).Gap(10).Radius(12).Background(p.hex(p.elevated)).Shadow(0, 10, 30, 0, ui.RGBA(0, 0, 0, 0.3)).Children(func() {
				ui.Text(c, "快捷键").FontSize(16).Bold()
				for _, row := range helpRows {
					r := row
					ui.Row(c).Gap(16).Children(func() {
						ui.Text(c, r.keys).FontSize(13).Bold().Width(150)
						ui.Text(c, r.desc).FontSize(13).TextColor(p.hex(p.muted))
					})
				}
				ui.Text(c, "按任意键关闭").FontSize(12).TextColor(p.hex(p.muted))
			})
		})
	})
}
