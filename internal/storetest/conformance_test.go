package storetest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/dynamostore"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/storetest"
)

// TestSQLiteConformance runs the suite against the specification backend. It
// needs nothing but a temp directory, so it runs in `just verify` on every
// machine — a case that fails here is a bug in the suite, not in a backend.
func TestSQLiteConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) store.API {
		db := filepath.Join(t.TempDir(), "bus.db")
		s, err := store.Open(db)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestDynamoConformance runs the SAME suite against DynamoDB. It skips without
// an endpoint so `just verify` stays container-free; `just verify-dynamo`
// stands DynamoDB Local up locally, and the `dynamo` job in ci.yml does the
// same in CI. A skip is not a pass.
func TestDynamoConformance(t *testing.T) {
	if os.Getenv(dynamostore.EndpointEnv) == "" {
		t.Skipf("%s unset; run `just verify-dynamo`", dynamostore.EndpointEnv)
	}
	storetest.RunConformance(t, func(t *testing.T) store.API {
		s, err := dynamostore.Open(context.Background(), testTableName(t))
		if err != nil {
			t.Fatalf("dynamostore.Open: %v", err)
		}
		t.Cleanup(func() {
			if err := s.DropTable(context.Background()); err != nil {
				t.Errorf("DropTable: %v", err)
			}
		})
		return s
	})
}

// testTableName derives a unique table per subtest so no two share state. It
// mirrors internal/dynamostore/store_test.go's helper of the same name:
// DynamoDB accepts only [a-zA-Z0-9_.-], and Go builds subtest names straight
// from the prose passed to t.Run, so filtering to the allowed set (rather than
// listing the offenders) keeps a new case from failing on a ValidationException
// that says nothing about the character it tripped over.
func testTableName(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			return r
		default:
			return '-'
		}
	}, t.Name())
	return dynamostore.TestTablePrefix + name
}
