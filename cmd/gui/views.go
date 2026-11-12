package main

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

var baudOptions = []string{"9600", "19200", "38400", "57600", "115200", "230400", "460800", "921600"}
var dataOptions = []string{"7", "8"}
var stopOptions = []string{"1", "2"}
var parityOptions = []string{"N", "O", "E"}
var termThemeOptions = []string{"dark", "light", "blue", "green"}

// draftFor returns a port row's editable draft, creating it from
// the reported config on first sight.
func (a *nativeApp) draftFor(id string, info PortInfo) *portDraft {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d, ok := a.drafts[id]; ok {
		return d
	}
	d := draftFromInfo(info)
	a.drafts[id] = d
	return d
}

// submitAddPort handles the add-port modal. Unlike the WebUI modal
// (which only adds the port to its local list), this really creates
// the port server-side via add_port, then switches to it. The
// server prefixes "/dev/" itself, so a typed "/dev/ttyUSB0" is
// stripped first.
func (a *nativeApp) submitAddPort() {
	a.mu.Lock()
	path, alias, client := strings.TrimSpace(a.addPath), strings.TrimSpace(a.addAlias), a.client
	ports := a.ports
	a.mu.Unlock()
	if path == "" {
		a.setStatus("请输入端口名称或路径")
		return
	}
	id := strings.TrimPrefix(path, "/dev/")
	if _, exists := ports[id]; exists {
		if alias != "" && client != nil {
			_ = client.SetPortAlias(id, alias)
		}
		a.mu.Lock()
		a.addOpen = false
		a.mu.Unlock()
		a.selectPort(id)
		return
	}
	if client == nil {
		a.setStatus("服务还没连上，无法添加端口")
		return
	}
	if err := client.AddPort(id, alias); err != nil {
		a.setStatus("添加端口失败: %v", err)
		return
	}
	a.mu.Lock()
	a.addOpen = false
	a.mu.Unlock()
	a.selectPort(id)
	a.setStatus("正在添加端口 %s…", id)
}

// portsTab renders the settings panel's ports section: one
// two-line row per port — line 1 is the alias (title size,
// bright) + dot, line 2 is the detail (small, muted: path ·
// baud frame · vendor) with the row's buttons at its right.
// Clicking a port's name switches the current terminal to it;
// the panel stays open (side-panel semantics — the terminal sits
// beside it). Config/alias expand inline, one row at a time. All
// buttons are flat and borderless; the top button row and the
// config selects wrap instead of clipping at the panel edge.
func (a *nativeApp) portsTab(c *ui.Context, snap snapshot, p palette) {
	ui.Column(c).Gap(16).Children(func() {
		ui.Row(c).Gap(8).Wrap().Children(func() {
			if flatButton(c, "ports-refresh", "刷新设备", p).Clicked() {
				a.mu.Lock()
				client := a.client
				a.mu.Unlock()
				if client != nil {
					_ = client.GetPorts()
				}
			}
			if flatButton(c, "ports-add", "添加端口", p).Clicked() {
				a.addPath, a.addAlias = "", ""
				a.addOpen = true
			}
			if flatButton(c, "ports-reconnect", "重连服务", p).Clicked() {
				go a.reconnect()
			}
		})
		if len(snap.portIDs) == 0 {
			ui.Column(c).Gap(8).Children(func() {
				ui.Text(c, "未发现可用串行端口").FontSize(15).Bold()
				ui.Text(c, "请插入 USB 转串口设备，或手动添加自定义端口路径").FontSize(13).TextColor(p.hex(p.muted))
				if flatButton(c, "ports-empty-add", "添加自定义端口", p).Clicked() {
					a.addPath, a.addAlias = "", ""
					a.addOpen = true
				}
			})
			return
		}
		for _, id := range snap.portIDs {
			info := snap.ports[id]
			draft := a.draftFor(id, info)
			isConn := info.Connected
			pid := id
			ui.Column(c).Gap(8).Children(func() {
				// Line 1: dot + alias (title). No status text
				// on the row, the dot is the state.
				ui.Row(c).Gap(8).Children(func() {
					dotIcon(c, p.hex(statusColor(p, info)))
					if quietButton(c, "port-row:"+pid, displayName(info), p, p.text).Clicked() {
						a.selectPort(pid)
					}
				})
				// Line 2: detail (wraps) + buttons at the right.
				// The detail Text takes the remaining width, so
				// a long vendor name wraps instead of pushing
				// the buttons out of the panel.
				ui.Row(c).Gap(8).Children(func() {
					sub := portDetailLabel(info)
					if info.Description != "" {
						sub += " · " + info.Description
					}
					ui.Text(c, sub).FontSize(12).TextColor(p.hex(p.muted)).Grow(1)
					if flatButton(c, "port-cfg:"+pid, "配置", p).Shrink(0).Clicked() {
						a.mu.Lock()
						if a.portConfigOpen == pid {
							a.portConfigOpen = ""
						} else {
							a.portConfigOpen = pid
						}
						a.mu.Unlock()
						a.invalidate()
					}
					if flatButton(c, "port-alias:"+pid, "别名", p).Shrink(0).Clicked() {
						a.mu.Lock()
						if a.portAliasOpen == pid {
							a.portAliasOpen = ""
						} else {
							a.portAliasOpen = pid
						}
						a.mu.Unlock()
						a.invalidate()
					}
					if isConn {
						if flatButton(c, "port-disconnect:"+pid, "断开", p).Shrink(0).Clicked() {
							a.disconnectPort(pid)
						}
					} else {
						if flatButton(c, "port-connect:"+pid, "连接", p).Shrink(0).Clicked() {
							a.connectPort(pid)
						}
					}
				})
				a.mu.Lock()
				cfgOpen := a.portConfigOpen == pid
				aliasOpen := a.portAliasOpen == pid
				a.mu.Unlock()
				if aliasOpen {
					ui.Row(c).Gap(8).Wrap().Children(func() {
						ui.Text(c, "设备别名").FontSize(12).TextColor(p.hex(p.muted))
						ui.TextInput(c, &draft.alias).Fill()
						if flatButton(c, "port-alias-save:"+pid, "保存别名", p).Clicked() {
							a.saveCardAlias(pid)
						}
					})
				}
				if cfgOpen {
					ui.Row(c).Gap(8).Wrap().Children(func() {
						ui.Column(c).Gap(2).Children(func() {
							ui.Text(c, "波特率").FontSize(12).TextColor(p.hex(p.muted))
							ui.Select(c, &draft.baud, baudOptions)
						})
						ui.Column(c).Gap(2).Children(func() {
							ui.Text(c, "数据位").FontSize(12).TextColor(p.hex(p.muted))
							ui.Select(c, &draft.databits, dataOptions)
						})
						ui.Column(c).Gap(2).Children(func() {
							ui.Text(c, "停止位").FontSize(12).TextColor(p.hex(p.muted))
							ui.Select(c, &draft.stopbit, stopOptions)
						})
						ui.Column(c).Gap(2).Children(func() {
							ui.Text(c, "校验位").FontSize(12).TextColor(p.hex(p.muted))
							ui.Select(c, &draft.parity, parityOptions)
						})
					})
					ui.Row(c).Gap(8).Children(func() {
						if flatButton(c, "port-cfg-save:"+pid, "保存", p).Clicked() {
							a.saveCardConfig(pid)
						}
					})
				}
			})
			// Hairline between rows: explicit 1px height +
			// cross-axis stretch. Fill() must NOT be used
			// here (same defect as the history divider:
			// Fill sets height to percent(100), which
			// resolves to 0 against this auto-height
			// Column, so the old line never painted).
			ui.Row(c).Height(1).AlignSelf(ui.Stretch).Shrink(0).Background(p.hex(p.border))
		}
	})
}

// historyTab renders the settings panel's history section: search
// filter, per-item send/edit/delete, export/import, two-step
// clear. Send goes straight to the selected port's serial line
// (no detour through the command bar); Edit turns the row into
// a multi-line inline editor (Enter inserts a newline, the 保存
// button saves, Esc cancels); Delete is immediate. Each row is
// vertical: the monospace command text, wrapped at the full
// width, on top, and its buttons (发送 / 编辑 / 删除) on a row
// below, left-aligned; a 1px hairline separates rows.
// histEditLinesMax caps the inline history editor's height: it
// grows with its wrapped text up to this many lines, then
// scrolls (mygo paints the scroll thumb on hover/drag).
const histEditLinesMax = 6

func (a *nativeApp) historyTab(c *ui.Context, snap snapshot, p palette) {
	filter := strings.ToLower(strings.TrimSpace(a.histFilter))
	var items []HistoryEntry
	for _, e := range snap.history {
		if filter == "" || strings.Contains(strings.ToLower(e.Command), filter) {
			items = append(items, e)
		}
	}
	ui.Column(c).Gap(16).Children(func() {
		ui.Row(c).Gap(8).Wrap().Children(func() {
			if flatButton(c, "hist-export", "导出 JSON", p).Clicked() {
				a.exportHistory()
			}
			if flatButton(c, "hist-import-merge", "导入 JSON（合并）", p).Clicked() {
				a.importHistory(true)
			}
			if flatButton(c, "hist-import-replace", "导入 JSON（替换）", p).Clicked() {
				a.importHistory(false)
			}
			clearLbl := "清空历史"
			if a.clearArmed {
				clearLbl = "确认清空？"
			}
			if flatButton(c, "hist-clear", clearLbl, p).Clicked() {
				a.mu.Lock()
				armed := a.clearArmed
				a.mu.Unlock()
				if armed {
					a.clearHistory()
				} else {
					a.mu.Lock()
					a.clearArmed = true
					a.mu.Unlock()
					a.invalidate()
				}
			}
		})
		ui.TextInput(c, &a.histFilter).Fill()
		if len(items) == 0 {
			if len(snap.history) == 0 {
				ui.Text(c, "暂无历史发送命令").FontSize(13).TextColor(p.hex(p.muted))
			} else {
				ui.Text(c, "没有匹配搜索条件的命令").FontSize(13).TextColor(p.hex(p.muted))
			}
			return
		}
		for i, e := range items {
			entry := e
			if a.histEditID == entry.ID {
				ui.Column(c).Gap(8).Padding(10, 0).Children(func() {
					// Multi-line editor: a history command can
					// itself contain newlines, which the old
					// single-line input could not edit. TextArea
					// grows with its wrapped text up to
					// histEditLinesMax lines, then scrolls; its
					// scroll thumb paints on hover/drag (mygo
					// has no always-visible scrollbar API, and
					// none is faked here). AutoFocus once on
					// appearance, so Esc in the editor cancels
					// the edit in place (render's panel-Esc
					// yields while an edit is active).
					in := ui.TextArea(c.Key("hist-edit-input"), &a.histEditText).Fill().Lines(1, histEditLinesMax).AutoFocus()
					// Enter inserts a newline in a TextArea
					// (the editor takes it), so it no longer
					// saves: save is the 保存 button below.
					// Esc is NOT taken by the editor, so the
					// element-level shortcut still cancels.
					if in.Shortcut(0, ui.KeyEscape) {
						a.cancelInlineHistoryEdit()
					}
					ui.Row(c).Gap(8).Children(func() {
						if flatButton(c, "hist-edit-save:"+entry.ID, "保存", p).Clicked() {
							a.saveInlineHistoryEdit()
						}
						if flatButton(c, "hist-edit-cancel:"+entry.ID, "取消", p).Clicked() {
							a.cancelInlineHistoryEdit()
						}
					})
				})
			} else {
				ui.Column(c).Gap(8).Padding(10, 0).Children(func() {
					// Commands render in the configured terminal
					// font (a monospace family in practice) and
					// wrap at the column's full width; the
					// buttons sit on their own row below,
					// left-aligned, so a long wrapped command
					// no longer shares a row with them.
					txt := ui.Text(c, entry.Command).Fill()
					if snap.settings.FontFamily != "" {
						txt = txt.Font(snap.settings.FontFamily)
					}
					ui.Row(c).Gap(8).Children(func() {
						if flatButton(c, "hist-send:"+entry.ID, "发送", p).Clicked() {
							a.sendHistoryDirect(entry.Command)
						}
						if flatButton(c, "hist-edit:"+entry.ID, "编辑", p).Clicked() {
							a.startInlineHistoryEdit(entry.ID, entry.Command)
						}
						if flatButton(c, "hist-del:"+entry.ID, "删除", p).Clicked() {
							a.deleteHistory(entry.ID)
						}
					})
				})
			}
			if i < len(items)-1 {
				// Hairline: explicit 1px height + cross-axis
				// stretch. Fill() must NOT be used here: it
				// sets width AND height to percent(100),
				// overwriting Height(1), and a percentage
				// height against this auto-height Column
				// resolves to 0 — which is why the old line
				// never painted (see the divider test).
				ui.Row(c).Height(1).AlignSelf(ui.Stretch).Shrink(0).Background(p.hex(p.border))
			}
		}
	})
}

// syncTab renders the settings panel's sync section: the WebDAV
// config form. The server never returns the password, only whether
// one is set: an empty password field on save keeps the stored one
// (server semantics), matching the WebUI. The remote backup list
// is not implemented (it needs a PROPFIND listing + restore flow);
// the button says so.
func (a *nativeApp) syncTab(c *ui.Context, snap snapshot, p palette) {
	// Fetch the backup list once per sync-tab entry: the first
	// render triggers it, later renders reuse the cache until a
	// refresh or an upload/delete/import invalidates it. The
	// wdListErr == "" guard matters: a failed fetch must NOT
	// retrigger every frame (an infinite retry loop) — only the
	// 刷新 button (which clears the error) refetches. The set is
	// locked (re-checked under mu): the fetch goroutine writes
	// these fields, so the render thread must not touch them
	// directly — it reads snap only.
	if !snap.wdListLoaded && !snap.wdListLoading && snap.wdListErr == "" {
		a.mu.Lock()
		if !a.wdListLoaded && !a.wdListLoading && a.wdListErr == "" {
			a.wdListLoading = true
			go a.fetchWebDAVBackups()
		}
		a.mu.Unlock()
	}
	ui.Column(c).Gap(16).Children(func() {
		ui.Column(c).Background(p.hex(p.card)).Radius(14).Border(1, p.hex(p.border)).Padding(12).Gap(8).Children(func() {
			ui.Text(c, "WebDAV 服务器设置").FontSize(14).Bold()
			ui.Text(c, "服务器 URL").FontSize(12).TextColor(p.hex(p.muted))
			ui.TextInput(c, &a.wdURL).Fill()
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "用户名").FontSize(12).TextColor(p.hex(p.muted))
					ui.TextInput(c, &a.wdUser).Fill()
				})
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "密码 / Token").FontSize(12).TextColor(p.hex(p.muted))
					pw := ui.TextInput(c, &a.webdavPass).Fill()
					pw.Password()
					if snap.wdPassSet {
						ui.Text(c, "已保存密码（留空则保持）").FontSize(11).TextColor(p.hex(p.muted))
					}
				})
			})
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "自动备份周期（分钟）").FontSize(12).TextColor(p.hex(p.muted))
					ui.NumberInput(c, &a.webdavIntF, 5, 10080, 1)
				})
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "保留备份版本数").FontSize(12).TextColor(p.hex(p.muted))
					ui.NumberInput(c, &a.webdavMaxF, 1, 50, 1)
				})
			})
			ui.Row(c).Gap(8).Wrap().Children(func() {
				if flatButton(c, "webdav-save", "保存配置", p).Clicked() {
					a.saveWebDAV()
				}
				if flatButton(c, "webdav-test", "测试连接", p).Clicked() {
					a.testWebDAV()
				}
				if flatButton(c, "webdav-backup-now", "立即备份", p).Clicked() {
					a.backupWebDAVNow()
				}
				if flatButton(c, "webdav-clear-pw", "清除已保存密码", p).Clicked() {
					a.clearWebDAVPassword()
				}
			})
		})
		// Cloud backup list: the server's /api/webdav/* proxy
		// endpoints, same as the WebUI's list (history_backup_
		// prefix). Rows are vertical like the history rows:
		// filename on top, date · size below, actions below
		// that. Import (merge) respects deletion tombstones
		// (ids preserved); import (replace) restores the file
		// exactly and clears the restored ids' tombstones.
		// Delete is two-step (arm, then execute) and touches
		// only the cloud copy. All failures land on the status
		// line; the list area never goes silently blank.
		ui.Column(c).Background(p.hex(p.card)).Radius(14).Border(1, p.hex(p.border)).Padding(12).Gap(8).Children(func() {
			ui.Row(c).Gap(8).Wrap().Children(func() {
				ui.Text(c, "云端备份").FontSize(14).Bold()
				if flatButton(c, "webdav-backups-refresh", "刷新", p).Clicked() {
					a.refreshWebDAVBackupList()
				}
			})
			switch {
			case snap.wdListLoading:
				ui.Text(c, "正在加载备份列表…").FontSize(12).TextColor(p.hex(p.muted))
			case snap.wdListErr != "":
				ui.Text(c, "加载失败，请重试").FontSize(12).TextColor(p.hex(p.muted))
			case len(snap.wdBackups) == 0:
				ui.Text(c, "暂无云端备份").FontSize(12).TextColor(p.hex(p.muted))
			default:
				ui.Column(c).Gap(12).Children(func() {
					for i, f := range snap.wdBackups {
						file := f
						ui.Column(c).Gap(4).Children(func() {
							ui.Text(c, file.Name).FontSize(13)
							ui.Text(c, file.Mod+" · "+file.Size).FontSize(12).TextColor(p.hex(p.muted))
							ui.Row(c).Gap(6).Wrap().Children(func() {
								if flatButton(c, "webdav-backup-import-merge:"+file.Name, "导入（合并）", p).Clicked() {
									a.importWebDAVBackup(file.Name, true)
								}
								if flatButton(c, "webdav-backup-import-replace:"+file.Name, "导入（替换）", p).Clicked() {
									a.importWebDAVBackup(file.Name, false)
								}
								delLbl := "删除"
								if snap.wdDelArmed == file.Name {
									delLbl = "确认删除？"
								}
								if flatButton(c, "webdav-backup-del:"+file.Name, delLbl, p).Clicked() {
									// Two-step delete, mirroring
									// the history clear's armed
									// pattern: locked like it.
									a.mu.Lock()
									armed := a.wdDelArmed == file.Name
									if armed {
										a.wdDelArmed = ""
									} else {
										a.wdDelArmed = file.Name
									}
									a.mu.Unlock()
									if armed {
										a.deleteWebDAVBackupFile(file.Name)
									} else {
										a.invalidate()
									}
								}
							})
						})
						if i < len(snap.wdBackups)-1 {
							ui.Row(c).Height(1).AlignSelf(ui.Stretch).Shrink(0).Background(p.hex(p.border))
						}
					}
				})
			}
		})
	})
}

// appearanceTab renders the settings panel's appearance section
// as TWO card groups: "外观" (app chrome theme toggle — saves
// immediately — plus terminal theme, system font, size) and
// "终端行为" (scrollback only, with this
// section's own Save button). The old standalone terminal
// section is gone; its items live here, minus the deleted
// line-ending setting. Font: when fc-list yields system families
// they are the only choices (Jemy's settled direction, flat list,
// no presets); otherwise a free text field.
func (a *nativeApp) appearanceTab(c *ui.Context, snap snapshot, p palette) {
	fontOpts := snap.fontFams
	if a.setFontFamily != "" {
		found := false
		for _, f := range fontOpts {
			if f == a.setFontFamily {
				found = true
				break
			}
		}
		if !found {
			fontOpts = append([]string{a.setFontFamily}, fontOpts...)
		}
	}
	// Icon shows the TARGET theme: sun in dark mode, moon in light
	// mode, with the matching tooltip.
	themeIcon := iconNight
	themeTip := "切换到深色"
	if snap.settings.Dark {
		themeIcon = iconDay
		themeTip = "切换到浅色"
	}
	ui.Column(c).Gap(16).Children(func() {
		ui.Column(c).Background(p.hex(p.card)).Radius(14).Border(1, p.hex(p.border)).Padding(12).Gap(8).Children(func() {
			ui.Text(c, "外观").FontSize(14).Bold()
			ui.Row(c).Gap(8).Wrap().Children(func() {
				ui.Text(c, "界面主题").FontSize(12).TextColor(p.hex(p.muted))
				if flatIconButton(c, "appearance-theme", themeIcon, p).Tooltip(themeTip).Clicked() {
					a.toggleAppTheme()
				}
			})
			ui.Row(c).Gap(8).Wrap().Children(func() {
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "终端配色").FontSize(12).TextColor(p.hex(p.muted))
					ui.Select(c, &a.setTermTheme, termThemeOptions)
				})
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "代码字体").FontSize(12).TextColor(p.hex(p.muted))
					if len(fontOpts) > 0 {
						ui.Select(c, &a.setFontFamily, fontOpts)
					} else {
						ui.TextInput(c, &a.setFontFamily).Fill()
					}
				})
			})
			ui.Row(c).Gap(8).Children(func() {
				ui.Column(c).Gap(2).Children(func() {
					ui.Text(c, "字号大小").FontSize(12).TextColor(p.hex(p.muted))
					ui.NumberInput(c, &a.setFontSizeF, 11, 22, 1)
				})
			})
		})
		ui.Column(c).Background(p.hex(p.card)).Radius(14).Border(1, p.hex(p.border)).Padding(12).Gap(8).Children(func() {
			ui.Text(c, "终端行为").FontSize(14).Bold()
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, "最大回滚行数 (Scrollback)").FontSize(12).TextColor(p.hex(p.muted))
				ui.NumberInput(c, &a.setScrollF, 1000, 100000, 1000)
			})
			// No auto-scroll checkbox: the old AutoScroll switch
			// vetoed wheel and Shift+PageUp/Down in the terminal
			// callbacks, which froze the viewport while it
			// defaulted ON — and the plugin exposes no
			// scroll-to-bottom call, so "force back to bottom on
			// new data" cannot be implemented honestly. Scrolling
			// is the plugin's native behavior now (wheel and
			// Shift+PageUp/Down scroll the scrollback, stay when
			// scrolled up, follow at the bottom); see README.
			// Copy-on-select: the v0.7.4 plugin copies a mouse
			// selection to the clipboard as it ends (and Select
			// All too) when CopyOnSelect is on; the switch below
			// saves with this section and applies to existing
			// terminals at once via SetCopyOnSelect.
			ui.Column(c).Gap(2).Children(func() {
				ui.Checkbox(c, &a.setCopyOnSelect, "选中即复制")
				ui.Text(c, "松开鼠标即把选中文字复制到剪贴板").FontSize(12).TextColor(p.hex(p.muted))
			})
			// Local terminal multiplexer: tmux attach-or-create
			// (default) or a raw login shell. The session name
			// only applies in tmux mode; both only affect a NEW
			// local terminal, like scrollback. No live apply to
			// a running shell: it already has its session.
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, "本地终端复用器").FontSize(12).TextColor(p.hex(p.muted))
				ui.Select(c, &a.setMultiplexer, multiplexerOptions)
				ui.Text(c, "tmux：新建本地终端时 attach 或创建会话；无：直接跑登录 shell").FontSize(12).TextColor(p.hex(p.muted))
			})
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, "tmux 会话名").FontSize(12).TextColor(p.hex(p.muted))
				ui.TextInput(c, &a.setTmuxSession).Fill().Disabled(a.setMultiplexer != multiplexerTmux)
			})
			ui.Row(c).Gap(8).Children(func() {
				if flatButton(c, "appearance-save", "保存", p).Clicked() {
					a.applyAppearanceSettings()
				}
			})
		})
	})
}
