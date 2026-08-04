# muster developer tasks — the SAME targets CI runs, so local and CI can't drift.
set shell := ["bash", "-uc"]

# Version stamp: cmd/muster, justfile, and .github/workflows/release.yml all
# target the SAME internal/version vars via -ldflags -X, so a local `just
# build`, `just verify`, and a release build report the same thing.
version := `cat VERSION`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
ldflags := "-X github.com/schuettc/muster/internal/version.version=" + version + " -X github.com/schuettc/muster/internal/version.commit=" + commit

# Format code.
fmt:
    gofmt -w .

# Verify formatting is clean (used by verify/CI).
fmt-check:
    test -z "$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

# Static analysis.
lint:
    golangci-lint run ./...

# Tests (race detector on).
test:
    go test -race ./...

# Build the binary.
build:
    CGO_ENABLED=0 go build -ldflags "{{ ldflags }}" -o bin/muster ./cmd/muster

# Cross-compile all release targets (no output, fail fast).
cross:
    set -e; \
    for goos in darwin linux; do \
      for goarch in arm64 amd64; do \
        CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -ldflags "{{ ldflags }}" -o /dev/null ./cmd/muster; \
      done; \
    done

# Full gate — what pre-push and CI run.
verify: fmt-check lint test build cross

# DynamoDB backend tests against DynamoDB Local. Requires Docker, so it is
# deliberately NOT part of `verify`: that gate must stay fast and
# dependency-free. Without an endpoint the dynamo tests skip, so `verify`
# still compiles and vets them — it just can't exercise the DynamoDB
# semantics (conditional writes, atomic counters) this recipe covers.
verify-dynamo:
    #!/usr/bin/env bash
    set -euo pipefail
    docker rm -f muster-ddb >/dev/null 2>&1 || true
    docker run -d --rm -p 8000:8000 --name muster-ddb amazon/dynamodb-local >/dev/null
    trap 'docker rm -f muster-ddb >/dev/null 2>&1 || true' EXIT
    for _ in $(seq 1 30); do
      curl -s http://localhost:8000 >/dev/null 2>&1 && break
      sleep 0.5
    done
    MUSTER_DDB_ENDPOINT=http://localhost:8000 go test -race ./internal/dynamostore/...
