package humancli

import (
	"fmt"
	"io"

	tools "github.com/schuettc/tools-common"

	"github.com/schuettc/muster/internal/version"
)

// cmdUpdate is the registry Run entry point for `muster update`. It honors the
// help convention every other command follows before doing any work.
func cmdUpdate(args []string, out io.Writer) error {
	if helpRequested(args) {
		return HelpFor("update", out)
	}
	return runUpdate(out)
}

// runUpdate self-updates muster in place from the family download standard
// (muster.tools/dl), via the shared tools-common self-update. Both the
// informational stream and the result line go to out; muster's other
// commands print to a single writer, so this stays consistent with them.
func runUpdate(out io.Writer) error {
	app := tools.New(tools.Config{
		Name:   "muster",
		Domain: "muster.tools",
		Version: tools.Version{
			Number: version.Version(),
			Commit: version.Commit(),
			Date:   version.Date(),
		},
	})
	updated, newVer, err := app.SelfUpdate(out, out)
	if err != nil {
		return err
	}
	if updated {
		_, err = fmt.Fprintf(out, "muster updated to %s\n", newVer)
	} else {
		_, err = fmt.Fprintf(out, "muster is already the latest (%s)\n", version.Version())
	}
	return err
}
