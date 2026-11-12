package web

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFilterTombstonedEntries(t *testing.T) {
	entries := []CommandEntry{
		{ID: "a", Command: "AT+VERSION"},
		{ID: "b", Command: "STATUS?"},
		{ID: "c", Command: "AT+RESET"},
	}
	got := filterTombstonedEntries(entries, map[string]bool{"b": true})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("filter = %v, want [a c]", got)
	}
	// Nil tomb set is a no-op (server without loaded sidecar).
	if got := filterTombstonedEntries(entries, nil); len(got) != 3 {
		t.Fatalf("nil tomb set filtered to %v, want all 3", got)
	}
}

func TestAddTombstoneIDCap(t *testing.T) {
	var ids []string
	for i := 0; i < 7; i++ {
		ids = addTombstoneID(ids, string(rune('a'+i)), 5)
	}
	if got := len(ids); got != 5 {
		t.Fatalf("len = %d, want 5", got)
	}
	// Oldest first evicted: a, b gone; c..g remain in order.
	want := []string{"c", "d", "e", "f", "g"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	// Re-adding an existing id is a no-op (no duplicate, no reorder).
	if got := addTombstoneID(ids, "e", 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("re-add = %v, want %v", got, want)
	}
}

func TestRemoveTombstoneIDs(t *testing.T) {
	ids := []string{"a", "b", "c"}
	kept, changed := removeTombstoneIDs(ids, map[string]bool{"b": true})
	if !changed || !reflect.DeepEqual(kept, []string{"a", "c"}) {
		t.Fatalf("kept = %v changed = %v, want [a c] true", kept, changed)
	}
	if _, changed := removeTombstoneIDs(ids, map[string]bool{"z": true}); changed {
		t.Error("removing an absent id must not report a change")
	}
}

func TestTombstoneSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history_tombstones.json")
	if err := saveTombstoneIDs(path, []string{"x", "y"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadTombstoneIDs(path)
	if err != nil || !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("load = %v, %v, want [x y], nil", got, err)
	}
	// Missing file means no tombstones, not an error.
	if got, err := loadTombstoneIDs(filepath.Join(dir, "nope.json")); err != nil || len(got) != 0 {
		t.Fatalf("missing file: got %v, %v, want empty, nil", got, err)
	}
	// Corrupt file fails loudly (caller fail-opens with a log).
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTombstoneIDs(path); err == nil {
		t.Error("corrupt sidecar should return an error")
	}
}

// TestMergeSkipsTombstones composes the exact restart-merge path at
// the function level: local history [A], tombstone {B}, cloud backup
// [B, C] (B deleted, C new from another device). The merge must keep
// A and C and must NOT resurrect B — acceptance (a) and (b).
func TestMergeSkipsTombstones(t *testing.T) {
	local := []CommandEntry{{ID: "a", Command: "CMD-A"}}
	remote := []CommandEntry{
		{ID: "b", Command: "CMD-B"},
		{ID: "c", Command: "CMD-C"},
	}
	tomb := map[string]bool{"b": true}
	merged := dedupHistory(append(local, filterTombstonedEntries(remote, tomb)...))
	if len(merged) != 2 {
		t.Fatalf("merged = %v, want 2 entries", merged)
	}
	ids := []string{merged[0].ID, merged[1].ID}
	if !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Fatalf("merged ids = %v, want [a c]", ids)
	}
}
