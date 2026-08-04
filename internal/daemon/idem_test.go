package daemon

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// threadCount decodes a list_threads response and reports how many threads it
// carries. Follows the re-marshal-then-unmarshal approach the other daemon
// tests use for typed response payloads.
func threadCount(t *testing.T, resp proto.Response) int {
	t.Helper()
	b, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("marshal list_threads data: %v", err)
	}
	var got struct {
		Threads []store.Thread `json:"threads"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode list_threads data: %v (%s)", err, b)
	}
	return len(got.Threads)
}

// wireForm renders a response the way a remote client actually receives it,
// then normalizes it back through a generic decode.
//
// Both halves matter, and reflect.DeepEqual on the raw proto.Response values
// would fail on both counts without a client being able to tell:
//
//   - The record is JSON, so a replayed Data decodes to generic map/float64
//     values where the original was a typed struct or an int64. Marshalling
//     both back to JSON erases that: int64(1) and float64(1) are both "1".
//   - A struct marshals its fields in declaration order while a
//     map[string]any marshals them sorted, so the replay's BYTES can differ in
//     key order alone. JSON objects are unordered, so this is invisible to any
//     client that parses the response — which is every client — but it is why
//     the comparison is semantic rather than byte-for-byte.
func wireForm(t *testing.T, resp proto.Response) string {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("decode response: %v (%s)", err, b)
	}
	normalized, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-marshal response: %v", err)
	}
	return string(normalized)
}

func seedAgent(t *testing.T, d *Daemon, alias string) {
	t.Helper()
	if r := d.Dispatch(proto.Request{Op: "register_agent", Args: map[string]any{
		"alias": alias, "role": "peer", "model_type": "claude",
	}}); !r.OK {
		t.Fatalf("register %s: %s", alias, r.Error)
	}
}

func TestDispatchDeduplicatesWritesByIdemKey(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	seedAgent(t, d, "a1")

	req := proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "once"},
		IdemKey: "k-dup",
	}
	first := d.Dispatch(req)
	if !first.OK {
		t.Fatalf("first send: %s", first.Error)
	}
	second := d.Dispatch(req)
	if !second.OK {
		t.Fatalf("replay: %s", second.Error)
	}

	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if !threads.OK {
		t.Fatalf("list_threads: %s", threads.Error)
	}
	if n := threadCount(t, threads); n != 1 {
		t.Fatalf("replayed write created %d threads, want 1", n)
	}
	if a, b := wireForm(t, first), wireForm(t, second); a != b {
		t.Fatalf("replay is distinguishable from the original:\n first=%s\nsecond=%s", a, b)
	}
}

// TestDispatchIgnoresIdemKeyOnReads pins the rule behaviourally rather than by
// shape: it is not enough that two reads both succeed — the read must leave no
// record behind, so a LATER write on the same key still gets to execute.
func TestDispatchIgnoresIdemKeyOnReads(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	seedAgent(t, d, "a1")

	r1 := d.Dispatch(proto.Request{Op: "list_agents", IdemKey: "k-read"})
	r2 := d.Dispatch(proto.Request{Op: "list_agents", IdemKey: "k-read"})
	if !r1.OK || !r2.OK {
		t.Fatalf("reads failed: %s / %s", r1.Error, r2.Error)
	}

	w := d.Dispatch(proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x"},
		IdemKey: "k-read",
	})
	if !w.OK {
		t.Fatalf("a read consumed the idempotency record: write on the same key got %q", w.Error)
	}
	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if n := threadCount(t, threads); n != 1 {
		t.Fatalf("write after two keyed reads produced %d threads, want 1", n)
	}
}

// TestDispatchWithoutIdemKeyIsUnaffected is the local-mode guarantee: local
// clients send no key, so two identical writes are two writes, exactly as
// before this wrapper existed.
func TestDispatchWithoutIdemKeyIsUnaffected(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	seedAgent(t, d, "a1")

	req := proto.Request{Op: "send_message", Args: map[string]any{
		"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "twice",
	}}
	if r := d.Dispatch(req); !r.OK {
		t.Fatalf("first send: %s", r.Error)
	}
	if r := d.Dispatch(req); !r.OK {
		t.Fatalf("second send: %s", r.Error)
	}
	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if n := threadCount(t, threads); n != 2 {
		t.Fatalf("unkeyed writes produced %d threads, want 2 (local mode must be untouched)", n)
	}
}

// TestDispatchInFlightCollisionIsRetryable drives the third state of the
// IdemBegin contract with a REAL record: the key is claimed directly on the
// store and never completed, which is exactly what a caller mid-execution
// leaves behind.
func TestDispatchInFlightCollisionIsRetryable(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	seedAgent(t, d, "a1")

	if _, _, found, err := s.IdemBegin("k-inflight"); err != nil || found {
		t.Fatalf("pre-claim: found=%v err=%v, want found=false", found, err)
	}
	resp := d.Dispatch(proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x"},
		IdemKey: "k-inflight",
	})
	if resp.OK {
		t.Fatal("a collision with an in-flight request must not execute the op")
	}
	if !IsRetryableIdemError(resp.Error) {
		t.Fatalf("in-flight collision error = %q, want a retryable one", resp.Error)
	}
	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if n := threadCount(t, threads); n != 0 {
		t.Fatalf("in-flight collision executed the op: %d threads, want 0", n)
	}
}

// idemFailingStore fails IdemBegin/IdemComplete on command; every other method
// passes through to a real store.
type idemFailingStore struct {
	*store.Store
	failBegin    bool
	failComplete bool
}

func (m *idemFailingStore) IdemBegin(key string) ([]byte, bool, bool, error) {
	if m.failBegin {
		return nil, false, false, fmt.Errorf("injected IdemBegin failure")
	}
	return m.Store.IdemBegin(key)
}

func (m *idemFailingStore) IdemComplete(key string, resp []byte) error {
	if m.failComplete {
		return fmt.Errorf("injected IdemComplete failure")
	}
	return m.Store.IdemComplete(key, resp)
}

// TestDispatchIdemBeginErrorDoesNotExecute pins the error path Task 8 paid to
// learn: an IdemBegin that errors is UNKNOWN, not "not claimed". The op must
// not run, and the error must tell the client to reissue under a new key
// rather than invite a same-key retry that could sit wedged until the TTL.
func TestDispatchIdemBeginErrorDoesNotExecute(t *testing.T) {
	fs := &idemFailingStore{Store: newDaemonTestStore(t)}
	d := New(fs, nil)
	seedAgent(t, d, "a1")

	fs.failBegin = true
	resp := d.Dispatch(proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x"},
		IdemKey: "k-err",
	})
	if resp.OK {
		t.Fatal("an IdemBegin failure must not execute the op")
	}
	if IsRetryableIdemError(resp.Error) {
		t.Fatalf("an IdemBegin failure must not be reported as same-key retryable: %q", resp.Error)
	}
	fs.failBegin = false
	threads := d.Dispatch(proto.Request{Op: "list_threads", Args: map[string]any{"limit": 100}})
	if n := threadCount(t, threads); n != 0 {
		t.Fatalf("IdemBegin failure executed the op anyway: %d threads, want 0", n)
	}
}

// TestDispatchIdemCompleteErrorStillAnswers: the caller's op has already
// committed by the time IdemComplete runs, so a failure there must not turn a
// successful write into an error response. The key is left pending, which
// makes a redelivery read as in-flight — the safe direction (no double
// execution).
func TestDispatchIdemCompleteErrorStillAnswers(t *testing.T) {
	fs := &idemFailingStore{Store: newDaemonTestStore(t)}
	d := New(fs, nil)
	seedAgent(t, d, "a1")

	fs.failComplete = true
	resp := d.Dispatch(proto.Request{
		Op:      "send_message",
		Args:    map[string]any{"from": "a1", "to_kind": "agent", "to_target": "a1", "body": "x"},
		IdemKey: "k-complete",
	})
	if !resp.OK {
		t.Fatalf("IdemComplete failure must not fail a committed write: %s", resp.Error)
	}
}

// TestDispatchReplaysFailedWritesToo: at-most-once covers failures. The
// wrapper cannot tell whether an op mutated state before failing, so
// re-running it under the same key is exactly the double execution the record
// exists to prevent — the recorded failure replays instead.
func TestDispatchReplaysFailedWritesToo(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)

	req := proto.Request{
		Op:      "prune_events",
		Args:    map[string]any{"older_than_ms": 0}, // rejected by the op
		IdemKey: "k-fail",
	}
	first := d.Dispatch(req)
	if first.OK {
		t.Fatalf("expected prune_events to reject older_than_ms=0, got %+v", first)
	}
	second := d.Dispatch(req)
	if a, b := wireForm(t, first), wireForm(t, second); a != b {
		t.Fatalf("failed write did not replay verbatim:\n first=%s\nsecond=%s", a, b)
	}
}

// TestGetInboxIsIdempotencyProtected is why get_inbox is in the write set.
// It mutates read state (MarkRead), so a lost response followed by a
// redelivery would hand the client an inbox with every unread count already
// zeroed — the messages silently stop looking new. The replay must return the
// ORIGINAL unread counts.
func TestGetInboxIsIdempotencyProtected(t *testing.T) {
	s := newDaemonTestStore(t)
	d := New(s, nil)
	seedAgent(t, d, "author")
	seedAgent(t, d, "reader")

	if r := d.Dispatch(proto.Request{Op: "send_message", Args: map[string]any{
		"from": "author", "to_kind": "agent", "to_target": "reader", "subject": "s", "body": "x",
	}}); !r.OK {
		t.Fatalf("send: %s", r.Error)
	}

	req := proto.Request{Op: "get_inbox", Args: map[string]any{"alias": "reader"}, IdemKey: "k-inbox"}
	first := d.Dispatch(req)
	if !first.OK {
		t.Fatalf("get_inbox: %s", first.Error)
	}
	if !IsWriteOp("get_inbox") {
		t.Fatal("get_inbox mutates read state and must be classified as a write op")
	}
	second := d.Dispatch(req)
	if !second.OK {
		t.Fatalf("get_inbox replay: %s", second.Error)
	}
	if a, b := wireForm(t, first), wireForm(t, second); a != b {
		t.Fatalf("get_inbox replay lost the unread counts:\n first=%s\nsecond=%s", a, b)
	}
}

// readOps is the other half of the classification: every op Dispatch's switch
// handles is either here or in writeOps. Hand-maintained on purpose — the
// point is that adding an op forces a deliberate answer to "does this mutate
// anything", which TestEveryDispatchOpIsClassified enforces.
var readOps = map[string]bool{
	"list_agents": true, "session_aliases": true, "session_unread": true,
	"get_thread": true, "list_threads": true, "kv_get": true,
	// device_poll reads a watermark and answers with it; the watermark lives
	// on the POLLING DEVICE (the daemon's own loop variable), never in the
	// store, so two identical polls are indistinguishable to the bus.
	"list_events": true, "get_agent": true, "device_poll": true,
}

// dispatchOps extracts the op names from dispatch's `switch req.Op` in
// daemon.go, so the classification is checked against the code rather than
// against a second hand-written list that can drift with it.
func dispatchOps(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}
	var ops []string
	ast.Inspect(f, func(n ast.Node) bool {
		sw, isSwitch := n.(*ast.SwitchStmt)
		if !isSwitch {
			return true
		}
		sel, isSel := sw.Tag.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Op" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, isCase := stmt.(*ast.CaseClause)
			if !isCase {
				continue
			}
			for _, e := range cc.List {
				lit, isLit := e.(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					continue
				}
				op, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote case %s: %v", lit.Value, err)
				}
				ops = append(ops, op)
			}
		}
		return true
	})
	sort.Strings(ops)
	return ops
}

// TestEveryDispatchOpIsClassified is the drift guard for the silent hole: an
// op added to the switch but left out of writeOps would quietly execute twice
// under a redelivered key, and nothing else would notice.
func TestEveryDispatchOpIsClassified(t *testing.T) {
	ops := dispatchOps(t)
	if len(ops) < 10 {
		t.Fatalf("only found %d ops in dispatch's switch (%v) — the AST walk is broken, not the classification", len(ops), ops)
	}
	for _, op := range ops {
		w, r := IsWriteOp(op), readOps[op]
		switch {
		case w && r:
			t.Errorf("%s: classified as BOTH a write and a read", op)
		case !w && !r:
			t.Errorf("%s: unclassified — add it to writeOps (it mutates state) or to readOps in this test", op)
		}
	}
	for op := range writeOps {
		if !contains(ops, op) {
			t.Errorf("writeOps has %q, which dispatch's switch does not handle", op)
		}
	}
	for op := range readOps {
		if !contains(ops, op) {
			t.Errorf("readOps has %q, which dispatch's switch does not handle", op)
		}
	}
}

// badgeSinks are the three functions that actually write a tmux badge. An op
// "moves a badge" exactly when its dispatch reaches one of them.
var badgeSinks = map[string]bool{
	"notifyForThread": true, "setSessionBadge": true, "reconcileBadge": true,
}

// calleeNames returns the name of every function or method called anywhere in
// n — `foo()` as "foo", `x.Foo()` as "Foo". Coarse on purpose: the graph it
// feeds only has to over-approximate reachability, and a false edge would make
// the drift guard demand MORE ops be classified as badge-moving, never fewer.
func calleeNames(n ast.Node) []string {
	var names []string
	ast.Inspect(n, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			names = append(names, fn.Name)
		case *ast.SelectorExpr:
			names = append(names, fn.Sel.Name)
		}
		return true
	})
	return names
}

// TestBadgeOpsMatchDispatch is the drift guard for badgeOps, remote mode's
// reconcile trigger. It derives the badge-moving set from dispatch's own
// switch — walking the call graph within daemon.go, so a case that reaches the
// badge through a handler (register_agent → handleRegisterAgent →
// reconcileBadge) still counts — and demands the map say the same thing.
//
// Without this, a new notifying op would keep working in local mode and
// silently never light a badge in remote mode: the wrong half of the failure,
// and invisible until an operator noticed mail they were never told about.
func TestBadgeOpsMatchDispatch(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatalf("parse daemon.go: %v", err)
	}
	callees := map[string][]string{}
	for _, decl := range f.Decls {
		fn, isFn := decl.(*ast.FuncDecl)
		if !isFn || fn.Body == nil {
			continue
		}
		callees[fn.Name.Name] = calleeNames(fn.Body)
	}
	var reaches func(name string, seen map[string]bool) bool
	reaches = func(name string, seen map[string]bool) bool {
		if badgeSinks[name] {
			return true
		}
		if seen[name] {
			return false
		}
		seen[name] = true
		for _, callee := range callees[name] {
			if reaches(callee, seen) {
				return true
			}
		}
		return false
	}

	derived := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sw, isSwitch := n.(*ast.SwitchStmt)
		if !isSwitch {
			return true
		}
		if sel, isSel := sw.Tag.(*ast.SelectorExpr); !isSel || sel.Sel.Name != "Op" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, isCase := stmt.(*ast.CaseClause)
			if !isCase {
				continue
			}
			hit := false
			for _, callee := range calleeNames(&ast.BlockStmt{List: cc.Body}) {
				if reaches(callee, map[string]bool{}) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			for _, e := range cc.List {
				lit, isLit := e.(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					continue
				}
				op, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote case %s: %v", lit.Value, err)
				}
				derived[op] = true
			}
		}
		return true
	})

	if len(derived) < 5 {
		t.Fatalf("derived only %v as badge-moving — the AST walk is broken, not the classification", derived)
	}
	for op := range derived {
		if !movesBadge(op) {
			t.Errorf("%s reaches the tmux badge in dispatch but is missing from badgeOps: in remote mode its badge would never light", op)
		}
	}
	for op := range badgeOps {
		if !derived[op] {
			t.Errorf("badgeOps has %q, but dispatch never reaches a badge for it — it buys a reconcile for nothing", op)
		}
		if !IsWriteOp(op) {
			t.Errorf("badgeOps has %q, which is not a write op: forward only triggers on writes, so it can never fire", op)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func TestIsWriteOpRejectsUnknownOps(t *testing.T) {
	if IsWriteOp("bogus_op") {
		t.Fatal("an unknown op must not be classified as a write")
	}
	if IsWriteOp("") {
		t.Fatal("the empty op must not be classified as a write")
	}
}
