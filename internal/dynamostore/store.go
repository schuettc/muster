// Package dynamostore is muster's DynamoDB implementation of store.API — the
// persistence behind the optional hosted bus. It is reachable only from
// lambda mode (build tag `lambda`); the binary devices run never links it.
//
// Single-table design. Thread metadata and entries share the THREAD#<id>
// partition with a numeric sort key, so GetThread is one query and entries
// come back already in id order. Two global secondary indexes cover the
// access patterns the SQLite backend gets from joins:
//
//	gsi1 — partitioned by recipient (RCPT#agent#<alias>, RCPT#role#<role>,
//	       RCPT#broadcast) and sorted by entry id, so "what is unread for me"
//	       is a sort-key-bounded query with no join. Entries carry their
//	       thread's recipient denormalized for exactly this reason. The agent
//	       roster lives in the disjoint ROSTER partition of the same index.
//	gsi2 — the global entry log (ENTRIES partition) in id order, which
//	       device_poll reads to find mail for one device.
//
// Ids come from counter items updated with an atomic ADD (see nextID). They
// must be globally monotonic, not per-thread: Agent.LastReadEntryID is a
// global watermark, and per-thread sequences would silently corrupt unread
// math.
package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// EndpointEnv points the client at a local DynamoDB instead of AWS. Tests
	// set it; when it is unset they skip, which is what keeps `just verify`
	// free of a container dependency.
	EndpointEnv = "MUSTER_DDB_ENDPOINT"

	// TableEnv names the table lambda mode opens.
	TableEnv = "MUSTER_DDB_TABLE"

	// TestTablePrefix guards DropTable. Any table outside this prefix is
	// refused, so a misconfigured MUSTER_DDB_TABLE can never delete real data.
	TestTablePrefix = "muster-test-"

	gsi1Name = "gsi1"
	gsi2Name = "gsi2"
)

// Store is the DynamoDB-backed implementation of store.API.
type Store struct {
	c     *dynamodb.Client
	table string
}

// Open connects to DynamoDB and ensures the table exists. Region and
// credentials come from the standard AWS chain, except when EndpointEnv is
// set: DynamoDB Local authenticates nothing, but the SDK still requires a
// region and a credential provider to sign with, so static dummies are
// supplied.
func Open(ctx context.Context, table string) (*Store, error) {
	if table == "" {
		return nil, errors.New("dynamostore: table name required")
	}
	endpoint := os.Getenv(EndpointEnv)

	var loadOpts []func(*awsconfig.LoadOptions) error
	if endpoint != "" {
		loadOpts = append(loadOpts,
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider("local", "local", "")),
		)
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("dynamostore: load aws config: %w", err)
	}
	c := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})

	s := &Store{c: c, table: table}
	if err := s.EnsureTable(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// EnsureTable creates the table and its indexes if absent, then waits for it
// to become ACTIVE. It is idempotent: an existing table is left alone.
func (s *Store) EnsureTable(ctx context.Context) error {
	exists, err := s.TableExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	_, err = s.c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(s.table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeN},
			{AttributeName: aws.String("gsi1pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gsi1sk"), AttributeType: types.ScalarAttributeTypeN},
			{AttributeName: aws.String("gsi2pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("gsi2sk"), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			secondaryIndex(gsi1Name, "gsi1pk", "gsi1sk"),
			secondaryIndex(gsi2Name, "gsi2pk", "gsi2sk"),
		},
	})
	if err != nil {
		// A concurrent Open won the race; that is success, not failure.
		var inUse *types.ResourceInUseException
		if errors.As(err, &inUse) {
			return nil
		}
		return fmt.Errorf("dynamostore: create table %q: %w", s.table, err)
	}

	if err := dynamodb.NewTableExistsWaiter(s.c).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: aws.String(s.table)},
		2*time.Minute); err != nil {
		return fmt.Errorf("dynamostore: wait for table %q: %w", s.table, err)
	}

	// TTL is how events expire on this backend — it supersedes PruneEvents,
	// which the SQLite backend still needs.
	if _, err := s.c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(s.table),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			AttributeName: aws.String("ttl"),
			Enabled:       aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("dynamostore: enable ttl on %q: %w", s.table, err)
	}
	return nil
}

// TableExists reports whether the table is present.
func (s *Store) TableExists(ctx context.Context) (bool, error) {
	_, err := s.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(s.table),
	})
	if err != nil {
		var notFound *types.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("dynamostore: describe table %q: %w", s.table, err)
	}
	return true, nil
}

// DropTable deletes the table. Test-only: it refuses any name outside
// TestTablePrefix, so this can never be pointed at real data.
func (s *Store) DropTable(ctx context.Context) error {
	if !strings.HasPrefix(s.table, TestTablePrefix) {
		return fmt.Errorf(
			"dynamostore: refusing to drop %q — DropTable is test-only and requires the %q prefix",
			s.table, TestTablePrefix)
	}
	_, err := s.c.DeleteTable(ctx, &dynamodb.DeleteTableInput{
		TableName: aws.String(s.table),
	})
	if err != nil {
		var notFound *types.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("dynamostore: delete table %q: %w", s.table, err)
	}
	return nil
}

func secondaryIndex(name, hashAttr, rangeAttr string) types.GlobalSecondaryIndex {
	return types.GlobalSecondaryIndex{
		IndexName: aws.String(name),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String(hashAttr), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(rangeAttr), KeyType: types.KeyTypeRange},
		},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	}
}

// --- key builders -----------------------------------------------------------
//
// Every partition key is prefixed by entity type so one table can hold them
// all without collision.

func pkThread(id int64) string     { return "THREAD#" + strconv.FormatInt(id, 10) }
func pkAgent(alias string) string  { return "AGENT#" + alias }
func pkKV(key string) string       { return "KV#" + key }
func pkEvent(id int64) string      { return "EVENT#" + strconv.FormatInt(id, 10) }
func pkCounter(name string) string { return "COUNTER#" + name }
func pkIdem(key string) string     { return "IDEM#" + key }

// rcpt is the gsi1 partition for a thread's recipient. Entries carry their
// thread's value so unread math is a bounded query rather than a join.
// A broadcast has no target, so every broadcast shares one partition.
func rcpt(toKind, toTarget string) string {
	if toKind == "broadcast" {
		return "RCPT#broadcast"
	}
	return "RCPT#" + toKind + "#" + toTarget
}

// Partition constants (ENTRIES, ROSTER, EVENTS), the metadata sort key, and
// the attribute read/write helpers land with the code that first uses them —
// the linter's `unused` check is what keeps this package from accumulating
// scaffolding ahead of its consumers.
