package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

func decodeThreadList(t *testing.T, response proto.Response) (threads []store.Thread, truncated bool) {
	t.Helper()
	if !response.OK {
		t.Fatalf("list_threads: %s", response.Error)
	}
	data, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Threads              []store.Thread `json:"threads"`
		ActiveTasksTruncated bool           `json:"active_tasks_truncated"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out.Threads, out.ActiveTasksTruncated
}

func TestListThreadsCanRetainOldActiveTasksWithoutChangingLegacyCalls(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := int64(1000)
	clock.SetForTesting(func() int64 { now++; return now })
	t.Cleanup(clock.ResetForTesting)

	oldTask, err := db.CreateThread(store.Thread{Kind: "task", FromAgent: "a", ToKind: "role", ToTarget: "reviewer", Status: "open"}, "old")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := db.CreateThread(store.Thread{Kind: "message", FromAgent: "a", ToKind: "broadcast"}, "recent"); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{s: db}

	legacy, legacyTruncated := decodeThreadList(t, d.dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": float64(200)}}))
	if legacyTruncated || len(legacy) != 200 {
		t.Fatalf("legacy len=%d truncated=%v", len(legacy), legacyTruncated)
	}
	for _, thread := range legacy {
		if thread.ID == oldTask {
			t.Fatal("legacy list_threads unexpectedly retained old task")
		}
	}

	withActive, truncated := decodeThreadList(t, d.dispatch(proto.Request{Op: "list_threads", Args: map[string]any{
		"limit": float64(200), "include_nonterminal_tasks": true,
	}}))
	if truncated || len(withActive) != 201 || withActive[len(withActive)-1].ID != oldTask {
		t.Fatalf("with active len=%d truncated=%v last=%+v", len(withActive), truncated, withActive[len(withActive)-1])
	}
	seen := map[int64]bool{}
	for _, thread := range withActive {
		if seen[thread.ID] {
			t.Fatalf("duplicate thread %d", thread.ID)
		}
		seen[thread.ID] = true
	}
}

func TestListThreadsCapsActiveTaskRetentionAt500(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for i := 0; i < 501; i++ {
		if _, err := db.CreateThread(store.Thread{Kind: "task", FromAgent: "a", ToKind: "role", ToTarget: "reviewer", Status: "open"}, "task"); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{s: db}
	threads, truncated := decodeThreadList(t, d.dispatch(proto.Request{Op: "list_threads", Args: map[string]any{
		"limit": float64(1), "include_nonterminal_tasks": true,
	}}))
	if !truncated || len(threads) != 500 {
		t.Fatalf("len=%d truncated=%v", len(threads), truncated)
	}
}
