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
//	       thread's recipient denormalized for exactly this reason, and a
//	       thread's metadata item sits at sort key 0 of the same partition, so
//	       "which threads are addressed to me" reads no entries at all. The
//	       agent roster lives in the disjoint ROSTER partition of the same
//	       index.
//	gsi2 — the global entry log (ENTRIES partition) in id order, which
//	       device_poll reads to find mail for one device, plus the disjoint
//	       THREADS partition holding every thread's metadata item in id order,
//	       which is what makes Threads() one query instead of a scan.
//
// Ids come from counter items updated with an atomic ADD (see nextID). They
// must be globally monotonic, not per-thread: Agent.LastReadEntryID is a
// global watermark, and per-thread sequences would silently corrupt unread
// math.
//
// # Read consistency
//
// DynamoDB's default read is eventually consistent; a strongly consistent one
// costs twice the read units and is available on the BASE table only, never on
// a secondary index. The rule this package follows:
//
//	A read that feeds a decision the caller itself just caused is strongly
//	consistent. Everything else is not.
//
// In practice that is the small set of base-table single-item reads on the
// daemon's synchronous op paths — GetAgent/agentByAlias (an agent's role and
// read watermark), threadMeta (AppendEntry's not-found gate), and GetThread.
// The daemon runs Inbox -> MarkRead -> setSessionBadge -> SessionUnread inside
// one get_inbox op, so a stale watermark there re-lights the tmux badge the
// operator just drained and leaves it lit until the next notify or read.
//
// Bulk reads stay eventually consistent by design: roster, Threads, concerns,
// entriesAfter and maxEntryID are all index queries, which could not be
// strongly consistent even if they wanted to be. Where one of them feeds a
// consistency-sensitive decision, it is used for MEMBERSHIP only and the
// per-item state is re-read from the base table — SessionUnread is the worked
// example. maxEntryID is the one place that rule is knowingly broken, and the
// break is not benign: the watermark MarkRead derives from it can overshoot an
// entry that has not landed yet, and that entry is then never unread for
// whoever marked read in the window. See the next section.
//
// # The MarkRead watermark can overshoot
//
// MarkRead stores the highest entry id maxEntryID can see, and unread is a
// strict `id > watermark` bound (entriesAfter, unreadByThread), so an entry
// that commits carrying an id at or below an already-written watermark is
// never unread for that alias — not delayed, never. Nothing moves a watermark
// back down over it, and the daemon's notifyForThread recompute runs the same
// predicate, so the badge is CLEARED rather than lit. The thread itself is not
// lost: Inbox lists every thread that concerns an alias whatever its unread
// count, and the entry is in GetThread. What is lost is the signal — the
// unread count and the tmux badge or wake hanging off it.
//
// Two independent windows let an entry commit under a live watermark.
//
// Allocation happens before the commit. nextID hands out an entry id in its
// own round trip and the owning TransactWriteItems commits later, so two
// writers can commit out of id order and the one holding the LOWER id can be
// the one that lands second. AppendEntry allocates once, outside its
// conflict-retry loop, so a retrying append holds its id across up to ~155ms
// of backoff (appendBackoff over appendMaxRetries) plus the re-read on the
// condition-failed branch. This window is not a DynamoDB consistency artifact
// at all — it is there against DynamoDB Local too, which is exactly why the
// endpoint tests are green.
//
// The two indexes propagate independently. One base write fans out to gsi1 and
// to gsi2 as separate replications, so gsi2 can hold an entry gsi1 does not yet
// — and Inbox reads gsi1 while MarkRead reads gsi2. A get_inbox landing inside
// that skew misses the entry and then marks it read, with no second writer
// involved at all.
//
// The SQLite backend can do neither. store.Open sets SetMaxOpenConns(1), so
// its MarkRead transaction (SELECT MAX(id) FROM entries, then the UPDATE)
// holds the only connection there is, and entry ids come from AUTOINCREMENT
// inside the inserting transaction rather than ahead of it. This is a real
// divergence between the two backends, not a limitation they share.
//
// None of the obvious repairs work. Reading the counter item is strongly
// consistent but strictly worse — it marks ids never written at all as read. A
// strongly consistent index read is not purchasable at any price. Deriving the
// watermark from the entries the alias was actually shown would narrow the
// first window to writers racing into the SAME recipient partition and close
// the second outright, at the cost of MarkRead repeating Inbox's fan-out;
// store.API.MarkRead(alias) has room to do that on its own (it can run
// concerns/entriesAfter itself, no signature change), and it is the shape to
// reach for if this ever shows up in practice. It is accepted for now: the
// exposure is one entry's notification, it needs a get_inbox landing inside a
// window measured in milliseconds, and it scales with concurrent writers —
// which is precisely what this backend exists to allow, so revisit it as the
// hosted bus grows.
//
// # The recipient denormalization is write-once
//
// entryItem freezes its thread's rcpt(to_kind, to_target) onto the entry at
// write time, and that is what makes unread a sort-key-bounded query instead
// of a join. It is safe only because a thread's ADDRESS never changes: the
// sole updates this package makes to a metadata item touch status, updated_at,
// origin_project, and the denormalized last-entry trio. If a thread were ever
// re-addressed — a TransitionTask or a reassignment that rewrote to_kind or
// to_target — every entry written before the change would strand in the OLD
// gsi1 partition, invisible to the new recipient's queries, and unread would
// silently under-count with no error anywhere. Re-addressing a thread
// therefore requires rewriting gsi1pk on every one of its entries in the same
// breath. Do not add an update that writes to_kind or to_target without it.
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
