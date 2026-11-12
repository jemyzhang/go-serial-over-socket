package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jemyzhang/serial-over-socket-go/config"
)

// validWebDAVFilename only allows flat file names: backup files are
// history_backup_*.json, so a separator or a parent-directory element
// can only be a traversal attempt against the configured WebDAV account.
func TestValidWebDAVFilename(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"history_backup_20261104123456.json", true},
		{"history_backup_20261104123456.000.json", true},
		{"history.json", true},
		{"a-b_c.json", true},
		{"", false},              // empty
		{"../evil.json", false},  // parent traversal
		{"..", false},            // bare parent
		{"a..b.json", false},     // ".." anywhere is rejected
		{"sub/file.json", false}, // sub-directory
		{"/abs.json", false},     // leading slash
		{`sub\file.json`, false}, // backslash separator
		{`..\evil.json`, false},  // backslash traversal
		{"history_backup_20261104123456.json/", false},
	}
	for _, c := range cases {
		if got := validWebDAVFilename(c.name); got != c.want {
			t.Errorf("validWebDAVFilename(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The proxy handlers must reject traversal filenames with 400 before
// any request reaches the WebDAV server, while a flat backup name
// still flows through.
func TestWebDAVProxyRejectsTraversal(t *testing.T) {
	mock := newMockWebDAVServer(0)
	defer mock.srv.Close()
	mock.files["history_backup_20261104123456.json"] = []byte(`{"commands":[]}`)
	ws := &WebServer{
		clients: map[string]*WebSocketClient{},
		config:  &config.Config{WebDAV: config.WebDAVConfig{URL: mock.srv.URL}},
	}
	good := "history_backup_20261104123456.json"
	bad := []string{"../evil.json", "sub/file.json", `..\evil.json`, ".."}

	call := func(handler func(http.ResponseWriter, *http.Request), body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	// Upload: bad names are 400 and never reach the server; the good
	// name is PUT to the flat path.
	for _, name := range bad {
		rec := call(ws.handleWebDAVUpload, `{"filename":"`+name+`","content":"{}"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upload %q: status = %d, want 400", name, rec.Code)
		}
	}
	if got := mock.putCounts(); len(got) != 0 {
		t.Errorf("rejected uploads reached the server: %v", got)
	}
	rec := call(ws.handleWebDAVUpload, `{"filename":"`+good+`","content":"{}"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("upload %q: status = %d, want 200", good, rec.Code)
	}
	if mock.putCounts()[good] != 1 {
		t.Errorf("upload %q did not reach the server", good)
	}

	// Download: bad names are 400; the good name returns the stored file.
	for _, name := range bad {
		rec := call(ws.handleWebDAVDownload, `{"filename":"`+name+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("download %q: status = %d, want 400", name, rec.Code)
		}
	}
	rec = call(ws.handleWebDAVDownload, `{"filename":"`+good+`"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("download %q: status = %d, want 200", good, rec.Code)
	}

	// Delete: bad names are 400 and the file stays; the good name deletes it.
	for _, name := range bad {
		rec := call(ws.handleWebDAVDelete, `{"filename":"`+name+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("delete %q: status = %d, want 400", name, rec.Code)
		}
	}
	if names := mock.filenames(); len(names) != 1 || names[0] != good {
		t.Fatalf("rejected deletes removed the file: %v", names)
	}
	rec = call(ws.handleWebDAVDelete, `{"filename":"`+good+`"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("delete %q: status = %d, want 200", good, rec.Code)
	}
	if len(mock.filenames()) != 0 {
		t.Errorf("delete %q did not remove the file: %v", good, mock.filenames())
	}
}
