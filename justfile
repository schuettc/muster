# muster developer tasks — the SAME targets CI runs, so local and CI can't drift.
set shell := ["bash", "-uc"]

# Version stamp: cmd/muster, justfile, and .github/workflows/release.yml all
# target the SAME internal/version vars via -ldflags -X, so a local `just
# build`, `just verify`, and a release build report the same thing.
version := `cat VERSION`
commit := `git rev-parse --short HEAD 2>/dev/null || echo none`
date := `date -u +%Y-%m-%d`
ldflags := "-X github.com/schuettc/muster/internal/version.version=" + version + " -X github.com/schuettc/muster/internal/version.commit=" + commit + " -X github.com/schuettc/muster/internal/version.date=" + date

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

# Cross-compile all release targets (no output, fail fast). The last line
# builds the Lambda artifact's configuration (-tags lambda, the only build that
# links the AWS SDK): it is not part of any other recipe, so without it here
# the tagged code would rot silently until a release tried to ship it.
cross:
    set -e; \
    for goos in darwin linux; do \
      for goarch in arm64 amd64; do \
        CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -ldflags "{{ ldflags }}" -o /dev/null ./cmd/muster; \
        CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -ldflags "{{ ldflags }}" -o /dev/null ./cmd/muster-deploy; \
      done; \
    done
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda -ldflags "{{ ldflags }}" -o /dev/null ./cmd/muster
    just aws-free

# Assert the device binary links no AWS code. This is the enforcement half of
# CLAUDE.md's hard rule: cmd/muster-deploy and the -tags lambda build may
# import the AWS SDK, and cmd/muster may NEVER reach either. A stray import in
# internal/daemon or internal/remote would otherwise ship the SDK to every
# device, and nothing else in the build would notice.
aws-free:
    #!/usr/bin/env bash
    set -euo pipefail
    n=$(go list -deps ./cmd/muster | grep -c aws || true)
    if [ "$n" -ne 0 ]; then
      echo "FAIL: cmd/muster links $n AWS package(s) — the device binary must be AWS-free:" >&2
      go list -deps ./cmd/muster | grep aws >&2
      exit 1
    fi
    echo "ok: cmd/muster links no AWS packages"

# Full gate — what pre-push and CI run.
verify: fmt-check lint test build cross

# DynamoDB backend tests against DynamoDB Local, plus the DynamoDB half of the
# cross-backend conformance suite. Requires Docker, so it is deliberately NOT
# part of `verify`: that gate must stay fast and dependency-free. Without an
# endpoint the dynamo tests skip, so `verify` still compiles and vets them (and
# runs the SQLite half of the conformance suite) — it just can't exercise the
# DynamoDB semantics (conditional writes, atomic counters, transactions) this
# recipe covers. The `dynamo` job in .github/workflows/ci.yml runs the same two
# packages against a service container.
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
    MUSTER_DDB_ENDPOINT=http://localhost:8000 go test -race ./internal/dynamostore/... ./internal/storetest/...
