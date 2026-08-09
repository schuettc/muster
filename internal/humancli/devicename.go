package humancli

import (
	"fmt"
	"io"
	"strings"

	"github.com/schuettc/muster/internal/device"
)

// cmdDevice prints this machine's device name, or sets it when given one.
//
// The name exists so an operator can say which machine they mean — "the ci-cd
// session on my work laptop" — and have an agent resolve it against the
// roster. The default is the machine's hostname reduced to alias-safe form,
// which is matchable but not usually what anyone says out loud, so setting it
// is the expected gesture rather than an advanced one.
//
// Setting writes a file under MUSTER_HOME rather than asking the operator to
// export anything. That is what makes it survive a reboot without touching a
// shell profile, and what makes every process agree on the answer regardless
// of which shell started it.
func cmdDevice(args []string, out io.Writer) error {
	// Checked before the name path, because every argument otherwise reads as
	// a name to set — without this, `muster device -h` cheerfully renames the
	// machine to "h" and reports success.
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		return HelpFor("device", out)
	}
	switch len(args) {
	case 0:
		name := device.Name()
		if name == "" {
			return fmt.Errorf("no device name and no usable hostname; set one with `muster device <name>`")
		}
		source := "hostname"
		if device.NameConfigured() != "" {
			source = "configured"
		}
		id, err := device.ID()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "name=%s (%s) id=%s\n", name, source, id)
		return err
	case 1:
		clean, err := device.SetName(args[0])
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "device name set to %s\n", clean); err != nil {
			return err
		}
		if clean != strings.TrimSpace(args[0]) {
			// Say so rather than silently accepting something else: the name
			// goes into aliases and the roster, so the operator needs to know
			// the string they will actually see.
			if _, err := fmt.Fprintf(out, "(normalised from %q — device names are lowercase letters, digits and dashes)\n", args[0]); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(out, "new registrations from this machine will carry it.")
		if _, err2 := fmt.Fprintln(out,
			"already registered under an unseeded alias? plain re-registration makes a SECOND identity\n"+
				"and leaves your mail on the first — carry it over with `muster become <new-alias>` instead."); err2 != nil {
			return err2
		}
		return err
	default:
		return usageErrorf("usage: muster device [<name>]")
	}
}
