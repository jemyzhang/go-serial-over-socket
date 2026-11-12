package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Concurrent delete + merge must never resurrect the deleted id.
// Previously the tombstone was written after the delete's lock was
// released, so a merge landing in that window re-added the entry
// and saved it back to disk. The test drives the real WS command
// handlers (no clients attached, history redirected to a temp
// HOME) in rounds: each round seeds the id with no tombstone,
// then races ONE delete against many merges. A single delete per
// round is deliberate — with repeated deletes, a later delete
// would clean up an earlier round's resurrection and hide the
// window; here any merge landing in the delete's unlock-to-
// tombstone window resurrects permanently and fails the round.
//
// Note on the disk assertion: saveLocalHistory snapshots at save
// time, so the final on-disk state is asserted after one last
// save of the (clean) in-memory list, not mid-storm — a stale
// snapshot racing a delete's write is a pre-existing ordering
// property shared by all handlers, and it cannot resurrect into
// memory anyway: the next load filters by tombstone.
func TestDeleteMergeRace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := &WebServer{
		clients:      map[string]*WebSocketClient{},
		backupSoonCh: make(chan struct{}, 1),
	}
	client := &WebSocketClient{}
	deleteMsg, _ := json.Marshal(map[string]any{"type": "delete_command", "id": "doomed"})
	mergeMsg, _ := json.Marshal(map[string]any{
		"type": "merge_history",
		"commands": []any{
			map[string]any{"id": "doomed", "command": "AT+DOOMED"},
			map[string]any{"id": "k1", "command": "AT+K1"},
			map[string]any{"id": "m1", "command": "AT+M1"},
		},
	})

	for round := 0; round < 50; round++ {
		// All goroutines from the previous round have joined,
		// so this reset needs no lock.
		ws.commandHistory = []CommandEntry{
			{ID: "doomed", Command: "AT+DOOMED"},
			{ID: "k1", Command: "AT+K1"},
			{ID: "k2", Command: "AT+K2"},
			{ID: "k3", Command: "AT+K3"},
		}
		ws.historyTombstoneIDs = nil
		ws.historyTombstoneSet = map[string]bool{}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws.handleWebSocketCommand(client, deleteMsg)
		}()
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					ws.handleWebSocketCommand(client, mergeMsg)
				}
			}()
		}
		wg.Wait()

		seen := map[string]bool{}
		for _, e := range ws.commandHistory {
			if e.ID == "doomed" {
				t.Fatalf("round %d: deleted id resurrected: %v", round, ws.commandHistory)
			}
			if seen[e.ID] {
				t.Fatalf("round %d: duplicate id %q in %v", round, e.ID, ws.commandHistory)
			}
			seen[e.ID] = true
		}
		for _, k := range []string{"k1", "k2", "k3", "m1"} {
			if !seen[k] {
				t.Fatalf("round %d: id %q lost: %v", round, k, ws.commandHistory)
			}
		}
	}

	ws.saveLocalHistory()
	data, err := os.ReadFile(filepath.Join(home, ".config", "go-serial-over-socket", "history.json"))
	if err != nil {
		t.Fatalf("history file unreadable: %v", err)
	}
	if strings.Contains(string(data), "doomed") {
		t.Errorf("deleted id persisted on disk: %s", data)
	}
}
