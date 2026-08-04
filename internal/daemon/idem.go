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
}

// IsWriteOp reports whether op mutates state, and so whether an idempotency
// key on the request has any effect. Reads ignore the key entirely — a read
// must never consume an idempotency record.
func IsWriteOp(op string) bool { return writeOps[op] }

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
		return proto.Response{Error: "idempotency: claim failed, reissue under a new key: " + err.Error()}
	case found && done:
		var resp proto.Response
		// The record is one marshalled proto.Response, so it is never empty
		// and never anything else; if it does not decode, the client must
		// reissue under a new key rather than be handed a fabricated answer.
		if err := json.Unmarshal(recorded, &resp); err != nil {
			return proto.Response{Error: "idempotency: corrupt record, reissue under a new key: " + err.Error()}
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
