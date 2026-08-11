package daemon

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
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
	d := New(s, nil, "")
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
	d := New(s, nil, "")
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
	d := New(s, nil, "")
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
	d := New(s, nil, "")
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
	d := New(fs, nil, "")
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
	d := New(fs, nil, "")
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
	d := New(s, nil, "")

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
	d := New(s, nil, "")
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

// packageFiles parses EVERY non-test .go file of package daemon, which is what
// the three drift guards below walk.
//
// Parsing daemon.go alone is not enough, and fails in the silent direction.
// The call graph these guards walk is name-based: a callee whose declaration
// is not in the parsed set simply resolves to nothing, and "resolves to
// nothing" reads identically to "reaches no badge sink" and "reads no
// device-scoped arg" — so a handler that moves out of daemon.go (the package
// already keeps dispatch helpers in resolve.go) would quietly stop being
// classified as badge-moving or session-scoped, with no test failing. The
// len(derived) floors cannot catch that: they only fire when the walk breaks
// wholesale, never when it loses one op.
//
// Build tags are deliberately ignored (parser mode 0 does not evaluate them):
// every file that could contribute a handler should be walked, whatever
// configuration it builds under.
func packageFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no package files — the walk below would derive nothing and pass vacuously")
	}
	return files
}

// packageCallees maps every function declared in the package to the names it
// calls, so reachability can be walked across files rather than within one.
func packageCallees(files []*ast.File) map[string][]string {
	callees := map[string][]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				continue
			}
			callees[fn.Name.Name] = calleeNames(fn.Body)
		}
	}
	return callees
}

// dispatchOps extracts the op names from dispatch's `switch req.Op`, so the
// classification is checked against the code rather than against a second
// hand-written list that can drift with it.
func dispatchOps(t *testing.T) []string {
	t.Helper()
	var ops []string
	inspectPackage(packageFiles(t), func(n ast.Node) bool {
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
// switch — walking the call graph across the whole package, so a case that
// reaches the badge through a handler (register_agent → handleRegisterAgent →
// reconcileBadge) still counts wherever that handler is declared — and demands
// the map say the same thing.
//
// Without this, a new notifying op would keep working in local mode and
// silently never light a badge in remote mode: the wrong half of the failure,
// and invisible until an operator noticed mail they were never told about.
// Package-wide parsing is part of the guard, not a detail — see packageFiles.
func TestBadgeOpsMatchDispatch(t *testing.T) {
	files := packageFiles(t)
	callees := packageCallees(files)
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
	for _, cc := range dispatchCases(t, files) {
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
		for _, op := range caseOps(t, cc) {
			derived[op] = true
		}
	}

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

// deviceArgKeys are the args a session-scoped op reads: the tuple that is not
// device-unique in a shared roster, plus the device id itself. A dispatch case
// that reads any of them is deciding something about ONE machine's session, so
// its forwarded form has to carry this device's id (see deviceOps).
var deviceArgKeys = map[string]bool{
	"socket_path": true, "session_id": true, "device_id": true,
}

// stampedOutsideForward are the device-scoped ops that forward never sees, with
// the reason each one is exempt from deviceOps. Every entry here is a claim that
// something else stamps the device id.
var stampedOutsideForward = map[string]string{
	// The poller builds this request itself and puts d.deviceID on it
	// (Daemon.devicePoll); no client can ask for it, so forward never carries one.
	"device_poll": "stamped by the poller, never forwarded from a client",
}

// TestDeviceOpsMatchDispatch is the drift guard for deviceOps, the set forward
// stamps this device's id onto. It derives the session-scoped ops from
// dispatch's own switch — a case counts when its reachable call graph across
// the package reads one of deviceArgKeys out of the args map, so
// register_agent still counts through handleRegisterAgent wherever that is
// declared — and demands the map agree.
//
// The failure it exists to catch is silent and one-directional: a new
// session-scoped op left out of deviceOps works perfectly in local mode (where
// every row carries device_id "") and, once forwarded unstamped, addresses
// whichever machine's session matched first in the shared roster. That is a
// wrong answer about another laptop, not an error anyone sees. writeOps and
// badgeOps are pinned this way for the same reason; this is the third.
func TestDeviceOpsMatchDispatch(t *testing.T) {
	files := packageFiles(t)
	callees := packageCallees(files)
	reads := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Body == nil {
				continue
			}
			reads[fn.Name.Name] = readsDeviceArg(t, fn.Body)
		}
	}
	var reaches func(name string, seen map[string]bool) bool
	reaches = func(name string, seen map[string]bool) bool {
		if reads[name] {
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
	for _, cc := range dispatchCases(t, files) {
		hit := readsDeviceArg(t, &ast.BlockStmt{List: cc.Body})
		if !hit {
			for _, callee := range calleeNames(&ast.BlockStmt{List: cc.Body}) {
				if reaches(callee, map[string]bool{}) {
					hit = true
					break
				}
			}
		}
		if !hit {
			continue
		}
		for _, op := range caseOps(t, cc) {
			derived[op] = true
		}
	}

	if len(derived) < 4 {
		t.Fatalf("derived only %v as session-scoped — the AST walk is broken, not the classification", derived)
	}
	for op := range derived {
		if needsDevice(op) {
			continue
		}
		if why, exempt := stampedOutsideForward[op]; exempt {
			t.Logf("%s: session-scoped but not in deviceOps — %s", op, why)
			continue
		}
		t.Errorf("%s reads a device-scoped arg in dispatch but is missing from deviceOps: "+
			"forwarded unstamped it would address another machine's session", op)
	}
	for op := range deviceOps {
		if !derived[op] {
			t.Errorf("deviceOps has %q, but dispatch never reads a device-scoped arg for it — "+
				"stamping it buys nothing", op)
		}
	}
	for op, why := range stampedOutsideForward {
		if !derived[op] {
			t.Errorf("stampedOutsideForward has %q (%s), which dispatch does not treat as session-scoped", op, why)
		}
		if needsDevice(op) {
			t.Errorf("%s is in BOTH deviceOps and stampedOutsideForward — one of the two claims is wrong", op)
		}
	}
}

// argsAccessors are the functions that pull a value OUT of a request's args
// map. readsDeviceArg counts a deviceArgKeys literal only when it is passed to
// one of these.
//
// The narrowing matters once the walk covers the whole package rather than
// daemon.go: remote mode BUILDS outgoing requests whose args maps name the
// same keys (sessionUnread's `{"device_id": d.deviceID, …}`, stampDevice's
// `args["device_id"] = …`), and a bare literal scan cannot tell "this case
// decides something about one machine's session" from "the daemon stamped its
// own id onto a call it is making". Left bare, get_inbox and eight other ops
// were derived as session-scoped through setSessionBadge → sessionUnread.
//
// TestArgsAccessorsMatchThePackage is what keeps this narrowing from becoming
// the silent direction: a new accessor spelled some other way would make
// readsDeviceArg miss reads, so the package is checked for accessors this list
// does not name.
var argsAccessors = map[string]bool{"str": true, "boolArg": true, "i64": true}

// readsDeviceArg reports whether n reads one of deviceArgKeys out of an args
// map — how every args read in dispatch is spelled (str(a, "…")). Still coarse
// in the safe direction within that: it does not check WHICH map is being
// read, so a false hit demands MORE ops be classified as session-scoped, never
// fewer.
func readsDeviceArg(t *testing.T, n ast.Node) bool {
	t.Helper()
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if !argsAccessors[name] {
			return true
		}
		for _, arg := range call.Args {
			lit, isLit := arg.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}
			if deviceArgKeys[v] {
				found = true
			}
		}
		return true
	})
	return found
}

// TestArgsAccessorsMatchThePackage keeps argsAccessors from silently going
// stale. Every function in the package taking (map[string]any, string) is an
// args accessor by construction, so one missing from the list would make
// readsDeviceArg blind to every read spelled through it — and a session-scoped
// op left out of deviceOps fails silently, addressing another machine's
// session (see TestDeviceOpsMatchDispatch).
func TestArgsAccessorsMatchThePackage(t *testing.T) {
	found := map[string]bool{}
	for _, f := range packageFiles(t) {
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Recv != nil || fn.Type.Params == nil {
				continue
			}
			var params []ast.Expr
			for _, field := range fn.Type.Params.List {
				for range max(len(field.Names), 1) {
					params = append(params, field.Type)
				}
			}
			if len(params) != 2 || !isArgsMap(params[0]) || !isIdentNamed(params[1], "string") {
				continue
			}
			found[fn.Name.Name] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("found no args accessors at all — this guard is not looking at the package it thinks it is")
	}
	for name := range found {
		if !argsAccessors[name] {
			t.Errorf("%s reads a request's args but is not in argsAccessors: "+
				"every device-scoped read spelled through it is invisible to readsDeviceArg", name)
		}
	}
	for name := range argsAccessors {
		if !found[name] {
			t.Errorf("argsAccessors names %q, which the package does not declare as an args accessor", name)
		}
	}
}

// isArgsMap reports whether e is the type map[string]any (or its
// map[string]interface{} spelling).
func isArgsMap(e ast.Expr) bool {
	m, isMap := e.(*ast.MapType)
	if !isMap || !isIdentNamed(m.Key, "string") {
		return false
	}
	if isIdentNamed(m.Value, "any") {
		return true
	}
	iface, isIface := m.Value.(*ast.InterfaceType)
	return isIface && (iface.Methods == nil || len(iface.Methods.List) == 0)
}

func isIdentNamed(e ast.Expr, name string) bool {
	id, isIdent := e.(*ast.Ident)
	return isIdent && id.Name == name
}

// inspectPackage runs ast.Inspect over every parsed file of the package.
func inspectPackage(files []*ast.File, fn func(ast.Node) bool) {
	for _, f := range files {
		ast.Inspect(f, fn)
	}
}

// dispatchCases returns every case clause of the switch on a request's Op,
// wherever in the package that switch is declared.
func dispatchCases(t *testing.T, files []*ast.File) []*ast.CaseClause {
	t.Helper()
	var cases []*ast.CaseClause
	inspectPackage(files, func(n ast.Node) bool {
		sw, isSwitch := n.(*ast.SwitchStmt)
		if !isSwitch {
			return true
		}
		if sel, isSel := sw.Tag.(*ast.SelectorExpr); !isSel || sel.Sel.Name != "Op" {
			return true
		}
		for _, stmt := range sw.Body.List {
			if cc, isCase := stmt.(*ast.CaseClause); isCase {
				cases = append(cases, cc)
			}
		}
		return true
	})
	return cases
}

// caseOps returns the op names a case clause matches.
func caseOps(t *testing.T, cc *ast.CaseClause) []string {
	t.Helper()
	var ops []string
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
	return ops
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
