package web

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// History deletion tombstones.
//
// Problem: history merges are unions with no notion of deletion.
// loadHistoryFromWebDAV (startup, and after saving WebDAV settings)
// merges the newest cloud backup into local history, and the
// merge_history import does the same for an uploaded file. A command
// deleted locally therefore comes back on the next restart whenever
// any cloud backup still contains it — the upload side only pushes on
// an interval tick, so the cloud copy is routinely older than the
// local deletion. Deleting every cloud backup "fixes" it only by
// destroying the backups.
//
// Fix: deleting an entry records its ID in a tombstone sidecar, and
// every merge path skips tombstoned IDs. The sidecar lives next to
// history.json (history_tombstones.json) and deliberately does NOT
// change the history.json bare-array format or the cloud backup
// file format.
//
// Scope notes:
//   - Tombstones are keyed by entry ID, not by command text. A user
//     re-sending the same command text creates a NEW id (add_command),
//     which is a new intent and is not blocked. Conversely, a
//     same-text re-add under a different id (e.g. a string-array
//     import, which synthesizes ids) is not blocked either —
//     tombstones stop id-stable resurrections (cloud backups,
//     entry-preserving imports), not deliberate re-adds.
//   - replace_history is NOT filtered: it is an explicit full
//     replacement, and restoring a backup that way also clears the
//     tombstones of the entries it brings back (a deliberate
//     un-delete; keeping them would fight every later merge).
//   - Tombstones are local to this server. They stop THIS server
//     resurrecting its own deletions; they are not synced, so a
//     second device that never deleted the entry still has it
//     locally — but re-merging it here is still blocked by id.

// maxHistoryTombstones bounds the sidecar. Deletions are low
// frequency, so 5000 ids (~150KB) is generous. Eviction drops the
// oldest first: the oldest deletions' cloud backups are the most
// likely already pruned (MaxBackups, default 10), so they are the
// safest to forget. Even after eviction, resurrection still needs
// a retained cloud backup containing that id to be merged.
const maxHistoryTombstones = 5000

// getHistoryTombstonePath returns the sidecar path next to history.json.
func getHistoryTombstonePath() string {
	return filepath.Join(filepath.Dir(getHistoryPath()), "history_tombstones.json")
}

type tombstoneFile struct {
	IDs []string `json:"ids"`
}

// loadTombstoneIDs reads the sidecar. A missing file means no
// tombstones; a corrupt file logs loudly and also yields none
// (fail-open: the worst case is the old resurrection behavior,
// never a startup failure).
func loadTombstoneIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc tombstoneFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.IDs, nil
}

// saveTombstoneIDs persists ids atomically (temp file + rename in
// the same directory) so a crash mid-write cannot leave a half
// written sidecar behind.
func saveTombstoneIDs(path string, ids []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tombstoneFile{IDs: ids}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tombstones-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// addTombstoneID appends id if absent, evicting the oldest entries
// while the list exceeds max. It is pure (no I/O) so the cap policy
// is unit-testable; callers persist the result.
func addTombstoneID(ids []string, id string, max int) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	ids = append(ids, id)
	for len(ids) > max {
		ids = ids[1:]
	}
	return ids
}

// removeTombstoneIDs drops every id in remove from ids, preserving
// order. It reports whether anything changed so callers only
// rewrite the sidecar when needed.
func removeTombstoneIDs(ids []string, remove map[string]bool) ([]string, bool) {
	if len(remove) == 0 {
		return ids, false
	}
	kept := ids[:0]
	changed := false
	for _, id := range ids {
		if remove[id] {
			changed = true
			continue
		}
		kept = append(kept, id)
	}
	return kept, changed
}

// filterTombstonedEntries drops entries whose id is tombstoned.
// A nil tomb set is a no-op. It is pure so merge behavior is
// unit-testable without a server.
func filterTombstonedEntries(entries []CommandEntry, tomb map[string]bool) []CommandEntry {
	if len(tomb) == 0 {
		return entries
	}
	kept := make([]CommandEntry, 0, len(entries))
	for _, e := range entries {
		if tomb[e.ID] {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// loadHistoryTombstones loads the sidecar into the server. Call
// before loadLocalHistory so the local file is filtered too.
func (ws *WebServer) loadHistoryTombstones() {
	ids, err := loadTombstoneIDs(getHistoryTombstonePath())
	if err != nil {
		log.Printf("history tombstones unreadable (%v); continuing without", err)
		ids = nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	ws.historyMu.Lock()
	ws.historyTombstoneIDs = ids
	ws.historyTombstoneSet = set
	ws.historyMu.Unlock()
}

// saveHistoryTombstonesLocked persists the in-memory set. The
// caller must hold historyMu: tombstone writes are small and
// infrequent, and holding the lock across the write (unlike
// saveLocalHistory's snapshot pattern) means a concurrent delete
// cannot snapshot an older set and clobber a newer one.
func (ws *WebServer) saveHistoryTombstonesLocked() {
	if err := saveTombstoneIDs(getHistoryTombstonePath(), ws.historyTombstoneIDs); err != nil {
		log.Printf("history tombstones save failed: %v", err)
	}
}

// addHistoryTombstoneLocked records one deletion. The caller must
// hold historyMu: delete_command removes the entry and plants its
// tombstone in a single lock hold, so a concurrent merge can
// neither slip in between nor observe a half-updated state (see
// there). Holding the lock and calling the wrapper instead would
// deadlock. No-op for an id that is already tombstoned.
func (ws *WebServer) addHistoryTombstoneLocked(id string) {
	if ws.historyTombstoneSet == nil {
		ws.historyTombstoneSet = map[string]bool{}
	}
	if ws.historyTombstoneSet[id] {
		return
	}
	ws.historyTombstoneIDs = addTombstoneID(ws.historyTombstoneIDs, id, maxHistoryTombstones)
	ws.historyTombstoneSet = make(map[string]bool, len(ws.historyTombstoneIDs))
	for _, kept := range ws.historyTombstoneIDs {
		ws.historyTombstoneSet[kept] = true
	}
	ws.saveHistoryTombstonesLocked()
}

// addHistoryTombstone records one deletion (locking wrapper around
// addHistoryTombstoneLocked).
func (ws *WebServer) addHistoryTombstone(id string) {
	ws.historyMu.Lock()
	defer ws.historyMu.Unlock()
	ws.addHistoryTombstoneLocked(id)
}

// clearHistoryTombstonesLocked forgets the tombstones of the given
// ids (the replace_history restore path: bringing entries back by
// hand is a deliberate un-delete). The caller must hold historyMu:
// the replace path clears in the same hold as the swap, so a
// concurrent delete can neither plant a tombstone the clear then
// removes nor observe a half-updated state. Holding the lock and
// calling the wrapper instead would deadlock. It persists only
// when changed.
func (ws *WebServer) clearHistoryTombstonesLocked(ids []string) {
	if len(ids) == 0 {
		return
	}
	remove := make(map[string]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}
	kept, changed := removeTombstoneIDs(ws.historyTombstoneIDs, remove)
	if !changed {
		return
	}
	ws.historyTombstoneIDs = kept
	ws.historyTombstoneSet = make(map[string]bool, len(kept))
	for _, id := range kept {
		ws.historyTombstoneSet[id] = true
	}
	ws.saveHistoryTombstonesLocked()
}

// clearHistoryTombstones is the locking wrapper around
// clearHistoryTombstonesLocked.
func (ws *WebServer) clearHistoryTombstones(ids []string) {
	if len(ids) == 0 {
		return
	}
	ws.historyMu.Lock()
	defer ws.historyMu.Unlock()
	ws.clearHistoryTombstonesLocked(ids)
}

// requestHistoryBackupSoon coalesces a "cloud copy is stale" signal
// into one background upload a few seconds later. It never blocks:
// the channel holds one pending signal, further signals while one
// is pending are dropped (the pending upload already covers them).
func (ws *WebServer) requestHistoryBackupSoon() {
	select {
	case ws.backupSoonCh <- struct{}{}:
	default:
	}
}

// backupSoonLoop uploads the current history a short while after a
// deletion, so the newest cloud backup converges to the post-delete
// list without waiting for the interval tick. It is best-effort
// acceleration only — tombstones, not this upload, are the
// guarantee: if the process quits before the window fires, the
// tombstone still blocks the stale backup on the next start.
// historyChanged is deliberately NOT cleared here: clearing it
// after snapshotting could drop a concurrent change's upload, while
// leaving it only costs the interval loop one redundant (pruned)
// upload. The interval loop owns the flag through
// tryHistoryUpload (swap-take, restore on failure); this loop
// only accelerates and never takes ownership.
func (ws *WebServer) backupSoonLoop() {
	for range ws.backupSoonCh {
		// Coalescing window: a clear-all is N rapid deletes; one
		// upload after the burst covers all of them.
		time.Sleep(3 * time.Second)
		// Drain any signal that arrived during the window so the
		// upload below already covers it (no second upload).
		select {
		case <-ws.backupSoonCh:
		default:
		}
		if wd := ws.getWebDAV(); wd.URL != "" {
			if err := ws.saveHistoryToWebDAV(); err != nil {
				log.Printf("history backup-soon upload failed: %v", err)
			}
		}
	}
}
