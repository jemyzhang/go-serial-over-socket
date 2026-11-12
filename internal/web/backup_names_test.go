package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jemyzhang/serial-over-socket-go/config"
)

func TestParseBackupTimestamp(t *testing.T) {
	utc := func(y, mo, d, h, mi, s, ms int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, s, ms*1e6, time.UTC)
	}
	cases := []struct {
		name     string
		wantTime time.Time
		wantOK   bool
		wantMs   bool
	}{
		{"history_backup_20261104123456.json", utc(2026, 11, 4, 12, 34, 56, 0), true, false},
		{"history_backup_20261104123456.789.json", utc(2026, 11, 4, 12, 34, 56, 789), true, true},
		{"history_backup_20261104123456.000.json", utc(2026, 11, 4, 12, 34, 56, 0), true, true},
		{"", time.Time{}, false, false},
		{"history_backup_foo.json", time.Time{}, false, false},
		{"history_backup_20261301123456.json", time.Time{}, false, false},
		{"history_backup_20261104123456.txt", time.Time{}, false, false},
		{"history_20261104123456.json", time.Time{}, false, false},
		{"history_backup_20261104123456.78x.json", time.Time{}, false, false},
		{"history_backup_2026110412345.json", time.Time{}, false, false},
	}
	for _, c := range cases {
		got, ok, ms := parseBackupTimestamp(c.name)
		if ok != c.wantOK || ms != c.wantMs {
			t.Errorf("%q: ok=%v ms=%v, want ok=%v ms=%v", c.name, ok, ms, c.wantOK, c.wantMs)
			continue
		}
		if ok && !got.Equal(c.wantTime) {
			t.Errorf("%q: time=%v, want %v", c.name, got, c.wantTime)
		}
	}
	// Same-second old and new names parse to the same instant: the
	// tie is real, and only the millisecond tie-break separates them.
	oldT, _, _ := parseBackupTimestamp("history_backup_20261104123456.json")
	newT, _, _ := parseBackupTimestamp("history_backup_20261104123456.000.json")
	if !oldT.Equal(newT) {
		t.Errorf("same-second old/new parse to %v vs %v, want equal", oldT, newT)
	}
}

func TestLatestBackupFile(t *testing.T) {
	oldSec := "history_backup_20261104123456.json"
	newMs := "history_backup_20261104123456.000.json"
	cases := []struct {
		names []string
		want  string
	}{
		// The reported bug: raw string order puts the old name
		// first ('j' > '.'), so lexicographic max picks stale.
		{[]string{oldSec, newMs}, newMs},
		{[]string{newMs, oldSec}, newMs},
		{[]string{"history_backup_20261104123455.json", oldSec}, oldSec},
		{[]string{"history_backup_20261104123456.789.json", newMs}, "history_backup_20261104123456.789.json"},
		{[]string{"history_backup_nope.json"}, ""},
		{[]string{"history_backup_nope.json", oldSec}, oldSec},
		{[]string{}, ""},
	}
	for _, c := range cases {
		if got := latestBackupFile(c.names); got != c.want {
			t.Errorf("latestBackupFile(%v) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestSortBackupFilesNewestFirst(t *testing.T) {
	names := []string{
		"history_backup_20261104123455.json",
		"history_backup_20261104123456.json",
		"history_backup_20261104123456.000.json",
		"history_backup_20261104123456.789.json",
		"history_backup_nope.json",
	}
	want := []string{
		"history_backup_20261104123456.789.json",
		"history_backup_20261104123456.000.json",
		"history_backup_20261104123456.json",
		"history_backup_20261104123455.json",
		"history_backup_nope.json",
	}
	sortBackupFilesNewestFirst(names)
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("sorted = %v, want %v", names, want)
		}
	}
}

// Mixed formats, same second, through the real loader: the newest
// backup is the millisecond file, not the lexicographically larger
// old file. Previously the loader's string max picked the old one.
func TestLoadHistoryMixedFormatsPicksNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mock := newMockWebDAVServer(0)
	defer mock.srv.Close()
	sec := "20261104123456"
	mock.mu.Lock()
	mock.files["history_backup_"+sec+".json"] = []byte(`{"commands":[{"id":"o1","command":"AT+OLD"}]}`)
	mock.files["history_backup_"+sec+".000.json"] = []byte(`{"commands":[{"id":"n1","command":"AT+NEW"}]}`)
	mock.mu.Unlock()
	ws := &WebServer{
		clients: map[string]*WebSocketClient{},
		config:  &config.Config{WebDAV: config.WebDAVConfig{URL: mock.srv.URL, MaxBackups: 10}},
	}
	ws.loadHistoryFromWebDAV()
	seen := map[string]bool{}
	for _, e := range ws.commandHistory {
		seen[e.ID] = true
	}
	if !seen["n1"] {
		t.Errorf("loaded history = %v, want the millisecond backup's entry", ws.commandHistory)
	}
	if seen["o1"] {
		t.Errorf("loaded history = %v, want only the millisecond backup's entry", ws.commandHistory)
	}
}

// Mixed formats, same second, through the real uploader with
// MaxBackups=1: pruning must keep the just-uploaded millisecond
// backup and delete the old second-precision one. The upload's own
// timestamp comes from time.Now, so the test reseeds the old file
// at the current second until the upload lands in that same second
// (a second-boundary crossing just retries; it never passes
// vacuously, because a non-tied attempt is discarded, not counted).
func TestPruneMixedFormatsKeepsNewest(t *testing.T) {
	mock := newMockWebDAVServer(0)
	defer mock.srv.Close()
	ws := &WebServer{config: &config.Config{WebDAV: config.WebDAVConfig{URL: mock.srv.URL, MaxBackups: 1}}}
	ws.commandHistory = []CommandEntry{{ID: "n1", Command: "AT+NEW"}}
	tied := false
	for attempt := 0; attempt < 20 && !tied; attempt++ {
		sec := time.Now().Format("20060102150405")
		oldName := "history_backup_" + sec + ".json"
		mock.mu.Lock()
		mock.files = map[string][]byte{oldName: []byte(`{"commands":[{"id":"o1","command":"AT+OLD"}]}`)}
		mock.puts = map[string]int{}
		mock.mu.Unlock()
		if err := ws.saveHistoryToWebDAV(); err != nil {
			t.Fatalf("attempt %d: upload: %v", attempt, err)
		}
		names := mock.filenames()
		for _, n := range names {
			if strings.HasPrefix(n, "history_backup_"+sec+".") {
				tied = true
			}
		}
		if !tied {
			continue
		}
		if len(names) != 1 {
			t.Fatalf("mixed-format prune: remote files = %v, want exactly the new backup", names)
		}
		if _, ok, hasMs := parseBackupTimestamp(names[0]); !ok || !hasMs {
			t.Errorf("kept file = %q, want the millisecond-format new backup", names[0])
		}
		mock.mu.Lock()
		content := string(mock.files[names[0]])
		mock.mu.Unlock()
		if !strings.Contains(content, "AT+NEW") {
			t.Errorf("kept file content = %q, want the new snapshot", content)
		}
	}
	if !tied {
		t.Fatal("never established a same-second tie in 20 attempts")
	}
}
