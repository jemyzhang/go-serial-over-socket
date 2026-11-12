package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jemyzhang/serial-over-socket-go/config"
)

// mockWebDAVServer is a minimal in-memory WebDAV remote for upload
// tests: PUT stores a file (optionally after a delay, so uploads
// overlap), PROPFIND lists stored files, DELETE removes one. It
// records how many times each filename was PUT, so a test can tell
// an overwrite apart from two distinct files.
type mockWebDAVServer struct {
	mu         sync.Mutex
	files      map[string][]byte
	puts       map[string]int
	putDelay   time.Duration
	putEntered chan string
	srv        *httptest.Server
}

func newMockWebDAVServer(putDelay time.Duration) *mockWebDAVServer {
	m := &mockWebDAVServer{
		files:      make(map[string][]byte),
		puts:       make(map[string]int),
		putDelay:   putDelay,
		putEntered: make(chan string, 100),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.Trim(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			select {
			case m.putEntered <- name:
			default:
			}
			if m.putDelay > 0 {
				time.Sleep(m.putDelay)
			}
			m.mu.Lock()
			m.files[name] = body
			m.puts[name]++
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			m.mu.Lock()
			names := make([]string, 0, len(m.files))
			for n := range m.files {
				names = append(names, n)
			}
			m.mu.Unlock()
			sort.Strings(names)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
			for _, n := range names {
				fmt.Fprintf(w, `<d:response><d:href>/%s</d:href></d:response>`, n)
			}
			fmt.Fprint(w, `</d:multistatus>`)
		case http.MethodGet:
			m.mu.Lock()
			body, ok := m.files[name]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				break
			}
			w.WriteHeader(http.StatusOK)
			w.Write(body)
		case http.MethodDelete:
			m.mu.Lock()
			delete(m.files, name)
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockWebDAVServer) filenames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.files))
	for n := range m.files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (m *mockWebDAVServer) putCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.puts))
	for n, c := range m.puts {
		out[n] = c
	}
	return out
}

// TestConcurrentUploadNewestWins reproduces the reported race: a
// slow first upload (old snapshot) and a second upload (new
// snapshot) running against the same remote. Serialization plus
// millisecond filenames must leave two distinct files, each PUT
// exactly once, with the newest file holding the newest snapshot —
// never an old snapshot overwriting the new one under its name.
func TestConcurrentUploadNewestWins(t *testing.T) {
	mock := newMockWebDAVServer(300 * time.Millisecond)
	defer mock.srv.Close()
	ws := &WebServer{config: &config.Config{WebDAV: config.WebDAVConfig{URL: mock.srv.URL, MaxBackups: 10}}}
	ws.commandHistory = []CommandEntry{{ID: "a1", Command: "OLD-SNAPSHOT"}}

	errCh := make(chan error, 1)
	go func() { errCh <- ws.saveHistoryToWebDAV() }()
	select {
	case <-mock.putEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first PUT never started")
	}

	ws.historyMu.Lock()
	ws.commandHistory = []CommandEntry{{ID: "b1", Command: "NEW-SNAPSHOT"}}
	ws.historyMu.Unlock()
	if err := ws.saveHistoryToWebDAV(); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("first upload: %v", err)
	}

	names := mock.filenames()
	if len(names) != 2 {
		t.Fatalf("remote files = %v, want exactly 2 distinct backups", names)
	}
	for name, count := range mock.putCounts() {
		if count != 1 {
			t.Errorf("filename %s PUT %d times, want exactly once (overwrite)", name, count)
		}
	}
	mock.mu.Lock()
	oldContent := string(mock.files[names[0]])
	newContent := string(mock.files[names[1]])
	mock.mu.Unlock()
	if !strings.Contains(oldContent, "OLD-SNAPSHOT") {
		t.Errorf("oldest file content = %q, want the old snapshot", oldContent)
	}
	if !strings.Contains(newContent, "NEW-SNAPSHOT") || strings.Contains(newContent, "OLD-SNAPSHOT") {
		t.Errorf("newest file content = %q, want exactly the new snapshot", newContent)
	}
}

// TestConcurrentUploadUniqueNames fires several uploads at once
// with the same history and requires every one to land under its
// own name: fixed-width millisecond timestamps plus the monotonic
// guard must keep names unique and strictly increasing even when
// serialized uploads land in the same millisecond.
func TestConcurrentUploadUniqueNames(t *testing.T) {
	mock := newMockWebDAVServer(0)
	defer mock.srv.Close()
	ws := &WebServer{config: &config.Config{WebDAV: config.WebDAVConfig{URL: mock.srv.URL, MaxBackups: 10}}}
	ws.commandHistory = []CommandEntry{{ID: "c1", Command: "SHARED"}}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ws.saveHistoryToWebDAV()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upload: %v", err)
		}
	}

	names := mock.filenames()
	if len(names) != n {
		t.Fatalf("remote files = %v, want %d distinct backups", names, n)
	}
	nameRe := regexp.MustCompile(`^history_backup_\d{14}\.\d{3}\.json$`)
	for i, name := range names {
		if !nameRe.MatchString(name) {
			t.Errorf("filename %q, want fixed-width millisecond form history_backup_YYYYMMDDHHMMSS.mmm.json", name)
		}
		if i > 0 && names[i-1] >= name {
			t.Errorf("filenames not strictly increasing: %v", names)
		}
	}
	for name, count := range mock.putCounts() {
		if count != 1 {
			t.Errorf("filename %s PUT %d times, want exactly once", name, count)
		}
	}
}
