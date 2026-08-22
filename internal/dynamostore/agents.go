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
// survives the round trip. superseded_by is left alone here — DynamoDB's
// UpdateItem only ever SETs the attributes named in this map, so simply
// omitting it from the set leaves an existing pointer untouched, exactly
// mirroring the SQLite ON CONFLICT no longer resetting it: a re-register on
// the seed's old tuple must not forget the successor. See Become.
func (s *Store) RegisterAgent(a store.Agent) error {
	ctx := backgroundCtx()
	now := clock.NowMillis()

	set := map[string]types.AttributeValue{
		"alias":              attrS(a.Alias),
		"role":               attrS(a.Role),
		"model_type":         attrS(a.ModelType),
		"socket_path":        attrS(a.SocketPath),
		"pane_id":            attrS(a.PaneID),
		"session_name":       attrS(a.SessionName),
		"session_id":         attrS(a.SessionID),
		"session_created":    attrN(a.SessionCreated),
		"device_id":          attrS(a.DeviceID),
		"device_name":        attrS(a.DeviceName),
		"harness_session_id": attrS(a.HarnessSessionID),
		"transcript_path":    attrS(a.TranscriptPath),
		"project":            attrS(a.Project),
		"label":              attrS(a.Label),
		"label_manual":       attrBool(a.LabelManual),
		"last_seen":          attrN(now),
		"departed":           attrBool(false),
		"gsi1pk":             attrS(rosterPartition),
		"gsi1sk":             attrN(0),
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

// FindConversation answers "which live row IS this conversation" (spec
// 2026-08-21 §2), the same question GetAgent answers by alias and this
// answers by identity instead. Like DepartStaleSiblings and
// SetSessionLabel, there is no index for "one agent by tuple", so this is a
// roster scan: transcript match first (deviceID + transcriptPath, non-empty,
// live only), highest LastSeen wins a tie; then the full live pane tuple
// (sameSession + paneID, live only), again highest LastSeen. Harness session
// ID is deliberately not a lookup key — it can change under /login, which is
// the defect this op exists to close. A zero sessionCreated or empty paneID
// never pane-matches, mirroring sameSession's own absence-of-proof rule.
func (s *Store) FindConversation(deviceID, transcriptPath, socketPath, sessionID string, sessionCreated int64, paneID string) (store.Agent, bool, error) {
	items, err := s.roster(backgroundCtx())
	if err != nil {
		return store.Agent{}, false, err
	}
	agents := make([]store.Agent, 0, len(items))
	for _, item := range items {
		agents = append(agents, itemToAgent(item))
	}

	if transcriptPath != "" {
		best, ok := store.Agent{}, false
		for _, a := range agents {
			if a.Departed || a.DeviceID != deviceID || a.TranscriptPath != transcriptPath {
				continue
			}
			if !ok || a.LastSeen > best.LastSeen {
				best, ok = a, true
			}
		}
		if ok {
			return best, true, nil
		}
	}

	if socketPath == "" || sessionID == "" || sessionCreated == 0 || paneID == "" {
		return store.Agent{}, false, nil
	}
	best, ok := store.Agent{}, false
	for _, a := range agents {
		if a.Departed || a.PaneID != paneID || !sameSession(a, deviceID, socketPath, sessionID, sessionCreated) {
			continue
		}
		if !ok || a.LastSeen > best.LastSeen {
			best, ok = a, true
		}
	}
	return best, ok, nil
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

// StampHarness attaches the harness link to an EXISTING row: the session
// UUID and the transcript path, each only when non-empty — a hook that
// knows one but not the other must not erase what another hook stamped.
// Identity, tuple, and read state are untouched; both empty is a no-op that
// skips the round trip entirely; unknown alias is a no-op, mirroring
// TouchAgent's contract.
//
// attribute_exists(pk) is load-bearing for the same reason it is on TouchAgent:
// UpdateItem is an upsert, so without it, stamping an unknown alias would
// CREATE a phantom row carrying nothing but a harness link — where the SQLite
// UPDATE simply matches no rows. That phantom would then be a roster member
// with an empty tuple, which is exactly the shape SessionUnread's empty-tuple
// guard exists to keep out.
func (s *Store) StampHarness(alias, harnessSessionID, transcriptPath string) error {
	set := map[string]types.AttributeValue{}
	if harnessSessionID != "" {
		set["harness_session_id"] = attrS(harnessSessionID)
	}
	if transcriptPath != "" {
		set["transcript_path"] = attrS(transcriptPath)
	}
	if len(set) == 0 {
		return nil
	}
	expr, names, values := setExpr(set)
	_, err := s.c.UpdateItem(backgroundCtx(), &dynamodb.UpdateItemInput{
		TableName:                 aws.String(s.table),
		Key:                       agentKey(alias),
		UpdateExpression:          aws.String(expr),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	if err != nil {
		if isConditionFailed(err) {
			return nil // unknown alias: no-op, matching the SQLite contract
		}
		return fmt.Errorf("dynamostore: stamp harness on %q: %w", alias, err)
	}
	return nil
}

// Become claims a new name for an existing identity: it writes `to` as a CLONE
// of `from` — tuple, device, harness link, project, label, role, model, and the
// READ WATERMARK, without which the claimed identity would see all of history
// as unread — then retires `from` as a tombstone stamped superseded_by = to.
//
// # This is a compare-and-swap, not a read-then-write
//
// `to` must not exist AT ALL: a live row is someone else's identity and a
// tombstone is another conversation's history, and merging identities is the
// exact confusion the feature exists to kill. Checking with a GetItem and then
// writing would leave a window in which two sessions both observe `to` absent
// and both claim it — the second silently obliterating the first. The guard is
// therefore attribute_not_exists(pk) ON THE WRITE ITSELF, which DynamoDB
// evaluates atomically with it.
//
// # Both writes are one transaction
//
// The SQLite implementation is one BEGIN/COMMIT precisely so that a crash
// mid-become never leaves both rows live. Sequential writes here could not
// hold that line: a successful clone followed by a failed retire would leave
// the seed live alongside its own successor. TransactWriteItems is the
// equivalent — the clone (guarded attribute_not_exists) and the retire
// (guarded attribute_exists) commit together or not at all, which also means
// the from-must-exist check is evaluated at COMMIT time rather than trusted
// from the read that built the clone.
//
// The item is cloned by copying `from`'s raw attributes and overriding only
// what the spec says changes, so any attribute this backend gains later is
// carried forward without touching this code. superseded_by is deliberately
// dropped rather than copied: the successor starts unsuperseded even when the
// seed was itself superseded (a chained become A→B→C must not inherit B's
// pointer backward onto C).
func (s *Store) Become(from, to string) error {
	ctx := backgroundCtx()

	// Strongly consistent: the clone's contents must reflect the seed as the
	// caller just left it (a register or MarkRead moments earlier is exactly
	// the read-your-writes case the package rule names). A miss here is the
	// fast path for ErrBecomeFromMissing; the transaction below re-checks it
	// at commit time, so a seed deleted in between still cannot slip through.
	raw, err := s.rawAgentItem(ctx, from)
	if err != nil {
		return err
	}
	if raw == nil {
		return store.ErrBecomeFromMissing
	}

	now := clock.NowMillis()
	clone := make(map[string]types.AttributeValue, len(raw)+4)
	for k, v := range raw {
		clone[k] = v
	}
	clone["pk"] = attrS(pkAgent(to))
	clone["sk"] = attrN(metaSK)
	clone["alias"] = attrS(to)
	clone["departed"] = attrBool(false)
	clone["registered_at"] = attrN(now)
	clone["last_seen"] = attrN(now)
	delete(clone, "superseded_by")

	_, err = s.c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName:           aws.String(s.table),
				Item:                clone,
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
			}},
			{Update: &types.Update{
				TableName:           aws.String(s.table),
				Key:                 agentKey(from),
				UpdateExpression:    aws.String("SET #departed = :departed, #superseded_by = :superseded_by"),
				ConditionExpression: aws.String("attribute_exists(pk)"),
				ExpressionAttributeNames: map[string]string{
					"#departed":      "departed",
					"#superseded_by": "superseded_by",
				},
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":departed":      attrBool(true),
					":superseded_by": attrS(to),
				},
			}},
		},
	})
	if err != nil {
		// CancellationReasons are positional: index 0 is the clone (to must
		// not exist), index 1 is the retire (from must still exist). Mapping
		// them back to the store's sentinels is what lets the daemon render
		// the two guard failures as the distinct, hint-carrying errors the
		// operator sees.
		if codes := transactCancelCodes(err); codes != nil {
			if len(codes) > 0 && codes[0] == conditionFailedCode {
				return store.ErrBecomeToExists
			}
			if len(codes) > 1 && codes[1] == conditionFailedCode {
				return store.ErrBecomeFromMissing
			}
		}
		return fmt.Errorf("dynamostore: become %q -> %q: %w", from, to, err)
	}
	return nil
}

// rawAgentItem reads an agent's UNMAPPED attributes, strongly consistent.
// Become needs the raw item rather than the store.Agent itemToAgent projects:
// cloning through the struct would silently drop every attribute the struct
// does not name (gsi1pk/gsi1sk, last_read_at), quietly unlisting the successor
// from the roster and resetting display state the clone is supposed to inherit.
func (s *Store) rawAgentItem(ctx context.Context, alias string) (map[string]types.AttributeValue, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            agentKey(alias),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("dynamostore: get agent item %q: %w", alias, err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	return out.Item, nil
}

// SessionAliasLineage returns every alias in a session's supersession lineage —
// the same walk SessionUnread runs, projected to aliases. Both go through
// sessionLineage so the two can never disagree about what a session is.
// Departed aliases are included ON PURPOSE (their unread mail still needs
// draining), and an empty sessionID matches nothing.
//
// sessionCreated scopes the walk's base case to one tmux incarnation, exactly
// as deviceID scopes it to one machine — see sameSession, which carries both,
// and store.API for why they land in the same place and nowhere else.
func (s *Store) SessionAliasLineage(deviceID, socketPath, sessionID string, sessionCreated int64) ([]string, error) {
	members, err := s.sessionLineage(backgroundCtx(), deviceID, socketPath, sessionID, sessionCreated)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, a := range members {
		out = append(out, a.Alias)
	}
	return out, nil
}

// sessionLineage is the ONE place this backend answers "which aliases are this
// session" — the equivalent of the SQLite WITH RECURSIVE `sess` CTE, which both
// SessionUnread and SessionAliasLineage share there and share here. Returned
// rows are base-table reads, sorted by alias, so callers get trustworthy
// per-alias state (watermarks) and not just names.
//
// Base case: every alias sitting on the queried (device, socket, session)
// tuple. Recursive step, iterated to a fixpoint: any alias whose superseded_by
// points at an alias already in the set — Become stamps the pointer on the SEED
// aimed FORWARD at its successor, so the walk runs backward down the chain and
// picks up retired seeds whose own rows still sit on long-dead tuples. That is
// the "mail follows the name" rule.
//
// BOTH scoping dimensions — device and incarnation — apply to the BASE CASE
// ONLY, matching store.Store line for line. sameSession is where they live, so
// the base case here carries them by construction rather than by remembering
// to. The recursive step is filtered by NEITHER: superseded_by is an
// alias-valued pointer to a primary key, so there is no coincidence to defend
// against and scoping it could only drop true positives — breaking the
// legitimate cross-device lineage the hosted backend exists to serve, and the
// equally legitimate cross-restart lineage the incarnation dimension arrived
// with. store.API argues this once for both dimensions and both backends.
//
// # What eventual consistency does to the walk
//
// The roster is a GSI read and can never be strongly consistent, so it is used
// for MEMBERSHIP ONLY — the universe of aliases that might be in the lineage.
// Every edge and every accepted row is then re-read from the BASE table
// (agentByAlias, cached so each alias costs at most one GetItem). That
// distinction is not academic here: the daemon's become op runs Become and then
// SessionUnread in the same round trip, and the superseded_by pointer Become
// just wrote is precisely a decision the caller itself just caused. Reading the
// edge off the index copy would miss it, drop the retired seed from its own
// lineage, and report the seed's waiting mail as zero on the very call that is
// supposed to announce it.
//
// The residual lag is confined to membership: an alias registered moments ago
// may not be in the index yet and so cannot be walked to. That under-reports
// rather than inventing lineage, and it does not affect the become path, where
// the seed has been registered since long before the claim and only its
// superseded_by attribute is fresh.
//
// Termination mirrors the SQLite UNION argument: an alias enters the set at
// most once, so a malformed superseded_by cycle (A→B→A) simply adds nothing on
// the round that would re-add a present alias, and the fixpoint loop exits.
func (s *Store) sessionLineage(ctx context.Context, deviceID, socketPath, sessionID string, sessionCreated int64) ([]store.Agent, error) {
	if sessionID == "" {
		return nil, nil
	}
	items, err := s.roster(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(items))
	for _, item := range items {
		if alias := strAttr(item, "alias"); alias != "" {
			candidates = append(candidates, alias)
		}
	}
	sort.Strings(candidates)

	// Base-table re-read, memoized: every alias is fetched at most once, and
	// nothing below ever consults the index copy of an agent's state.
	fresh := make(map[string]store.Agent, len(candidates))
	seen := make(map[string]bool, len(candidates))
	get := func(alias string) (store.Agent, bool, error) {
		if seen[alias] {
			a, ok := fresh[alias]
			return a, ok, nil
		}
		seen[alias] = true
		a, found, err := s.agentByAlias(ctx, alias)
		if err != nil {
			return store.Agent{}, false, err
		}
		if !found {
			return store.Agent{}, false, nil
		}
		fresh[alias] = a
		return a, true, nil
	}

	inSet := make(map[string]bool)
	var out []store.Agent
	for _, alias := range candidates {
		a, found, err := get(alias)
		if err != nil {
			return nil, err
		}
		// Re-confirm the tuple against the fresh item: the index copy may
		// have been written before the alias moved to a different session.
		// A departed row with no successor is another conversation's
		// tombstone on this tuple, not this conversation's identity — it
		// only belongs in the walk if some later alias points back at it
		// (picked up by the recursive step below), never as a base-case seed.
		if !found || !sameSession(a, deviceID, socketPath, sessionID, sessionCreated) {
			continue
		}
		if a.Departed && a.SupersededBy == "" {
			continue
		}
		inSet[a.Alias] = true
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, nil
	}
	for {
		added := false
		for _, alias := range candidates {
			if inSet[alias] {
				continue
			}
			a, found, err := get(alias)
			if err != nil {
				return nil, err
			}
			if !found || a.SupersededBy == "" || !inSet[a.SupersededBy] {
				continue
			}
			inSet[a.Alias] = true
			out = append(out, a)
			added = true
		}
		if !added {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
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
//
// attribute_exists(pk) is load-bearing here for the same reason it is on
// TouchAgent and StampHarness, and more so: membership comes from
// roster(), a GSI, so a purge_agent that already removed the base item can
// still be lagging in the index. Without the condition this upsert would
// RECREATE the deleted alias as a phantom base item holding only label and
// label_manual — no gsi1pk, so ListAgents could never see it again to clean it
// up, while GetAgent answered ok=true with an empty identity. Skipping is also
// the SQLite contract: its UPDATE matches no rows for a deleted alias.
//
// sessionCreated narrows the write to the caller's PROVEN incarnation, via
// sameSession — a label write lands on the session running now, never on a
// recycled-ID ghost (created mismatch) or an unprovable legacy row (created 0).
// The empty-tuple guard above already excludes the paneless case, so
// sameSession's socketPath exemption cannot fire here; the incarnation clause
// is unconditional in practice.
func (s *Store) SetSessionLabel(deviceID, socketPath, sessionID string, sessionCreated int64, label string, manual bool) (int64, error) {
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
		if a.Departed || !sameSession(a, deviceID, socketPath, sessionID, sessionCreated) {
			continue
		}
		_, err := s.c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:           aws.String(s.table),
			Key:                 agentKey(a.Alias),
			UpdateExpression:    aws.String("SET #label = :label, #label_manual = :label_manual"),
			ConditionExpression: aws.String("attribute_exists(pk)"),
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
			if isConditionFailed(err) {
				// The alias was deleted between the roster read and this
				// write. Skip it — and do NOT count it, since the SQLite
				// UPDATE would have matched no rows either.
				continue
			}
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
		// sameTuple, NOT sameSession: this method reasons across incarnations
		// of one tuple, so scoping to `created` would hide every row it is
		// hunting. The created comparison two lines down is the incarnation
		// logic, inverted on purpose.
		if a.Departed || !sameTuple(a, deviceID, socketPath, sessionID) {
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
// session": the full (device, socket_path, session_id) identity AND the one
// incarnation of it named by sessionCreated — never the pair alone (see
// store.API, which argues both dimensions and why they land in the same
// place). Every session-scoped surface here goes through it so none of them
// can quietly drop a dimension again, which is exactly how the device
// dimension was lost once already.
//
// The incarnation clause mirrors the SQLite base case character for
// character:
//
//	AND (?1 = '' OR (session_created = ?4 AND ?4 != 0))
//
// An empty socketPath — the paneless harness tuple — is EXEMPT: a harness UUID
// is never recycled, so there is no second incarnation to tell apart. A zero
// sessionCreated on a real tmux tuple matches NOTHING, because zero is the
// absence of proof rather than a value. The device clause has no such
// exemption; a paneless registration still belongs to one machine.
//
// sameTuple is the same predicate with the incarnation left out, for the one
// caller whose question is about incarnations rather than within one.
func sameSession(a store.Agent, deviceID, socketPath, sessionID string, sessionCreated int64) bool {
	if !sameTuple(a, deviceID, socketPath, sessionID) {
		return false
	}
	if socketPath == "" {
		return true // paneless: harness UUIDs are never recycled
	}
	return sessionCreated != 0 && a.SessionCreated == sessionCreated
}

// sameTuple is sameSession's location half — device and tuple, no incarnation.
// It exists for DepartStaleSiblings alone, whose whole job is to compare rows
// ACROSS incarnations of one tuple ("another row claims my session id under a
// different creation time"), so an incarnation-scoped predicate would filter
// out the very rows it is looking for. Every other caller wants sameSession.
func sameTuple(a store.Agent, deviceID, socketPath, sessionID string) bool {
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
		Alias:            strAttr(item, "alias"),
		Role:             strAttr(item, "role"),
		ModelType:        strAttr(item, "model_type"),
		SocketPath:       strAttr(item, "socket_path"),
		PaneID:           strAttr(item, "pane_id"),
		SessionName:      strAttr(item, "session_name"),
		SessionID:        strAttr(item, "session_id"),
		SessionCreated:   numAttr(item, "session_created"),
		DeviceID:         strAttr(item, "device_id"),
		DeviceName:       strAttr(item, "device_name"),
		HarnessSessionID: strAttr(item, "harness_session_id"),
		TranscriptPath:   strAttr(item, "transcript_path"),
		Project:          strAttr(item, "project"),
		Label:            strAttr(item, "label"),
		LabelManual:      boolAttr(item, "label_manual"),
		RegisteredAt:     numAttr(item, "registered_at"),
		LastSeen:         numAttr(item, "last_seen"),
		LastReadEntryID:  numAttr(item, "last_read_entry_id"),
		Departed:         boolAttr(item, "departed"),
		SupersededBy:     strAttr(item, "superseded_by"),
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

// conditionFailedCode is the CancellationReason code DynamoDB reports for the
// transaction item whose ConditionExpression failed.
const conditionFailedCode = "ConditionalCheckFailed"

// transactCancelCodes returns the per-item cancellation reason codes of a
// cancelled TransactWriteItems, positionally aligned with the TransactItems
// that were submitted, or nil if err is not a cancellation.
//
// A transaction failure does NOT surface as ConditionalCheckFailedException,
// so isConditionFailed cannot see it — this is the transactional counterpart,
// not a second copy of it. The codes are what let a caller tell WHICH guard
// tripped when a transaction carries more than one (see Become).
func transactCancelCodes(err error) []string {
	var tc *types.TransactionCanceledException
	if !errors.As(err, &tc) {
		return nil
	}
	codes := make([]string, len(tc.CancellationReasons))
	for i, r := range tc.CancellationReasons {
		if r.Code != nil {
			codes[i] = *r.Code
		}
	}
	return codes
}
