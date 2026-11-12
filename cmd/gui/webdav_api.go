package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebDAV HTTP helpers for the native GUI.
//
// These call the server's existing /api/webdav/* proxy endpoints
// (internal/web/webdav_proxy.go), which test/list/download/delete
// with the server's SAVED configuration — the same endpoints the
// WebUI uses. The native GUI is a Go process, so there is no CORS
// issue, and no new WS handler was needed for list/import/delete.
// (The test button still needs one WS step first: pushing the form
// values to the server with an ack wait — see testWebDAV in app.go.)

// webdavBackupFile is one cloud backup as shown in the sync tab's
// backup list. Mod and Size are display strings, formatted once at
// fetch time (raw values live only in the fetch response).
type webdavBackupFile struct {
	Name string // e.g. history_backup_20261103193000.000.json
	Mod  string // e.g. "2026-11-03 19:30", or the raw value on parse failure
	Size string // e.g. "12.4 KB", or the raw value on parse failure
}

// webdavAPIPost POSTs a JSON body to base+path and returns the raw
// body plus HTTP status. Only transport errors are returned as
// error: HTTP error statuses are the caller's to decode, because
// the proxy endpoints answer those with a JSON {error} body.
func webdavAPIPost(base, path string, body any) ([]byte, int, error) {
	if base == "" {
		return nil, 0, fmt.Errorf("未连接服务器")
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return data, resp.StatusCode, nil
}

func decodeWebDAVError(data []byte) string {
	var doc struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &doc); err == nil && doc.Error != "" {
		return doc.Error
	}
	return ""
}

// fetchWebDAVBackupList lists history_backup_* files, newest first
// (filenames embed a timestamp, so name-descending is newest-first).
// It returns the display-ready rows; an empty list is not an error.
func fetchWebDAVBackupList(base string) ([]webdavBackupFile, error) {
	data, status, err := webdavAPIPost(base, "/api/webdav/list", map[string]any{"prefix": "history_backup_"})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		if msg := decodeWebDAVError(data); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var doc struct {
		Files []struct {
			Name    string `json:"name"`
			LastMod string `json:"lastMod"`
			Size    string `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("备份列表解析失败: %v", err)
	}
	files := make([]webdavBackupFile, 0, len(doc.Files))
	for _, f := range doc.Files {
		if f.Name == "" {
			continue
		}
		files = append(files, webdavBackupFile{
			Name: f.Name,
			Mod:  formatWebDAVDate(f.LastMod),
			Size: formatWebDAVSize(f.Size),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files, nil
}

// formatWebDAVDate renders a WebDAV getlastmodified value (HTTP-date,
// e.g. "Mon, 02 Jan 2006 15:04:05 GMT") as local "2006-01-02 15:04".
// Unparseable input passes through raw; empty input is "未知日期".
func formatWebDAVDate(raw string) string {
	if raw == "" {
		return "未知日期"
	}
	if t, err := time.Parse(http.TimeFormat, raw); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return raw
}

// formatWebDAVSize renders a byte count string via formatBytes.
// Unparseable input passes through raw; empty input is "未知大小".
func formatWebDAVSize(raw string) string {
	if raw == "" {
		return "未知大小"
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return formatBytes(n)
	}
	return raw
}

// parseWebDAVBackupContent parses a downloaded backup file body.
// It accepts the server's wrapped form {"commands": [...]} and a raw
// array (objects with ids, or plain strings, like the server's own
// parseHistoryBytes). Ids are preserved where present — the merge
// path needs them for the deletion tombstones. Empty or unparseable
// content is an error: importing nothing is never a silent success.
func parseWebDAVBackupContent(data []byte) ([]HistoryEntry, error) {
	var doc struct {
		Commands []json.RawMessage `json:"commands"`
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &doc); err == nil && doc.Commands != nil {
		raws = doc.Commands
	} else {
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, fmt.Errorf("备份文件格式不正确，需要 {\"commands\": [...]} 或命令数组")
		}
		raws = arr
	}
	var entries []HistoryEntry
	for _, raw := range raws {
		var e HistoryEntry
		if err := json.Unmarshal(raw, &e); err == nil && e.Command != "" {
			entries = append(entries, e)
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			entries = append(entries, HistoryEntry{Command: s})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("备份文件无有效命令")
	}
	return entries, nil
}

// downloadWebDAVBackup downloads one cloud backup and parses it.
// The download endpoint streams the file body directly (no wrapper).
func downloadWebDAVBackup(base, filename string) ([]HistoryEntry, error) {
	data, status, err := webdavAPIPost(base, "/api/webdav/download", map[string]any{"filename": filename})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		if msg := decodeWebDAVError(data); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("HTTP %d", status)
	}
	return parseWebDAVBackupContent(data)
}

// deleteWebDAVBackup deletes one cloud backup file. It touches only
// the cloud copy, never local history.
func deleteWebDAVBackup(base, filename string) error {
	data, status, err := webdavAPIPost(base, "/api/webdav/delete", map[string]any{"filename": filename})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		if msg := decodeWebDAVError(data); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

// webdavTestResult is a decoded /api/webdav/test response. The proxy
// answers HTTP 200 with {status, ok} on a completed probe, or an
// HTTP error status with {error} on a proxy-level failure (not
// configured, upstream unreachable).
type webdavTestResult struct {
	Status int
	Ok     bool
	ErrMsg string
}

func testWebDAVEndpoint(base string) (webdavTestResult, error) {
	data, _, err := webdavAPIPost(base, "/api/webdav/test", map[string]any{})
	if err != nil {
		return webdavTestResult{}, err
	}
	var doc struct {
		Status int    `json:"status"`
		Ok     bool   `json:"ok"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return webdavTestResult{}, fmt.Errorf("测试结果解析失败: %v", err)
	}
	if doc.Error != "" {
		return webdavTestResult{ErrMsg: doc.Error}, nil
	}
	return webdavTestResult{Status: doc.Status, Ok: doc.Ok}, nil
}
