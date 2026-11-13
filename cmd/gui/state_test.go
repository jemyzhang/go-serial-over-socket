package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubLocalServer marks an app as local-mode (embedded server
// present) for tests. Recording pops the save dialog in both
// modes now; the stub only matters for local/remote display.
type stubLocalServer struct{}

func (stubLocalServer) URL() string { return "http://127.0.0.1:2025" }
func (stubLocalServer) Shutdown(ctx context.Context) error {
	return nil
}

func TestHistoryMatches(t *testing.T) {
	entries := []HistoryEntry{
		{ID: "1", Command: "AT+VERSION"},
		{ID: "2", Command: "at+reset"},
		{ID: "3", Command: "STATUS"},
		{ID: "4", Command: "AT+BAUD=115200"},
	}
	got := historyMatches("at+", entries)
	// WebUI rule: filter (case-insensitive), last 10, reversed.
	want := []string{"AT+BAUD=115200", "at+reset", "AT+VERSION"}
	if len(got) != len(want) {
		t.Fatalf("historyMatches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("historyMatches[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
	if historyMatches("  ", entries) != nil {
		t.Error("blank query should give no matches")
	}
	if historyMatches("zzz", entries) != nil {
		t.Error("non-matching query should give no matches")
	}
	// More than 10 matches: only the last 10 survive, reversed.
	var many []HistoryEntry
	for i := 0; i < 12; i++ {
		many = append(many, HistoryEntry{ID: string(rune('a' + i)), Command: "cmd" + string(rune('a'+i))})
	}
	got = historyMatches("cmd", many)
	if len(got) != 10 || got[0] != "cmdl" || got[9] != "cmdc" {
		t.Errorf("historyMatches(12 matches) = %v, want 10 items from cmdl down to cmdc", got)
	}
}

func TestDraftFromInfoDefaults(t *testing.T) {
	d := draftFromInfo(PortInfo{ID: "ttyUSB0"})
	if d.baud != "115200" || d.databits != "8" || d.stopbit != "1" || d.parity != "N" {
		t.Errorf("defaults = %+v, want 115200/8/1/N", d)
	}
	d = draftFromInfo(PortInfo{Baudrate: 9600, Databits: 7, Stopbit: 2, Parity: "E", Alias: "sensor"})
	if d.baud != "9600" || d.databits != "7" || d.stopbit != "2" || d.parity != "E" || d.alias != "sensor" {
		t.Errorf("from info = %+v", d)
	}
}

func TestConfigFromDraft(t *testing.T) {
	baud, data, stop, parity := configFromDraft(nil, PortInfo{})
	if baud != 115200 || data != 8 || stop != 1 || parity != "N" {
		t.Errorf("nil draft = %d/%d/%v/%s", baud, data, stop, parity)
	}
	baud, data, stop, parity = configFromDraft(
		&portDraft{baud: "57600", databits: "7", stopbit: "2", parity: "O"},
		PortInfo{Baudrate: 9600},
	)
	if baud != 57600 || data != 7 || stop != 2 || parity != "O" {
		t.Errorf("draft overrides = %d/%d/%v/%s", baud, data, stop, parity)
	}
	// Invalid draft values fall back to the reported config.
	baud, data, stop, parity = configFromDraft(
		&portDraft{baud: "junk", databits: "0", stopbit: "x", parity: ""},
		PortInfo{Baudrate: 19200, Databits: 8, Stopbit: 1, Parity: "E"},
	)
	if baud != 19200 || data != 8 || stop != 1 || parity != "E" {
		t.Errorf("invalid draft fallback = %d/%d/%v/%s", baud, data, stop, parity)
	}
}

func TestOnDataRoutesByPort(t *testing.T) {
	fake := &fakeSender{}
	connA := newPortConn(fake, "ttyUSB0")
	connB := newPortConn(fake, "ttyUSB1")
	app := &nativeApp{
		selected: "ttyUSB0",
		terms: map[string]*termSession{
			"ttyUSB0": {conn: connA},
			"ttyUSB1": {conn: connB},
		},
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0"},
			"ttyUSB1": {ID: "ttyUSB1"},
		},
	}
	// Background tab's data must reach its own Conn even though
	// another tab is selected — the old single-Conn behavior dropped it.
	app.onData("ttyUSB1", []byte("bg"))
	buf := make([]byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		n, err := connB.Read(buf)
		if err != nil || string(buf[:n]) != "bg" {
			t.Errorf("connB Read = (%q, %v), want (\"bg\", nil)", buf[:n], err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading background port data")
	}
	// Unknown ports are dropped, not given a zombie terminal.
	app.onData("ttyUSB9", []byte("ghost"))
	if _, ok := app.terms["ttyUSB9"]; ok {
		t.Error("onData created a terminal for an unknown port")
	}
}

func TestNavigateCmdHistory(t *testing.T) {
	app := &nativeApp{
		history: []HistoryEntry{
			{ID: "1", Command: "newest"},
			{ID: "2", Command: "older"},
			{ID: "3", Command: "oldest"},
		},
		cmdHistIdx: -1,
		histNavIdx: -1,
	}
	// Dropdown navigation fills the input live and wraps.
	app.cmdList = []string{"m1", "m2"}
	if got := app.navigateDropdown(1); got != "m1" || app.cmdHistIdx != 0 || app.cmdInput != "m1" {
		t.Fatalf("down into matches: got = %q idx = %d input = %q, want m1/0/m1", got, app.cmdHistIdx, app.cmdInput)
	}
	app.navigateDropdown(1)
	app.navigateDropdown(1)
	if app.cmdHistIdx != 0 || app.cmdInput != "m1" {
		t.Fatalf("matches wrap: idx = %d input = %q, want 0/m1", app.cmdHistIdx, app.cmdInput)
	}
	// Full history, no dropdown.
	app.cmdList = nil
	app.cmdHistIdx = -1
	app.navigateCmdHistory(-1)
	if app.cmdInput != "newest" || app.histNavIdx != 0 {
		t.Fatalf("up in full history: input = %q idx = %d", app.cmdInput, app.histNavIdx)
	}
	app.navigateCmdHistory(-1)
	if app.cmdInput != "older" {
		t.Fatalf("up again: input = %q, want older", app.cmdInput)
	}
	app.navigateCmdHistory(1)
	app.navigateCmdHistory(1)
	if app.cmdInput != "" || app.histNavIdx != -1 {
		t.Fatalf("down past newest: input = %q idx = %d", app.cmdInput, app.histNavIdx)
	}
}

// The line-ending setting is gone (it was the same item as
// "发送换行符"): sending always appends the old default, CRLF.
func TestDefaultLineEnding(t *testing.T) {
	if defaultLineEnding != "\r\n" {
		t.Errorf("defaultLineEnding = %q, want CRLF", defaultLineEnding)
	}
}

func TestParseFcListFamilies(t *testing.T) {
	got := parseFcListFamilies("DejaVu Sans Mono,DejaVu Sans Mono Book\nJetBrains Mono\nJetBrains Mono,JetBrainsMono Nerd Font\n")
	want := []string{"DejaVu Sans Mono", "DejaVu Sans Mono Book", "JetBrains Mono", "JetBrainsMono Nerd Font"}
	if len(got) != len(want) {
		t.Fatalf("parse = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parse[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(strings.Join(got, ","), "JetBrains") {
		t.Error("expected JetBrains family in output")
	}
}

// TestViewRendersHeadless renders the Tern layout through the
// headless Tester: no window is involved, but the builders run end
// to end (toolbar port row, command bar, settings sections) and
// must not panic.
func TestViewRendersHeadless(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N", Description: "USB Serial"},
		},
		portIDs:      []string{"ttyUSB0"},
		selected:     "ttyUSB0",
		terms:        map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings:     defaultSettings(),
		panelSection: "",
		status:       "服务连接成功",
		history: []HistoryEntry{
			{ID: "h1", Command: "AT+VERSION"},
			{ID: "h2", Command: "STATUS?"},
		},
		drafts: map[string]*portDraft{},
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	// Slim toolbar: each port is one pill tab holding the name
	// and the short config (baudrate only) — the full detail
	// lives in the pill's hover tooltip, not in visible text —
	// plus scheme A buttons inside each pill (power connect, record,
	// settings icon) and icon actions (search,
	// download, settings) on the right, and the command bar send. The
	// icon buttons are SVGs (see icons.go), so they carry no
	// text for HasText to see — only real labels are asserted
	// here (the theme button is an SVG too but lives in the panel, not
	// in this view). Final
	// toolbar action set: save output / gear (recording moved
	// into the pills, one button per port) — no clear button
	// (the terminal's native right-click menu clears), no
	// separate connect/disconnect button (each pill's power button
	// and the panel's port rows own that), no pill right-click menu.
	for _, want := range []string{"sensor", "· 115200", "发送"} {
		if !tt.HasText(want) {
			t.Errorf("terminal view missing %q; texts = %v", want, tt.Texts())
		}
	}
	// The full detail and the action words must not be visible
	// text anymore: the detail is tooltip-only, and the record /
	// save actions are icon-only with tooltip words (tooltips
	// are not label texts, so they cannot trip this check).
	for _, bad := range []string{"/dev/ttyUSB0 · 115200 8N1", "开始记录", "保存输出"} {
		if tt.HasText(bad) {
			t.Errorf("terminal view should not show %q as visible text; texts = %v", bad, tt.Texts())
		}
	}
	for _, bad := range []string{"开始日志", "清屏"} {
		for _, txt := range tt.Texts() {
			if strings.Contains(txt, bad) {
				t.Errorf("rendered text %q contains deleted toolbar item %q", txt, bad)
			}
		}
	}
	if tt.HasText("sos-gui-native") {
		t.Errorf("host chip should be gone; texts = %v", tt.Texts())
	}
	// Settings panel, all four sections: numbered sections and
	// titles, the appearance section's own Save button — and NO
	// panel-wide footer bar anymore (no "取消" anywhere).
	app.settingsOpen = true
	tt.Frame()
	for _, want := range []string{"Settings", "01 · 端口", "端口与连接", "02 · 历史", "命令历史", "03 · 同步", "WebDAV 同步", "04 · 外观", "外观与终端行为", "保存"} {
		if !tt.HasText(want) {
			t.Errorf("settings panel missing %q; texts = %v", want, tt.Texts())
		}
	}
	if tt.HasText("05 · 终端") {
		t.Errorf("terminal section should be merged into appearance; texts = %v", tt.Texts())
	}
	if tt.HasText("取消") {
		t.Errorf("footer save/cancel bar should be gone; texts = %v", tt.Texts())
	}
	// Category filter: history only — ports section gone, but
	// the command bar still shows the port label.
	app.panelSection = settingsTabHistory
	tt.Frame()
	if !tt.HasText("AT+VERSION") {
		t.Errorf("filtered panel missing history entry; texts = %v", tt.Texts())
	}
	if tt.HasText("端口与连接") {
		t.Errorf("filtered panel should hide other sections; texts = %v", tt.Texts())
	}
	// Search filter: "波特率" matches only the ports section.
	app.panelSection = ""
	app.panelSearch = "波特率"
	tt.Frame()
	if !tt.HasText("端口与连接") {
		t.Errorf("search should keep the ports section; texts = %v", tt.Texts())
	}
	if tt.HasText("命令历史") {
		t.Errorf("search should hide non-matching sections; texts = %v", tt.Texts())
	}
	app.panelSearch = "zzzz-no-match"
	tt.Frame()
	if !tt.HasText("没有匹配的设置") {
		t.Errorf("empty search should say so; texts = %v", tt.Texts())
	}
	app.panelSearch = ""
	app.settingsOpen = false
	// Empty state: no ports at all.
	app.ports = map[string]PortInfo{}
	app.portIDs = nil
	tt.Frame()
	if !tt.HasText("暂无可用的终端端口") {
		t.Errorf("empty state missing; texts = %v", tt.Texts())
	}
}

// The toolbar's port row shows the alias as the item text; the
// detail after the row is path · baud frame only (no vendor, no
// status text — the dot carries the state).
func TestPortDetailLabel(t *testing.T) {
	got := portDetailLabel(PortInfo{ID: "ttyUSB0", Alias: "D766", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"})
	if want := "/dev/ttyUSB0 · 115200 8N1"; got != want {
		t.Errorf("detail aliased = %q, want %q", got, want)
	}
	got = portDetailLabel(PortInfo{ID: "ttyUSB1", Baudrate: 9600, Databits: 8, Stopbit: 1, Parity: "N"})
	if want := "/dev/ttyUSB1 · 9600 8N1"; got != want {
		t.Errorf("detail bare = %q, want %q", got, want)
	}
	// Unknown baud/frame: path only, never a guessed default.
	got = portDetailLabel(PortInfo{ID: "ttyACM0"})
	if want := "/dev/ttyACM0"; got != want {
		t.Errorf("detail unknown = %q, want %q", got, want)
	}
	// No status text in any detail, whatever the state.
	for _, info := range []PortInfo{
		{ID: "ttyUSB0", Status: "connected", Connected: true},
		{ID: "ttyUSB1", Status: "disconnected"},
		{ID: "ttyUSB2", Status: "connecting"},
	} {
		lbl := portDetailLabel(info)
		for _, bad := range []string{"已连接", "未连接", "连接中"} {
			if strings.Contains(lbl, bad) {
				t.Errorf("detail %q contains status text %q", lbl, bad)
			}
		}
	}
}

func TestFrameFormat(t *testing.T) {
	if got := frameFormat(PortInfo{Databits: 8, Stopbit: 1, Parity: "N"}); got != "8N1" {
		t.Errorf("frame = %q, want 8N1", got)
	}
	if got := frameFormat(PortInfo{Databits: 7, Stopbit: 2, Parity: "E"}); got != "7E2" {
		t.Errorf("frame = %q, want 7E2", got)
	}
	if got := frameFormat(PortInfo{Databits: 8, Parity: "N"}); got != "" {
		t.Errorf("missing stopbit should give empty frame, got %q", got)
	}
	if got := frameFormat(PortInfo{}); got != "" {
		t.Errorf("empty info should give empty frame, got %q", got)
	}
}

// Terminal key interceptions after the AutoScroll follow-lock
// removal: only Ctrl+S (save) and Ctrl+, (settings) are handled
// by the app; everything else — notably Shift+PageUp/Down —
// passes through to the plugin's native scrollback scrolling.
// (The old veto helpers are deleted; there is no wheel veto left
// to test, and terminals carry no OnScroll callback anymore.)
func TestTerminalKeyHandled(t *testing.T) {
	app := &nativeApp{}
	if !app.terminalKeyHandled("ttyUSB0", ui.Ctrl, ui.KeyS) {
		t.Error("Ctrl+S must be handled (save output)")
	}
	if !app.terminalKeyHandled("ttyUSB0", ui.Ctrl, ui.KeyComma) {
		t.Error("Ctrl+, must be handled (open settings)")
	}
	if !app.settingsOpen {
		t.Error("Ctrl+, must open the settings panel")
	}
	for _, tc := range []struct {
		name string
		mods ui.Modifiers
		key  ui.Key
	}{
		{"Shift+PageUp scrolls natively", ui.Shift, ui.KeyPageUp},
		{"Shift+PageDown scrolls natively", ui.Shift, ui.KeyPageDown},
		{"plain PageUp goes to the program", 0, ui.KeyPageUp},
		{"Shift+Up is not intercepted", ui.Shift, ui.KeyUp},
		{"plain character goes to the program", 0, ui.KeyA},
	} {
		if app.terminalKeyHandled("ttyUSB0", tc.mods, tc.key) {
			t.Errorf("%s: must not be intercepted by the app", tc.name)
		}
	}
}

// An old config file's autoScroll key must load without error
// and without effect: the field is deleted, so JSON ignores it
// as an unknown field, and freshly saved settings never contain
// it again.
func TestSettingsIgnoresAutoScrollKey(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"autoScroll":true,"dark":false}`), &s); err != nil {
		t.Fatalf("old autoScroll key must be ignored, got error: %v", err)
	}
	data, err := json.Marshal(defaultSettings())
	if err != nil {
		t.Fatalf("marshal default settings: %v", err)
	}
	if strings.Contains(string(data), "autoScroll") {
		t.Errorf("saved settings must not contain autoScroll, got %s", data)
	}
}

// CopyOnSelect defaults on, and an old settings file without the
// key must keep it on: the field is new, so a plain bool decode
// would read it as false and silently switch the feature off for
// every existing user. Only an explicit false turns it off — the
// same presence-check pattern as dark.
func TestCopyOnSelectDefaultsOn(t *testing.T) {
	if !defaultSettings().CopyOnSelect {
		t.Error("defaultSettings().CopyOnSelect must be true")
	}
	data, err := json.Marshal(defaultSettings())
	if err != nil {
		t.Fatalf("marshal default settings: %v", err)
	}
	if !strings.Contains(string(data), `"copyOnSelect":true`) {
		t.Errorf("saved defaults must contain copyOnSelect:true, got %s", data)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Fresh install: no file at all.
	if got := loadSettings(); !got.CopyOnSelect {
		t.Error("fresh install must load CopyOnSelect on")
	}
	// Old file without the key: still on, other keys still load.
	writeSettingsFile(t, home, `{"dark":false,"fontSize":16}`)
	if got := loadSettings(); !got.CopyOnSelect {
		t.Error("old file without copyOnSelect must keep it on")
	}
	if got := loadSettings(); got.Dark {
		t.Error("old file's explicit dark:false must still load")
	}
	// Explicit false turns it off; explicit true keeps it on.
	writeSettingsFile(t, home, `{"copyOnSelect":false}`)
	if got := loadSettings(); got.CopyOnSelect {
		t.Error("explicit copyOnSelect:false must load as off")
	}
	writeSettingsFile(t, home, `{"copyOnSelect":true}`)
	if got := loadSettings(); !got.CopyOnSelect {
		t.Error("explicit copyOnSelect:true must load as on")
	}
}

func writeSettingsFile(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "go-serial-over-socket")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gui-native.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write settings file: %v", err)
	}
}

func TestCopyOnSelectMode(t *testing.T) {
	if got := copyOnSelectMode(true); got != terminal.CopyOnSelectClipboard {
		t.Errorf("copyOnSelectMode(true) = %v, want CopyOnSelectClipboard", got)
	}
	if got := copyOnSelectMode(false); got != terminal.CopyOnSelectNone {
		t.Errorf("copyOnSelectMode(false) = %v, want CopyOnSelectNone", got)
	}
}

// TestCopyOnSelectLiveToggle drives the whole path headlessly: a
// terminal created with the setting on copies Select All to the
// clipboard; flipping the appearance section's switch off and
// saving applies SetCopyOnSelect to the live terminal at once, so
// the next Select All copies nothing. The mouse-drag release path
// shares the same CopyOnSelect gate, but its coordinates need a
// real display to locate the text; Select All exercises the gate
// without them.
func TestCopyOnSelectLiveToggle(t *testing.T) {
	// Create the terminal BEFORE redirecting HOME: terminal.New
	// loads libghostty-vt from the user cache, and a temp HOME
	// would make it try to download the library instead.
	app := &nativeApp{
		ports:    map[string]PortInfo{"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"}},
		portIDs:  []string{"ttyUSB0"},
		selected: "ttyUSB0",
		terms:    map[string]*termSession{},
		settings: defaultSettings(),
		status:   "服务连接成功",
		drafts:   map[string]*portDraft{},
		// Staging mirrors settings, as newNativeApp sets it.
		setScrollF:      10000,
		setFontSizeF:    14,
		setTermTheme:    "dark",
		setCopyOnSelect: true,
	}
	sess := app.ensureTerm("ttyUSB0")
	if sess == nil {
		t.Fatal("ensureTerm returned nil")
	}
	defer sess.term.Close()
	// Redirect HOME only for the save below, so the settings
	// file lands in a temp dir instead of the real one.
	t.Setenv("HOME", t.TempDir())
	sess.term.Feed([]byte("HELLOSELECT\r\n"))
	if got := sess.term.Text(); !strings.Contains(got, "HELLOSELECT") {
		t.Fatalf("fed text not on screen: %q", got)
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	selectAll := func() {
		t.Helper()
		// Mid-window is inside the terminal view.
		tt.RightClickAt(600, 400)
		found := false
		for _, item := range tt.Menu() {
			if item == "Select All" {
				found = true
			}
		}
		if !found {
			t.Fatalf("terminal context menu = %v, want Select All", tt.Menu())
		}
		if err := tt.ChooseMenuItem("Select All"); err != nil {
			t.Fatalf("choose Select All: %v", err)
		}
		tt.Frame()
	}
	selectAll()
	if got := tt.Clipboard(); !strings.Contains(got, "HELLOSELECT") {
		t.Errorf("Select All with copy-on-select on copied %q, want HELLOSELECT", got)
	}

	// Flip the switch off through the appearance section's own
	// save: the live terminal must stop copying at once.
	app.setCopyOnSelect = false
	app.applyAppearanceSettings()
	if app.settings.CopyOnSelect {
		t.Error("applyAppearanceSettings must copy the switch off into settings")
	}
	tt.SetClipboard("")
	selectAll()
	if got := tt.Clipboard(); got != "" {
		t.Errorf("Select All with copy-on-select off copied %q, want empty", got)
	}
}

// TestCopyOnSelectToggleRendered renders the appearance section
// headlessly and checks the switch is actually in the panel.
func TestCopyOnSelectToggleRendered(t *testing.T) {
	app := &nativeApp{
		settings:     defaultSettings(),
		settingsOpen: true,
		panelSection: settingsTabAppearance,
		status:       "服务连接成功",
		drafts:       map[string]*portDraft{},
		// Staging mirrors settings, as newNativeApp sets it.
		setScrollF:      10000,
		setFontSizeF:    14,
		setTermTheme:    "dark",
		setCopyOnSelect: true,
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	if !tt.HasText("选中即复制") {
		t.Errorf("appearance section must show the copy-on-select switch, texts = %v", tt.Texts())
	}
	if !tt.HasText("松开鼠标即把选中文字复制到剪贴板") {
		t.Errorf("copy-on-select switch needs its description, texts = %v", tt.Texts())
	}
}

func TestRexPalette(t *testing.T) {
	// GoRex style.go darkColors / lightColors, verbatim stops.
	if darkPalette.bgTop != "2c2131" || darkPalette.bgBottom != "2a2619" {
		t.Errorf("dark gradient = %s -> %s, want 2c2131 -> 2a2619", darkPalette.bgTop, darkPalette.bgBottom)
	}
	if lightPalette.bgTop != "f6e7f6" || lightPalette.bgBottom != "ece1bd" {
		t.Errorf("light gradient = %s -> %s, want f6e7f6 -> ece1bd", lightPalette.bgTop, lightPalette.bgBottom)
	}
	if darkPalette.card != "242429f0" || darkPalette.border != "ffffff24" {
		t.Errorf("dark card/border = %s / %s, want translucent Rex values", darkPalette.card, darkPalette.border)
	}
	if darkPalette.success != "4ade80" || darkPalette.warning != "fbbf24" {
		t.Errorf("dark status = %s / %s, want 4ade80 / fbbf24", darkPalette.success, darkPalette.warning)
	}
	if darkPalette.bgTop == lightPalette.bgTop {
		t.Error("dark and light gradients must differ")
	}
}

func TestSettingsSections(t *testing.T) {
	// Four numbered panel sections, ports first, numbers fixed
	// per category (filtering never renumbers them). The old
	// terminal section is merged into appearance.
	if len(settingsSections) != 4 {
		t.Fatalf("settingsSections has %d items, want 4", len(settingsSections))
	}
	want := []struct {
		id, label, num string
		icon           *ui.SVG
	}{
		{settingsTabPorts, "端口", "01", iconSerial},
		{settingsTabHistory, "历史", "02", iconHistory},
		{settingsTabSync, "同步", "03", iconWebDAV},
		{settingsTabAppearance, "外观", "04", iconAppearance},
	}
	for i, w := range want {
		got := settingsSections[i]
		if got.id != w.id || got.label != w.label || got.icon != w.icon || got.num != w.num {
			t.Errorf("settingsSections[%d] = %+v, want %+v", i, got, w)
		}
		if got.title == "" || got.desc == "" {
			t.Errorf("settingsSections[%d] needs a title and description", i)
		}
	}
	// Default filter shows all sections.
	if app := newNativeApp(nil); app.panelSection != "" {
		t.Errorf("default panel filter = %q, want empty (all)", app.panelSection)
	}
}

func TestMatchSectionText(t *testing.T) {
	hay := "01 端口 端口与连接 管理串口设备、连接状态、别名与波特率配置。"
	cases := []struct {
		query string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"端口", true},
		{"波特率", true},
		{"BO特率", false}, // substring, not fuzzy: mixed-script query misses
		{"别名", true},
		{"历史", false},
		{"01", true},
	}
	for _, tc := range cases {
		if got := matchSectionText(tc.query, hay); got != tc.want {
			t.Errorf("matchSectionText(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
	// Case-insensitive for Latin text.
	if !matchSectionText("webdav", "03 同步 WebDAV 同步 WebDAV 同步配置与连接测试。") {
		t.Error("match should be case-insensitive")
	}
}

func TestVisiblePanelSections(t *testing.T) {
	all := visiblePanelSections("", "")
	if len(all) != 4 {
		t.Fatalf("no filter = %d sections, want 4", len(all))
	}
	only := visiblePanelSections("", settingsTabSync)
	if len(only) != 1 || only[0].id != settingsTabSync {
		t.Fatalf("category filter = %+v, want only sync", only)
	}
	found := visiblePanelSections("字号", "")
	if len(found) != 1 || found[0].id != settingsTabAppearance {
		t.Fatalf("search 字号 = %+v, want only appearance", found)
	}
	// Filter + query combine: history section does not mention 字体.
	if got := visiblePanelSections("字体", settingsTabHistory); len(got) != 0 {
		t.Fatalf("filter+query should be empty, got %+v", got)
	}
	if got := visiblePanelSections("不存在的词", ""); len(got) != 0 {
		t.Fatalf("no-match query should be empty, got %+v", got)
	}
	// Numbers stay fixed when filtered.
	if got := visiblePanelSections("", settingsTabAppearance); len(got) != 1 || got[0].num != "04" {
		t.Fatalf("appearance section number = %+v, want 04", got)
	}
}

func TestStatusDotColor(t *testing.T) {
	// The lamp's color IS the state: no status text anywhere.
	// Disconnected is stop red, not muted gray — a dead port
	// must not hide among idle UI.
	cases := []struct {
		info PortInfo
		want string
	}{
		{PortInfo{Connected: true}, darkPalette.success},
		{PortInfo{Status: "connecting"}, darkPalette.warning},
		{PortInfo{Status: "error"}, darkPalette.danger},
		{PortInfo{Status: "disconnected"}, darkPalette.danger},
		{PortInfo{}, darkPalette.danger},
	}
	for _, tc := range cases {
		if got := statusColor(darkPalette, tc.info); got != tc.want {
			t.Errorf("statusColor(%+v) = %q, want %q", tc.info, got, tc.want)
		}
	}
}

func TestPillConnectButton(t *testing.T) {
	// Scheme A power button, four states: connected green
	// disconnects, disconnected muted connects, error red
	// reconnects, connecting amber is disabled (no second
	// connect while one is in flight).
	cases := []struct {
		info         PortInfo
		wantColor    string
		wantTip      string
		wantAction   string
		wantDisabled bool
	}{
		{PortInfo{Connected: true}, darkPalette.success, "断开", "disconnect", false},
		{PortInfo{Status: "disconnected"}, darkPalette.muted, "连接", "connect", false},
		{PortInfo{Status: "connecting"}, darkPalette.warning, "连接中", "", true},
		{PortInfo{Status: "error"}, darkPalette.danger, "连接", "connect", false},
		{PortInfo{}, darkPalette.muted, "连接", "connect", false},
	}
	for _, tc := range cases {
		color, tip, action, disabled := pillConnectButton(darkPalette, tc.info)
		if color != tc.wantColor || tip != tc.wantTip || action != tc.wantAction || disabled != tc.wantDisabled {
			t.Errorf("pillConnectButton(%+v) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				tc.info, color, tip, action, disabled, tc.wantColor, tc.wantTip, tc.wantAction, tc.wantDisabled)
		}
	}
}

func TestPillRecordButton(t *testing.T) {
	// Scheme A ● button: recording amber stops, a live idle
	// port muted starts, any port without a live connection is
	// dimmed and disabled with a tip that says so.
	cases := []struct {
		info         PortInfo
		recording    bool
		wantColor    string
		wantTip      string
		wantDisabled bool
	}{
		{PortInfo{Connected: true}, true, darkPalette.warning, "停止记录", false},
		{PortInfo{Connected: true}, false, darkPalette.muted, "开始记录", false},
		{PortInfo{Status: "disconnected"}, false, darkPalette.dim, "记录", true},
		{PortInfo{Status: "connecting"}, false, darkPalette.dim, "记录", true},
		{PortInfo{Status: "error"}, false, darkPalette.dim, "记录", true},
	}
	for _, tc := range cases {
		color, tip, disabled := pillRecordButton(darkPalette, tc.info, tc.recording)
		if color != tc.wantColor || tip != tc.wantTip || disabled != tc.wantDisabled {
			t.Errorf("pillRecordButton(%+v, %v) = (%q, %q, %v), want (%q, %q, %v)",
				tc.info, tc.recording, color, tip, disabled, tc.wantColor, tc.wantTip, tc.wantDisabled)
		}
	}
}

func TestSettingsPanelState(t *testing.T) {
	app := &nativeApp{settings: defaultSettings()}
	if app.settingsOpen {
		t.Error("settings panel should start closed")
	}
	app.openSettings()
	if !app.settingsOpen {
		t.Error("openSettings should open the panel")
	}
	app.cancelSettings()
	if app.settingsOpen {
		t.Error("cancelSettings should close the panel")
	}
	app.openSettingsSection(settingsTabSync)
	if !app.settingsOpen || app.panelSection != settingsTabSync {
		t.Errorf("openSettingsSection should open filtered to sync, got open=%v filter=%q", app.settingsOpen, app.panelSection)
	}
}

// reloadPanelStaging drops un-saved edits in every section at
// once: appearance staging, WebDAV staging (back to the last
// server-pushed config), port drafts, expansion state, and any
// inline history edit — so nothing leaks across sections or
// into a later open.
func TestReloadPanelStaging(t *testing.T) {
	app := &nativeApp{
		settings:       defaultSettings(),
		setScrollF:     999,
		setFontSizeF:   99,
		wdURL:          "edited",
		wdLastURL:      "saved",
		wdUser:         "edited",
		wdLastUser:     "saved",
		webdavIntF:     7,
		wdLastIntF:     60,
		drafts:         map[string]*portDraft{"ttyUSB0": {alias: "edited"}},
		portConfigOpen: "ttyUSB0",
		portAliasOpen:  "ttyUSB0",
		histEditID:     "h1",
		histEditText:   "edited",
	}
	app.reloadPanelStaging()
	if app.setScrollF != float64(app.settings.Scrollback) || app.setFontSizeF != float64(app.settings.FontSize) {
		t.Errorf("appearance staging not reloaded: scroll=%v size=%v", app.setScrollF, app.setFontSizeF)
	}
	if app.wdURL != "saved" || app.wdUser != "saved" || app.webdavIntF != 60 {
		t.Errorf("webdav staging not reloaded: url=%q user=%q int=%v", app.wdURL, app.wdUser, app.webdavIntF)
	}
	if len(app.drafts) != 0 || app.portConfigOpen != "" || app.portAliasOpen != "" {
		t.Errorf("port drafts/expansion not reset: drafts=%v cfg=%q alias=%q", app.drafts, app.portConfigOpen, app.portAliasOpen)
	}
	if app.histEditID != "" {
		t.Errorf("inline history edit not cleared: %q", app.histEditID)
	}
}

// Inline history editing is row state, not a modal: start sets
// id+text, cancel clears it without touching the entry, and save
// with no client keeps the row open (nothing was sent).
func TestInlineHistoryEditState(t *testing.T) {
	app := &nativeApp{}
	app.startInlineHistoryEdit("h1", "AT+VERSION")
	if app.histEditID != "h1" || app.histEditText != "AT+VERSION" {
		t.Errorf("start edit = %q %q", app.histEditID, app.histEditText)
	}
	app.cancelInlineHistoryEdit()
	if app.histEditID != "" {
		t.Errorf("cancel edit should clear id, got %q", app.histEditID)
	}
	app.startInlineHistoryEdit("h1", "AT+VERSION")
	app.saveInlineHistoryEdit() // no client: early return, row stays
	if app.histEditID != "h1" {
		t.Errorf("save with no client should keep the row open, got %q", app.histEditID)
	}
}

// TestCmdBarDropdownFlow drives the command bar through the
// headless Tester: typing opens the dropdown WITHOUT losing the
// input's focus, Down selects AND fills the input live with the
// match, Enter sends the input's content (the selected match)
// AND clears the input AND moves the focus to the serial
// terminal, and Esc closes only the dropdown with focus still
// in the input. The send here has no client, so it fails into
// the status line instead of touching a device (no "[Not
// connected]" line is fed anymore — with a client, the send
// would auto-connect and buffer).
func TestCmdBarDropdownFlow(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "sensor", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		},
		portIDs:  []string{"ttyUSB0"},
		selected: "ttyUSB0",
		terms:    map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings: defaultSettings(),
		history: []HistoryEntry{
			{ID: "h1", Command: "AT+VERSION"},
			{ID: "h2", Command: "AT+RESET"},
			{ID: "h3", Command: "STATUS?"},
		},
		drafts:     map[string]*portDraft{},
		cmdHistIdx: -1,
		histNavIdx: -1,
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	// Focus the command input by clicking the middle of its row:
	// Find returns the Send button's TEXT box, so a point just
	// left of it would still land on the button itself.
	sendRect, ok := tt.Find("发送")
	if !ok {
		t.Fatalf("send button not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(600, sendRect.Y+sendRect.H/2)
	tt.Frame()
	tt.Type("AT")
	tt.Frame()
	if app.cmdInput != "AT" {
		t.Fatalf("typing did not reach the input (focus lost?): input = %q", app.cmdInput)
	}
	if !tt.HasText("AT+VERSION") || !tt.HasText("AT+RESET") {
		t.Fatalf("dropdown should show matches; texts = %v", tt.Texts())
	}
	// Down selects the first match and fills the input live.
	// Matches are newest first, and "STATUS?" contains "AT"
	// too, so the first match is "STATUS?", then "AT+RESET".
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if app.cmdHistIdx != 0 {
		t.Fatalf("down should select index 0, got %d", app.cmdHistIdx)
	}
	if app.cmdInput != "STATUS?" {
		t.Fatalf("down should fill the input live, got %q, want STATUS?", app.cmdInput)
	}
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	if app.cmdHistIdx != 1 || app.cmdInput != "AT+RESET" {
		t.Fatalf("second down: idx = %d input = %q, want 1/AT+RESET", app.cmdHistIdx, app.cmdInput)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if app.cmdHistIdx != -1 || !app.cmdHistHide {
		t.Fatalf("enter send should clear selection and hide, idx=%d hide=%v", app.cmdHistIdx, app.cmdHistHide)
	}
	if app.cmdInput != "" {
		t.Fatalf("enter send must clear the input, got %q", app.cmdInput)
	}
	// Enter moved the focus to the terminal: typing now reaches
	// the device (the fake sender), not the input.
	tt.Type("AT")
	tt.Frame()
	if app.cmdInput != "" {
		t.Fatalf("typing after enter should not reach the input, got %q", app.cmdInput)
	}
	waitFakeWrites(t, fake, "AT")
	// Clicking the input row brings the focus back; typing again
	// re-opens the dropdown.
	tt.ClickAt(600, sendRect.Y+sendRect.H/2)
	tt.Frame()
	tt.Type("AT")
	tt.Frame()
	if app.cmdHistHide {
		t.Error("typing should clear the dropdown hide flag")
	}
	if app.cmdInput != "AT" {
		t.Fatalf("input after retype = %q, want AT (focus lost?)", app.cmdInput)
	}
	if !tt.HasText("AT+VERSION") {
		t.Fatalf("retyped query should show matches again; texts = %v", tt.Texts())
	}
	// Esc closes only the dropdown; the next keystroke still
	// lands in the input.
	app.cmdInput = "AT"
	tt.Frame()
	if app.cmdHistHide {
		t.Fatalf("input change should clear hide, hide=%v", app.cmdHistHide)
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if !app.cmdHistHide || app.cmdHistIdx != -1 {
		t.Fatalf("esc should hide and clear selection, hide=%v idx=%d", app.cmdHistHide, app.cmdHistIdx)
	}
	tt.Type("Y")
	tt.Frame()
	if app.cmdInput != "ATY" {
		t.Fatalf("input after esc+type = %q, want ATY (focus lost?)", app.cmdInput)
	}
}

// portBoxTexts: aliased ports show alias + detail, bare ports
// show the detail alone (which already carries the /dev name).
// The pill tab shows only the name and the short config
// (baudrate); the full detail lives in the tooltip, so a strip
// of many ports compresses into one row without a scrollbar.
func TestPortPillTexts(t *testing.T) {
	aliased := portPillTexts(PortInfo{ID: "ttyUSB0", Alias: "D766", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"})
	if len(aliased) != 2 || aliased[0] != "D766" || aliased[1] != "· 115200" {
		t.Errorf("aliased pill = %q, want [D766 · 115200]", aliased)
	}
	bare := portPillTexts(PortInfo{ID: "ttyUSB1", Baudrate: 9600, Databits: 8, Stopbit: 1, Parity: "N"})
	if len(bare) != 2 || bare[0] != "ttyUSB1" || bare[1] != "· 9600" {
		t.Errorf("bare pill = %q, want [ttyUSB1 · 9600]", bare)
	}
	// Unknown baud: name only, never a guessed default.
	unknown := portPillTexts(PortInfo{ID: "ttyACM0"})
	if len(unknown) != 1 || unknown[0] != "ttyACM0" {
		t.Errorf("unknown-baud pill = %q, want [ttyACM0]", unknown)
	}
}

// The pill tooltip carries the full device info, one fact per
// line: name, path, baud + frame, vendor description when
// present. Lines with no known facts are omitted, never filled
// with a guessed default.
func TestPortTooltip(t *testing.T) {
	got := portTooltip(PortInfo{ID: "ttyUSB0", Alias: "D766", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N", Description: "Prolific Technology Inc. USB-Serial Controller"})
	if want := "D766\n/dev/ttyUSB0\n115200 8N1\nProlific Technology Inc. USB-Serial Controller"; got != want {
		t.Errorf("tooltip aliased = %q, want %q", got, want)
	}
	got = portTooltip(PortInfo{ID: "ttyUSB1", Baudrate: 921600, Databits: 8, Stopbit: 1, Parity: "N"})
	if want := "ttyUSB1\n/dev/ttyUSB1\n921600 8N1"; got != want {
		t.Errorf("tooltip bare = %q, want %q", got, want)
	}
	// No vendor line when there is no description, and no
	// config line when neither baud nor frame is known.
	if got := portTooltip(PortInfo{ID: "ttyUSB0", Baudrate: 115200}); strings.Count(got, "\n") != 2 {
		t.Errorf("tooltip without vendor = %q, want 3 lines", got)
	}
	if got := portTooltip(PortInfo{ID: "ttyACM0"}); got != "ttyACM0\n/dev/ttyACM0" {
		t.Errorf("tooltip unknown config = %q, want name + path only", got)
	}
}

// A failed auto-connect leaves no dialog: the server's error
// message lands in the status line via onError, which clears the
// outstanding flag and marks the port "error" (red dot by the
// existing rule), while the buffered bytes are kept so the next
// write retries.
func TestAutoConnectFailureState(t *testing.T) {
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "disconnected"},
		},
		pendingSend: map[string][]byte{"ttyUSB0": []byte("AT\r")},
		autoConnect: map[string]bool{"ttyUSB0": true},
	}
	app.onError("device or resource busy")
	app.mu.Lock()
	status, flag, buf := app.ports["ttyUSB0"].Status, app.autoConnect["ttyUSB0"], app.pendingSend["ttyUSB0"]
	line := app.status
	app.mu.Unlock()
	if line != "服务器错误: device or resource busy" {
		t.Errorf("status = %q, want the server's reason", line)
	}
	if status != "error" {
		t.Errorf("port status = %q, want error (red dot)", status)
	}
	if flag {
		t.Error("auto-connect flag should clear so the next write retries")
	}
	if string(buf) != "AT\r" {
		t.Errorf("buffer = %q, want the bytes kept for retry", buf)
	}
}

// With no auto-connect outstanding, an unrelated server error
// only touches the status line and re-marks nothing.
func TestOnErrorWithoutAutoConnect(t *testing.T) {
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "disconnected"},
		},
	}
	app.onError("boom")
	if got := app.ports["ttyUSB0"].Status; got != "disconnected" {
		t.Errorf("port status = %q, want untouched disconnected", got)
	}
}

// A port_status error ends the outstanding request but keeps
// the buffer, so the next write retries the connect.
func TestOnStatusErrorKeepsBuffer(t *testing.T) {
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connecting"},
		},
		pendingSend: map[string][]byte{"ttyUSB0": []byte("AT\r")},
		autoConnect: map[string]bool{"ttyUSB0": true},
	}
	app.onStatus("ttyUSB0", "error")
	app.mu.Lock()
	flag, buf := app.autoConnect["ttyUSB0"], app.pendingSend["ttyUSB0"]
	app.mu.Unlock()
	if flag {
		t.Error("auto-connect flag should clear on error status")
	}
	if string(buf) != "AT\r" {
		t.Errorf("buffer = %q, want the bytes kept for retry", buf)
	}
}

// nextBreathTarget flips between dim and full; anything below
// full goes to full, so a zero value (e.g. a struct-literal app
// that never started the ticker) still starts at full brightness.
func TestNextBreathTarget(t *testing.T) {
	if got := nextBreathTarget(1.0); got != 0.45 {
		t.Errorf("from full = %v, want 0.45", got)
	}
	if got := nextBreathTarget(0.45); got != 1.0 {
		t.Errorf("from dim = %v, want 1.0", got)
	}
	if got := nextBreathTarget(0); got != 1.0 {
		t.Errorf("from zero = %v, want 1.0", got)
	}
}

// lampDotColor: flashing means recording, nothing else. A
// connected port that records flashes green at the snapshot
// target directly (no interpolation — the view used to Animate
// toward the target, which never settled and kept the whole
// window redrawing at display refresh rate); every other lamp
// is its static status color at full opacity.
func TestLampDotColor(t *testing.T) {
	p := darkPalette
	r, g, b := parseHex6(p.success)
	cases := []struct {
		name string
		info PortInfo
		rec  bool
		want ui.Color
	}{
		{"disconnected red static", PortInfo{Status: "disconnected"}, false, p.hex(p.danger)},
		{"disconnected red static at dim target", PortInfo{Status: "disconnected"}, false, p.hex(p.danger)},
		{"connecting amber static", PortInfo{Status: "connecting"}, false, p.hex(p.warning)},
		{"connected green static", PortInfo{Connected: true}, false, p.hex(p.success)},
		{"connected green static at dim target", PortInfo{Connected: true}, false, p.hex(p.success)},
		{"error red static", PortInfo{Status: "error"}, false, p.hex(p.danger)},
		{"unknown red static", PortInfo{}, false, p.hex(p.danger)},
	}
	for _, tc := range cases {
		for _, breath := range []float32{0.45, 1.0} {
			if got := lampDotColor(p, tc.info, tc.rec, breath); got != tc.want {
				t.Errorf("%s (breath %v) = %+v, want %+v", tc.name, breath, got, tc.want)
			}
		}
	}
	if got := lampDotColor(p, PortInfo{Connected: true}, true, 0.45); got != ui.RGBA(r, g, b, 0.45) {
		t.Errorf("recording dim flash = %+v, want alpha 0.45", got)
	}
	if got := lampDotColor(p, PortInfo{Connected: true}, true, 1.0); got != ui.RGBA(r, g, b, 1.0) {
		t.Errorf("recording full flash = %+v, want alpha 1.0", got)
	}
}

// TestNoDeadFeatures renders the toolbar + open panel and
// asserts the deleted features leave no trace: no search button,
// no remote-backup-list card. Copy-on-select used to be on this
// list (the v0.4.8 plugin could not implement it), but the v0.7.4
// plugin can, so its checkbox is a live feature now and its
// wording is no longer banned. The panel search box is an input,
// not text, so it cannot trip the "搜索" absence check.
func TestNoDeadFeatures(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "D766", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N", Description: "USB Serial"},
			"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected", Baudrate: 921600, Databits: 8, Stopbit: 1, Parity: "N"},
		},
		portIDs:      []string{"ttyUSB0", "ttyUSB1"},
		selected:     "ttyUSB0",
		terms:        map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings:     defaultSettings(),
		settingsOpen: true,
		panelSection: "",
		status:       "服务连接成功",
		history:      []HistoryEntry{{ID: "h1", Command: "AT+VERSION"}},
		drafts:       map[string]*portDraft{},
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	for _, want := range []string{"D766", "ttyUSB1", "/dev/ttyUSB0 · 115200 8N1", "/dev/ttyUSB1 · 921600 8N1"} {
		if !tt.HasText(want) {
			t.Errorf("toolbar/panel should show %q; texts = %v", want, tt.Texts())
		}
	}
	// "云端备份" was a deleted placeholder once; the sync tab now
	// has a real backup list card, so it is no longer dead.
	for _, bad := range []string{"搜索"} {
		for _, txt := range tt.Texts() {
			if strings.Contains(txt, bad) {
				t.Errorf("rendered text %q contains deleted feature %q", txt, bad)
			}
		}
	}
}

// The merged save action: toolbar "保存输出" and Ctrl+S both funnel
// into saveOutputFor (the button passes the selected port, the
// terminal OnKey passes its focused port). It saves rendered
// terminal text, always shows the save dialog, and a cancelled
// dialog writes NOTHING — the old export fallback that wrote the
// default path anyway is gone.
func TestSaveOutput(t *testing.T) {
	fake := &fakeSender{}
	conn := newPortConn(fake, "ttyUSB0")
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term.Close()
	term.Feed([]byte("hello serial\r\n"))
	app := &nativeApp{
		ports:    map[string]PortInfo{"ttyUSB0": {ID: "ttyUSB0", Connected: true}},
		portIDs:  []string{"ttyUSB0"},
		selected: "ttyUSB0",
		terms:    map[string]*termSession{"ttyUSB0": {term: term, conn: conn}},
		settings: defaultSettings(),
	}
	// Success path: injected dialog returns a temp path.
	out := filepath.Join(t.TempDir(), "out.txt")
	app.saveDialog = func(defPath string) (string, error) {
		if !strings.HasPrefix(defPath, os.TempDir()) && !strings.Contains(defPath, "serial_ttyUSB0_") {
			t.Errorf("default path = %q, want home serial_ttyUSB0_<timestamp>.txt", defPath)
		}
		return out, nil
	}
	app.saveOutputFor("ttyUSB0")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("saveOutputFor did not write: %v", err)
	}
	if !strings.Contains(string(data), "hello serial") {
		t.Errorf("saved content = %q, want it to contain fed terminal text", data)
	}
	if !strings.Contains(app.status, "输出已保存") {
		t.Errorf("status = %q, want save confirmation", app.status)
	}
	// Cancel path: dialog returns "" — no file may appear under
	// the default name, and the status must say so.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	before, _ := filepath.Glob(filepath.Join(home, "serial_ttyUSB0_*.txt"))
	app.saveDialog = func(defPath string) (string, error) { return "", nil }
	app.saveOutputFor("ttyUSB0")
	after, _ := filepath.Glob(filepath.Join(home, "serial_ttyUSB0_*.txt"))
	if len(after) != len(before) {
		t.Errorf("cancelled save wrote a file: before %v, after %v", before, after)
	}
	if !strings.Contains(app.status, "已取消保存") {
		t.Errorf("status = %q, want cancellation note", app.status)
	}
}

// The port box right-click menu's "设置" item opens the panel
// filtered to the ports section with that port's config expanded,
// without switching the current terminal.
func TestOpenPortSettings(t *testing.T) {
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Connected: true},
			"ttyUSB1": {ID: "ttyUSB1"},
		},
		portIDs:  []string{"ttyUSB0", "ttyUSB1"},
		selected: "ttyUSB0",
		settings: defaultSettings(),
	}
	app.openPortSettings("ttyUSB1")
	if !app.settingsOpen || app.panelSection != settingsTabPorts || app.portConfigOpen != "ttyUSB1" {
		t.Errorf("openPortSettings = open %v section %q configOpen %q, want true/ports/ttyUSB1",
			app.settingsOpen, app.panelSection, app.portConfigOpen)
	}
	if app.selected != "ttyUSB0" {
		t.Errorf("openPortSettings switched terminal to %q, want selection untouched", app.selected)
	}
}

// Recording is per port, toggled from that port's pill dot:
// starting always pops the save dialog first (cancelling it
// must NOT send start_logging — no server-default fallback),
// picking a path sends start_logging with that port and
// filename, and toggling one port never touches another
// port's recording or the selected terminal.
// toggleLoggingFor is client-side now: the picked path is a local
// file, raw frames land in it verbatim, and stopping flushes and
// reports the local path and size. No WS logging command exists
// anymore; the loopback server below only proves none is sent.
func TestToggleLoggingDialog(t *testing.T) {
	srv := &loopbackServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer httpSrv.Close()
	client, err := dialWS(httpSrv.URL, wsCallbacks{})
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer client.conn.Close()
	fake := &fakeSender{}
	conn1 := newPortConn(fake, "ttyUSB1")
	term1, err := terminal.New(terminal.Options{Conn: conn1})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term1.Close()
	app := &nativeApp{
		client:      client,
		wsConnected: true,
		embedded:    stubLocalServer{},
		selected:    "ttyUSB0",
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Alias: "D766"},
			"ttyUSB1": {ID: "ttyUSB1"},
		},
		portIDs:  []string{"ttyUSB0", "ttyUSB1"},
		settings: defaultSettings(),
		terms:    map[string]*termSession{"ttyUSB1": {term: term1, conn: conn1}},
	}

	// Cancel path, on a port that is NOT selected: nothing is
	// recorded, the note names the port, selection kept.
	app.saveDialog = func(defPath string) (string, error) {
		if !strings.Contains(defPath, "serial_ttyUSB1_") || !strings.HasSuffix(defPath, ".log") {
			t.Errorf("default dialog path = %q, want serial_ttyUSB1_<timestamp>.log", defPath)
		}
		return "", nil
	}
	app.toggleLoggingFor("ttyUSB1")
	if app.isRecording("ttyUSB1") {
		t.Fatalf("cancelled recording is recording")
	}
	if !strings.Contains(app.status, "已取消记录 ttyUSB1") {
		t.Errorf("status = %q, want cancellation note naming ttyUSB1", app.status)
	}
	if app.selected != "ttyUSB0" {
		t.Errorf("toggle changed selection to %q, want ttyUSB0 kept", app.selected)
	}

	// Pick path for ttyUSB1: the file is local, frames land in
	// it verbatim (ANSI included), and nothing goes to the
	// server — recording follows the port's data stream, not
	// the visible tab.
	picked := filepath.Join(t.TempDir(), "rec.log")
	app.saveDialog = func(defPath string) (string, error) { return picked, nil }
	app.toggleLoggingFor("ttyUSB1")
	if !app.isRecording("ttyUSB1") {
		t.Fatalf("ttyUSB1 not recording after pick")
	}
	app.onData("ttyUSB1", []byte("hello\x1b[31mRED"))
	app.onData("ttyUSB0", []byte("other port"))
	for _, cmd := range loopbackCommands(srv) {
		if cmd["type"] == "start_logging" || cmd["type"] == "stop_logging" || cmd["type"] == "get_log_status" {
			t.Fatalf("client-side recording sent server logging command: %v", cmd)
		}
	}
	if app.isRecording("ttyUSB0") {
		t.Errorf("ttyUSB0 is recording, want only ttyUSB1")
	}

	// Toggling the recording port stops ONLY that port, with
	// no dialog (a dialog call here would fail the test), and
	// reports the local path.
	app.saveDialog = func(defPath string) (string, error) {
		t.Errorf("stopping must not pop the save dialog")
		return "", nil
	}
	app.toggleLoggingFor("ttyUSB1")
	if app.isRecording("ttyUSB1") {
		t.Errorf("ttyUSB1 still recording after stop")
	}
	if !strings.Contains(app.status, picked) {
		t.Errorf("status = %q, want it to name the local file", app.status)
	}
	got, err := os.ReadFile(picked)
	if err != nil {
		t.Fatalf("read picked file: %v", err)
	}
	if string(got) != "hello\x1b[31mRED" {
		t.Errorf("picked file = %q, want verbatim hello+ANSI RED", got)
	}
}

// Remote mode records exactly like local mode now: the save dialog
// pops, the picked path is a local file, and no server logging
// command is sent. (The old remote branch — no dialog, server
// default name, server-side file note — is gone with server-side
// recording.)
func TestToggleLoggingRemote(t *testing.T) {
	srv := &loopbackServer{}
	httpSrv := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer httpSrv.Close()
	client, err := dialWS(httpSrv.URL, wsCallbacks{})
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	defer client.conn.Close()
	fake := &fakeSender{}
	conn0 := newPortConn(fake, "ttyUSB0")
	term0, err := terminal.New(terminal.Options{Conn: conn0})
	if err != nil {
		t.Fatalf("terminal.New: %v", err)
	}
	defer term0.Close()
	app := &nativeApp{
		client:      client,
		wsConnected: true,
		selected:    "ttyUSB0",
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Alias: "D766"},
		},
		portIDs:  []string{"ttyUSB0"},
		settings: defaultSettings(),
		terms:    map[string]*termSession{"ttyUSB0": {term: term0, conn: conn0}},
	}
	// embedded stays nil: remote mode — and the dialog must
	// still pop, because the file is local now.
	dialogCalled := false
	picked := filepath.Join(t.TempDir(), "remote-rec.log")
	app.saveDialog = func(defPath string) (string, error) {
		dialogCalled = true
		return picked, nil
	}
	app.toggleLoggingFor("ttyUSB0")
	if !dialogCalled {
		t.Errorf("remote start did not pop the save dialog")
	}
	if !app.isRecording("ttyUSB0") {
		t.Fatalf("ttyUSB0 not recording after pick")
	}
	app.onData("ttyUSB0", []byte("remote-bytes"))
	for _, cmd := range loopbackCommands(srv) {
		if cmd["type"] == "start_logging" || cmd["type"] == "stop_logging" || cmd["type"] == "get_log_status" {
			t.Fatalf("remote recording sent server logging command: %v", cmd)
		}
	}
	app.toggleLoggingFor("ttyUSB0")
	if app.isRecording("ttyUSB0") {
		t.Errorf("ttyUSB0 still recording after stop")
	}
	got, err := os.ReadFile(picked)
	if err != nil {
		t.Fatalf("read picked file: %v", err)
	}
	if string(got) != "remote-bytes" {
		t.Errorf("picked file = %q, want remote-bytes", got)
	}
}

// Export cancel/error must not write anything: an earlier version
// fell back to the day's default filename after a cancelled or
// failed dialog, truncating whatever that file already held.
func TestExportHistoryCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := &nativeApp{
		history:  []HistoryEntry{{ID: "h1", Command: "AT+VERSION"}},
		settings: defaultSettings(),
	}
	defName := "command_history_" + time.Now().Format("2006-01-02") + ".json"

	// Success path still writes the picked file.
	out := filepath.Join(t.TempDir(), "export.json")
	app.saveDialog = func(defPath string) (string, error) { return out, nil }
	app.exportHistory()
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("export did not write picked file: %v", err)
	}

	// Cancel: no file under the default name, status says so.
	app.saveDialog = func(defPath string) (string, error) { return "", nil }
	app.exportHistory()
	if _, err := os.Stat(filepath.Join(home, defName)); !os.IsNotExist(err) {
		t.Error("cancelled export wrote the default file")
	}
	if !strings.Contains(app.status, "已取消导出") {
		t.Errorf("status = %q, want cancellation note", app.status)
	}

	// Dialog error: same — no file, failure note.
	app.saveDialog = func(defPath string) (string, error) { return "", errors.New("dialog boom") }
	app.exportHistory()
	if _, err := os.Stat(filepath.Join(home, defName)); !os.IsNotExist(err) {
		t.Error("failed export wrote the default file")
	}
	if !strings.Contains(app.status, "导出失败") || !strings.Contains(app.status, "dialog boom") {
		t.Errorf("status = %q, want failure note with reason", app.status)
	}
}

// A port that goes away stops its local recording: the file is
// flushed and closed, the status line names it, and a later
// reconnect does not resume it — but unlike the old server-side
// files, a recording that is still alive survives a reconnect,
// because it is a local file fed by whatever connection is
// current. A disconnect for a port that never recorded sends
// nothing and leaves no auto-stop message.
func TestDisconnectStopsRecording(t *testing.T) {
	srv := &loopbackServer{}
	app, _ := dialAutoConnectApp(t, srv)
	app.mu.Lock()
	app.ports["ttyUSB0"] = PortInfo{ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "D766"}
	app.mu.Unlock()
	recPath := filepath.Join(t.TempDir(), "rec.log")
	if err := app.startLocalRecording("ttyUSB0", recPath); err != nil {
		t.Fatalf("startLocalRecording: %v", err)
	}
	app.writeRecording("ttyUSB0", []byte("before-disconnect"))

	app.onStatus("ttyUSB0", "disconnected")
	if app.isRecording("ttyUSB0") {
		t.Errorf("ttyUSB0 still recording after disconnect")
	}
	app.mu.Lock()
	status := app.status
	app.mu.Unlock()
	if status == "" || !strings.Contains(status, "D766") || !strings.Contains(status, "记录已自动停止") {
		t.Errorf("status = %q, want it to name D766 and say recording auto-stopped", status)
	}
	got, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}
	if string(got) != "before-disconnect" {
		t.Errorf("record file = %q, want flushed before-disconnect", got)
	}
	for _, cmd := range loopbackCommands(srv) {
		if cmd["type"] == "stop_logging" {
			t.Errorf("disconnect sent server stop_logging, want none: %v", cmd)
		}
	}

	// A live recording survives a reconnect: the file is local,
	// the connection is not.
	if err := app.startLocalRecording("ttyUSB0", filepath.Join(t.TempDir(), "rec2.log")); err != nil {
		t.Fatalf("restart recording: %v", err)
	}
	app.attachClient(app.client)
	if !app.isRecording("ttyUSB0") {
		t.Errorf("recording did not survive attachClient")
	}
	app.onStatus("ttyUSB0", "connected")
	if !app.isRecording("ttyUSB0") {
		t.Errorf("recording did not survive reconnect status")
	}

	// The error broadcast stops recording the same way.
	app.onStatus("ttyUSB0", "error")
	if app.isRecording("ttyUSB0") {
		t.Errorf("ttyUSB0 still recording after error status")
	}

	// A disconnect for a never-recording port leaves no
	// auto-stop message.
	app.mu.Lock()
	app.status = ""
	app.mu.Unlock()
	app.onStatus("ttyUSB0", "disconnected")
	app.mu.Lock()
	status = app.status
	app.mu.Unlock()
	if strings.Contains(status, "记录已自动停止") {
		t.Errorf("status = %q, want no auto-stop message for a never-recording port", status)
	}
}

// Disconnect auto-stop reports a flush failure instead of the
// success sentence, and keeps the (possibly partial) file: /dev/full
// accepts the open but fails every flush, reproducing the reported
// silent-data-loss path without a fake writer.
func TestDisconnectFlushFailure(t *testing.T) {
	srv := &loopbackServer{}
	app, _ := dialAutoConnectApp(t, srv)
	app.mu.Lock()
	app.ports["ttyUSB0"] = PortInfo{ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "D766"}
	app.mu.Unlock()
	if err := app.startLocalRecording("ttyUSB0", "/dev/full"); err != nil {
		t.Fatalf("startLocalRecording: %v", err)
	}
	app.writeRecording("ttyUSB0", []byte("before-disconnect"))
	app.onStatus("ttyUSB0", "disconnected")
	if app.isRecording("ttyUSB0") {
		t.Errorf("ttyUSB0 still recording after disconnect")
	}
	app.mu.Lock()
	status := app.status
	app.mu.Unlock()
	if !strings.Contains(status, "D766") || !strings.Contains(status, "刷盘失败") || !strings.Contains(status, "缺数据") || !strings.Contains(status, "/dev/full") {
		t.Errorf("status = %q, want port, path, flush failure and incomplete-file warning", status)
	}
	if strings.Contains(status, "记录已自动停止") {
		t.Errorf("status = %q, want error text, not the success sentence", status)
	}
}

// Quit path no longer silently drops flush/close errors:
// closeAllRecordings returns them (shutdown logs them).
func TestCloseAllRecordingsReportsErrors(t *testing.T) {
	app := &nativeApp{}
	if err := app.startLocalRecording("ttyUSB0", "/dev/full"); err != nil {
		t.Fatalf("startLocalRecording: %v", err)
	}
	app.writeRecording("ttyUSB0", []byte("x"))
	errs := app.closeAllRecordings()
	if len(errs) == 0 {
		t.Errorf("closeAllRecordings returned no errors for /dev/full, want the flush failure retained")
	}
	if app.isRecording("ttyUSB0") {
		t.Errorf("recording entry survived closeAllRecordings")
	}
	recPath := filepath.Join(t.TempDir(), "ok.log")
	if err := app.startLocalRecording("ttyUSB0", recPath); err != nil {
		t.Fatalf("restart recording: %v", err)
	}
	app.writeRecording("ttyUSB0", []byte("ok"))
	if errs := app.closeAllRecordings(); len(errs) != 0 {
		t.Errorf("closeAllRecordings errors = %v, want none on success", errs)
	}
}

// Ctrl+, toggles: closed opens keeping the category, open
// closes with cancel semantics (un-saved staging dropped).
func TestToggleSettings(t *testing.T) {
	app := &nativeApp{settings: defaultSettings()}
	app.panelSection = settingsTabPorts
	app.toggleSettings()
	app.mu.Lock()
	open, section := app.settingsOpen, app.panelSection
	app.mu.Unlock()
	if !open || section != settingsTabPorts {
		t.Errorf("closed toggle: open = %v, section = %q, want open with ports kept", open, section)
	}
	app.mu.Lock()
	app.setFontFamily = "NoSuchFont"
	app.mu.Unlock()
	app.toggleSettings()
	app.mu.Lock()
	open, fam := app.settingsOpen, app.setFontFamily
	wantFam := app.settings.FontFamily
	app.mu.Unlock()
	if open {
		t.Errorf("open toggle: panel still open, want it closed")
	}
	if fam != wantFam {
		t.Errorf("open toggle: staging font = %q, want it dropped back to %q", fam, wantFam)
	}
}

// The status line is sticky: a connect/disconnect announces with
// an in-progress line, and only its own result may replace it.
// Anything newer — another operation, another port's line, an
// error reason, the disconnect auto-stop note — must survive.
func TestStaleStatusReplacement(t *testing.T) {
	line := func(app *nativeApp) string {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.status
	}

	// Manual connect success replaces the stale line.
	app := &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		status: "正在连接 ttyUSB1…",
	}
	app.onStatus("ttyUSB1", "connected")
	if got := line(app); got != "ttyUSB1 连接成功" {
		t.Errorf("manual connect: status = %q, want %q", got, "ttyUSB1 连接成功")
	}

	// Auto-connect success replaces it too, and flushes the buffer.
	app = &nativeApp{
		ports:       map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		pendingSend: map[string][]byte{"ttyUSB1": []byte("AT\r")},
		autoConnect: map[string]bool{"ttyUSB1": true},
		status:      "正在连接 ttyUSB1…",
	}
	app.onStatus("ttyUSB1", "connected")
	if got := line(app); got != "ttyUSB1 连接成功" {
		t.Errorf("auto-connect: status = %q, want %q", got, "ttyUSB1 连接成功")
	}
	app.mu.Lock()
	_, pending := app.pendingSend["ttyUSB1"]
	_, flag := app.autoConnect["ttyUSB1"]
	app.mu.Unlock()
	if pending || flag {
		t.Error("auto-connect buffer/flag should clear on success")
	}

	// A newer message is never clobbered by a later success.
	app = &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		status: "D766 已断开，记录已自动停止",
	}
	app.onStatus("ttyUSB1", "connected")
	if got := line(app); got != "D766 已断开，记录已自动停止" {
		t.Errorf("newer message: status = %q, want it unchanged", got)
	}

	// Another port's in-progress line is untouched.
	app = &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		status: "正在连接 ttyUSB0…",
	}
	app.onStatus("ttyUSB1", "connected")
	if got := line(app); got != "正在连接 ttyUSB0…" {
		t.Errorf("other port: status = %q, want it unchanged", got)
	}

	// Disconnect success replaces the stale disconnect line.
	app = &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true}},
		status: "正在断开 ttyUSB1…",
	}
	app.onStatus("ttyUSB1", "disconnected")
	if got := line(app); got != "ttyUSB1 已断开" {
		t.Errorf("disconnect: status = %q, want %q", got, "ttyUSB1 已断开")
	}

	// A recording port keeps its more informative auto-stop note.
	app = &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Alias: "D766"}},
		status: "正在断开 ttyUSB1…",
	}
	if err := app.startLocalRecording("ttyUSB1", filepath.Join(t.TempDir(), "x.log")); err != nil {
		t.Fatalf("startLocalRecording: %v", err)
	}
	app.onStatus("ttyUSB1", "disconnected")
	if got := line(app); got != "D766 已断开，记录已自动停止" {
		t.Errorf("auto-stop: status = %q, want the auto-stop note", got)
	}

	// Failure never replaces a connect line: the reason arrives
	// via onError and must stay.
	app = &nativeApp{
		ports:  map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		status: "正在连接 ttyUSB1…",
	}
	app.onStatus("ttyUSB1", "error")
	if got := line(app); got != "正在连接 ttyUSB1…" {
		t.Errorf("error status: status = %q, want it unchanged", got)
	}

	// Broadcast safety net: connected in the fresh list replaces
	// the connect line even when port_status was missed.
	app = &nativeApp{
		ports:    map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}},
		selected: "ttyUSB1",
		status:   "正在连接 ttyUSB1…",
	}
	app.onPorts(map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true}})
	if got := line(app); got != "ttyUSB1 连接成功" {
		t.Errorf("broadcast connect: status = %q, want %q", got, "ttyUSB1 连接成功")
	}

	// Broadcast safety net, disconnect side.
	app = &nativeApp{
		ports:    map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true}},
		selected: "ttyUSB1",
		status:   "正在断开 ttyUSB1…",
	}
	app.onPorts(map[string]PortInfo{"ttyUSB1": {ID: "ttyUSB1", Status: "disconnected"}})
	if got := line(app); got != "ttyUSB1 已断开" {
		t.Errorf("broadcast disconnect: status = %q, want %q", got, "ttyUSB1 已断开")
	}
}

// TestInlineHistoryEditKeepsNewlines: a multi-line command survives
// the inline edit round trip verbatim. The editor's backing string
// is never stripped or folded, and saveInlineHistoryEdit passes the
// text unchanged to EditCommand (only the empty check trims), so a
// server-side edit keeps the newlines too. No live client exists
// headlessly, so the no-client save below only proves the row stays
// open with the multi-line text intact.
func TestInlineHistoryEditKeepsNewlines(t *testing.T) {
	app := &nativeApp{}
	cmd := "line1\nline2\nline3"
	app.startInlineHistoryEdit("h1", cmd)
	if app.histEditID != "h1" || app.histEditText != cmd {
		t.Fatalf("edit state = (%q, %q), want (h1, %q)", app.histEditID, app.histEditText, cmd)
	}
	app.saveInlineHistoryEdit() // no client: early return, row stays
	if app.histEditID != "h1" || app.histEditText != cmd {
		t.Fatalf("after no-client save = (%q, %q), want (h1, %q)", app.histEditID, app.histEditText, cmd)
	}
}

// TestPillClickFocusesTerminal drives two terminals through the
// headless Tester: clicking the second port's pill body switches
// the visible terminal AND moves the keyboard focus into it, so
// typing lands on the second port's serial line with no extra
// click into the terminal.
func TestPillClickFocusesTerminal(t *testing.T) {
	fake := &fakeSender{}
	connA := newPortConn(fake, "ttyUSB0")
	connB := newPortConn(fake, "ttyUSB1")
	termA, err := terminal.New(terminal.Options{Conn: connA})
	if err != nil {
		t.Fatalf("terminal.New A: %v", err)
	}
	defer termA.Close()
	termB, err := terminal.New(terminal.Options{Conn: connB})
	if err != nil {
		t.Fatalf("terminal.New B: %v", err)
	}
	defer termB.Close()
	app := &nativeApp{
		ports: map[string]PortInfo{
			"ttyUSB0": {ID: "ttyUSB0", Status: "connected", Connected: true, Alias: "alpha", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
			"ttyUSB1": {ID: "ttyUSB1", Status: "connected", Connected: true, Alias: "beta", Baudrate: 115200, Databits: 8, Stopbit: 1, Parity: "N"},
		},
		portIDs:  []string{"ttyUSB0", "ttyUSB1"},
		selected: "ttyUSB0",
		terms: map[string]*termSession{
			"ttyUSB0": {term: termA, conn: connA},
			"ttyUSB1": {term: termB, conn: connB},
		},
		settings: defaultSettings(),
		drafts:   map[string]*portDraft{},
	}
	tt := ui.NewTester(func(c *ui.Context) { app.render(c) }, 1200, 750)
	tt.Frame()
	// Click the second pill's alias text: texts are not
	// interactive, so the click lands on the pill body.
	rect, ok := tt.Find("beta")
	if !ok {
		t.Fatalf("pill alias not found; texts = %v", tt.Texts())
	}
	tt.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tt.Frame()
	if app.selected != "ttyUSB1" {
		t.Fatalf("pill click should select ttyUSB1, got %q", app.selected)
	}
	if app.termFocusPending {
		t.Errorf("focus request should be consumed by the terminal render")
	}
	tt.Type("Z")
	tt.Frame()
	// The terminal plugin delivers keys through an async input
	// queue (a goroutine dequeues and writes to the Conn), so the
	// write may land after the frame settles: on a loaded or
	// single-CPU runner the queue goroutine may not have run yet
	// when the first frame returns (reproduced with GOMAXPROCS=1
	// as calls == [], never as a write to the wrong port — the
	// focus itself was already consumed above). The expectation
	// is unchanged, only the wait is bounded: poll for exactly
	// the same write, and a genuine focus miss still fails,
	// only slower.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		found := false
		for _, call := range fake.calls {
			if call == (writeCall{portID: "ttyUSB1", data: "Z"}) {
				found = true
				break
			}
		}
		fake.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	t.Errorf("typing after pill click should reach ttyUSB1, got calls %v", fake.calls)
}

func TestSelectAdjacentPort(t *testing.T) {
	fake := &fakeSender{}
	mk := func(ids []string, selected string) *nativeApp {
		terms := map[string]*termSession{}
		for _, id := range ids {
			terms[id] = &termSession{conn: newPortConn(fake, id)}
		}
		return &nativeApp{portIDs: ids, selected: selected, terms: terms}
	}
	app := mk([]string{"a", "b", "c"}, "b")
	app.selectAdjacentPort(1)
	if app.selected != "c" {
		t.Errorf("right from b = %q, want c", app.selected)
	}
	if !app.termFocusPending {
		t.Error("switch should request terminal focus")
	}
	if !app.cmdHistHide {
		t.Error("switch should hide the command dropdown")
	}
	app.selectAdjacentPort(1)
	if app.selected != "a" {
		t.Errorf("right from c = %q, want a (wrap)", app.selected)
	}
	app.selectAdjacentPort(-1)
	if app.selected != "c" {
		t.Errorf("left from a = %q, want c (wrap)", app.selected)
	}
	// One port: nowhere to go.
	one := mk([]string{"a"}, "a")
	one.selectAdjacentPort(1)
	one.selectAdjacentPort(-1)
	if one.selected != "a" || one.termFocusPending {
		t.Errorf("single port: selected=%q focusPending=%v, want a/false", one.selected, one.termFocusPending)
	}
	// No ports: nothing to do, no panic.
	none := mk(nil, "")
	none.selectAdjacentPort(1)
	none.selectAdjacentPort(-1)
	if none.selected != "" || none.termFocusPending {
		t.Errorf("no ports: selected=%q focusPending=%v, want \"\"/false", none.selected, none.termFocusPending)
	}
	// No selection: right takes the first, left the last.
	empty := mk([]string{"a", "b", "c"}, "")
	empty.selectAdjacentPort(1)
	if empty.selected != "a" {
		t.Errorf("right with no selection = %q, want a", empty.selected)
	}
	empty2 := mk([]string{"a", "b", "c"}, "")
	empty2.selectAdjacentPort(-1)
	if empty2.selected != "c" {
		t.Errorf("left with no selection = %q, want c", empty2.selected)
	}
}

func TestTerminalKeyHandledPortSwitch(t *testing.T) {
	fake := &fakeSender{}
	terms := map[string]*termSession{}
	for _, id := range []string{"a", "b", "c"} {
		terms[id] = &termSession{conn: newPortConn(fake, id)}
	}
	app := &nativeApp{portIDs: []string{"a", "b", "c"}, selected: "b", terms: terms}
	if !app.terminalKeyHandled("b", ui.Ctrl|ui.Shift, ui.KeyLeft) {
		t.Fatal("Ctrl+Shift+Left should be handled")
	}
	if app.selected != "a" {
		t.Errorf("after Ctrl+Shift+Left selected = %q, want a", app.selected)
	}
	if !app.terminalKeyHandled("a", ui.Ctrl|ui.Shift, ui.KeyRight) {
		t.Fatal("Ctrl+Shift+Right should be handled")
	}
	if app.selected != "b" {
		t.Errorf("after Ctrl+Shift+Right selected = %q, want b", app.selected)
	}
	if app.terminalKeyHandled("b", 0, ui.KeyLeft) {
		t.Error("bare Left should reach the device, not switch ports")
	}
	if app.selected != "b" {
		t.Errorf("after bare Left selected = %q, want b (unchanged)", app.selected)
	}
	// Ctrl+Shift+C is deliberately not intercepted here: with
	// DevTools off it reaches the plugin's own copy handler.
	if app.terminalKeyHandled("b", ui.Ctrl|ui.Shift, ui.KeyC) {
		t.Error("Ctrl+Shift+C must not be intercepted by the app")
	}
	if app.selected != "b" {
		t.Errorf("after Ctrl+Shift+C selected = %q, want b (unchanged)", app.selected)
	}
}
