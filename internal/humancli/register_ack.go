package humancli

import (
	"encoding/json"
	"fmt"
)

// registerAck decodes register_agent's response data (durable-alias spec
// change 1): how the daemon classified this registration, and the alias's
// unread-thread count at that moment.
type registerAck struct {
	Outcome string `json:"outcome"`
	Unread  int    `json:"unread"`
}

// decodeRegisterAck tolerantly decodes a register_agent response. A daemon
// predating the outcome field (or any decode failure) yields the zero value,
// whose line() renders nothing — callers degrade to today's output.
func decodeRegisterAck(raw json.RawMessage) registerAck {
	var a registerAck
	_ = json.Unmarshal(raw, &a)
	return a
}

// line renders the human-facing follow-up for a registration worth
// remarking on: a revival, or pending mail. "" for an unremarkable new or
// refreshed registration with an empty inbox — the existing "registered X"
// line already says everything.
func (a registerAck) line(alias string) string {
	if a.Outcome != "revived" && a.Unread == 0 {
		return ""
	}
	alias = dispAlias(alias)
	msg := fmt.Sprintf("reconnected: identity '%s' %s", alias, a.Outcome)
	if a.Unread > 0 {
		msg += fmt.Sprintf(" — %d unread thread(s); run `muster inbox '%s'`", a.Unread, alias)
	}
	return msg + "\n"
}
