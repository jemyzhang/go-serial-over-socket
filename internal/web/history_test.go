package web

import (
	"testing"
)

func TestDedupHistory(t *testing.T) {
	entries := []CommandEntry{
		{ID: "1", Command: "ls -la", Timestamp: 100},
		{ID: "2", Command: "pwd", Timestamp: 90},
		{ID: "3", Command: "ls -la", Timestamp: 80}, // duplicate
		{ID: "4", Command: "  pwd  ", Timestamp: 70}, // duplicate with whitespace
		{ID: "5", Command: "git status", Timestamp: 60},
	}

	result := dedupHistory(entries)

	if len(result) != 3 {
		t.Fatalf("expected 3 entries after deduplication, got %d", len(result))
	}

	expected := []string{"ls -la", "pwd", "git status"}
	for i, exp := range expected {
		if result[i].Command != exp {
			t.Errorf("at index %d: expected %q, got %q", i, exp, result[i].Command)
		}
	}
}

func TestParseHistoryBytesDeduplication(t *testing.T) {
	rawJSON := `[
		{"id": "1", "command": "echo 1", "timestamp": 100},
		{"id": "2", "command": "echo 2", "timestamp": 90},
		{"id": "3", "command": "echo 1", "timestamp": 80}
	]`

	entries := parseHistoryBytes([]byte(rawJSON))
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries after parse, got %d", len(entries))
	}

	if entries[0].Command != "echo 1" || entries[1].Command != "echo 2" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestParseLegacyStringArrayDeduplication(t *testing.T) {
	rawJSON := `["cat file", "grep foo", "cat file", "ls"]`

	entries := parseHistoryBytes([]byte(rawJSON))
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries after parse, got %d", len(entries))
	}

	expected := []string{"cat file", "grep foo", "ls"}
	for i, exp := range expected {
		if entries[i].Command != exp {
			t.Errorf("at index %d: expected %q, got %q", i, exp, entries[i].Command)
		}
	}
}
