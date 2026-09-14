package daemon

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

func TestListTasksDefaultsAndValidation(t *testing.T) {
	got, err := parseListTasksArgs(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []string{"open", "claimed", "needs_info", "blocked"}
	if !reflect.DeepEqual(got.Query.Statuses, wantStatuses) || got.Limit != 100 {
		t.Fatalf("defaults = %+v", got)
	}
	for name, args := range map[string]map[string]any{
		"invalid status": {"statuses": []any{"open", "bogus"}},
		"invalid kind":   {"to_kind": "queue"},
		"target no kind": {"to_target": "reviewer"},
		"zero limit":     {"limit": float64(0)},
		"over max":       {"limit": float64(501)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseListTasksArgs(args); err == nil {
				t.Fatalf("parseListTasksArgs(%v) must fail", args)
			}
		})
	}
}

func TestDaemonListTasksDefaultsFiltersAndIsPure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, agent := range []store.Agent{{Alias: "a1", Project: "alpha"}, {Alias: "b1", Project: "beta"}} {
		if err := db.RegisterAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	for _, thread := range []store.Thread{
		{Kind: "task", FromAgent: "a1", ToKind: "role", ToTarget: "reviewer", Status: "open", Subject: "open", OriginProject: "alpha"},
		{Kind: "task", FromAgent: "a1", ToKind: "agent", ToTarget: "b1", Status: "blocked", Subject: "blocked", OriginProject: "alpha"},
		{Kind: "task", FromAgent: "b1", ToKind: "role", ToTarget: "reviewer", Status: "completed", Subject: "done", OriginProject: "beta"},
		{Kind: "message", FromAgent: "a1", ToKind: "broadcast", Subject: "message"},
	} {
		if _, err := db.CreateThread(thread, "body"); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{s: db}
	before, err := db.MaxEventID()
	if err != nil {
		t.Fatal(err)
	}

	decodeTasks := func(args map[string]any) (tasks []store.Thread, truncated bool) {
		t.Helper()
		resp := d.dispatch(proto.Request{Op: "list_tasks", Args: args})
		if !resp.OK {
			t.Fatalf("list_tasks(%v): %s", args, resp.Error)
		}
		b, _ := json.Marshal(resp.Data)
		var out struct {
			Tasks     []store.Thread `json:"tasks"`
			Truncated bool           `json:"truncated"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out.Tasks, out.Truncated
	}
	defaults, truncated := decodeTasks(nil)
	if truncated || len(defaults) != 2 || defaults[0].Subject != "blocked" || defaults[1].Subject != "open" {
		t.Fatalf("defaults = %+v truncated=%v", defaults, truncated)
	}
	terminal, _ := decodeTasks(map[string]any{"statuses": []any{"completed"}, "from": "b1"})
	if len(terminal) != 1 || terminal[0].Subject != "done" {
		t.Fatalf("terminal filter = %+v", terminal)
	}
	beta, _ := decodeTasks(map[string]any{"project": "beta"})
	if len(beta) != 1 || beta[0].Subject != "blocked" {
		t.Fatalf("project filter = %+v", beta)
	}
	limited, truncated := decodeTasks(map[string]any{"limit": float64(1)})
	if !truncated || len(limited) != 1 {
		t.Fatalf("limited = %+v truncated=%v", limited, truncated)
	}
	after, err := db.MaxEventID()
	if err != nil || after != before {
		t.Fatalf("event watermark moved: before=%d after=%d err=%v", before, after, err)
	}
}

func TestFinishTaskListFiltersProjectsAndReportsTruncation(t *testing.T) {
	req := listTasksRequest{Project: "alpha", Limit: 2}
	tasks := []store.Thread{
		{ID: 4, FromAgent: "a1", ToKind: "role", ToTarget: "reviewer", LastFrom: "outsider", OriginProject: "alpha"},
		{ID: 3, FromAgent: "b1", ToKind: "agent", ToTarget: "a1", LastFrom: "b1"},
		{ID: 2, FromAgent: "b1", ToKind: "role", ToTarget: "reviewer", LastFrom: "b1", OriginProject: "beta"},
		{ID: 1, FromAgent: "gone", ToKind: "role", ToTarget: "reviewer", OriginProject: "alpha"},
	}
	agents := []store.Agent{{Alias: "a1", Project: "alpha"}, {Alias: "b1", Project: "beta"}}

	got, truncated := finishTaskList(tasks, agents, req)
	if !truncated || len(got) != 2 || got[0].ID != 4 || got[1].ID != 3 {
		t.Fatalf("tasks = %+v truncated=%v", got, truncated)
	}
}
