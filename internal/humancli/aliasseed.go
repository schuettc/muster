package humancli

import (
	"strings"

	"github.com/schuettc/muster/internal/device"
)

// seedAlias prefixes a DERIVED alias with this machine's configured device
// name, so two machines cannot silently claim the same one.
//
// The problem it solves is specific and, before a bus could span devices,
// impossible. Derived aliases come from a tmux session name or a working
// directory basename — both of which are IDENTICAL on two machines with the
// same repos checked out and the same tmux conventions. Registering is an
// upsert, so the second machine to register would take the alias, and because
// the roster row IS the identity it would take the inbox and read-state with
// it. Worse, session hooks re-register on every session start, so the two
// machines would trade the alias back and forth with mail following whoever
// registered last, and nothing anywhere would report it.
//
// Only DERIVED aliases are seeded. An alias the operator typed, or set via
// $MUSTER_ALIAS, is an explicit choice and is left exactly as given — if
// someone deliberately registers "worker" on two machines, that is a decision
// muster should not quietly rewrite.
//
// Seeding is gated on the device name being CONFIGURED rather than merely
// derived from the hostname. A machine still answering to whatever the OS
// calls it contributes nothing worth putting in front of every alias, and the
// overwhelmingly common case — one machine, local bus, no device name ever
// set — is byte-for-byte unchanged.
func seedAlias(derived string) string {
	name := device.NameConfigured()
	if name == "" || derived == "" {
		return derived
	}
	// Already carrying the prefix: re-registering must produce the SAME alias
	// it produced last time, or a resume would register a second identity
	// ("work-laptop-work-laptop-muster") and orphan the first one's mail.
	if strings.HasPrefix(derived, name+"-") || derived == name {
		return derived
	}
	return name + "-" + derived
}
