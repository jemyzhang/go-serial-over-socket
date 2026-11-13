package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// snapshot is the view's read-only copy of app state, taken once per
// frame so the view never reads the live map (mutated by WS
// callbacks) directly.
type snapshot struct {
	p        palette
	ports    map[string]PortInfo
	portIDs  []string
	selected string
	terms    map[string]*termSession
	// localTerm is the open split-pane local terminal, if any
	// (copied under mu like terms).
	localTerm *localTerminal
	localMode bool
	settings  Settings
	fontFams  []string
	wsConn    bool
	status    string
	history   []HistoryEntry
	wdPassSet bool
	logState  map[string]portLogState
	breath    float32 // toolbar dot flash target, stepped (see startBreathing)
	// Cloud backup list state (sync tab), copied under mu like
	// the rest: the fetch goroutine writes the app fields, the
	// render thread must only read this snapshot, never the
	// fields directly (or -race catches the pair).
	wdBackups     []webdavBackupFile
	wdListLoading bool
	wdListLoaded  bool
	wdListErr     string
	wdDelArmed    string
}

func (a *nativeApp) snapshot() snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	ports := make(map[string]PortInfo, len(a.ports))
	for id, info := range a.ports {
		ports[id] = info
	}
	terms := make(map[string]*termSession, len(a.terms))
	for id, sess := range a.terms {
		terms[id] = sess
	}
	snap := snapshot{
		ports:         ports,
		portIDs:       append([]string(nil), a.portIDs...),
		selected:      a.selected,
		terms:         terms,
		localTerm:     a.localTerm,
		localMode:     a.embedded != nil,
		settings:      a.settings,
		fontFams:      a.fontFams,
		wsConn:        a.wsConnected,
		status:        a.status,
		history:       append([]HistoryEntry(nil), a.history...),
		wdPassSet:     a.wdPassSet,
		breath:        a.breathTarget,
		wdBackups:     append([]webdavBackupFile(nil), a.wdBackups...),
		wdListLoading: a.wdListLoading,
		wdListLoaded:  a.wdListLoaded,
		wdListErr:     a.wdListErr,
		wdDelArmed:    a.wdDelArmed,
	}
	// Recording indicators come from local files, not the
	// server. snapshot holds the app mutex here, so this is
	// the one place that nests app mutex -> recMu; no path
	// takes them in the reverse order (see recording.go).
	a.recMu.Lock()
	if len(a.recordings) > 0 {
		snap.logState = make(map[string]portLogState, len(a.recordings))
		for id, rec := range a.recordings {
			snap.logState[id] = portLogState{filename: rec.path, size: rec.size}
		}
	}
	a.recMu.Unlock()
	return snap
}

func formatBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// render draws the Tern-style layout: a Row with the main
// column (slim toolbar + terminal + command bar) taking the
// remaining width and, when open, the fixed 400px settings panel
// docked on the right — the terminal narrows instead of being
// covered, and the panel's size never changes with its content.
// Add-port is the only modal left; history editing is inline.
func (a *nativeApp) render(c *ui.Context) {
	// Ctrl+, toggles settings wherever the focus is, unless a
	// focused element handled the key first; the terminal's OnKey
	// covers the terminal-focused case (see ensureTerm).
	if c.Shortcut(ui.Ctrl, ui.KeyComma) {
		a.toggleSettings()
	}
	// Ctrl+Shift+Left/Right switches the visible terminal to the
	// adjacent port wherever the focus is, unless a focused
	// element handled the key first; the terminal's OnKey covers
	// the terminal-focused case (see terminalKeyHandled). Whoever
	// queries a delivered key first consumes it, so the two
	// levels never both fire.
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyLeft) {
		a.selectAdjacentPort(-1)
	}
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyRight) {
		a.selectAdjacentPort(1)
	}
	// Ctrl+Shift+F opens the find bar on the focused terminal
	// pane (Ghostty's Linux find key; Ctrl+F would steal the
	// serial ^F and the shell's forward-char). The Shortcut
	// claims the key window-wide, so the terminal never sees it:
	// the plugin binds no find key, and an unclaimed key would
	// reach the serial line or the shell as text.
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyF) {
		a.openFindBar()
	}
	snap := a.snapshot()
	// The widget theme defaults to the OS appearance, but the app
	// chrome follows settings.Dark — on a dark desktop with the
	// app in light mode, default-styled widgets (plain Text,
	// Button, TextInput, Select, NumberInput) would render
	// dark-theme colors on light backgrounds. Set the widget
	// theme explicitly every frame, before any widget is created:
	// SetTheme only affects the root and widgets created after
	// the call. Dark mode is pixel-identical by construction
	// (the default was already DarkTheme's values). widgetTheme
	// also sets the tooltip inverse colors (see theme.go).
	if snap.settings.Dark {
		c.SetTheme(widgetTheme(true))
	} else {
		c.SetTheme(widgetTheme(false))
	}
	// Esc closes the panel — EXCEPT when the command bar's
	// dropdown is open (then Esc closes only the dropdown) or an
	// inline history edit is active (then Esc cancels the edit):
	// whoever queries a delivered key first consumes it, and this
	// frame-level check runs before the command bar and panel
	// are built, so it must yield in those two cases or the
	// element-level Esc handlers below would never see the key.
	cmdDropdown := len(historyMatches(a.cmdInput, snap.history)) > 0 && strings.TrimSpace(a.cmdInput) != "" && !a.cmdHistHide
	if a.settingsOpen && !cmdDropdown && a.histEditID == "" && c.Shortcut(0, ui.KeyEscape) {
		a.cancelSettings()
	}
	p := darkPalette
	if !snap.settings.Dark {
		p = lightPalette
	}
	wasOpen := a.settingsOpen
	ui.Row(c).Fill().Background(p.hex(p.termBg)).Children(func() {
		ui.Column(c).Grow(1).FillHeight().Children(func() {
			a.toolbarView(c, snap, p)
			a.terminalView(c, snap, p)
		})
		if a.settingsOpen {
			a.settingsPanel(c, snap, p)
		}
	})

	// The add-port modal overlays the window (ui.Modal dismisses
	// on outside click / Escape, mirroring the WebUI modal-overlay
	// behavior). History editing is inline in its row now, so this
	// is the only modal left.
	ui.Modal(c, &a.addOpen, func() {
		ui.Column(c).Padding(16).Gap(8).Children(func() {
			ui.Text(c, "添加串口设备").FontSize(15).Bold()
			ui.Text(c, "端口路径 / 名称").FontSize(12).TextColor(p.hex(p.muted))
			ui.TextInput(c, &a.addPath).Fill()
			ui.Text(c, "别名（可选）").FontSize(12).TextColor(p.hex(p.muted))
			ui.TextInput(c, &a.addAlias).Fill()
			ui.Row(c).Gap(8).Children(func() {
				if flatButton(c, "add-cancel", "取消", p).Clicked() {
					a.addOpen = false
				}
				if flatButton(c, "add-confirm", "确认添加", p).Clicked() {
					a.submitAddPort()
				}
			})
		})
	})
	// Any panel close path (Escape, ×) drops un-saved staging
	// edits across all sections.
	if wasOpen && !a.settingsOpen {
		a.reloadPanelStaging()
	}
}

// quietButton is a plain-text toolbar button: muted text on a
// transparent background, with the hover background only while
// the pointer is over it. Hovered() reflects the previous frame's
// hit test, which is the immediate-mode way to do hover styling;
// on the first frame (or headless) it is simply false. Borderless:
// the theme button's outline is killed explicitly, or every
// toolbar button would still draw its box.
func quietButton(c *ui.Context, key, label string, p palette, color string) ui.Element {
	btn := ui.Button(c.Key(key), label).Background(ui.RGBA(0, 0, 0, 0)).Border(0, ui.RGBA(0, 0, 0, 0)).TextColor(p.hex(color))
	if btn.Hovered() {
		btn = btn.Background(p.hex(p.hover))
	}
	return btn
}

// pillButtonHeight is the explicit height, in DIPs, of all three pill
// buttons. mygo's Row() defaults its cross-axis alignment to Center
// (not Stretch), so without it each button keeps its own natural
// height — the old ⏻ text glyph's fallback-font line box was ~28
// DIPs tall while the dot's was ~20, which is why hovering ⏻
// showed a taller fill box than hovering the dot. 20 matches the
// natural height (14px theme font line ~16-17 plus 2px/2px padding)
// within a DIP; the buttonRow centers all three, so equal height
// means coincident boxes, not just equal size. The ⏻ is an SVG
// now, so the tall-box cause is gone; the explicit height stays —
// it is what keeps the three hover fills coincident.
const pillButtonHeight = 20

// connectIconButton builds the pill's power button: the SVG power
// icon (see icons.go) on the same plain button iconButton builds —
// transparent background, no border, theme-radius hover fill, pill
// Padding(2, 4) and pillButtonHeight. The old ⏻ text glyph needed
// ButtonBase plus a powerGlyphNudgeY Translate because its
// fallback-font ink rode high and its line box was taller than
// the dot's; the SVG's ink is centered in its own box, so
// iconButton (ui.Button) does the job and the nudge went away
// with the glyph.
func connectIconButton(c *ui.Context, key string, p palette, color, tip string) ui.Element {
	return iconButton(c, key, iconPower, p, color).Padding(2, 4).Height(pillButtonHeight).Tooltip(tip)
}

// flatButton is the app-wide panel/modal button: flat fill, no
// border, darker on hover (same Hovered() pattern as quietButton;
// the theme button's styleFn bails out once the background is
// overridden, so the hover fill here is the only hover effect).
// Native dialogs and system menus are exempt — they are not app
// buttons.
func flatButton(c *ui.Context, key, label string, p palette) ui.Element {
	btn := ui.Button(c.Key(key), label).Background(p.hex(p.hover)).Border(0, ui.RGBA(0, 0, 0, 0))
	if btn.Hovered() {
		btn = btn.Background(p.hex(p.border))
	}
	return btn
}

// toolbarView renders the single slim (46px) toolbar: every port
// as one pill tab on the left, icon actions on the right. The
// pills are NOT buttons: a pill is a plain Row (small 10px
// radius, no border, no boxy fill) whose Clicked comes from the
// generic Element API — the previous flat-button strip
// with its visible scrollbar was rejected for looking like a
// row of buttons. Each pill holds, in order, the lamp dot +
// the name (alias when set, else the port id) + the short
// config (baudrate only) + a thin divider + three small icon
// buttons (scheme A): power connect/disconnect, record,
// settings icon. A click on a button performs only that button's
// action, never switches the terminal; a click on the pill
// body switches it. There is no right-click menu on pills —
// its items moved into the buttons. The full detail (path ·
// baud frame · vendor) lives in the pill's native hover
// tooltip, and each button has its own tip. The lamp's
// color IS the state (green = connected and still, amber =
// connecting, red = disconnected or error); flashing means
// recording and nothing else — a connected port that records
// flashes green on the ticker-flipped snapshot target (see
// startBreathing), every other lamp is static.
// The selected pill gets bright text on a light fill, the rest
// stay muted plain text.
// There is deliberately NO scroll container here, so no
// scrollbar can ever appear: mygo v0.4.8 has no
// hide-scrollbar API, so many ports compress instead — the
// strip and each pill shrink, and both texts are SingleLine
// with an ellipsis, so the row always fits one line.
func (a *nativeApp) toolbarView(c *ui.Context, snap snapshot, p palette) {
	ui.Column(c).Children(func() {
		ui.Row(c).Height(46).Padding(0, 18).Gap(10).Background(p.hex(p.termBg)).Children(func() {
			if len(snap.portIDs) > 0 {
				ui.Row(c).Gap(4).Shrink(1).MinWidth(0).Children(func() {
					for _, id := range snap.portIDs {
						pid := id
						info := snap.ports[pid]
						pill := ui.Row(c.Key("toolbar-pill:"+pid)).Radius(10).Padding(4, 8).Gap(6).Shrink(1).Children(func() {
							// Lamp: color is the state, flash
							// means recording only. A
							// recording port's lamp takes
							// the ticker-flipped snapshot
							// target (1.0 or 0.45) directly
							// — a step flash on a 1.8s
							// cycle, not a smooth animation
							// (a smooth Animate interpolation
							// used to live here, but its
							// duration matched the ticker
							// period, so the animation never
							// settled and mygo kept redrawing
							// the whole window at display
							// refresh rate, driving CPU up
							// with window size; the stepped
							// version redraws once per 900ms
							// tick instead). Every other lamp
							// is static at full opacity.
							_, recording := snap.logState[pid]
							dotEl := dotIcon(c, lampDotColor(p, info, recording, snap.breath))
							_ = dotEl
							texts := portPillTexts(info)
							for i, txt := range texts {
								if i == 0 {
									nameColor := p.muted
									if pid == snap.selected {
										nameColor = p.text
									}
									ui.Text(c, txt).FontSize(12).SingleLine().Ellipsis("…").TextColor(p.hex(nameColor))
								} else {
									ui.Text(c, txt).FontSize(11).SingleLine().Ellipsis("…").TextColor(p.hex(p.muted))
								}
							}
							// Scheme A button zone, after a thin
							// divider: power connect/disconnect,
							// dot record, settings icon. Each is
							// its own clickable element, so a
							// click on a button performs only
							// that button's action: mygo
							// delivers a click to the deepest
							// interactive element, so the
							// pill's Clicked below stays false
							// for button clicks (the proven
							// record-dot pattern). Disabled
							// states are action-gated, not
							// native-Disabled — see
							// pillConnectButton. Each button
							// has its own tip, which wins
							// over the pill's tip (innermost
							// tip shows).
							ui.Row(c).Width(1).Height(18).Background(p.hex(p.border))
							// Tight button group: the three icon
							// buttons sit in their own Row at
							// Gap 2 with 2px/4px padding, so the
							// icons sit ~10px apart instead of
							// ~22px, and all three share the
							// explicit pillButtonHeight (mygo
							// Rows center their children across
							// the axis, so natural heights would
							// leave the hover fills uneven).
							// The pill info zone keeps
							// its Gap 6, and quietButton itself
							// is untouched (other quietButtons
							// keep the theme default padding).
							ui.Row(c).Gap(2).Children(func() {
								connColor, connTip, connAction, connDisabled := pillConnectButton(p, info)
								if connectIconButton(c, "toolbar-connect:"+pid, p, connColor, connTip).Clicked() && !connDisabled {
									switch connAction {
									case "disconnect":
										a.disconnectPort(pid)
									case "connect":
										a.connectPort(pid)
									}
								}
								recColor, recTip, recDisabled := pillRecordButton(p, info, recording)
								if iconButton(c, "toolbar-record:"+pid, iconRecord, p, recColor).Padding(2, 4).Height(pillButtonHeight).Tooltip(recTip).Clicked() && !recDisabled {
									a.toggleLoggingFor(pid)
								}
								if iconButton(c, "toolbar-settings:"+pid, iconSettings, p, p.text).Padding(2, 4).Height(pillButtonHeight).Tooltip("设置").Clicked() {
									a.openPortSettings(pid)
								}
							})
						})
						if pid == snap.selected {
							pill = pill.Background(p.hex(p.active))
						} else {
							pill = pill.Background(ui.RGBA(0, 0, 0, 0))
							if pill.Hovered() {
								pill = pill.Background(p.hex(p.hover))
							}
						}
						pill = pill.Tooltip(portTooltip(info))
						// No right-click menu on pills anymore:
						// its connect/disconnect and settings
						// items moved into the button zone
						// above. Switching is the left click
						// on the pill body (and the panel's
						// port rows). The terminal view's own
						// native right-click menu is untouched
						// — only the pill menu is gone.
						if pill.Clicked() {
							a.selectPort(pid)
							// Focus the newly shown terminal:
							// its AutoFocus fires on appearance
							// only, and a pill switch does not
							// reliably re-trigger it, so ask
							// explicitly (consumed by the
							// terminal section below). Hide the
							// command dropdown too, or the input's
							// per-frame Focus() would yank focus
							// right back while it is open; it
							// reappears on the next input edit.
							// Pill buttons never reach here (a
							// button click leaves Clicked false),
							// so the settings button keeps the panel's focus rules.
							a.termFocusPending = true
							a.cmdHistHide = true
						}
					}
				})
			} else {
				dotIcon(c, p.hex(p.dim))
				if quietButton(c, "toolbar-port", "未选择串口", p, p.muted).Clicked() {
					a.openSettingsSection(settingsTabPorts)
				}
			}
			// "+" opens the single global local terminal beside
			// the selected serial terminal (browser-tab style).
			// It lives in the toolbar row itself, not in the
			// pill row, so it is always there — including with
			// no serial ports at all. Greyed while the split
			// is open — v1 allows one; openLocalTerminal is a
			// no-op in that case too.
			plusColor := p.muted
			if snap.localTerm != nil {
				plusColor = p.dim
			}
			if iconButton(c, "toolbar-local-term", iconPlus, p, plusColor).Padding(2, 4).Height(pillButtonHeight).Disabled(snap.localTerm != nil).Tooltip("新建本地终端").Clicked() {
				a.openLocalTerminal(false)
			}
			ui.Spacer(c)
			// Toolbar actions, final set: save output /
			// gear, icon-only with native hover tooltips
			// (like the Web UI's icon buttons). Recording
			// no longer lives here: each pill carries its
			// own record button, so ports record independently.
			// No clear button (the terminal's native
			// right-click menu already clears), no
			// connect/disconnect button (each pill's power
			// button and the panel's port rows own that).
			// The icons are Jemy's SVGs (see icons.go),
			// drawn in the text color.
			if iconButton(c, "toolbar-find", iconSearch, p, p.muted).Tooltip("查找").Clicked() {
				a.openFindBar()
			}
			if iconButton(c, "save-output", iconDownload, p, p.muted).Tooltip("保存输出").Clicked() {
				a.saveOutputFor(snap.selected)
			}
			if iconButton(c, "open-settings", iconSettings, p, p.muted).Clicked() {
				a.openSettings()
			}
		})
		ui.Row(c).Height(1).FillWidth().Background(p.hex(p.border))
	})
}

// settingsPanel renders the Tern-style settings side panel: fixed
// 400px wide, full height, docked right of the terminal. Its size
// never changes with its content — the body scrolls internally,
// the header/search/icons stay fixed. There is NO footer save
// bar: every section saves where it lives (its own Save button,
// or immediately), and closing the panel or switching categories
// reloads un-saved staging away. Category icons filter the
// sections (click the active one again to show all); the search
// box filters by section number, label, title, and description.
// Section numbers are fixed per category, so filtering never
// renumbers them.
func (a *nativeApp) settingsPanel(c *ui.Context, snap snapshot, p palette) {
	ui.Column(c).Width(400).FillHeight().Background(p.hex(p.termBg)).Children(func() {
		ui.Row(c).Height(52).Padding(0, 20).Gap(10).Children(func() {
			ui.Row(c).Gap(6).Children(func() {
				ui.Icon(c, iconSettings).FontSize(15)
				ui.Text(c, "Settings").FontSize(15).Bold()
			})
			ui.Spacer(c)
			if iconButton(c, "panel-close", iconClose, p, p.muted).Clicked() {
				a.cancelSettings()
			}
		})
		ui.Row(c).Padding(0, 20).Children(func() {
			ui.TextInput(c, &a.panelSearch).Fill().Radius(10).Background(p.hex(p.hover))
		})
		ui.Row(c).Padding(14, 20, 0, 20).Gap(4).Children(func() {
			for _, s := range settingsSections {
				sec := s
				btn := iconButton(c, "panel-section:"+sec.id, sec.icon, p, p.muted)
				if a.panelSection == sec.id {
					btn = btn.Background(p.hex(p.hover)).TextColor(p.hex(p.text))
				}
				if btn.Clicked() {
					a.mu.Lock()
					if a.panelSection == sec.id {
						a.panelSection = ""
					} else {
						a.panelSection = sec.id
					}
					a.mu.Unlock()
					// Switching categories drops un-saved edits
					// in the section left behind.
					a.reloadPanelStaging()
					a.invalidate()
				}
			}
		})
		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).Gap(20).Padding(18, 20, 20, 20).Children(func() {
				sections := visiblePanelSections(a.panelSearch, a.panelSection)
				if len(sections) == 0 {
					ui.Text(c, "没有匹配的设置").FontSize(13).TextColor(p.hex(p.muted))
					return
				}
				for _, sec := range sections {
					a.settingsSection(c, snap, p, sec)
				}
			})
		})
	})
}

// settingsSection renders one numbered panel section: the fixed
// number + label, the big title, the one-line description, then
// the section's content renderer (ports/history/sync/appearance).
func (a *nativeApp) settingsSection(c *ui.Context, snap snapshot, p palette, sec sectionDef) {
	ui.Column(c).Gap(6).Children(func() {
		ui.Text(c, sec.num+" · "+sec.label).FontSize(12).TextColor(p.hex(p.section))
		ui.Text(c, sec.title).FontSize(20).Bold()
		ui.Text(c, sec.desc).FontSize(12).TextColor(p.hex(p.muted))
		switch sec.id {
		case settingsTabHistory:
			a.historyTab(c, snap, p)
		case settingsTabSync:
			a.syncTab(c, snap, p)
		case settingsTabAppearance:
			a.appearanceTab(c, snap, p)
		default:
			a.portsTab(c, snap, p)
		}
	})
}

// terminalView renders the selected port's resident terminal,
// the status line, and the command bar footer.
func (a *nativeApp) terminalView(c *ui.Context, snap snapshot, p palette) {
	ui.Column(c).Fill().Background(p.hex(p.termBg)).Children(func() {
		// Consume pending focus requests before choosing the
		// branch: a missing terminal drops its request instead
		// of letting it yank focus on a later switch.
		focusTerm := a.termFocusPending
		a.termFocusPending = false
		focusLocal := a.localFocusPending
		a.localFocusPending = false
		if snap.localTerm != nil {
			// Split open: the global local terminal is
			// independent of the serial ports, so it stays
			// visible even when the selected port is gone
			// (unplugged) — full-width then, split otherwise.
			if sess, ok := snap.terms[snap.selected]; ok && snap.selected != "" {
				// Split: serial first, local second, on the
				// framework's draggable Split (SplitPercent
				// side by side, SplitVerticalPercent stacked).
				// The divider is the framework's own 1 DIP
				// line: drag it to resize, or focus it and use
				// the arrow keys. a.splitPercent is bound live,
				// so dragging writes here directly. A zero
				// value (e.g. a test-built app) reads as 50.
				if a.splitPercent <= 0 || a.splitPercent > 100 {
					a.splitPercent = defaultSplitPercent
				}
				first := func() { a.serialPaneView(c, snap, p, sess, focusTerm) }
				second := func() { a.localPaneView(c, snap, p, snap.localTerm, focusLocal, true) }
				if a.splitLayout == splitLayoutBelow {
					ui.SplitVerticalPercent(c, &a.splitPercent, first, second).Fill()
				} else {
					ui.SplitPercent(c, &a.splitPercent, first, second).Fill()
				}
			} else {
				a.localPaneView(c, snap, p, snap.localTerm, focusLocal, false)
			}
		} else if len(snap.portIDs) == 0 {
			ui.Column(c).Fill().Center().Gap(8).Children(func() {
				ui.Text(c, "暂无可用的终端端口").FontSize(14)
				if ui.Button(c.Key("empty-goto-ports"), "前往端口管理连接串口").Clicked() {
					a.openSettingsSection(settingsTabPorts)
				}
			})
		} else if sess, ok := snap.terms[snap.selected]; ok && snap.selected != "" {
			a.serialPaneView(c, snap, p, sess, focusTerm)
		} else {
			ui.Column(c).Fill().Center().Children(func() {
				ui.Text(c, "未选择串口")
			})
		}
	})
	// Status line: the native equivalent of WebUI toasts, plain
	// text. Operational messages avoid 已连接/未连接 phrasing —
	// the toolbar dot carries connection state.
	ui.Row(c).Padding(6, 12).Children(func() {
		ui.Text(c, snap.status).FontSize(12).TextColor(p.hex(p.muted))
	})
	// Command bar: status dot + port label + input + send, with the
	// history dropdown above the input (WebUI historyDropdown).
	a.cmdBarView(c, snap, p)
}

// serialPaneView renders the serial half of a terminal view: the
// recording strip (selected port only) above the terminal. It
// renders into the current column, so single-pane mode keeps its
// exact layout and split mode reuses it as the left half.
func (a *nativeApp) serialPaneView(c *ui.Context, snap snapshot, p palette, sess *termSession, focusTerm bool) {
	// Recording strip for the SELECTED port only (other ports'
	// recordings stay visible as amber buttons in their pills).
	// Amber, matching the pill record button — recording's color
	// everywhere, not green.
	if st, ok := snap.logState[snap.selected]; ok {
		ui.Row(c).Padding(2, 12).Gap(6).Children(func() {
			dotIcon(c, p.hex(p.warning)).FontSize(12)
			ui.Text(c, "录制中 "+st.filename+" ("+formatBytes(st.size)+")").
				FontSize(12).TextColor(p.hex(p.warning))
		})
	}
	// The terminal widget's backing is its own theme's
	// Background, not the app palette's termBg: in light app mode
	// termBg is white, which leaves a dark terminal theme's light
	// glyphs invisible. Applied settings only — staging
	// (a.setTermTheme) does not count until saved, same as
	// creation and apply (see app.go).
	tv := terminal.View(c, sess.term).Fill().AutoFocus().Background(terminalTheme(snap.settings.TermTheme).Background)
	if focusTerm {
		tv.Focus()
	}
	// Focus tracking for the find bar: a click, an explicit
	// focus request, or the view holding the keyboard focus all
	// say this pane is the focused one. When the focus is
	// elsewhere (command bar, panel) nothing here fires, so the
	// last pane stands.
	if tv.Clicked() || focusTerm || tv.Focused() {
		a.findPane = findPaneSerial
	}
	a.findBarOverlay(c, p, findPaneSerial, sess.term, tv)
}

// localPaneView renders the local terminal half of a split: a VS
// Code-style header (>_ + name + live directory, layout switch,
// × to close) above the terminal. The directory is the plugin's
// OSC 7 report (live); the starting directory is the fallback
// when the shell has not reported one yet. split says the pane
// is beside a serial pane (false when it is full-width with no
// serial port): only then do the layout icons show. The local
// terminal has no OnKey, so no serial interception lives here:
// Ctrl+S reaches the shell.
func (a *nativeApp) localPaneView(c *ui.Context, snap snapshot, p palette, lt *localTerminal, focusLocal bool, split bool) {
	dir := lt.term.Dir()
	if dir == "" {
		dir = lt.dir
	}
	ui.Row(c).Height(32).Padding(0, 12).Gap(8).Background(p.hex(p.elevated)).Children(func() {
		ui.Text(c, ">_").FontSize(12).TextColor(p.hex(p.muted))
		ui.Text(c, lt.name).FontSize(12).Bold()
		if dir != "" {
			ui.Text(c, shortenHome(dir)).FontSize(12).TextColor(p.hex(p.muted))
		}
		ui.Spacer(c)
		if split {
			// Layout switch: Jemy's split SVGs (see
			// icons.go) — split-right side by side,
			// split-below stacked. The active one takes
			// the text color, the other stays muted.
			rightColor, belowColor := p.muted, p.muted
			if a.splitLayout == splitLayoutBelow {
				belowColor = p.text
			} else {
				rightColor = p.text
			}
			if iconButton(c, "local-layout-right", iconSplitRight, p, rightColor).Tooltip("左右排布").Clicked() {
				a.setSplitLayout(splitLayoutRight)
			}
			if iconButton(c, "local-layout-below", iconSplitBelow, p, belowColor).Tooltip("上下排布").Clicked() {
				a.setSplitLayout(splitLayoutBelow)
			}
		}
		if iconButton(c, "local-close", iconClose, p, p.muted).Tooltip("关闭本地终端").Clicked() {
			a.closeLocalTerminal()
		}
	})
	ui.Row(c).Height(1).FillWidth().Background(p.hex(p.border))
	lv := terminal.View(c, lt.term).Fill().AutoFocus().Background(terminalTheme(snap.settings.TermTheme).Background)
	if focusLocal {
		lv.Focus()
	}
	// Focus tracking for the find bar, mirroring the serial pane.
	if lv.Clicked() || focusLocal || lv.Focused() {
		a.findPane = findPaneLocal
	}
	a.findBarOverlay(c, p, findPaneLocal, lt.term, lv)
}

// cmdDropdownLine is the one line a dropdown row shows for a
// history command: the first line only, with " …" when the
// command runs on to more lines. An overlong first line is cut
// by the row's SingleLine with its own ellipsis at the row
// width; a short first line of a multi-line command still gets
// the marker, so a row never looks like the whole command.
func cmdDropdownLine(cmd string) string {
	if i := strings.Index(cmd, "\n"); i >= 0 {
		return strings.TrimSuffix(cmd[:i], "\r") + " …"
	}
	return strings.TrimSuffix(cmd, "\r")
}

func (a *nativeApp) cmdBarView(c *ui.Context, snap snapshot, p palette) {
	info, ok := snap.ports[snap.selected]
	dot := p.dim
	label := "None"
	if ok {
		dot = statusColor(p, info)
		label = displayName(info)
	}
	// The dropdown hides on Esc (and on an Enter send) and stays
	// hidden until the input text changes again. The input is a
	// multi-line TextArea: a probe (temporary test, since
	// deleted) verified that OnTextKey — not Shortcut or
	// Submitted, which the editor swallows for Enter/Up/Down —
	// intercepts Enter/Up/Down before the editor acts, so every
	// keyboard rule below lives in cmdBarKey. The area grows
	// from 1 line to 5, then scrolls — but Lines(1, 5) alone
	// does not get there: the theme gives every TextArea a
	// MinHeight of 80 DIPs, so an empty bar would sit ~4 lines
	// tall, and the Lines cap counts the text system's line,
	// which runs taller than the lines as rendered on some
	// fonts, so a 6th line's glyphs could still show below the
	// fold. MinHeight(0) drops the floor to the 1-line height,
	// and FixedLineHeight(20) pins the rendered pitch to the
	// line the cap counts, so the box is exactly 1..5 rendered
	// lines (probe-verified, headless: 32/52/72/92/112 for
	// 0..5 lines, 112 past that). Radius 10, like the panel
	// search box: the old 999 made a tall bar a stadium.
	if a.cmdInput != a.prevCmdInput {
		// The user edited the text (typing, or a fill from the
		// no-dropdown history path): rebuild the dropdown list
		// from the new query, drop any stale selection, and
		// re-show the dropdown. A navigation fill syncs
		// prevCmdInput itself, so it never reaches here.
		a.prevCmdInput = a.cmdInput
		a.cmdHistHide = false
		a.cmdHistIdx = -1
		a.cmdList = historyMatches(a.cmdInput, snap.history)
	}
	dropdown := len(a.cmdList) > 0 && strings.TrimSpace(a.cmdInput) != "" && !a.cmdHistHide

	// Both the column and the input's row are keyed: an unkeyed
	// element's id mixes in its sibling index, so the row's id
	// (and with it the keyed input's id, which mixes in the row's
	// id) would flip whenever the dropdown column appears or
	// disappears above it — and focus, which points at an id,
	// would be lost with the old id. Keying both breaks that
	// chain: the input keeps one id whether the list is open or
	// not. (Keying only the input is NOT enough — that was the
	// first attempt, and the probes proved focus still dropped.)
	ui.Column(c.Key("cmd-bar")).Gap(2).Padding(14, 16).Background(p.hex(p.elevated)).Children(func() {
		if dropdown {
			// KeepFocus: while the dropdown (or its rows) are
			// pressed, focus stays where it is — the combobox
			// pattern this API is documented for.
			ui.Column(c).Gap(2).KeepFocus().Children(func() {
				for i, m := range a.cmdList {
					idx := i
					// A Button with an empty label + Children is
					// the documented way to get a clickable row:
					// plain button labels are forced SingleLine
					// and would truncate long commands. Each row
					// shows one line only (see cmdDropdownLine):
					// the text itself is SingleLine, so an
					// overlong first line is cut with its own
					// ellipsis instead of wrapping. The row is
					// as wide as the input (same column, same
					// padding), so it never exceeds the input
					// width.
					btn := ui.Button(c.Key("hist-sug:"+strconv.Itoa(i)), "").Border(0, ui.RGBA(0, 0, 0, 0)).Background(ui.RGBA(0, 0, 0, 0)).Padding(4, 8)
					if i == a.cmdHistIdx {
						btn = btn.Background(p.hex(p.hover))
					}
					btn.Children(func() {
						ui.Text(c, cmdDropdownLine(m)).Grow(1).SingleLine()
					})
					// A click only fills the input: a click is
					// easy to land by accident while aiming at
					// the input, and direct-send on click could
					// fire a dangerous command at the device.
					// Enter on a selection sends what the
					// input holds (see navigateDropdown). The
					// caret goes to the end of the filled
					// text, as the keyboard navigation fill
					// and the WebUI's selectHistoryItem do:
					// the area then reveal-scrolls to the end,
					// so a fill longer than 5 lines shows its
					// last 5, never a 6th line peeking below.
					if btn.Clicked() {
						a.mu.Lock()
						a.cmdInput = a.cmdList[idx]
						a.cmdHistIdx = -1
						a.cmdHistHide = true
						a.mu.Unlock()
						a.cmdCaretEnd = true
						a.invalidate()
					}
				}
			})
		}
		ui.Row(c.Key("cmd-row")).Gap(8).Children(func() {
			dotIcon(c, p.hex(dot))
			ui.Text(c, label).FontSize(12).Bold()
			in := ui.TextArea(c.Key("cmd-input"), &a.cmdInput).Fill().Lines(1, 5).FixedLineHeight(20).MinHeight(0).Radius(10).Background(p.hex(p.hover))
			if dropdown {
				in.Focus()
			}
			if a.cmdCaretEnd {
				// A history fill put the caret at the end of
				// the filled text, as the WebUI does (its
				// navigateHistory sets selectionStart/End to
				// the value length): k.SetText only clamps
				// the caret, it does not move it.
				a.cmdCaretEnd = false
				n := len([]rune(a.cmdInput))
				in.SetTextSelection(n, n)
			}
			// The caret as of this render, for the ↑/↓ split in
			// cmdBarKey: TextSelection can only be read on a
			// live element, so it is stashed here and the
			// OnTextKey closure captures it (see cmdBarKey).
			caretStart, caretEnd := in.TextSelection()
			// Enter/Up/Down are intercepted in OnTextKey, not
			// via Shortcut or Submitted: the editor wants them
			// (Enter inserts a newline, ↑/↓ move the caret),
			// so no Shortcut for them ever fires. Esc is the
			// exception — the editor does not want it, so the
			// Shortcut below still works, as before.
			in.OnTextKey(func(k ui.TextKey) bool {
				return a.cmdBarKey(k, caretStart, caretEnd)
			})
			if in.Shortcut(0, ui.KeyEscape) {
				// Close ONLY the dropdown; the input keeps its
				// focus (it is keyed, so its identity survives
				// the dropdown's disappearance) and stays in
				// editing state.
				a.mu.Lock()
				a.cmdHistIdx = -1
				a.cmdHistHide = true
				a.mu.Unlock()
				a.invalidate()
			}
			if ui.Button(c.Key("cmd-send"), "发送").Disabled(snap.selected == "").Radius(999).Background(p.hex(p.active)).Border(0, ui.RGBA(0, 0, 0, 0)).Clicked() {
				a.sendCommandBar()
			}
		})
	})
}

// navigateDropdown moves the dropdown selection one step in dir
// (+1 down, -1 up) and fills the input live with the selected
// match, so the user can edit it before sending. The input
// stays in sync: cmdHistIdx only ever points at a match the
// input already holds, so Enter can send the input and reach
// the same bytes the old direct-send reached. prevCmdInput is
// synced too, so the fill does not look like an edit to
// cmdBarView and the frozen cmdList does not move under the
// selection. With no selection yet, Down takes the first match
// and Up the last, as before. It returns the match ("" when
// there is no list); the caller drives the fill into the editor
// through the key (see cmdBarKey) — writing the bound value
// from OnTextKey alone is not enough, as the editor writes what
// it holds over it (see TextKey.SetText).
func (a *nativeApp) navigateDropdown(dir int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cmdList) == 0 {
		return ""
	}
	if a.cmdHistIdx < 0 {
		if dir > 0 {
			a.cmdHistIdx = 0
		} else {
			a.cmdHistIdx = len(a.cmdList) - 1
		}
	} else {
		a.cmdHistIdx = (a.cmdHistIdx + dir + len(a.cmdList)) % len(a.cmdList)
	}
	sel := a.cmdList[a.cmdHistIdx]
	a.cmdInput = sel
	a.prevCmdInput = sel
	return sel
}

// navigateCmdHistory walks the full history with ↑/↓ when no
// dropdown is open: Up toward older entries, Down back toward
// newer ones, and past the newest back to a blank input. It
// returns the fill text and whether the input changes at all:
// Down with no history position, or Up already at the oldest
// entry, changes nothing — and must not blank the draft. The
// fill deliberately does not sync prevCmdInput, so the edit
// tracker in cmdBarView rebuilds the dropdown for the filled
// text on the next frame, as before.
func (a *nativeApp) navigateCmdHistory(dir int) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cmdHistHide = false
	if n := len(a.history); n > 0 {
		if dir < 0 { // up: older
			if a.histNavIdx < n-1 {
				a.histNavIdx++
			} else {
				return "", false
			}
		} else if a.histNavIdx > 0 { // down: newer
			a.histNavIdx--
		} else if a.histNavIdx == 0 {
			a.histNavIdx = -1
		} else {
			return "", false
		}
	} else {
		return "", false
	}
	var fill string
	if a.histNavIdx >= 0 {
		fill = a.history[a.histNavIdx].Command
	}
	a.cmdInput = fill
	return fill, true
}

// cmdBarKey handles the command bar's keys in the TextArea's
// OnTextKey. It runs between frames, on the main thread.
// caretStart/caretEnd are the caret (rune offsets) read from the
// input during the last render — the probe (a temporary test,
// since deleted) verified OnTextKey returning true consumes
// Enter/Up/Down before the editor acts, Shift+Enter still
// inserts a newline, and the render-time caret is current when
// the next key arrives, because frames settle between keys.
// The one residual risk is a frame lagging under load: a single
// ↑/↓ could then navigate history instead of moving the caret,
// or the reverse; pressing the other key recovers it.
//
// The key's own text is used, not the bound value: k.Text is
// the editor's text as the key comes, which may be ahead of the
// bound value (a fast type-then-Enter would otherwise drop the
// last characters), and k.SetText is the only way to clear or
// fill the input from here.
func (a *nativeApp) cmdBarKey(k ui.TextKey, caretStart, caretEnd int) bool {
	switch k.Key {
	case ui.KeyEnter:
		if k.Mods != 0 {
			// Shift+Enter (and the rest): let the editor
			// insert the newline.
			return false
		}
		text := k.Text()
		if strings.TrimSpace(text) == "" {
			// Enter on a blank bar: swallow it (an empty
			// newline would only grow the bar) and stay.
			return true
		}
		// Plain Enter sends the input — which, after a
		// dropdown navigation, already holds the selected
		// match (see navigateDropdown), so this one path
		// covers both the old direct-send and the plain
		// send. Then the focus leaves the input for the
		// serial terminal: the next terminal render takes
		// it via termFocusPending.
		a.deliverCommand(a.selectedID(), text)
		k.SetText("")
		a.mu.Lock()
		a.cmdHistIdx = -1
		a.histNavIdx = -1
		a.cmdHistHide = true
		a.prevCmdInput = ""
		a.cmdList = nil
		a.mu.Unlock()
		// The fill's caret request is moot now: the input is
		// empty and the focus leaves it.
		a.cmdCaretEnd = false
		a.termFocusPending = true
		a.invalidate()
		return true
	case ui.KeyUp, ui.KeyDown:
		if k.Mods != 0 {
			// e.g. Ctrl+Shift+↑/↓: the editor's word
			// selection, as before.
			return false
		}
		dir := 1
		if k.Key == ui.KeyUp {
			dir = -1
		}
		a.mu.Lock()
		dropdown := len(a.cmdList) > 0 && strings.TrimSpace(a.cmdInput) != "" && !a.cmdHistHide
		a.mu.Unlock()
		if dropdown {
			// The fill goes through the key as well as the
			// bound value: the bound value alone would be
			// overwritten by what the editor holds. The caret
			// goes to the end of the filled text, as the
			// WebUI does.
			k.SetText(a.navigateDropdown(dir))
			a.cmdCaretEnd = true
			a.invalidate()
			return true
		}
		// No dropdown: navigate the full history only when
		// the caret sits at the edge the key points at —
		// the WebUI rule (its textarea does the same split
		// on selectionStart/selectionEnd). Otherwise the
		// caret moves inside the multi-line text.
		if caretStart != caretEnd {
			return false
		}
		text := k.Text()
		if dir < 0 && caretStart == 0 || dir > 0 && caretStart == len([]rune(text)) {
			if fill, ok := a.navigateCmdHistory(dir); ok {
				k.SetText(fill)
				if fill != "" {
					a.cmdCaretEnd = true
				}
				a.invalidate()
			}
			return true
		}
		return false
	default:
		return false
	}
}
