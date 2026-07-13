package mcpserver

import (
	"context"
	"testing"
)

func TestKVSetGetTools(t *testing.T) {
	startTestDaemon(t)

	if _, out, err := kvSetHandler(context.Background(), nil, KVSetIn{Key: "api.base", Value: "http://localhost:4000", By: "backend"}); err != nil || !out.OK {
		t.Fatalf("set: err=%v out=%+v", err, out)
	}
	_, got, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "api.base"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Found || got.Value != "http://localhost:4000" {
		t.Fatalf("unexpected get: %+v", got)
	}
	// Missing key → Found false, no error.
	_, missing, err := kvGetHandler(context.Background(), nil, KVGetIn{Key: "nope"})
	if err != nil || missing.Found {
		t.Fatalf("missing key: err=%v out=%+v", err, missing)
	}
}
