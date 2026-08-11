// Package lambdamode is muster's AWS API Gateway adapter: it turns an HTTP
// request body into a proto.Request, hands it to daemon.Dispatch, and writes
// the proto.Response back. It is a TRANSPORT, not a second implementation of
// the bus — every op runs the same daemon code the unix socket serves, over
// the DynamoDB backend instead of SQLite.
//
// The event type is APIGatewayV2HTTPRequest — an HTTP API's $default route
// with a payload-format-2.0 proxy integration. A Lambda Function URL delivers
// that SAME wire format, so the two are interchangeable at the JSON layer and
// the choice between them is not a transport detail: it is where the auth
// upgrade path lives. A Function URL authenticates with AuthType NONE or
// SigV4 and nothing else, while an HTTP API takes a JWT authorizer as
// configuration — and a JWT authorizer's validated claims arrive in
// RequestContext.Authorizer.JWT, a field the Function URL request context
// does not have. Naming the v2 type is what makes the move from a shared
// bearer token to OIDC a change to Authenticator rather than to this
// signature.
//
// It is the sole path that links the AWS SDK, and cmd/muster reaches it only
// through a build-tag indirection (cmd/muster/lambda_on.go, built with
// `-tags lambda`). The binary every device runs — local mode and remote mode
// alike — must not carry AWS code, so nothing in the default build may import
// this package.
//
// Like mcp mode, this package writes NOTHING to stdout: the Lambda runtime
// treats stdout as log output, and all diagnostics here go to stderr.
package lambdamode

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/schuettc/muster/internal/daemon"
	"github.com/schuettc/muster/internal/dynamostore"
	"github.com/schuettc/muster/internal/proto"
)

// maxBodyBytes caps the request body. Lambda's own synchronous payload limit
// is 6MB, so anything larger cannot be a legitimate muster request and is
// rejected before it costs a JSON parse.
const maxBodyBytes = 6 << 20

// TokenEnv and PreviousTokenEnv name the shared bearer tokens EnvAuth accepts.
// Two exist so a rotation can roll across devices: the new token goes in
// TokenEnv, the outgoing one stays in PreviousTokenEnv until every device has
// been rolled forward, and then it is removed. With a single token, rotating
// breaks every device simultaneously — which in practice means it never
// happens.
const (
	TokenEnv         = "MUSTER_TOKEN"
	PreviousTokenEnv = "MUSTER_TOKEN_PREVIOUS"
)

// Authenticator decides whether a presented bearer token is valid. The v1
// implementation compares against MUSTER_TOKEN / MUSTER_TOKEN_PREVIOUS; the
// planned successor looks up per-device hashed tokens in DynamoDB so a single
// device can be revoked without rotating the fleet, and OIDC after that.
// Resolving credentials through this seam is what keeps that upgrade a
// one-file change instead of a rewrite of the request path.
type Authenticator interface {
	Valid(ctx context.Context, token string) bool
}

// EnvAuth is the v1 Authenticator: constant-time comparison against
// MUSTER_TOKEN and, during a rotation, MUSTER_TOKEN_PREVIOUS. Exported so
// tests (and later wiring) can construct it.
//
// Valid returns false when MUSTER_TOKEN is unset. The HTTP API's $default
// route carries no authorizer — the endpoint is publicly reachable and this
// token is the only thing in front of the bus — so a misconfigured deployment
// must fail closed. An empty-token fallback would silently serve the whole bus
// to anyone who found the URL.
type EnvAuth struct{}

// Valid implements Authenticator.
func (EnvAuth) Valid(_ context.Context, token string) bool {
	current := os.Getenv(TokenEnv)
	if current == "" || token == "" {
		return false
	}
	// Both comparisons always run: an early return on the first match would
	// leak, through timing, which of the two tokens a caller presented.
	ok := subtle.ConstantTimeCompare([]byte(token), []byte(current)) == 1
	if prev := os.Getenv(PreviousTokenEnv); prev != "" {
		ok = subtle.ConstantTimeCompare([]byte(token), []byte(prev)) == 1 || ok
	}
	return ok
}

// bearerToken extracts the token from the request's header map. Payload
// format 2.0 delivers header names LOWERCASED (they are not canonicalized the
// way net/http canonicalizes them), so the lookup is case-insensitive rather
// than assuming either casing.
func bearerToken(headers map[string]string) string {
	const prefix = "Bearer "
	for k, v := range headers {
		if !strings.EqualFold(k, "authorization") {
			continue
		}
		if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			return v[len(prefix):]
		}
		return ""
	}
	return ""
}

// Handler adapts one HTTP request to one daemon.Dispatch call. The
// checks run cheapest-first, and none of them reaches DynamoDB:
//
//  1. Authenticate — 401. Runs first so an anonymous caller costs nothing and
//     learns nothing about the payload rules.
//  2. Cap the body — 413, before parsing.
//  3. Decode (honouring IsBase64Encoded) and dispatch.
//
// Status codes carry meaning, and the split is transport-versus-protocol. A
// malformed body is 400 but still carries a proto.Response rather than a Go
// error, because a returned error is rendered by Lambda as a 502 with the
// message lost. A dispatch whose Response.OK is false is HTTP 200: the
// protocol has its own error channel and the client must see it rather than a
// transport failure it would retry. The one exception is the in-flight
// idempotency collision — the single same-key-retryable outcome — which is
// 409 so the transport's retry policy can tell it apart.
func Handler(d *daemon.Daemon, auth Authenticator) func(context.Context, events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
	return func(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		if !auth.Valid(ctx, bearerToken(req.Headers)) {
			return errorResponse(http.StatusUnauthorized, "unauthorized"), nil
		}
		if len(req.Body) > maxBodyBytes {
			return errorResponse(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)), nil
		}

		body := []byte(req.Body)
		if req.IsBase64Encoded {
			decoded, err := base64.StdEncoding.DecodeString(req.Body)
			if err != nil {
				return errorResponse(http.StatusBadRequest, "decode base64 body: "+err.Error()), nil
			}
			if len(decoded) > maxBodyBytes {
				return errorResponse(http.StatusRequestEntityTooLarge,
					fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)), nil
			}
			body = decoded
		}

		var pr proto.Request
		if err := json.Unmarshal(body, &pr); err != nil {
			return errorResponse(http.StatusBadRequest, "decode request: "+err.Error()), nil
		}

		resp := d.Dispatch(pr)
		status := http.StatusOK
		if !resp.OK && daemon.IsRetryableIdemError(resp.Error) {
			status = http.StatusConflict
		}
		return jsonResponse(status, resp), nil
	}
}

// errorResponse builds a transport-level rejection that still speaks the
// protocol: the client parses a proto.Response either way, and only the status
// code tells it what happened at the transport.
func errorResponse(status int, msg string) events.APIGatewayV2HTTPResponse {
	return jsonResponse(status, proto.Response{Error: msg})
}

// jsonResponse marshals resp as the HTTP body. The marshal cannot fail for a
// daemon-produced proto.Response, but a store backend could in principle put
// an unmarshalable value in Data, so the fallback keeps the client parsing a
// proto.Response rather than raw text.
func jsonResponse(status int, resp proto.Response) events.APIGatewayV2HTTPResponse {
	body, err := json.Marshal(resp)
	if err != nil {
		body = []byte(`{"ok":false,"error":"encode response failed"}`)
		status = http.StatusInternalServerError
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       string(body),
	}
}

// Run serves the Lambda runtime until the execution environment is torn down,
// returning a process exit code. It only ever returns non-zero: lambda.Start
// does not return on the happy path.
//
// The daemon is built with a nil wake.Notifier — wake is a tmux concern and
// there is no tmux in Lambda; devices learn about new mail by polling.
func Run() int {
	ctx := context.Background()
	table := os.Getenv(dynamostore.TableEnv)
	if table == "" {
		fmt.Fprintln(os.Stderr, "muster: lambda: "+dynamostore.TableEnv+" is required")
		return 1
	}
	if os.Getenv(TokenEnv) == "" {
		// Not fatal — the handler fails closed on every request — but the
		// operator should see the cause at cold start rather than only as a
		// wall of 401s from every device.
		fmt.Fprintln(os.Stderr, "muster: lambda: "+TokenEnv+" is unset; every request will be rejected with 401")
	}
	s, err := dynamostore.Open(ctx, table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster: lambda: open store:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "muster: lambda mode over DynamoDB table", table)
	// "" disables daemon-side short-alias expansion: this Lambda fronts a
	// shared store for every device on the bus, so it has no single device's
	// name to expand against — see daemon.New.
	lambda.Start(Handler(daemon.New(s, nil, ""), EnvAuth{}))
	// Unreachable on the happy path — lambda.Start blocks for the life of the
	// container. Reaching here means the runtime died, which is a failure
	// however quietly it happened, so do not report success.
	return 1
}
