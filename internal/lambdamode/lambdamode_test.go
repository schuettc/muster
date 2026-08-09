package lambdamode_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/lambdamode"
	"github.com/schuettc/muster/internal/mustertest"
	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// newLambdaTestStore builds a bare *store.Store. The handler is a transport
// adapter over daemon.Dispatch, so the SQLite backend is exactly as good a
// Dispatch target here as DynamoDB would be — and needs no endpoint.
func newLambdaTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir, cleanup, err := mustertest.ShortHome()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s, err := store.Open(filepath.Join(dir, "bus.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newHandler builds the handler under test over a fresh store, wired to the
// v1 environment authenticator (the tests set MUSTER_TOKEN with t.Setenv).
func newHandler(t *testing.T) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	t.Helper()
	return lambdamode.Handler(daemon.New(newLambdaTestStore(t), nil), lambdamode.EnvAuth{})
}

// authed is the header set every handler test needs; the handler rejects
// anything else with 401 before it reaches Dispatch. Payload format 2.0
// delivers header names lowercased, which is what these tests send.
func authed() map[string]string {
	return map[string]string{"authorization": "Bearer good-token"}
}

func TestHandlerDispatchesRequestBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)

	body, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body: string(body), Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, resp.Body)
	}
	if got := resp.Headers["Content-Type"]; got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var got proto.Response
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !got.OK {
		t.Fatalf("dispatch failed: %s", got.Error)
	}
}

func TestHandlerRejectsMalformedBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body: "{not json", Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler must not return a transport error for a bad body: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// The 400 must still carry a proto.Response: a Go error would surface as
	// a 502 with the message lost, and the client parses proto.Response.
	var got proto.Response
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("400 body is not a proto.Response: %v (body %q)", err, resp.Body)
	}
	if got.OK || got.Error == "" {
		t.Errorf("400 response = %+v, want OK=false with an error message", got)
	}
}

func TestHandlerRejectsMissingOrWrongToken(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	body, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"missing", nil},
		{"wrong", map[string]string{"authorization": "Bearer nope"}},
		{"malformed", map[string]string{"authorization": "good-token"}},
		{"empty-bearer", map[string]string{"authorization": "Bearer "}},
		{"prefix-of-token", map[string]string{"authorization": "Bearer good"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
				Body: string(body), Headers: tc.hdr,
			})
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if resp.StatusCode != 401 {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestHandlerFailsClosedWithoutToken is the deployment-misconfiguration case:
// the $default route carries no authorizer, so an unset MUSTER_TOKEN with an
// empty-token fallback would expose the whole bus to anyone who found the URL.
func TestHandlerFailsClosedWithoutToken(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "")
	t.Setenv("MUSTER_TOKEN_PREVIOUS", "")
	h := newHandler(t)
	body, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}

	for _, hdr := range []map[string]string{
		nil,
		{"authorization": "Bearer "},
		{"authorization": "Bearer good-token"},
		{"authorization": "Bearer"},
	} {
		resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
			Body: string(body), Headers: hdr,
		})
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if resp.StatusCode != 401 {
			t.Fatalf("headers %v: status = %d, want 401 — an unset MUSTER_TOKEN must reject everything", hdr, resp.StatusCode)
		}
	}
}

func TestHandlerAcceptsPreviousToken(t *testing.T) {
	// Rotation overlap: a device still holding the old token must keep working
	// until it is rolled forward. Without this, rotating breaks every device
	// at once, which means it never happens.
	t.Setenv("MUSTER_TOKEN", "new-token")
	t.Setenv("MUSTER_TOKEN_PREVIOUS", "old-token")
	h := newHandler(t)
	body, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{"new-token", "old-token"} {
		resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
			Body:    string(body),
			Headers: map[string]string{"authorization": "Bearer " + tok},
		})
		if err != nil {
			t.Fatalf("handler with %s: %v", tok, err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s rejected: status %d", tok, resp.StatusCode)
		}
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body:    strings.Repeat("x", 8*1024*1024),
		Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 413 {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestHandlerChecksAuthBeforeBodySize pins the check order: an oversized body
// from an unauthenticated caller is a 401, not a 413. The cheap check runs
// first and an anonymous caller learns nothing about the payload rules.
func TestHandlerChecksAuthBeforeBodySize(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body:    strings.Repeat("x", 8*1024*1024),
		Headers: map[string]string{"authorization": "Bearer nope"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401 — auth must run before the size cap", resp.StatusCode)
	}
}

func TestHandlerDecodesBase64Body(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	body, err := json.Marshal(proto.Request{Op: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body:            base64.StdEncoding.EncodeToString(body),
		IsBase64Encoded: true,
		Headers:         authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 — base64 bodies must be decoded", resp.StatusCode)
	}
}

// TestHandlerRejectsUndecodableBase64 covers the other half of the
// IsBase64Encoded flag: a body that claims to be base64 and is not is a client
// error carrying a proto.Response, not a transport failure.
func TestHandlerRejectsUndecodableBase64(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body:            "!!!not base64!!!",
		IsBase64Encoded: true,
		Headers:         authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandlerKeepsProtocolErrorsAt200 is the transport/protocol split: an op
// the daemon rejects is a valid exchange. The client must see the
// proto.Response's error channel rather than a transport failure it would
// retry.
func TestHandlerKeepsProtocolErrorsAt200(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	h := newHandler(t)
	body, err := json.Marshal(proto.Request{Op: "no_such_op"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body: string(body), Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 — protocol errors ride the proto.Response", resp.StatusCode)
	}
	var got proto.Response
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.OK || got.Error == "" {
		t.Fatalf("response = %+v, want OK=false with an error", got)
	}
}

// TestHandlerSurfacesIdempotencyCollisionAs409 is the one exception to the
// rule above: an identical request already in flight under the same key is the
// single same-key-retryable outcome, and the transport's retry policy needs to
// tell it apart from every other protocol error. daemon.IsRetryableIdemError
// owns that distinction — this asserts the handler routes on it.
func TestHandlerSurfacesIdempotencyCollisionAs409(t *testing.T) {
	t.Setenv("MUSTER_TOKEN", "good-token")
	s := newLambdaTestStore(t)
	h := lambdamode.Handler(daemon.New(s, nil), lambdamode.EnvAuth{})

	// Claim the key without completing it: the record is now in flight, so
	// the next dispatch under it is the collision.
	if _, _, found, err := s.IdemBegin("collide-key"); err != nil || found {
		t.Fatalf("IdemBegin: found=%v err=%v", found, err)
	}
	body, err := json.Marshal(proto.Request{
		Op:      "register_agent",
		Args:    map[string]any{"alias": "a", "role": "r"},
		IdemKey: "collide-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h(context.Background(), events.APIGatewayV2HTTPRequest{
		Body: string(body), Headers: authed(),
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409 (body %q)", resp.StatusCode, resp.Body)
	}
	var got proto.Response
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !daemon.IsRetryableIdemError(got.Error) {
		t.Fatalf("409 body error = %q, want the retryable in-flight error", got.Error)
	}
}
