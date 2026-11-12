package web

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────
// Shared request / response types
// ─────────────────────────────────────────────────────────────

type webdavCfg struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// savedWebDAV returns the server-side saved WebDAV configuration.
// ALL proxy endpoints use this and ignore any credentials supplied in the
// request body, so the server can't be abused as an open relay against
// arbitrary WebDAV servers.
func (ws *WebServer) savedWebDAV() (webdavCfg, bool) {
	wd := ws.getWebDAV()
	if wd.URL == "" {
		return webdavCfg{}, false
	}
	return webdavCfg{URL: wd.URL, Username: wd.Username, Password: wd.Password}, true
}

// doWebDAV makes an authenticated request to the remote WebDAV server.
func doWebDAV(method, url, username, password string,
	extraHeaders map[string]string, body io.Reader) (*http.Response, error) {

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// jsonErr writes a JSON error response.
func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ─────────────────────────────────────────────────────────────
// POST /api/webdav/test   – PROPFIND depth:0 on the saved config's URL
// ─────────────────────────────────────────────────────────────

func (ws *WebServer) handleWebDAVTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := ws.savedWebDAV()
	if !ok {
		jsonErr(w, http.StatusBadRequest, "WebDAV not configured on server — save settings first")
		return
	}

	propfindBody := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`
	resp, err := doWebDAV("PROPFIND", cfg.URL, cfg.Username, cfg.Password,
		map[string]string{"Depth": "0", "Content-Type": "application/xml"},
		strings.NewReader(propfindBody))
	if err != nil {
		log.Printf("[webdav/test] error: %v", err)
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": resp.StatusCode,
		"ok":     resp.StatusCode == 207 || resp.StatusCode == 200,
	})
}

// ─────────────────────────────────────────────────────────────
// POST /api/webdav/list   – PROPFIND depth:1, return file list
// ─────────────────────────────────────────────────────────────

// Minimal WebDAV multistatus XML types for parsing
type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}
type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}
type davProp struct {
	DisplayName   string `xml:"displayname"`
	LastModified  string `xml:"getlastmodified"`
	ContentLength string `xml:"getcontentlength"`
	ResourceType  struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

type webdavFile struct {
	Name    string `json:"name"`
	Href    string `json:"href"`
	LastMod string `json:"lastMod"`
	Size    string `json:"size"`
}

func (ws *WebServer) handleWebDAVList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := ws.savedWebDAV()
	if !ok {
		jsonErr(w, http.StatusBadRequest, "WebDAV not configured on server — save settings first")
		return
	}
	var req struct {
		Prefix string `json:"prefix"` // filename prefix filter, e.g. "history_backup_"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	propfindBody := `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:displayname/>
    <d:getlastmodified/>
    <d:getcontentlength/>
    <d:resourcetype/>
  </d:prop>
</d:propfind>`

	resp, err := doWebDAV("PROPFIND", cfg.URL, cfg.Username, cfg.Password,
		map[string]string{"Depth": "1", "Content-Type": "application/xml"},
		strings.NewReader(propfindBody))
	if err != nil {
		log.Printf("[webdav/list] error: %v", err)
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("WebDAV returned %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "failed to read response")
		return
	}

	var ms davMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		log.Printf("[webdav/list] xml parse error: %v\nbody: %s", err, string(body))
		jsonErr(w, http.StatusBadGateway, "failed to parse WebDAV response")
		return
	}

	var files []webdavFile
	prefix := req.Prefix
	for _, r := range ms.Responses {
		// Extract filename from href
		parts := strings.Split(strings.TrimRight(r.Href, "/"), "/")
		name := parts[len(parts)-1]
		if name == "" {
			continue
		}
		// Apply prefix filter
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		// Skip collections (directories)
		isDir := false
		var lastMod, size string
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			if ps.Prop.ResourceType.Collection != nil {
				isDir = true
			}
			lastMod = ps.Prop.LastModified
			size = ps.Prop.ContentLength
		}
		if isDir {
			continue
		}
		files = append(files, webdavFile{
			Name:    name,
			Href:    r.Href,
			LastMod: lastMod,
			Size:    size,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"files": files})
}

// validWebDAVFilename rejects anything but a flat file name. Backup
// files are flat (history_backup_*.json), so a filename with a path
// separator or a parent-directory element can only be a traversal
// attempt against the configured WebDAV account, not a legitimate
// backup name.
func validWebDAVFilename(name string) bool {
	if name == "" {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// ─────────────────────────────────────────────────────────────
// POST /api/webdav/upload  – PUT a file to WebDAV
// ─────────────────────────────────────────────────────────────

func (ws *WebServer) handleWebDAVUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := ws.savedWebDAV()
	if !ok {
		jsonErr(w, http.StatusBadRequest, "WebDAV not configured on server — save settings first")
		return
	}
	var req struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validWebDAVFilename(req.Filename) {
		jsonErr(w, http.StatusBadRequest, "invalid filename")
		return
	}

	uploadURL := strings.TrimRight(cfg.URL, "/") + "/" + req.Filename
	resp, err := doWebDAV("PUT", uploadURL, cfg.Username, cfg.Password,
		map[string]string{"Content-Type": "application/json"},
		strings.NewReader(req.Content))
	if err != nil {
		log.Printf("[webdav/upload] error: %v", err)
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("WebDAV PUT returned %d", resp.StatusCode))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "status": resp.StatusCode})
}

// ─────────────────────────────────────────────────────────────
// POST /api/webdav/download  – GET a file from WebDAV
// ─────────────────────────────────────────────────────────────

func (ws *WebServer) handleWebDAVDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := ws.savedWebDAV()
	if !ok {
		jsonErr(w, http.StatusBadRequest, "WebDAV not configured on server — save settings first")
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validWebDAVFilename(req.Filename) {
		jsonErr(w, http.StatusBadRequest, "invalid filename")
		return
	}

	downloadURL := strings.TrimRight(cfg.URL, "/") + "/" + req.Filename
	resp, err := doWebDAV("GET", downloadURL, cfg.Username, cfg.Password, nil, nil)
	if err != nil {
		log.Printf("[webdav/download] error: %v", err)
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("WebDAV GET returned %d", resp.StatusCode))
		return
	}

	// Stream the file content directly back to the browser
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

// ─────────────────────────────────────────────────────────────
// POST /api/webdav/delete  – DELETE a file from WebDAV
// ─────────────────────────────────────────────────────────────

func (ws *WebServer) handleWebDAVDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := ws.savedWebDAV()
	if !ok {
		jsonErr(w, http.StatusBadRequest, "WebDAV not configured on server — save settings first")
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validWebDAVFilename(req.Filename) {
		jsonErr(w, http.StatusBadRequest, "invalid filename")
		return
	}

	deleteURL := strings.TrimRight(cfg.URL, "/") + "/" + req.Filename
	resp, err := doWebDAV("DELETE", deleteURL, cfg.Username, cfg.Password, nil, nil)
	if err != nil {
		log.Printf("[webdav/delete] error: %v", err)
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != 404 {
		jsonErr(w, http.StatusBadGateway, fmt.Sprintf("WebDAV DELETE returned %d", resp.StatusCode))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
}
