package dynamostore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/schuettc/muster/internal/clock"
	"github.com/schuettc/muster/internal/store"
)

const (
	// rosterPartition is gsi1's partition holding every agent item, so
	// ListAgents is one Query rather than a Scan. Single-partition is fine at
	// muster's scale — a roster is tens of rows and writes are single-digit
	// per minute against DynamoDB's 1000 WCU / 3000 RCU per-partition ceiling.
	rosterPartition = "ROSTER"

	// metaSK is the sort key of a partition's metadata item. Entries in a
	// THREAD# partition use their (always positive) entry id, so 0 is
	// reserved for metadata and can never collide.
	metaSK = 0
)

// backgroundCtx is the context every store.API method runs under.
//
// store.API is context-free — it was extracted verbatim from the daemon's
// original SQLite-shaped interface, and widening all 24 methods to take a
// ctx is a change to a surface the SQLite backend and the whole daemon share.
// In lambda mode the function's own timeout bounds these calls, so nothing
// runs unbounded; the cost is that a cancelled invocation cannot abort an
// in-flight DynamoDB call early.
func backgroundCtx() context.Context { return context.Background() }

// nextID atomically allocates the next id for the named counter ("thread",
// "entry", "event"). ADD on a single item is DynamoDB's atomic increment:
// concurrent callers each get a distinct value with no read-modify-write
// race, which is what lets the Lambda scale without a single-writer
// constraint.
//
// Entry ids in particular must be GLOBALLY monotonic, not per-thread:
// Agent.LastReadEntryID is a global watermark, so per-thread sequences would
// make unread math treat entries from unrelated threads as already seen.
func (s *Store) nextID(ctx context.Context, name string) (int64, error) {
	out, err := s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.table),
		Key: map[string]types.AttributeValue{
			"pk": attrS(pkCounter(name)),
			"sk": attrN(metaSK),
		},
		UpdateExpression:          aws.String("ADD #n :one"),
		ExpressionAttributeNames:  map[string]string{"#n": "n"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": attrN(1)},
		ReturnValues:              types.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, fmt.Errorf("dynamostore: allocate %s id: %w", name, err)
	}
	id := numAttr(out.Attributes, "n")
	if id <= 0 {
		return 0, fmt.Errorf("dynamostore: allocate %s id: counter returned %d", name, id)
	}
	return id, nil
}

// RegisterAgent upserts by alias, mirroring the SQLite ON CONFLICT exactly:
// registered_at is stamped only on first sight, departed is always reset to
// false so a returning session revives its tombstone, and read state
// (last_read_entry_id / last_read_at) is never touched by either branch so it
// survives the round trip.
func (s *Store) RegisterAgent(a store.Agent) error {
	ctx := backgroundCtx()
	now := clock.NowMillis()

	set := map[string]types.AttributeValue{
		"alias":           attrS(a.Alias),
		"role":            attrS(a.Role),
		"model_type":      attrS(a.ModelType),
		"socket_path":     attrS(a.SocketPath),
		"pane_id":         attrS(a.PaneID),
		"session_name":    attrS(a.SessionName),
		"session_id":      attrS(a.SessionID),
		"session_created": attrN(a.SessionCreated),
		"device_id":       attrS(a.DeviceID),
		"project":         attrS(a.Project),
		"label":           attrS(a.Label),
		"label_manual":    attrBool(a.LabelManual),
		"last_seen":       attrN(now),
		"departed":        attrBool(false),
		"gsi1pk":          attrS(rosterPartition),
		"gsi1sk":          attrN(0),
	}
	expr, names, values := setExpr(set)

	// registered_at is insert-only: if_not_exists preserves the original
	// stamp across every re-registration, matching the SQLite upsert.
	expr += ", #registered_at = if_not_exists(#registered_at, :registered_at)"
	names["#registered_at"] = "registered_at"
	values[":registered_at"] = attrN(now)

	_, err := s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       agentKey(a.Alias),
		UpdateExpression:          aws.String(expr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		return fmt.Errorf("dynamostore: register agent %q: %w", a.Alias, err)
	}
	return nil
}

// ListAgents returns all agents ordered by alias — departed (tombstoned)
// agents included: their rows are history, not gone.
func (s *Store) ListAgents() ([]store.Agent, error) {
	items, err := s.roster(backgroundCtx())
	if err != nil {
		return nil, err
	}
	agents := make([]store.Agent, 0, len(items))
	for _, item := range items {
		agents = append(agents, itemToAgent(item))
	}
	// DynamoDB cannot order by a non-key attribute, and gsi1sk is 0 for every
	// agent, so the ORDER BY alias the SQLite backend gets for free happens
	// here instead.
	sort.Slice(agents, func(i, j int) bool { return agents[i].Alias < agents[j].Alias })
	return agents, nil
}

// GetAgent returns one agent by alias, strongly consistent — see the read
// consistency rule in the package comment. Every caller uses the result to
// decide something about a row the daemon itself may have just written: the
// register op reads the pre-mutation tuple to drive its CAS, and get_inbox
// recomputes the session badge from this row's watermark immediately after
// MarkRead wrote it. An eventually consistent read here hands the daemon a
// state it has already superseded.
func (s *Store) GetAgent(alias string) (store.Agent, bool, error) {
	return s.agentByAlias(backgroundCtx(), alias)
}

// agentByAlias is the ONE canonical single-agent read: a strongly consistent
// GetItem on the BASE table. Every read of an agent's role or read watermark
// funnels through it — GetAgent, unreadFor, and SessionUnread's per-member
// re-read alike — so no surface can quietly settle for the eventually
// consistent copy the roster index projects.
func (s *Store) agentByAlias(ctx context.Context, alias string) (store.Agent, bool, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            agentKey(alias),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return store.Agent{}, false, fmt.Errorf("dynamostore: get agent %q: %w", alias, err)
	}
	if len(out.Item) == 0 {
		return store.Agent{}, false, nil
	}
	return itemToAgent(out.Item), true, nil
}

// TouchAgent bumps last_seen. No error if the agent is unknown.
//
// The condition keeps this from resurrecting a deleted agent as a row with
// nothing but a timestamp: DynamoDB's UpdateItem is an upsert, so without it
// touching an unknown alias would create one, where the SQLite UPDATE simply
// matches no rows.
func (s *Store) TouchAgent(alias string) error {
	_, err := s.c.UpdateItem(backgroundCtx(), &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       agentKey(alias),
		UpdateExpression:          aws.String("SET #last_seen = :last_seen"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  map[string]string{"#last_seen": "last_seen"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":last_seen": attrN(clock.NowMillis())},
	})
	if err != nil {
		if isConditionFailed(err) {
			return nil // unknown alias: no-op, matching the SQLite contract
		}
		return fmt.Errorf("dynamostore: touch agent %q: %w", alias, err)
	}
	return nil
}

// DepartAgent tombstones alias: sets departed in place, preserving identity,
// project, label, and read state. Unknown alias is a no-op. RegisterAgent's
// upsert is the only way back to departed=false.
func (s *Store) DepartAgent(alias string) error {
	_, err := s.c.UpdateItem(backgroundCtx(), &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       agentKey(alias),
		UpdateExpression:          aws.String("SET #departed = :departed"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  map[string]string{"#departed": "departed"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":departed": attrBool(true)},
	})
	if err != nil {
		if isConditionFailed(err) {
			return nil
		}
		return fmt.Errorf("dynamostore: depart agent %q: %w", alias, err)
	}
	return nil
}

// DeleteAgent hard-deletes a registration — irreversible. Unknown alias is a
// no-op. Message and task history is unaffected: threads store the alias as
// text, not a reference.
func (s *Store) DeleteAgent(alias string) error {
	_, err := s.c.DeleteItem(backgroundCtx(), &dynamodb.DeleteItemInput{
		TableName: aws.String(s.table),
		Key:       agentKey(alias),
	})
	if err != nil {
		return fmt.Errorf("dynamostore: delete agent %q: %w", alias, err)
	}
	return nil
}

// SetSessionLabel updates the stored label for every non-departed alias
// registered to the (socketPath, sessionID) tuple — a label is a
// session-level property, so sibling aliases move together. Returns how many
// rows changed; 0 with an empty tuple component.
// deviceID scopes the session to one machine (see store.API): a label is an
// address, and the tuple alone is not device-unique in a shared roster.
func (s *Store) SetSessionLabel(deviceID, socketPath, sessionID, label string, manual bool) (int64, error) {
	if socketPath == "" || sessionID == "" {
		return 0, nil
	}
	ctx := backgroundCtx()
	items, err := s.roster(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, item := range items {
		a := itemToAgent(item)
		if a.Departed || !sameSession(a, deviceID, socketPath, sessionID) {
			continue
		}
		_, err := s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:        aws.String(s.table),
			Key:              agentKey(a.Alias),
			UpdateExpression: aws.String("SET #label = :label, #label_manual = :label_manual"),
			ExpressionAttributeNames: map[string]string{
				"#label":        "label",
				"#label_manual": "label_manual",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":label":        attrS(label),
				":label_manual": attrBool(manual),
			},
		})
		if err != nil {
			return n, fmt.Errorf("dynamostore: set label on %q: %w", a.Alias, err)
		}
		n++
	}
	return n, nil
}

// DepartStaleSiblings tombstones every OTHER non-departed alias registered to
// the same (socketPath, sessionID) tuple whose session_created differs from
// created — ghosts from a previous tmux server incarnation whose session ID
// was recycled.
//
// The ghost-guard rules are carried over from the SQLite implementation
// unchanged, and each matters:
//   - session_created 0 is SPARED. A pre-upgrade registration on the same
//     still-running session is indistinguishable from a ghost, and it
//     self-heals the next time that agent re-registers.
//   - already-departed rows are skipped; they are tombstones, not ghosts.
//   - keepAlias is never departed — it is the caller registering right now.
//
// No-op when any tuple component is empty or zero. Returns the tombstoned
// aliases so the caller can reconcile their badges.
// deviceID scopes the reaping to one machine (see store.API): differing
// session_created is evidence of a recycled session id only among rows from
// the SAME machine, so without it this would tombstone another device's live
// agent on a colliding tuple.
func (s *Store) DepartStaleSiblings(deviceID, socketPath, sessionID string, created int64, keepAlias string) ([]string, error) {
	if socketPath == "" || sessionID == "" || created == 0 {
		return nil, nil
	}
	ctx := backgroundCtx()
	items, err := s.roster(ctx)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, item := range items {
		a := itemToAgent(item)
		if a.Departed || !sameSession(a, deviceID, socketPath, sessionID) {
			continue
		}
		if a.SessionCreated == 0 || a.SessionCreated == created || a.Alias == keepAlias {
			continue
		}
		stale = append(stale, a.Alias)
	}
	sort.Strings(stale)
	for _, alias := range stale {
		if err := s.DepartAgent(alias); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// roster returns every agent item via gsi1's ROSTER partition, paginating.
//
// This is a GSI read, so it is EVENTUALLY CONSISTENT and cannot be made
// otherwise — DynamoDB offers no ConsistentRead on a secondary index. Treat
// what it returns as MEMBERSHIP, not as current per-agent state: a caller that
// needs an agent's watermark or role to be read-your-writes must re-read the
// base item through agentByAlias (SessionUnread does exactly that). The lag is
// in the safe direction for membership — an alias registered a moment ago may
// be missing, which under-counts rather than resurrecting a stale row.
func (s *Store) roster(ctx context.Context) ([]map[string]types.AttributeValue, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(s.table),
		IndexName:                 aws.String(gsi1Name),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": attrS(rosterPartition)},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamostore: query roster: %w", err)
	}
	return items, nil
}

// sameSession is the ONE place this backend answers "is this agent in that
// session": the full (device, socket_path, session_id) identity, never the
// pair alone (see store.API's note — the pair collides across machines in a
// shared roster). Every session-scoped surface here goes through it so none of
// them can quietly drop the device dimension again.
func sameSession(a store.Agent, deviceID, socketPath, sessionID string) bool {
	return a.DeviceID == deviceID && a.SocketPath == socketPath && a.SessionID == sessionID
}

func agentKey(alias string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": attrS(pkAgent(alias)),
		"sk": attrN(metaSK),
	}
}

func itemToAgent(item map[string]types.AttributeValue) store.Agent {
	return store.Agent{
		Alias:           strAttr(item, "alias"),
		Role:            strAttr(item, "role"),
		ModelType:       strAttr(item, "model_type"),
		SocketPath:      strAttr(item, "socket_path"),
		PaneID:          strAttr(item, "pane_id"),
		SessionName:     strAttr(item, "session_name"),
		SessionID:       strAttr(item, "session_id"),
		SessionCreated:  numAttr(item, "session_created"),
		DeviceID:        strAttr(item, "device_id"),
		Project:         strAttr(item, "project"),
		Label:           strAttr(item, "label"),
		LabelManual:     boolAttr(item, "label_manual"),
		RegisteredAt:    numAttr(item, "registered_at"),
		LastSeen:        numAttr(item, "last_seen"),
		LastReadEntryID: numAttr(item, "last_read_entry_id"),
		Departed:        boolAttr(item, "departed"),
	}
}

// setExpr builds a SET update expression from attrs, aliasing every attribute
// name. DynamoDB reserves a long list of words — role, status, name, count,
// value, size, ref, intent among them — so aliasing all of them is simpler
// and safer than tracking which ones need it. Names are sorted so the
// generated expression is deterministic.
func setExpr(attrs map[string]types.AttributeValue) (string, map[string]string, map[string]types.AttributeValue) {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	names := make(map[string]string, len(attrs))
	values := make(map[string]types.AttributeValue, len(attrs))
	b.WriteString("SET ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "#%s = :%s", k, k)
		names["#"+k] = k
		values[":"+k] = attrs[k]
	}
	return b.String(), names, values
}

// --- attribute helpers ------------------------------------------------------

func attrS(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func attrN(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func attrBool(v bool) types.AttributeValue { return &types.AttributeValueMemberBOOL{Value: v} }

// attrB writes opaque bytes. It must never be handed an empty slice: DynamoDB
// rejects an empty AttributeValue in an update expression, so callers with
// nothing to store REMOVE the attribute instead (see IdemComplete).
func attrB(v []byte) types.AttributeValue { return &types.AttributeValueMemberB{Value: v} }

// numAttr reads a Number attribute, treating absence as zero. An item written
// before an attribute existed simply reads as its zero value — this backend's
// equivalent of the SQLite additive-column migration, and why it needs no
// migration step of its own.
func numAttr(item map[string]types.AttributeValue, name string) int64 {
	m, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(m.Value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func strAttr(item map[string]types.AttributeValue, name string) string {
	m, ok := item[name].(*types.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return m.Value
}

// binAttr reads a Binary attribute, treating absence as nil.
func binAttr(item map[string]types.AttributeValue, name string) []byte {
	m, ok := item[name].(*types.AttributeValueMemberB)
	if !ok {
		return nil
	}
	return m.Value
}

func boolAttr(item map[string]types.AttributeValue, name string) bool {
	m, ok := item[name].(*types.AttributeValueMemberBOOL)
	if !ok {
		return false
	}
	return m.Value
}

// isConditionFailed reports whether err is a failed ConditionExpression —
// which callers use as a signal (already claimed, unknown alias), not an
// error to propagate.
func isConditionFailed(err error) bool {
	var cf *types.ConditionalCheckFailedException
	return errors.As(err, &cf)
}
