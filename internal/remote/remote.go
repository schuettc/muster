// Package remote is the upstream transport a device uses to reach the hosted
// bus: it POSTs one proto.Request as JSON over HTTPS and reads back one
// proto.Response.
//
// It links NO AWS SDK, deliberately. A device in remote mode has no AWS
// credentials, no region and no profile to configure — it holds a bearer token
// in a file and speaks plain HTTP. That is the whole reason the hosted design
// authenticates with a shared token instead of SigV4: keeping the SDK out of
// the device binary keeps `muster` a single small static download, and keeps
// "point this laptop at the hosted bus" a one-line setup rather than an IAM
// exercise. Nothing here may import github.com/aws/aws-sdk-go-v2/...
//
// The idempotency contract lives here, on the client side, because only the
// client knows what "one logical write" is:
//
//   - A key identifies ONE REQUEST. Call mints exactly one key per invocation
//     and holds it constant across every retry of that invocation. A fresh key
//     per attempt would duplicate the write, which is the exact failure the
//     key exists to prevent; a key reused across two DIFFERENT requests would
//     serve the first one's response for the second, silently.
//   - Only writes get a key, and daemon.IsWriteOp is the single source of
//     truth for which ops those are. This package does not keep a second list.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/proto"
)

// TokenFile is the token's name inside paths.Home(). Exported so setup docs
// and `muster` subcommands can name the file without repeating the literal.
const TokenFile = "remote-token"

// maxResponseBytes caps how much of an upstream response is read. The Lambda's
// own request cap is 6MB and no legitimate proto.Response is larger, so a body
// beyond this is a misrouted request or a captive-portal HTML page, and either
// way is not worth buffering.
const maxResponseBytes = 6 << 20

// Defaults for the knobs below. Every one of them is an Option because an
// operator on a slow link needs different numbers than one on a LAN.
const (
	defaultTimeout     = 20 * time.Second
	defaultBackoff     = 200 * time.Millisecond
	defaultBackoffCap  = 5 * time.Second
	defaultMaxAttempts = 5
	// defaultMaxConflicts is strictly below defaultMaxAttempts so a 409 loop
	// always ends as ErrPending — the actionable diagnosis — rather than as a
	// generic attempts-exhausted error.
	defaultMaxConflicts = 3
)

// ErrPending reports that a write's idempotency key was still in flight
// upstream after the same-key retry bound, so the write's outcome is UNKNOWN:
// it may have committed, or it may never have run.
//
// The upstream answers 409 for exactly one condition — an identical request is
// already in flight — and a same-key retry is the only safe reissue for it,
// because it either finds the recorded response or finds it still running.
// That is normally transient. It is permanent in two cases: a record whose
// IdemComplete failed (the op DID commit) and a record whose IdemBegin
// acknowledgement was lost (the op did NOT commit). Both answer 409 forever,
// until the record's TTL expires a day later.
//
// Call does not reissue under a fresh key on its own. It cannot distinguish
// those two cases, and they demand opposite responses: for get_inbox a blind
// reissue recomputes the inbox AFTER the first execution already advanced the
// read watermark, so unread mail silently disappears; for task_claim it
// returns a wrong "not claimable". Surfacing ErrPending puts the choice where
// the op's semantics are known, and the reissue itself is trivial there —
// calling Call again IS a fresh key.
var ErrPending = errors.New("upstream still reports this write in flight after the same-key retry bound; its outcome is unknown")

// HTTPError is an upstream reply the transport cannot turn into a
// proto.Response: a permanent status such as 401, or a retryable one that
// outlived the retry budget. Callers can errors.As it to read StatusCode —
// 401 in particular means the token is wrong, which no retry will fix.
type HTTPError struct {
	StatusCode int
	// Body is the upstream's reply, truncated. It is usually a proto.Response
	// with an Error set, but a proxy or the Function URL itself can answer
	// with something else entirely, so it is kept as text.
	Body string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("upstream returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("upstream returned HTTP %d: %s", e.StatusCode, e.Body)
}

// Client is a device's connection to the hosted bus. It is safe for concurrent
// use: everything it holds is read-only after New, and each Call keeps its own
// idempotency key.
type Client struct {
	url   string
	token string
	http  *http.Client

	// timeout is what WithTimeout asked for, held separately from http.Timeout
	// so that WithHTTPClient and WithTimeout cannot cancel each other out
	// depending on the order they were passed in. New resolves the two into
	// http.Timeout once, after every option has run.
	timeout time.Duration

	backoff      time.Duration
	backoffCap   time.Duration
	maxAttempts  int
	maxConflicts int

	// jitter returns a fraction in [0,1) and is the only source of randomness
	// in the package. It is a field rather than a direct rand call so a test
	// can pin it and assert the backoff exactly; see export_test.go.
	jitter func() float64
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the per-attempt HTTP timeout. It bounds one attempt, not
// the whole Call — a Call that retries can take up to roughly maxAttempts
// times this plus backoff, and the caller's context bounds that.
//
// It wins over any Timeout already set on a client passed to WithHTTPClient,
// and the order the two options are passed in does not matter.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithBackoff sets the base backoff. Attempt n waits base<<n, capped.
func WithBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.backoff = d
		}
	}
}

// WithBackoffCap sets the ceiling on a single backoff wait.
func WithBackoffCap(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.backoffCap = d
		}
	}
}

// WithMaxAttempts sets the total number of attempts one Call may make,
// counting the first. It must be at least 1.
func WithMaxAttempts(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxAttempts = n
		}
	}
}

// WithMaxConflicts bounds how many 409 (in-flight) replies one Call tolerates
// before giving up with ErrPending. This is the wedge guard: see ErrPending.
func WithMaxConflicts(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxConflicts = n
		}
	}
}

// WithHTTPClient replaces the underlying *http.Client — for a custom
// transport, proxy, or TLS config.
//
// It cannot leave a Call unbounded. New shallow-copies the client given here
// and resolves its Timeout after every option has run: WithTimeout wins if it
// was passed at all, otherwise this client's own Timeout is kept, and a zero
// Timeout falls back to the package default. Order does not matter, and the
// caller's own *http.Client is never mutated.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New builds a Client for the upstream at rawURL authenticating with token.
// Both are required and validated here rather than at first use, so a
// misconfigured device fails at startup with a clear message instead of on
// whatever op happens to run first.
func New(rawURL, token string, opts ...Option) (*Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	token = strings.TrimSpace(token)
	if rawURL == "" {
		return nil, errors.New("remote: upstream URL is empty")
	}
	if token == "" {
		return nil, errors.New("remote: bearer token is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("remote: parse upstream URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("remote: upstream URL %q must be http or https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("remote: upstream URL %q has no host", rawURL)
	}

	c := &Client{
		url:          rawURL,
		token:        token,
		http:         &http.Client{},
		backoff:      defaultBackoff,
		backoffCap:   defaultBackoffCap,
		maxAttempts:  defaultMaxAttempts,
		maxConflicts: defaultMaxConflicts,
		jitter:       rand.Float64,
	}
	for _, opt := range opts {
		opt(c)
	}

	// Resolve the per-attempt timeout LAST, so no combination of options can
	// leave an attempt unbounded. A Call with no client-side timeout is not a
	// slow Call, it is a hung one: the retry ladder never advances and the
	// caller's context is the only thing left that can end it.
	//
	// The copy is what lets this be safe. WithHTTPClient's argument belongs to
	// the caller — it may be shared with other code — so its Timeout is read,
	// not written.
	hc := *c.http
	switch {
	case c.timeout > 0:
		hc.Timeout = c.timeout
	case hc.Timeout <= 0:
		hc.Timeout = defaultTimeout
	}
	c.http = &hc
	return c, nil
}

// Call sends one request upstream and returns the daemon's response.
//
// A returned error is a TRANSPORT failure — the request may or may not have
// been executed. A returned Response with OK false is the protocol's own error
// channel and means the upstream ran the op and refused it; that is not
// retried, because retrying it would just be told no again.
//
// Retries cover connection errors, 5xx, 429 and 409, with exponential backoff.
// Every other 4xx is permanent and is returned on the first attempt: a 401 is
// a wrong token, and retrying it only burns Lambda invocations against
// credentials that will never work.
func (c *Client) Call(ctx context.Context, req proto.Request) (proto.Response, error) {
	// One key per logical call, minted before the loop and never touched
	// inside it. Reads get none: a read must not consume an idempotency
	// record. daemon.IsWriteOp is the only classifier — notably get_inbox is a
	// write, because it advances the read watermark.
	if daemon.IsWriteOp(req.Op) {
		req.IdemKey = uuid.NewString()
	}
	// Marshalled once so that every retry is byte-identical to the attempt it
	// is retrying, key included.
	body, err := json.Marshal(req)
	if err != nil {
		return proto.Response{}, fmt.Errorf("remote: encode %s request: %w", req.Op, err)
	}

	var lastErr error
	conflicts := 0
	sawConflict := false
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, c.wait(attempt-1)); err != nil {
				return proto.Response{}, fmt.Errorf("remote: %s: %w", req.Op, err)
			}
		}

		resp, status, err := c.attempt(ctx, body)
		switch {
		case err != nil:
			// A connection-level failure. The request may still have been
			// delivered and executed, which is exactly why the key is held.
			if ctx.Err() != nil {
				return proto.Response{}, fmt.Errorf("remote: %s: %w", req.Op, err)
			}
			lastErr = err
		case status == http.StatusConflict:
			conflicts++
			sawConflict = true
			if conflicts >= c.maxConflicts {
				return proto.Response{}, fmt.Errorf("remote: %s (idem key %s): %w", req.Op, req.IdemKey, ErrPending)
			}
			lastErr = &HTTPError{StatusCode: status, Body: resp.Error}
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = &HTTPError{StatusCode: status, Body: resp.Error}
		case status >= 400:
			// Permanent. Do not spend another attempt on it.
			return proto.Response{}, fmt.Errorf("remote: %s: %w", req.Op, &HTTPError{StatusCode: status, Body: resp.Error})
		case status/100 != 2:
			// Anything left is a non-2xx no case above claimed: a 304, or a 3xx
			// the http.Client declined to follow because it carried no Location.
			// Falling through to the success branch would hand the caller a
			// zero proto.Response — not OK, no Error, no status — which is the
			// least diagnosable outcome the transport can produce. A captive
			// portal or an interfering proxy is exactly where this shows up, so
			// it must arrive as an *HTTPError carrying the status.
			return proto.Response{}, fmt.Errorf("remote: %s: %w", req.Op, &HTTPError{StatusCode: status, Body: resp.Error})
		default:
			return resp, nil
		}
	}
	// If ANY attempt saw a 409, the write's outcome is unknown and the
	// diagnosis is the same wedge as hitting the conflict bound — a caller
	// testing errors.Is(err, ErrPending) must not have to know which of the two
	// bounds happened to trip first, nor whether the last attempt in a mixed
	// 409/5xx sequence happened to be the 5xx. Once a 409 is observed, no later
	// status can restore the certainty it took away.
	if sawConflict {
		return proto.Response{}, fmt.Errorf("remote: %s (idem key %s): %w", req.Op, req.IdemKey, ErrPending)
	}
	return proto.Response{}, fmt.Errorf("remote: %s: gave up after %d attempts: %w", req.Op, c.maxAttempts, lastErr)
}

// attempt performs one POST. It returns the decoded response and the HTTP
// status; a non-nil error means the exchange itself failed (connection,
// timeout, or an undecodable body on a 2xx) rather than the upstream replying.
func (c *Client) attempt(ctx context.Context, body []byte) (proto.Response, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return proto.Response{}, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return proto.Response{}, 0, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return proto.Response{}, httpResp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	var resp proto.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		if httpResp.StatusCode/100 == 2 {
			// A 2xx that is not a proto.Response is not something a retry
			// fixes on its own, but it is also not a protocol answer — treat
			// it as a transport failure so the retry loop's bound applies.
			return proto.Response{}, httpResp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
		// A non-2xx body is often not JSON at all (a proxy's HTML error page,
		// an API Gateway message). Keep it as the HTTPError's text.
		resp.Error = truncate(string(raw), 512)
	}
	return resp, httpResp.StatusCode, nil
}

// wait is the backoff before retry n (0-based), capped, and jittered into
// [d/2, d).
//
// The jitter is not decoration. Every device in a fleet talks to the SAME
// Lambda, so a 5xx or a throttle is something they all see at the same instant;
// a purely deterministic base<<n then has them retry in lockstep, rebuilding
// the herd that caused the failure at every rung of the ladder. Spreading each
// wait over the lower half of its slot desynchronises the wave while keeping
// the ceiling and the shape of the ladder intact.
func (c *Client) wait(n int) time.Duration {
	d := c.backoff
	for i := 0; i < n && d < c.backoffCap; i++ {
		d *= 2
	}
	if d > c.backoffCap {
		d = c.backoffCap
	}
	half := d / 2
	return half + time.Duration(c.jitter()*float64(half))
}

// sleep waits for d, or returns early if the context ends first. The caller's
// deadline outranks the retry budget.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Cutting at a byte offset can split a rune, and this string ends up in an
	// error message; drop the partial one rather than emit invalid UTF-8.
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// TokenPath is where ReadToken looks: <paths.Home()>/remote-token.
func TokenPath() string { return filepath.Join(paths.Home(), TokenFile) }

// ReadToken reads the device's bearer token from TokenPath(), trims
// surrounding whitespace, and REFUSES a file whose mode is anything but 0600.
//
// The token is a file rather than an environment variable on purpose. muster
// runs alongside coding agents that read their own environment as a matter of
// course, so a MUSTER_REMOTE_TOKEN would be one `env` call away from landing in
// an agent transcript. A file is only actually safer than that if it is
// actually private, so a loose mode is an error and not a warning: a warning
// on a daemon's stderr is a warning nobody reads, and the token would stay
// world-readable for the life of the install.
func ReadToken() (string, error) {
	p := TokenPath()
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("remote: no token at %s: write the bus token there with mode 0600", p)
		}
		return "", fmt.Errorf("remote: stat token %s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("remote: token %s is not a regular file", p)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return "", fmt.Errorf("remote: token %s has mode %04o, want 0600 (it is readable or writable by others): chmod 600 %s", p, perm, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("remote: read token %s: %w", p, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("remote: token %s is empty", p)
	}
	return token, nil
}
