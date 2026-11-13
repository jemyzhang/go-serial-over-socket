# sos-gui

A fully-native GUI client for go-serial-over-socket: MyGo native UI
plus the terminal plugin (Ghostty's libghostty-vt), no webview. It
speaks the same WebSocket protocol as the WebUI, so the server is
untouched and this client can share sessions with the TUI and the
WebUI.

This is the desktop GUI client (`cmd/gui`). The WebUI remains the
client that any browser on the LAN can open without installing
anything.

## Run

```sh
go run ./cmd/gui            # local mode: embedded server on 127.0.0.1:2025
go run ./cmd/gui -port 2026
go run ./cmd/gui -remote http://192.168.1.10:2025
```

Local mode is Linux-only (serial enumeration needs udev); other
platforms are remote-only clients.

## Tray

Mirrors the previous webview client's tray, in a narrower shape (see `tray.go`):

- Clicking the tray icon shows the main window; closing the
  window only parks the app in the tray — the WS connection, the
  embedded server (local mode) and the serial sessions keep
  running. A real quit comes from the tray menu's "退出" item
  (`App.Quit()`); only then does shutdown stop everything.
- Tray menu, three sections only: a disabled status line
  (local mode: "本地模式 · 127.0.0.1:<port>", remote mode:
  "远程模式 · <base>"), "显示主窗口", and "退出". No mode
  switch and no connection settings — native has no saved
  preferences or mode-switching mechanism to back them.
- `SOS_GUI_NO_TRAY=1` disables the tray (same variable as the
  previous client); without a tray — disabled, or the platform has
  none (e.g. Linux without libayatana-appindicator) — closing
  the window quits, as before. The icon is the same
  `web/static/icons/icon-192x192.png` the previous client read.

## How close to the WebUI is it?

The layout follows `web/templates/index.html` and the behavior
follows `web/static/js/app.js`. The chrome follows the Tern-style
mockup (`tern-style.html` in the goal workspace's
`gui-previews/`): a slim quiet toolbar, and settings as a fixed
side panel — no pill containers anywhere. The scheme-A card
chrome, the full-card pass, and the floating-pill pass that
followed it were all superseded by this pass (the floating pass
failed on device review: pill buttons inside a pill container,
plus the port pill and command-bar pills, stacked three layers
of rounded shadowed containers). Status per area:

### Done

- **Slim toolbar, no navigation at all**: dark theme by
  default, colors taken verbatim from GoRex's `style.go`
  darkColors/lightColors (text `#f2f2f7` / muted `#aeaeb2`,
  status green `#4ade80` / amber `#fbbf24`), with the
  light/dark toggle persisted (now in the settings panel's
  appearance section). Terminal colors stay on the WebUI
  mapping, independent of the chrome theme, as before. There is
  no icon rail, no tab strip, no card, no gradient margin, and
  no floating overlay: one 46px toolbar row (dot + plain port
  text on the left, plain-text actions on the right, hover
  background only), then the terminal, then the bottom command
  bar. Reconnect is a real re-dial (settings ports section);
  resident terminals survive it because their Conns write
  through the app's current client.
- **Toolbar port pills**: every port as a pill tab — NOT a
  button: a plain Row (small 10px radius, tight 4px/8px
  padding, no border, no boxy fill) whose Clicked comes from
  the generic Element API. The
  previous flat-button strip with its visible scrollbar was
  rejected for looking like a row of buttons. Each pill holds
  the status dot + the name (alias when set, else the port id)
  + the short config (baudrate only), e.g. `● D766 · 115200`
  + a thin divider + three small icon buttons (scheme A):
  ⏻ connect/disconnect, ● record, ⚙ settings, packed in
  their own Row at Gap 2 with 2px/4px button padding (glyphs
  ~10px apart, each button still ~24px wide to click; the
  pill info zone keeps its Gap 6). A hovered pill button
  shows its hover fill (8% white, theme radius 6) — that
  fill, not a focus ring or a border, is the rounded box
  seen around ⏻ in the 2026-11-03 hover screenshot: the
  box's pixels match 8% white over the pill background
  exactly, mygo draws no focus ring for a mouse click
  (pointer focus sets focusVisible=false; only Tab sets
  it), and quietButton kills the theme border outright.
  The same screenshot showed ⏻ riding high: its U+23FB
  glyph falls back to a symbols font whose ink sits
  ~2.5 DIPs above the line-box center, so the ⏻ button is
  built by connectIconButton on ButtonBase with its glyph
  Text translated down 2.5 DIPs — paint-only, so the button
  box, hit area, and hover fill do not move, and ●/⚙
  (already centered) are untouched. Separately, all three
  buttons share an explicit 20 DIP height: mygo's Row()
  defaults its cross-axis alignment to Center, not Stretch,
  so natural heights would leave ⏻'s hover fill taller
  than ●'s (⏻'s fallback line box is ~28 DIPs, ●'s ~20) —
  20 matches ●'s natural height, so ●/⚙ do not visibly
  change while ⏻ shrinks to the same box. Hover any button
  to check: all three hover fills are the same size and
  centered on the text row, and all three glyphs sit on
  the same center line. The full
  detail lives in the pill's native hover tooltip, never in
  the pill itself, one fact per line: name, path, baud +
  frame, vendor when known (e.g. `D766` / `/dev/ttyUSB0` /
  `115200 8N1` / vendor line; lines with no known facts are
  omitted, never guessed). mygo's text layout breaks at
  newlines, so the plain `Element.Tooltip` with a "\n"-joined
  string renders the stacked lines — no custom tooltip layer.
  The selected pill gets bright text on a light fill, the rest
  stay muted plain text. Clicking the pill body switches the
  terminal to that port (resident terminals, no screen loss)
  and moves the keyboard focus into that terminal, so typing
  goes straight to the serial line: the terminal's AutoFocus
  fires on appearance only and does not reliably re-fire on a
  pill switch, so the click sets a one-shot focus request the
  terminal render consumes (a missing terminal drops it rather
  than leaving it stale). The command dropdown is hidden on a
  pill click too, or the input's per-frame Focus() would yank
  focus right back while it is open. Pill buttons never trigger
  the focus request, and neither do panel port rows, add-port
  switches, or broadcast reselection — only the pill body does;
  clicking a button performs only that button's action, never
  switches — mygo delivers a click to the deepest interactive
  element. There is NO right-click menu on pills anymore: its
  connect/disconnect and settings items moved into the
  buttons. There is deliberately NO scroll container in the
  strip, so no scrollbar can ever appear: mygo v0.4.8 has no
  hide-scrollbar API, so many ports compress instead — the
  strip and each pill shrink, and both texts are SingleLine
  with an ellipsis, so the row always fits one line. The old
  tab menu's 复制设备路径 item is gone with the tabs — the
  path is visible in the pill tooltip. The command-palette
  placeholder button and its bottom status strip are deleted;
  the palette comes back only when it is really built.
- **Status is the lamp's color, never text**: no toolbar text
  anywhere shows 已连接/未连接/连接中, and neither do status
  messages (initial status is 就绪, service connect reports
  服务连接成功). The lamp inside each port pill (a separate
  Text, since a label has one text color) is green =
  connected and still, amber = connecting, red = disconnected
  or error, following the ports/port_status broadcasts — never
  set locally, so a failed attempt cannot leave it showing a
  wrong state. Flashing means recording, nothing else: a
  connected port that records step-flashes green — a 900ms
  ticker flips the flash target between dim and full, and the
  lamp takes each target directly, so it alternates bright/dim
  on a 1.8s cycle. It used to be a smooth native `Animate`
  interpolation, but the interpolation duration matched the
  ticker period, so the animation never settled and mygo kept
  redrawing the whole window at display refresh rate while any
  port was connected — CPU stayed high and grew with window
  size. The stepped version redraws once per 900ms tick
  instead. Only the pill lamp of a recording port flashes — the
  command bar dot and the panel row dots share the colors but
  stay static, or the whole
  window would flicker. Note that live serial output still causes
  data-driven terminal redraws; that is unrelated to the flash and
  cannot be avoided. Flash appearance cannot be verified headless;
  check it on device.
- **Toolbar actions**: save output / ⚙, icon-only with native
  hover tooltips (like the Web UI's icon buttons): save is a
  download arrow with tooltip "保存输出". Recording no longer
  lives on the toolbar — each pill carries its own record
  button (see "Per-port recording" below). Glyphs are text
  glyphs like ⚙ (there is no icon font); the WebUI's SVGs
  cannot be reused in native text rendering, so these are
  approximations. Connect/disconnect lives on each pill's ⏻
  button and the panel's port rows, not on the toolbar; there
  is no clear button (the terminal's native right-click menu
  already clears) and no search button (the plugin has no
  search API, so a button that only reported "not
  implemented" was deleted rather than kept). The pill's ⚙
  button opens the panel filtered to the ports section with
  that port's config expanded, without switching the current
  terminal. The hover
  background uses the previous frame's `Hovered()` hit test,
  the immediate-mode way to do hover styling.
- **Terminal view**: the selected port's resident terminal fills
  the main column, status line above the command bar (status dot
  + port label + pill input + 发送）, empty state with a button
  opening the settings panel filtered to ports when no ports
  exist.
- **Status line is sticky, and results replace their own
  in-progress line**: the line shows the last message until
  another replaces it. A connect announces `正在连接 <id>…`
  and a disconnect `正在断开 <id>…`; when the port's status
  turns connected (port_status, or a ports broadcast showing
  it connected) the connect line — and only if it is still
  exactly that line — becomes `<id> 连接成功`, and a
  disconnected/errored port turns its disconnect line into
  `<id> 已断开`. Anything newer is never clobbered: another
  operation, another port's line, an error reason from
  onError, or the disconnect auto-stop note (which wins over
  the plain disconnect line because it lands first). Failure
  paths are untouched: a failed connect still reports its
  real reason via onError.
- **Settings is a fixed side panel (Ctrl+,)**: Ctrl+, toggles it
  (also the toolbar's gear button); Esc / × close it and reload
  ALL staging, so cancelled edits never linger. Closing via
  Ctrl+, drops staging exactly like Esc / ×; opening keeps the
  current category filter. The panel is
  a layout sibling, 400px wide and full height: opening it
  narrows the terminal instead of covering it, and its size
  never changes with its content — the body scrolls internally
  while the header/search/icons stay fixed. There is NO footer
  save bar: every section saves where it lives (see below), and
  switching categories also reloads un-saved staging, so an edit
  never leaks into another section. Structure top to bottom:
  ⚙ Settings + ×, search box, category icon row (端口/历史/
  同步/外观 glyphs), numbered sections (01 端口 / 02 历史 /
  03 同步 / 04 外观, numbers fixed per category). Clicking a
  category icon filters to that section (click again for all);
  the search box filters by section number, label, title, and
  description, case-insensitive substring, empty sections hidden.
  Coverage: the frame-level `Context.Shortcut` in render plus
  the terminal `OnKey` fallback, so Ctrl+, fires with the
  terminal focused too. No outside-click dismissal exists
  anymore — the panel is not a modal.
- **Per-section save**: ports rows save in place (per-row 保存
  for config/alias); history edits/imports/clear apply
  immediately; WebDAV has its own 保存配置; appearance has its
  own 保存 button (scrollback, font, terminal theme);
  the app theme toggle saves at once. Each save writes only its
  own section. Closing the panel or switching categories
  reloads every section's staging from saved values (appearance
  staging, WebDAV staging back to the last server-pushed config,
  port drafts, expansion state, any inline history edit).
- **Per-port terminals**: every port gets a resident terminal on
  first select (or first data), kept in a map. WS frames are routed
  by port id, so switching tabs never rebuilds a terminal and never
  loses its screen — the WebUI's per-port xterm model. Terminals
  pump their Conn from creation, so background tabs accumulate
  output before they are ever shown.
- **Command bar**: multi-line TextArea, 1–5 lines (then it
  scrolls) — the keys are intercepted in `OnTextKey`, not via
  `Shortcut` or `Submitted`, which the editor swallows for
  Enter/Up/Down (probe-verified: returning true from
  `OnTextKey` consumes the key before the editor acts, so
  Shift+Enter still inserts a newline while plain Enter does
  not; Esc is not wanted by the editor, so its Shortcut still
  fires). The height is exactly 1–5 rendered lines: `Lines`
  alone does not get there — the theme gives every TextArea a
  MinHeight of 80 DIPs (an empty bar would sit ~4 lines tall),
  and the Lines cap counts the text system's line, which runs
  taller than the lines as rendered on some fonts, so a 6th
  line's glyphs could still show below the fold. `MinHeight(0)`
  drops the floor to the 1-line height and `FixedLineHeight(20)`
  pins the rendered pitch to the line the cap counts
  (probe-verified, headless: 32/52/72/92/112 for 0–5 lines, 112
  past that). Radius 10, like the panel search box — the old
  999 turned a tall bar into a stadium. A dropdown click fills
  the input and puts the caret at the end, as the keyboard
  navigation fill and the WebUI's selectHistoryItem do, so a
  fill longer than 5 lines reveal-scrolls to its last 5. Focus
  stays in the input while the dropdown is open:
  the input, its row, and its column are all keyed, because an
  unkeyed element's id mixes in its sibling index — keying only
  the input was tried first and still lost focus when the list
  opened or closed, since the row's own id flipped with the list
  above it. The dropdown itself carries `KeepFocus()` (the
  combobox pattern). The dropdown list is frozen while
  navigating it, so the rows never move under the selection.
  Keyboard rules: ↑/↓ move the selection between dropdown items
  and fill the input LIVE with the selected match (so it can be
  edited before sending); with no dropdown, ↑/↓ walk the full
  history only when the caret sits at the edge the key points
  at (the WebUI rule — the caret is read during the render and
  the keys settle between frames, so it is current), otherwise
  the caret moves inside the text; a fill puts the caret at the
  end of the filled text, as the WebUI does. Enter sends the
  input's content — after a navigation that is the selected
  match, so one path covers both — clears the input, and moves
  the focus to the serial terminal (the Send button still only
  sends and clears, focus untouched); Enter on a blank bar is
  swallowed. Esc closes ONLY the dropdown with focus still in
  the input (the frame-level panel-Esc yields while the dropdown
  is open, or it would close the panel instead). A row CLICK
  still only fills the input: a click is easy to land by
  accident while aiming at the input, and direct-send on click
  could fire a dangerous command at the device. Each dropdown
  row shows one line only: the first line of the command, with
  " …" when it runs on, and an overlong first line is cut with
  its own ellipsis. Sends convert inner newlines to CRLF (every
  line CRLF-terminated, never a bare LF — the WebUI still sends
  inner newlines as bare LF) and append the fixed CRLF line
  ending (see below), and are recorded with `add_command`. When
  the port is not connected, the send auto-connects it instead
  of the old `[Not connected]` in-terminal dead end — see
  "Auto-connect on typing" below.
- **Ports section**: one two-line row per port — line 1 alias
  (larger, bright) + dot, line 2 detail (smaller, muted:
  path · baud frame · vendor) with 配置/别名/连接·断开 at the
  second line's right; the detail text takes the remaining width
  and wraps, so a long vendor name cannot push the buttons out
  of the panel. Inline expandable 配置 selects + save and 别名
  field + save, add-port modal, refresh, reconnect. Clicking a
  port's name switches the current terminal to it and the panel
  stays open (side-panel semantics — the terminal sits beside
  it). Add-port really creates the port server-side via
  `add_port` (the WebUI modal only adds it locally); a typed
  `/dev/ttyUSB0` is stripped first because the server prefixes
  `/dev/` itself.
- **History section**: server history (`get_history` /
  `history_update`), search filter, per-item 发送 (straight to
  the selected port's serial line — no detour through the command
  bar; a failure goes to the status line) / 编辑 (the row becomes
  a multi-line inline editor with its own save/cancel buttons;
  Enter inserts a newline, the 保存 button saves, Esc cancels
  back to the original value — the editor never takes Esc, so
  the element-level shortcut still fires) / 删除
  (`delete_command`), export JSON (native save dialog
  defaulting to the home directory; cancel or dialog error
  writes nothing, never falls back to the default file),
  import JSON (merge or replace),
  two-step clear. Clear deletes each entry by id: the server has
  no clear-all message, and `replace_history` with an empty list
  is a no-op there. Each row is vertical: monospace text that
  wraps at the full width on top, its buttons (发送 / 编辑 /
  删除) on a row below, left-aligned, with the 1px hairline
  separators between rows kept. The editor is a TextArea that
  grows with its wrapped text up to 6 lines, then scrolls; mygo
  paints its scroll thumb on hover/drag only — there is no
  always-visible scrollbar API, and none is faked. The command
  bar's input is multi-line as well (1–5 lines, see above). Save passes the text
  unchanged, so multi-line commands keep their newlines, not one
  inner card per row. Button groups wrap to two rows instead of
  clipping at the panel edge.
- **Sync section**: WebDAV config form (URL, username, password,
  interval, max backups) with save (`set_webdav_config`) and
  clear-password; the server never returns the password, only
  whether one is set, so an empty password field on save keeps
  the stored one. Test connection pushes the form values to the
  server first (with an ack wait, so the probe cannot race the
  push), then probes the server's saved configuration via
  `/api/webdav/test` — the WebUI's path. The old direct PROPFIND
  from the app always failed 401 when the password field was left
  blank, because the panel never loads the saved password.
  Pushing also saves the form values, exactly like the WebUI
  test does. 立即备份 uploads the current history now via a
  `backup_history_now` WS message (the server prunes to
  MaxBackups); its result lands on the status line.
- **Cloud backup list** (sync section, below the config form):
  lists the server's `history_backup_*` files via
  `/api/webdav/list` (newest first), fetched once when the sync
  tab first renders and again on 刷新 or after an upload,
  delete, or import. Each row is vertical like the history rows:
  filename on top, date · size below, actions below that:
  导入（合并） / 导入（替换） / 删除. Import downloads via
  `/api/webdav/download` and validates the commands array;
  merge preserves entry ids so the deletion tombstones apply
  (see below), replace restores the file exactly and clears the
  restored ids' tombstones (a deliberate un-delete). Delete is
  two-step (first click arms "确认删除？", second executes) and
  touches only the cloud copy. Loading, empty, and failed states
  all have their own text; failures also land on the status line.
- **History deletion tombstones** (server,
  `internal/web/history_tombstones.go`): deleting a history entry
  records its id in `history_tombstones.json` next to
  `history.json`; every merge (startup cloud merge, the merge
  after saving WebDAV settings, `merge_history` imports) skips
  tombstoned ids, so a stale cloud backup can no longer
  resurrect a deleted entry. Tombstones are capped at 5000
  (oldest evicted first) and keyed by id, not text — re-sending
  the same command text creates a new id and is not blocked.
  Delete also triggers a coalesced cloud upload ~3s later so the
  newest backup converges to the post-delete list (best-effort
  only; the tombstone, not the upload, is the guarantee).
  `replace_history` is NOT filtered: it is an explicit restore
  and clears the restored ids' tombstones. Lock discipline:
  removal and tombstone write happen in a single `historyMu`
  hold (a merge can therefore never land between them);
  broadcast, disk save, and the backup nudge run after unlock,
  and the `historyChanged` flag is atomic because handler
  goroutines set it while the interval loop reads and clears it.
- **History row hairlines** use `Height(1)` +
  `AlignSelf(Stretch)` + `Shrink(0)`, never `Fill()`: Fill sets
  width AND height to percent(100), overwriting the 1px height,
  and a percentage height against the auto-height Column
  resolves to 0 — which is why the old line never painted. The
  same construction is used for the backup list rows. (The ports
  section's older hairline still uses the Fill construction and
  has the same defect; it is left alone pending a decision.)
- **Appearance section**: TWO card groups. 外观: interface theme
  toggle (immediate + saved) plus terminal theme (dark/light/
  blue/green, the WebUI's four), font (see below), font size.
  终端行为: scrollback lines, autoScroll, with this
  section's own 保存 button. The old standalone terminal section
  is merged here; the line-ending setting is deleted entirely —
  "发送换行符" and "行尾" were the same single `LineEnding`
  field, so per the feedback the whole item is gone and sending
  always appends CRLF, the old default (old saved config files
  with a `lineEnding` key still load: JSON ignores unknown
  fields). Settings persist to
  `~/.config/go-serial-over-socket/gui-native.json` (the WebUI
  uses localStorage); font size / family / terminal theme apply
  live to all terminals via `SetFont`/`SetTheme`.
- **Terminal scrolling is plugin-native**: wheel and
  Shift+PageUp/Down scroll the scrollback (stay when scrolled
  up, follow at the bottom); terminals carry no `OnScroll`
  callback and no key interception for scrolling, so nothing in
  the app can freeze the viewport anymore. The scrollbar beside
  the terminal is a position indicator only: the plugin paints
  it only while scrolled up (hidden at the bottom) and
  implements no drag handling, and `Terminal` exposes no scroll
  position or scroll-to-bottom API, so it cannot be made
  draggable from the app layer — scroll with the wheel or
  Shift+PageUp/Down. The old AutoScroll checkbox is deleted for
  the same reason: its follow-lock vetoed wheel and
  Shift+PageUp/Down in the terminal callbacks, which froze the
  viewport while the switch defaulted ON (that was the reported
  "wheel dead, scrollbar undraggable" bug — the scrollbar was
  never draggable, and the wheel was vetoed, not broken), and
  without a plugin scroll-to-bottom call there is no honest
  "force back to bottom on new data" implementation — no
  reflection/unsafe poking at unexported fields, no resize
  tricks, and no dead switch left in the UI. Old saved config
  files with an `autoScroll` key still load: JSON ignores
  unknown fields.
- **Font picker**: system fonts only, flat list, no presets — the
  settled direction. Enumerated with `fc-list :spacing=mono` (a
  small local copy of the previous webview client's approach).
  When enumeration yields nothing,
  the field falls back to free text.
- **Auto-connect on typing**: typing into a disconnected
  terminal (or sending from the command bar / history) does not
  show `[Not connected]` anymore — it auto-connects that port.
  The triggering input is buffered (64 KiB per port, excess
  dropped) and flushes in order once the port reports connected;
  while a connect is outstanding, further input only appends,
  never re-initiates. A failed connect pops no dialog and no
  toast: the server's error message lands in the status line,
  the port's dot goes red by the existing error rule, and the
  buffered bytes are kept, so the next keystroke retries. This
  is inherently focused-port-only (keystrokes only reach the
  focused terminal's Conn); the explicit connect/disconnect
  buttons and menus are unchanged. Without a server connection
  there is nothing to connect to, so the send fails into the
  status line instead.
- **Per-port recording (pill ● button)**: each pill's button
  zone has a record button (muted when idle, amber while that
  port records; tooltip "开始记录" / "停止记录" per live
  state; dimmed and disabled with the plain tip "记录"
  while the port has no live connection). Clicking it toggles
  ONLY that port's client-side recording and never switches
  the terminal; several ports record concurrently, each to
  its own local file, and the selected port's recording
  additionally shows a strip above its terminal (local
  filename + size, refreshed by rendering — there is no
  server round trip and no poll). Raw serial frames are
  appended verbatim to the file as they arrive, whether or
  not the port is the visible tab. Starting always pops the
  native save dialog first (default
  `serial_<port>_<timestamp>.log` in the home directory),
  in local and remote mode alike — the file is always local
  now; cancelling the dialog does NOT start recording —
  there is no fallback. Stopping never prompts and reports
  the local path and size. The server has no recording
  mechanism at all anymore (the old `start/stop_logging` /
  `get_log_status` commands and the per-connection record
  files are deleted); the WebUI and the TUI never used them
  and are unaffected. Lifetime: recording binds to the GUI
  process, not to the server connection — a live recording
  survives a WS reconnect (frames keep flowing from the new
  connection into the same file), and every file is flushed
  and closed on quit. It does NOT resume after a GUI
  restart: the state lives in this process only.
  Disconnect stops recording: when a port goes away (explicit
  disconnect, a port_status disconnected/error broadcast, or a
  ports broadcast showing it gone), the GUI flushes and
  closes that port's local file and reports "<port> 已断开，
  记录已自动停止" in the status line; reconnect never
  resumes a stopped recording.
- **保存输出 (toolbar button and Ctrl+S, one action)**: saves
  the visible terminal's screen + scrollback text
  (`Terminal.Text()`, the native equivalent of the WebUI's
  selectAll/getSelection). It always shows the native save
  dialog (default file `serial_<port>_<timestamp>.txt` in the
  home directory); cancelling or closing the dialog writes
  NOTHING — the old export fallback that wrote the default
  path anyway is gone. Ctrl+S is intercepted at the plugin
  `OnKey` level per terminal and never reaches the device as
  XOFF (the PR #14 rule). The old Ctrl+S raw 1 MiB snapshot
  save is removed: the merged action saves rendered text only,
  and the port Conn no longer keeps a snapshot buffer.


### Approximate (deliberate, noted in code)

- **No gradient anymore**: the Tern-style layout drops the window
  gradient entirely — the terminal background is the window, and
  only the panel and its flat cards carry GoRex's translucent
  surface/border tokens (via `#rrggbbaa` hex). Panel cards are
  flat (border only, no shadow) per the Tern rule.
- **Toolbar and panel glyphs are text** (⚙ ↻ ⇄ 🔌 🕘 ☁ ◐),
  not drawn tiles. The old ⌨ terminal glyph is gone with the
  merged section.
- **Category icons filter, they do not scroll-to**: v0.4.8's
  `ScrollState.ScrollTo` takes absolute offsets, which cannot be
  known for content laid out below without measuring it, so
  clicking an icon shows only that section (click again for
  all) instead of scrolling to it. The search box is a
  case-insensitive substring filter over section number, label,
  title, and description — not a fuzzy matcher.
- **Toolbar hover uses the previous frame's hit test**: the hover
  background appears one frame after the pointer arrives; on the
  first frame (or headless) there is simply no hover background.
- **Esc closes the panel with Shortcut semantics, except the two
  cases that own Esc**: with the command-bar dropdown open, Esc
  closes only the dropdown (render's panel-Esc yields, or it
  would consume the key first and close the panel instead); with
  an inline history edit active, Esc cancels the edit in place
  (the editor autofocuses on appearance). One wart: if you click
  away from the inline editor first, Esc does nothing until you
  click back into it (or press its 取消 button) — render yields
  the key, but the unfocused editor's own Esc handler cannot
  claim it.
- **Add-port keeps the native `ui.Modal` styling**, not Rex
  panels; history editing is inline, not a modal.
- **Panel docking, toolbar layout, hover, and the right-click
  menu are not verifiable headless** — check them on device: the
  headless Tester renders the texts, but not whether the flex
  Row actually gives the main column the remaining width beside
  a fixed 400px panel, whether many pill tabs really compress
  into one row without a scrollbar, whether the OS shows the
  native tooltips, or whether the OS pops the menu.
- **Command bar is multi-line now**: a TextArea, 1–5 lines, with
  Enter sending (and moving the focus to the terminal),
  Shift+Enter breaking the line, and ↑/↓ splitting on the
  caret position as the WebUI does. One remaining difference:
  the WebUI sends inner newlines as bare LF; the GUI converts
  them to CRLF. Long entries are no longer fully readable in
  the dropdown: each row shows one line only (see above).
- **Flat buttons are borderless by construction, not by theme**:
  the theme button's outline is killed with an explicit
  `Border(0, transparent)` (and a manual `Hovered()` fill, since
  the theme's style function bails out once the background is
  overridden). Native save dialogs and system menus are exempt —
  they are not app buttons. Whether the borders are really gone
  on device is a visual check, not something the headless
  Tester can prove.
- **Ports rows are a vertical list**, not a CSS auto-fit grid:
  the native layout has no auto-fit grid.
- **Port alias/config save is button-driven**, not on-change as
  the WebUI's card inputs are.
- **Scrollback lines are converted to plugin bytes** at roughly
  160 bytes/line, and only apply to terminals created after saving.
- **History dropdown navigation** follows the WebUI's split:
  with the dropdown open ↑/↓ move the selection (filling the
  input live); without it, ↑/↓ walk the full history only when
  the caret sits at the edge the key points at, otherwise the
  caret moves inside the text.
- **Background tabs are laid out at 80×24 until first shown**,
  then resized to fit; the emulator reflows on show.
- **WebDAV test goes through the server** (push form values with
  an ack wait, then `/api/webdav/test`), not a direct PROPFIND
  from the app: the saved password never reaches the panel, so a
  direct probe could only test a re-typed password.
- **Export/import fall back to the home directory** when the
  native dialog is cancelled or unavailable.
- **Clear-screen** feeds the emulator `\x1b[2J\x1b[H` plus the
  WebUI's "Terminal cleared" line (the plugin has no clear API in
  v0.4.8); nothing is sent to the device.

### Deleted, not postponed

Features that cannot be implemented were removed from the UI
rather than kept as dead switches or stub buttons:

- **In-terminal search**: v0.4.8's terminal plugin has no search
  API (it arrived on the plugin's main branch later). The
  toolbar search button was deleted; it comes back only with a
  real search implementation.
- **copyOnSelect**: blocked, not faked. The plugin has no
  selection-finished callback, no selection-text accessor, and
  its view state is unexported — and a wrapping Box's input
  handler cannot help, because the innermost handler wins and
  the selection text stays unreadable; reimplementing selection
  from `Text()` + mouse geometry would be fragile (cell metrics
  are view-internal) and would break link-clicking and mouse
  programs; patching mygo via a go.mod replace is out of scope
  for this client (it would fork the dependency). The setting,
  its staging, and its checkbox were all deleted (an old config
  file's key loads as an unknown field and is ignored). What
  works today, unchanged and plugin-native: Ctrl+C / right-click
  → Copy copies the selection.

### Not done
- **PWA / browser-only pieces**: update banner, service worker,
  file-bridge-only flows — not applicable to a native app.
- **Styling**: native buttons/inputs use the OS theme, not the
  WebUI's CSS; only the app's own containers and text carry the
  WebUI palette.

## Verification (headless VM, no display)

- `go build -o /tmp/sos-gui ./cmd/gui`, `go vet`,
  `gofmt` clean, `go test -race ./cmd/gui` (including a
  headless `ui.NewTester` render pass over the slim toolbar port
  row, the four settings sections, the category/search filters,
  and the empty state; a driven command-bar flow test — type →
  dropdown → Down → Enter direct-send → retype → Esc → retype,
  asserting focus survives every open/close cycle; plus
  detail-label / frame-format / autoScroll-veto / dot-color /
  sections / search-match / toggle-action / panel-state /
  panel-staging-reload / inline-edit-state / default-line-ending
  unit tests), `go build ./...`, and
  `GOOS=darwin/arm64` + `GOOS=windows/amd64` cross-compiles.
- **Not real-device verified**: window, focus, native dialogs
  and context menus, panel docking beside the terminal, toolbar
  hover, pill-tab compression with many ports, tooltip display,
  the record save-dialog flow, pill record-dot clicks (a dot
  click must toggle recording without switching the terminal),
  flat-button borderless
  rendering, and real serial traffic on a machine with a display
  are still pending — the earlier v1 checks (terminal, Ctrl+S,
  Send) were verified on device, the scheme-A, full-card,
  floating, and Tern-style passes were not. In particular, check
  on device: Ctrl+,; the toolbar pill tabs' left-click switch
  and right-click connect/disconnect menu; the flex Row actually
  giving the main column the remaining width beside a fixed 400px
  panel; the command-bar dropdown's focus/keyboard behavior on a
  real OS; the inline history editor's Enter-newline/Esc-cancel
  and its hover-reveal scroll thumb; pill-click focus into the
  terminal; wheel and
  Shift+PageUp/Down scrolling and the scrollbar indicator (the
  follow-lock veto is deleted — verify the wheel actually moves
  the viewport now); the lamp states on device (disconnected
  stop red, connecting amber, connected green still, recording
  green step-flash, error red) and that panel/command-bar dots
  share the colors without flashing; disconnect auto-stop
  (status line names the port, no resume on reconnect); and
  Ctrl+, closing the panel and dropping un-saved staging; and
  whether panel text really never overflows at 400px (the
  headless Tester asserts texts render, not measured widths).
  The APIs exist in v0.4.8 and the headless tests pass, but that
  cannot prove the OS delivers the key events, shows the menus,
  or lays the Rows out the same way.

## Packaging note

`libghostty-vt` is loaded by the plugin at runtime: under `go run`
and `go test` it downloads itself into the user cache
(`~/.cache/mygo/natives/`). A packaged app never downloads it — it
must be built with `mygo build`, which bundles the library. This
branch does not do packaging.
