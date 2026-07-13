# muster developer tasks — the SAME targets CI runs, so local and CI can't drift.
set shell := ["bash", "-uc"]

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
    CGO_ENABLED=0 go build -o bin/muster ./cmd/muster

# Full gate — what pre-push and CI run.
verify: fmt-check lint test build
