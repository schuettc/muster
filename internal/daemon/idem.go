package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/schuettc/muster/internal/proto"
)

// writeOps are the ops that mutate state. Idempotency applies to all of them
// uniformly rather than to a classified subset: several are naturally
// idempotent (kv_set is last-write-wins, register_agent is an upsert) and
// would not strictly need a key, but a uniform rule cannot be got wrong, and
// the cost is about one extra write unit per mutation.
//
// CAS ops need this despite looking idempotent: if a task_claim succeeds but
// its response is lost, a naive replay returns ErrNotClaimable and the original
// caller wrongly concludes it failed.
//
// get_inbox is here because "write" means MUTATES STATE, not "has no data in
// its response". It advances the agent's last_read_entry_id (MarkRead), pushes
// the tmux badge, and journals a read event. Its response is the interesting
// part: a redelivery that re-executed would recompute the inbox AFTER the
// first delivery already marked everything read, so the client would get an
// inbox with every unread count zeroed and the messages would silently stop
// looking new. That is the exact loss the record exists to prevent.
//
// Every op dispatch handles must be classified as a write here or as a read in
// the test's readOps set; TestEveryDispatchOpIsClassified walks the switch and
// fails on anything that is neither, so a new op cannot quietly become a hole.
var writeOps = map[string]bool{
	"register_agent": true, "deregister_agent": true, "purge_agent": true,
	"send_message": true, "task_create": true, "reply": true,
	"task_claim": true, "task_transition": true,
	"kv_set": true, "log_event": true, "set_label": true,
	"prune_events": true, "get_inbox": true,
	// become is a CAS (it refuses an existing target), so it needs a key for
	// the same reason task_claim does: a claim that succeeded but lost its
	// response would replay into ErrBecomeToExists and tell the caller its own
	// completed claim failed. stamp_harness_session is a plain attribute write.
	"become": true, "stamp_harness_session": true,
}

// IsWriteOp reports whether op mutates state, and so whether an idempotency
// key on the request has any effect. Reads ignore the key entirely — a read
// must never consume an idempotency record.
func IsWriteOp(op string) bool { return writeOps[op] }

// badgeOps are the writes that can move a tmux badge, and so the ones remote
// mode reconciles after (see forward). It is a strict subset of writeOps: a
// write that changes no badge still costs a reconcile a list_agents plus one
// session_unread per local session, and kv_set/log_event are the frequent ops
// on this bus — log_event fires on every nudge typed and submitted — so paying
// a fan-out for them is a steady idle cost, not a burst the coalescer bounds.
//
// The membership rule is not a judgement call: an op belongs here exactly when
// its LOCAL dispatch reaches the badge, i.e. calls notifyForThread,
// setSessionBadge or reconcileBadge. That makes local and remote mode agree on
// what "this changed a badge" means by construction rather than by two people
// reading the same op the same way. TestBadgeOpsMatchDispatch derives the set
// from dispatch's own switch (AST walk, following one hop through the
// per-op handlers) and fails if this map and the code disagree — so a new
// notifying op cannot be silently left out and go un-badged in remote mode.
//
// The reverse mistake is the cheap one: an op wrongly listed here costs a
// redundant reconcile. An op wrongly missing means a badge that never lights.
var badgeOps = map[string]bool{
	"register_agent": true, "deregister_agent": true, "purge_agent": true,
	"send_message": true, "task_create": true, "reply": true,
	"task_claim": true, "task_transition": true, "get_inbox": true,
	// become calls reconcileBadge: the claimed identity inherits the seed's
	// waiting mail, so the badge on its session has to be recomputed. Without
	// this, a claim in remote mode would leave the badge showing the pre-claim
	// count. stamp_harness_session is absent on purpose — it writes one
	// attribute and reaches no badge sink.
	"become": true,
}

// movesBadge reports whether op can change what a tmux badge shows. Only
// meaningful for writes; every read answers false.
func movesBadge(op string) bool { return badgeOps[op] }

// idemRetryPrefix marks the ONE idempotency outcome a client may retry under
// the SAME key: an identical request is still in flight, so the op will have a
// recorded response shortly. Every other idempotency failure is unknown rather
// than not-executed (see IdemBegin's residual-hazard note in
// internal/dynamostore/idem.go), and a same-key retry of one of those can sit
// wedged until the record's TTL expires.
const idemRetryPrefix = "retry: "

// IsRetryableIdemError reports whether a Response.Error is the in-flight
// collision, i.e. whether reissuing the identical request under the SAME
// idempotency key is safe. It is false for every other idempotency error,
// including a failed claim — those must be reissued under a NEW key.
func IsRetryableIdemError(respErr string) bool {
	return strings.HasPrefix(respErr, idemRetryPrefix)
}

// idemNewKeyPrefix marks the idempotency outcomes whose write MAY OR MAY NOT
// have executed: the claim errored (it may have committed with its
// acknowledgement lost), or the recorded response is corrupt (the op ran, but
// what it answered is unrecoverable). Both are unknown rather than failed —
// the client learns nothing about whether its state changed.
//
// The same key can never answer either of them (a same-key retry reads the
// orphaned or unreadable record and sits wedged until its TTL), so the only
// reissue available is under a NEW key. Whether reissuing is SAFE is a
// separate question and an op-specific one — under a corrupt record the write
// provably ran, so a reissue duplicates it — which is why nothing below the
// caller ever reissues on its behalf.
//
// Both messages are BUILT from this constant so the predicate below and the
// strings it classifies cannot drift apart.
const idemNewKeyPrefix = "idempotency: unknown outcome, reissue under a new key: "

// IsNewKeyReissueError reports whether a Response.Error is one of the
// unknown-outcome idempotency results described on idemNewKeyPrefix. It is
// disjoint from IsRetryableIdemError by construction (different prefixes), and
// the two mean opposite things: that one says retry the SAME key, this one says
// the same key is dead and the outcome is unknown.
//
// It is exported for the same reason IsRetryableIdemError is: internal/daemon
// owns these strings, and a transport or a forwarding path that re-derived the
// prefix would be a second copy to keep in sync. lambdamode answers HTTP 200
// for these (they are NOT same-key retryable), so a remote client sees them as
// an ordinary failed Response and needs this to tell them apart.
func IsNewKeyReissueError(respErr string) bool {
	return strings.HasPrefix(respErr, idemNewKeyPrefix)
}

// Dispatch executes one request. When req.IdemKey is set on a write op, the op
// runs AT MOST ONCE: a replay returns the recorded response verbatim, and a
// collision with an identical request still in flight returns a retryable
// error. Local clients send no key, so local mode takes the first branch and
// is unaffected — no extra store round trip, no behaviour change.
//
// A key identifies ONE request, not a caller and not an op. Nothing here binds
// the record to the request that created it (the store primitive carries no
// digest), so a client that reuses a key across two DIFFERENT requests is
// handed the first one's response for the second. That is the standard
// idempotency-key contract and it is the client's to keep; the daemon's job is
// that a key it has already seen never executes twice.
//
// "Verbatim" is a value-level claim, not a byte-level one: the record decodes
// into a proto.Response whose Data is generic JSON, so re-encoding it can order
// object keys differently from the original (which encoded typed structs).
// JSON objects are unordered, so no client that parses the response can tell.
//
// The three IdemBegin outcomes are a contract, not a hint, and each is handled
// on its own:
//
//   - found=false — this caller owns execution and MUST record a response.
//   - found=true, done=true — the op already ran; replay its response.
//   - found=true, done=false — an identical request is in flight; neither
//     execute nor replay.
//
// An ERROR is a fourth outcome and the subtle one: it is not "not claimed."
// The claim may have committed and the acknowledgement been lost, leaving a
// record no live caller can complete (see IdemBegin in
// internal/dynamostore/idem.go). So the op does NOT run — running it would be
// the double execution the record exists to prevent — and the error is
// deliberately NOT marked same-key retryable, because a same-key retry would
// read that orphaned record as in-flight and spin until its TTL expires. The
// client must reissue under a NEW key.
func (d *Daemon) Dispatch(req proto.Request) proto.Response {
	if req.IdemKey == "" || !IsWriteOp(req.Op) {
		return d.dispatch(req)
	}
	recorded, done, found, err := d.s.IdemBegin(req.IdemKey)
	switch {
	case err != nil:
		return proto.Response{Error: idemNewKeyPrefix + "claim failed: " + err.Error()}
	case found && done:
		var resp proto.Response
		// The record is one marshalled proto.Response, so it is never empty
		// and never anything else; if it does not decode, the client must
		// reissue under a new key rather than be handed a fabricated answer.
		if err := json.Unmarshal(recorded, &resp); err != nil {
			return proto.Response{Error: idemNewKeyPrefix + "corrupt record: " + err.Error()}
		}
		return resp
	case found:
		return proto.Response{Error: idemRetryPrefix + "an identical request is in flight"}
	}

	resp := d.dispatch(req)
	// From here the op has ALREADY COMMITTED, so nothing below may turn its
	// response into a failure. A record that stays pending makes a redelivery
	// read as in-flight, which is the safe direction: the client retries or
	// reissues, and the op never runs twice.
	b, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "muster: idem: response not recordable:", err)
		return resp
	}
	if err := d.s.IdemComplete(req.IdemKey, b); err != nil {
		fmt.Fprintln(os.Stderr, "muster: idem complete:", err)
	}
	return resp
}
