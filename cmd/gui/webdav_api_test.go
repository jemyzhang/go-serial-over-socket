package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestFormatWebDAVDateAndSize(t *testing.T) {
	if got := formatWebDAVDate(""); got != "未知日期" {
		t.Errorf("empty date = %q, want 未知日期", got)
	}
	if got := formatWebDAVSize(""); got != "未知大小" {
		t.Errorf("empty size = %q, want 未知大小", got)
	}
	// HTTP-date parses to a local "2006-01-02 15:04" (timezone
	// dependent, so only the shape is asserted here).
	if got := formatWebDAVDate("Mon, 03 Nov 2026 11:30:00 GMT"); !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`).MatchString(got) {
		t.Errorf("parsed date = %q, want YYYY-MM-DD HH:MM shape", got)
	}
	// Unparseable values pass through raw, never empty.
	if got := formatWebDAVDate("not-a-date"); got != "not-a-date" {
		t.Errorf("bad date = %q, want passthrough", got)
	}
	if got := formatWebDAVSize("not-a-number"); got != "not-a-number" {
		t.Errorf("bad size = %q, want passthrough", got)
	}
	if got := formatWebDAVSize("1536"); got != "1.5 KB" {
		t.Errorf("1536 bytes = %q, want 1.5 KB", got)
	}
}

func TestFetchWebDAVBackupList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/webdav/list" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			Prefix string `json:"prefix"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Prefix != "history_backup_" {
			t.Errorf("prefix = %q, want history_backup_", req.Prefix)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files": [
			{"name": "history_backup_20261101090000.json", "lastMod": "Sat, 01 Nov 2025 09:00:00 GMT", "size": "2048"},
			{"name": "history_backup_20261103193000.json", "lastMod": "Tue, 03 Nov 2026 19:30:00 GMT", "size": "1536"},
			{"name": "", "lastMod": "", "size": ""}
		]}`))
	}))
	defer srv.Close()

	files, err := fetchWebDAVBackupList(srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Empty-name entry skipped; newest first by name.
	if len(files) != 2 || files[0].Name != "history_backup_20261103193000.json" || files[1].Name != "history_backup_20261101090000.json" {
		t.Fatalf("files = %+v, want newest-first 2 entries", files)
	}
	if files[0].Size != "1.5 KB" {
		t.Errorf("size = %q, want 1.5 KB", files[0].Size)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`).MatchString(files[0].Mod) {
		t.Errorf("mod = %q, want YYYY-MM-DD HH:MM shape", files[0].Mod)
	}
}

func TestFetchWebDAVBackupListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "WebDAV not configured on server"}`))
	}))
	defer srv.Close()
	if _, err := fetchWebDAVBackupList(srv.URL); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("err = %v, want the server error text", err)
	}
}

func TestParseWebDAVBackupContent(t *testing.T) {
	// Wrapped form with ids (the server's own backup format).
	entries, err := parseWebDAVBackupContent([]byte(`{"version": "1.0", "commands": [
		{"id": "abc", "command": "AT+VERSION", "timestamp": 100},
		{"id": "def", "command": "STATUS?", "timestamp": 90}
	]}`))
	if err != nil || len(entries) != 2 || entries[0].ID != "abc" || entries[1].Command != "STATUS?" {
		t.Fatalf("wrapped = %+v, %v, want 2 entries with ids", entries, err)
	}
	// Raw array form.
	if entries, err := parseWebDAVBackupContent([]byte(`[{"id": "x", "command": "PING"}]`)); err != nil || len(entries) != 1 || entries[0].ID != "x" {
		t.Fatalf("raw array = %+v, %v, want 1 entry", entries, err)
	}
	// Plain string array form (ids absent, tolerated like the server).
	if entries, err := parseWebDAVBackupContent([]byte(`["PING", ""]`)); err != nil || len(entries) != 1 || entries[0].Command != "PING" {
		t.Fatalf("string array = %+v, %v, want 1 entry", entries, err)
	}
	// Empty commands and garbage are errors, never silent no-ops.
	if _, err := parseWebDAVBackupContent([]byte(`{"commands": []}`)); err == nil {
		t.Error("empty commands should fail")
	}
	if _, err := parseWebDAVBackupContent([]byte(`not json`)); err == nil {
		t.Error("garbage should fail")
	}
}

func TestDownloadAndDeleteWebDAVBackup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/webdav/download":
			// The download endpoint streams the file body
			// directly (no wrapper).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"commands": [{"id": "k", "command": "AT"}]}`))
		case "/api/webdav/delete":
			var req struct {
				Filename string `json:"filename"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Filename != "history_backup_20261103193000.json" {
				t.Errorf("delete filename = %q", req.Filename)
			}
			_, _ = w.Write([]byte(`{"ok": true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	entries, err := downloadWebDAVBackup(srv.URL, "history_backup_20261103193000.json")
	if err != nil || len(entries) != 1 || entries[0].ID != "k" {
		t.Fatalf("download = %+v, %v, want 1 entry id k", entries, err)
	}
	if err := deleteWebDAVBackup(srv.URL, "history_backup_20261103193000.json"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestTestWebDAVEndpoint(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": 207, "ok": true}`))
	}))
	defer okSrv.Close()
	if res, err := testWebDAVEndpoint(okSrv.URL); err != nil || !res.Ok || res.Status != 207 {
		t.Fatalf("ok probe = %+v, %v, want ok status 207", res, err)
	}

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status": 401, "ok": false}`))
	}))
	defer authSrv.Close()
	if res, err := testWebDAVEndpoint(authSrv.URL); err != nil || res.Ok || res.Status != 401 {
		t.Fatalf("401 probe = %+v, %v, want not-ok status 401", res, err)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "WebDAV not configured on server"}`))
	}))
	defer errSrv.Close()
	if res, err := testWebDAVEndpoint(errSrv.URL); err != nil || res.ErrMsg == "" {
		t.Fatalf("error probe = %+v, %v, want ErrMsg", res, err)
	}
}
