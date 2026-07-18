package humancli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/schuettc/muster/internal/paths"
	"github.com/schuettc/muster/internal/version"
)

// roffEscape neutralizes roff's leading-dot, backslash, and double-quote
// special meaning in free text pulled from the registry (synopses/help
// paragraphs). A literal "-h" or "--foo" can't be misread as a macro
// request by man(1); a literal "body" (send's synopsis quotes its body
// argument) can't have its quotes silently eaten by .B/.TP's
// whitespace-and-quote argument splitting — groff treats a bare `"word"`
// argument as a QUOTED argument and strips the quotes, which is exactly
// wrong for text we want printed verbatim.
func roffEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\(dq`)
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "'") {
			b.WriteString(`\&`)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ManPage renders muster's man(1) page as roff, generated from the same
// Registry that drives `muster help` — the man page is never hand-maintained
// and never committed (see justfile's `man` recipe and
// .github/workflows/release.yml), so it cannot drift from the CLI's actual
// commands.
func ManPage() string {
	var b strings.Builder

	fmt.Fprintf(&b, `.TH MUSTER 1 "%s" "muster" "User Commands"
`, version.Version())
	b.WriteString(`.SH NAME
muster \- local multi-agent coordination bus
.SH SYNOPSIS
.B muster
.I command
.RI [ args ...]
.SH DESCRIPTION
muster is a local multi-agent coordination bus: independent coding-agent
sessions (each in its own tmux tab) hand tasks and messages to each other
without copy/paste. It never calls a model itself \- it only routes between
agents already running on their own plans.
.PP
The binary has three modes:
.TP
.B serve
runs the daemon, a lazy unix-socket API server every other mode talks to.
.TP
.B mcp
runs an MCP stdio server exposing the daemon's operations as tools for a
coding agent to call directly.
.TP
.I (default)
every other subcommand is the human operator CLI: a plain client of the
daemon, exactly as capable as the MCP tools.
`)

	b.WriteString(".SH COMMANDS\n")
	for _, g := range groupOrder {
		fmt.Fprintf(&b, ".SS %s\n", roffEscape(groupHeading[g]))
		for _, c := range Registry {
			if c.Group != g {
				continue
			}
			fmt.Fprintf(&b, ".TP\n.B muster %s\n%s\n", roffEscape(c.Synopsis), roffEscape(c.Summary))
		}
	}

	fmt.Fprintf(&b, `.SH FILES
.TP
.I ~/%s
Default data directory (override with $MUSTER_HOME).
.TP
.I ~/%s/%s
SQLite journal/store database.
.TP
.I ~/%s/%s
The daemon's unix socket.
`,
		paths.HomeSuffix(),
		paths.HomeSuffix(), filepath.Base(paths.DBPath()),
		paths.HomeSuffix(), filepath.Base(paths.SocketPath()))

	b.WriteString(`.SH SEE ALSO
https://muster.tools
`)
	return b.String()
}
