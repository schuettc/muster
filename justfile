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
