package web

import (
	"encoding/json"
	"sync"
	"testing"
)

// Concurrent replace + delete must never lose the deletion's
// tombstone. Previously replace_history swapped the history,
// unlocked, and only then cleared the restored ids' tombstones:
// a delete landing in that window planted a tombstone the delayed
// clear removed, leaving the entry deleted but tombstoneless, so a
// later merge of an old backup resurrected it. The test drives the
// real WS command handlers (no clients attached, history redirected
// to a temp HOME) in rounds: each round seeds the id tombstoned but
// absent, then races ONE replace (restoring the id) against several
// deletes. After both settle, exactly one outcome is valid per
// side: either the id is back and its tombstone is gone (replace
// won), or the id is gone and its tombstone stands (delete won).
// "Gone and tombstoneless" is the lost-deletion state and fails the
// round; "back and still tombstoned" would break a later merge's
// filtering of the restored entry and fails too.
func TestReplaceDeleteTombstoneRace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := &WebServer{
		clients:      map[string]*WebSocketClient{},
		backupSoonCh: make(chan struct{}, 1),
	}
	client := &WebSocketClient{}
	replaceMsg, _ := json.Marshal(map[string]any{
		"type": "replace_history",
		"commands": []any{
			map[string]any{"id": "doomed", "command": "AT+DOOMED"},
			map[string]any{"id": "k1", "command": "AT+K1"},
		},
	})
	deleteMsg, _ := json.Marshal(map[string]any{"type": "delete_command", "id": "doomed"})

	for round := 0; round < 50; round++ {
		// All goroutines from the previous round have joined,
		// so this reset needs no lock.
		ws.commandHistory = []CommandEntry{
			{ID: "k1", Command: "AT+K1"},
			{ID: "k2", Command: "AT+K2"},
		}
		ws.historyTombstoneIDs = []string{"doomed"}
		ws.historyTombstoneSet = map[string]bool{"doomed": true}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws.handleWebSocketCommand(client, replaceMsg)
		}()
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ws.handleWebSocketCommand(client, deleteMsg)
			}()
		}
		wg.Wait()

		present := false
		for _, e := range ws.commandHistory {
			if e.ID == "doomed" {
				present = true
			}
		}
		tombstoned := ws.historyTombstoneSet["doomed"]
		if !present && !tombstoned {
			t.Fatalf("round %d: deleted id lost its tombstone (history=%v tombstones=%v)",
				round, ws.commandHistory, ws.historyTombstoneIDs)
		}
		if present && tombstoned {
			t.Fatalf("round %d: restored id still tombstoned (history=%v tombstones=%v)",
				round, ws.commandHistory, ws.historyTombstoneIDs)
		}
	}
}
