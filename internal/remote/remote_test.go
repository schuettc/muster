package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/remote"
)

// recorder captures what the upstream saw. Every field is guarded because the
// httptest handler runs on its own goroutine and the retry tests read these
// while more attempts may still be arriving.
type recorder struct {
	mu   sync.Mutex
	reqs []proto.Request
	auth []string
}

func (r *recorder) record(req proto.Request, auth string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	r.auth = append(r.auth, auth)
	return len(r.reqs)
}

func (r *recorder) snapshot() ([]proto.Request, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.Request(nil), r.reqs...), append([]string(nil), r.auth...)
}

func (r *recorder) keys() []string {
	reqs, _ := r.snapshot()
	keys := make([]string, len(reqs))
	for i, q := range reqs {
		keys[i] = q.IdemKey
	}
	return keys
}

// newServer stands up an upstream whose per-attempt behaviour the test dictates
// via reply: it is handed the 1-based attempt number and returns the status to
// write. A zero status means "200 with an OK proto.Response".
func newServer(t *testing.T, rec *recorder, reply func(attempt int) int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := rec.record(req, r.Header.Get("Authorization"))
		status := 0
		if reply != nil {
			status = reply(n)
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(proto.Response{Error: "upstream says no"})
			return
		}
		_ = json.NewEncoder(w).Encode(proto.Response{OK: true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustNew(t *testing.T, url string, opts ...remote.Option) *remote.Client {
	t.Helper()
	c, err := remote.New(url, "test-token", opts...)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	return c
}

// fast is the option set every retry test uses so the suite does not sleep
// through real backoff.
func fast() []remote.Option {
	return []remote.Option{remote.WithBackoff(time.Millisecond), remote.WithTimeout(2 * time.Second)}
}

func TestCallAttachesIdemKeyToWrites(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, nil)

	c := mustNew(t, srv.URL)
	if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	reqs, _ := rec.snapshot()
	if len(reqs) != 1 || reqs[0].IdemKey == "" {
		t.Fatalf("write op sent without an IdemKey: %+v", reqs)
	}
}

func TestCallOmitsIdemKeyOnReads(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, nil)

	c := mustNew(t, srv.URL)
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	reqs, _ := rec.snapshot()
	if reqs[0].IdemKey != "" {
		t.Fatalf("read op carried an IdemKey: %q", reqs[0].IdemKey)
	}
}

// TestCallKeysExactlyTheWriteOpsDaemonDeclares pins the classification to
// daemon.IsWriteOp rather than to a list this package maintains. get_inbox is
// the one that catches a hand-rolled copy: it looks like a read and is a write.
func TestCallKeysExactlyTheWriteOpsDaemonDeclares(t *testing.T) {
	for _, op := range []string{"get_inbox", "send_message", "task_claim", "list_agents", "get_thread"} {
		rec := &recorder{}
		srv := newServer(t, rec, nil)
		c := mustNew(t, srv.URL)
		if _, err := c.Call(context.Background(), proto.Request{Op: op}); err != nil {
			t.Fatalf("Call(%s): %v", op, err)
		}
		reqs, _ := rec.snapshot()
		got := reqs[0].IdemKey != ""
		if want := daemon.IsWriteOp(op); got != want {
			t.Fatalf("op %q: keyed=%v, but daemon.IsWriteOp=%v", op, got, want)
		}
	}
}

func TestCallReusesIdemKeyAcrossRetries(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return 0
	})

	c := mustNew(t, srv.URL, fast()...)
	if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	keys := rec.keys()
	if len(keys) != 3 {
		t.Fatalf("got %d attempts, want 3", len(keys))
	}
	if keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("retries used different keys: %v — this duplicates writes", keys)
	}
}

// TestCallMintsAFreshKeyPerLogicalCall is the other half of the contract: a key
// identifies ONE request, so two distinct calls must never share one. A key
// cached on the Client would return the first call's response for the second.
func TestCallMintsAFreshKeyPerLogicalCall(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, nil)

	c := mustNew(t, srv.URL)
	for range 2 {
		if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err != nil {
			t.Fatalf("Call: %v", err)
		}
	}
	keys := rec.keys()
	if len(keys) != 2 {
		t.Fatalf("got %d attempts, want 2", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("two distinct calls shared key %q — the second would be served the first's response", keys[0])
	}
}

func TestCallRetriesOn409InFlight(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(n int) int {
		if n == 1 {
			return http.StatusConflict
		}
		return 0
	})

	c := mustNew(t, srv.URL, fast()...)
	resp, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if err != nil || !resp.OK {
		t.Fatalf("409 must be retried: err=%v resp=%+v", err, resp)
	}
	if keys := rec.keys(); keys[0] != keys[1] {
		t.Fatalf("409 retry changed keys (%v) — same-key retry is the ONLY safe reissue here", keys)
	}
}

func TestCallRetriesOn429(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(n int) int {
		if n == 1 {
			return http.StatusTooManyRequests
		}
		return 0
	})

	c := mustNew(t, srv.URL, fast()...)
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err != nil {
		t.Fatalf("429 must be retried: %v", err)
	}
}

// TestCallBoundsSameKeyConflictRetries is the wedge guard. A record left
// pending by a failed IdemComplete answers 409 forever, so an unbounded
// same-key loop would spin until the record's 24h TTL.
func TestCallBoundsSameKeyConflictRetries(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusConflict })

	c := mustNew(t, srv.URL, append(fast(), remote.WithMaxConflicts(3))...)
	_, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if err == nil {
		t.Fatal("an upstream stuck on 409 must eventually fail, not spin")
	}
	if !errors.Is(err, remote.ErrPending) {
		t.Fatalf("err = %v, want remote.ErrPending so the caller can tell a wedge from a hard failure", err)
	}
	keys := rec.keys()
	if len(keys) != 3 {
		t.Fatalf("made %d attempts, want the bound of 3", len(keys))
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("bounded retries must still hold ONE key: %v", keys)
		}
	}
}

// TestCallDoesNotReissueUnderAFreshKeyItself records the resolution of the
// bounded-retry-then-reissue tradeoff: Call surfaces remote.ErrPending and lets the
// caller decide, because an automatic fresh-key reissue would re-execute a
// write that may already have committed. Calling Call again IS the reissue —
// every invocation mints its own key.
func TestCallDoesNotReissueUnderAFreshKeyItself(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusConflict })

	c := mustNew(t, srv.URL, append(fast(), remote.WithMaxConflicts(2))...)
	if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err == nil {
		t.Fatal("expected remote.ErrPending")
	}
	keys := rec.keys()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("Call reissued under a new key on its own: %v", keys)
	}

	// The caller's reissue: a second Call, a genuinely new key.
	_, _ = c.Call(context.Background(), proto.Request{Op: "send_message"})
	keys = rec.keys()
	if keys[0] == keys[len(keys)-1] {
		t.Fatalf("a caller-driven reissue must use a fresh key: %v", keys)
	}
}

// TestCallReportsPendingWhicheverBoundTrips: with the attempt budget lower
// than the conflict budget it is maxAttempts that ends a 409 loop, but the
// diagnosis is the same wedge, so callers must not have to know which bound
// happened to trip.
func TestCallReportsPendingWhicheverBoundTrips(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusConflict })

	c := mustNew(t, srv.URL, append(fast(), remote.WithMaxAttempts(2), remote.WithMaxConflicts(9))...)
	_, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if !errors.Is(err, remote.ErrPending) {
		t.Fatalf("err = %v, want ErrPending even when the attempt bound tripped first", err)
	}
	if n := len(rec.keys()); n != 2 {
		t.Fatalf("made %d attempts, want the attempt bound of 2", n)
	}
}

func TestCallSendsBearerToken(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, nil)

	c := mustNew(t, srv.URL)
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	_, auth := rec.snapshot()
	if auth[0] != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want %q", auth[0], "Bearer test-token")
	}
}

func TestCallDoesNotRetry401(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusUnauthorized })

	c := mustNew(t, srv.URL, fast()...)
	_, err := c.Call(context.Background(), proto.Request{Op: "list_agents"})
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	var he *remote.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want an *HTTPError carrying 401", err)
	}
	if n := len(rec.keys()); n != 1 {
		t.Fatalf("401 was sent %d times — a bad token is permanent, not transient", n)
	}
}

// TestCallDoesNotRetryOther4xx generalises the 401 rule: only 409 and 429 are
// transient among the 4xx.
func TestCallDoesNotRetryOther4xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge} {
		rec := &recorder{}
		srv := newServer(t, rec, func(int) int { return status })
		c := mustNew(t, srv.URL, fast()...)
		if _, err := c.Call(context.Background(), proto.Request{Op: "send_message"}); err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if n := len(rec.keys()); n != 1 {
			t.Fatalf("status %d was sent %d times, want 1", status, n)
		}
	}
}

func TestCallBoundsRetriesOn5xx(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusInternalServerError })

	c := mustNew(t, srv.URL, append(fast(), remote.WithMaxAttempts(4))...)
	_, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if err == nil {
		t.Fatal("expected an error after the attempt bound")
	}
	if n := len(rec.keys()); n != 4 {
		t.Fatalf("made %d attempts, want the bound of 4", n)
	}
}

// TestCallReturnsProtocolErrorsWithoutRetry: a 200 whose proto.Response carries
// an error is the protocol's own error channel, not a transport failure.
func TestCallReturnsProtocolErrorsWithoutRetry(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proto.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		rec.record(req, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(proto.Response{Error: "no such agent"})
	}))
	t.Cleanup(srv.Close)

	c := mustNew(t, srv.URL, fast()...)
	resp, err := c.Call(context.Background(), proto.Request{Op: "send_message"})
	if err != nil {
		t.Fatalf("a protocol-level error must come back in the Response, not as a transport error: %v", err)
	}
	if resp.Error != "no such agent" {
		t.Fatalf("resp = %+v", resp)
	}
	if n := len(rec.keys()); n != 1 {
		t.Fatalf("a protocol error was retried %d times", n)
	}
}

func TestCallSurfacesUnreachableUpstreamPromptly(t *testing.T) {
	c := mustNew(t, "http://127.0.0.1:1", remote.WithBackoff(time.Millisecond), remote.WithTimeout(200*time.Millisecond))
	start := time.Now()
	if _, err := c.Call(context.Background(), proto.Request{Op: "list_agents"}); err == nil {
		t.Fatal("expected an error from an unreachable upstream")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s — an unreachable upstream must fail fast, not hang", time.Since(start))
	}
}

// TestCallStopsOnCanceledContext: the caller's deadline outranks the retry
// budget, including mid-backoff.
func TestCallStopsOnCanceledContext(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, func(int) int { return http.StatusServiceUnavailable })

	ctx, cancel := context.WithCancel(context.Background())
	c := mustNew(t, srv.URL, remote.WithBackoff(2*time.Second), remote.WithTimeout(time.Second))
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := c.Call(ctx, proto.Request{Op: "list_agents"}); err == nil {
		t.Fatal("expected an error once the context is canceled")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s — cancellation must cut the backoff short", d)
	}
}

// TestCallDoesNotMutateCallerArgs: Call stamps the key on its own copy.
func TestCallDoesNotMutateCallerArgs(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec, nil)

	c := mustNew(t, srv.URL)
	req := proto.Request{Op: "send_message", Args: map[string]any{"to": "a"}}
	if _, err := c.Call(context.Background(), req); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if req.IdemKey != "" {
		t.Fatalf("Call mutated the caller's request: %+v", req)
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	for name, tc := range map[string]struct{ url, token string }{
		"empty url":     {"", "t"},
		"empty token":   {"http://example.invalid", ""},
		"no scheme":     {"example.invalid/bus", "t"},
		"wrong scheme":  {"ftp://example.invalid", "t"},
		"unparseable":   {"http://[::1", "t"},
		"scheme only":   {"https://", "t"},
		"blank token":   {"https://example.invalid", "   "},
		"blank url str": {"   ", "t"},
	} {
		if _, err := remote.New(tc.url, tc.token); err == nil {
			t.Fatalf("%s: remote.New(%q, %q) succeeded, want an error", name, tc.url, tc.token)
		}
	}
}

func TestReadTokenRejectsLoosePermissions(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	p := filepath.Join(dir, "remote-token")
	if err := os.WriteFile(p, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := remote.ReadToken(); err == nil {
		t.Fatal("world-readable token file must be rejected")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got, err := remote.ReadToken()
	if err != nil {
		t.Fatalf("remote.ReadToken: %v", err)
	}
	if got != "secret" {
		t.Fatalf("token = %q, want secret (trailing newline must be trimmed)", got)
	}
}

// TestReadTokenRejectsEveryLooseMode: 0600 is the only mode accepted. A
// group-readable file on a shared box is as bad as a world-readable one, and
// 0700 means the file is executable, which a secret never is.
func TestReadTokenRejectsEveryLooseMode(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	p := filepath.Join(dir, "remote-token")
	if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o700, 0o666, 0o400} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatalf("chmod %04o: %v", mode, err)
		}
		if _, err := remote.ReadToken(); err == nil {
			t.Fatalf("mode %04o was accepted; only 0600 may be", mode)
		}
	}
}

func TestReadTokenReportsMissingAndEmptyFiles(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	if _, err := remote.ReadToken(); err == nil {
		t.Fatal("a missing token file must be an error")
	}
	p := filepath.Join(dir, "remote-token")
	if err := os.WriteFile(p, []byte("\n  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := remote.ReadToken(); err == nil {
		t.Fatal("a whitespace-only token file must be an error, not an empty token")
	}
}

func TestTokenPathIsUnderMusterHome(t *testing.T) {
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatalf("ShortHome: %v", err)
	}
	t.Cleanup(cleanup)
	t.Setenv("MUSTER_HOME", dir)

	if got, want := remote.TokenPath(), filepath.Join(dir, "remote-token"); got != want {
		t.Fatalf("remote.TokenPath() = %q, want %q", got, want)
	}
}
