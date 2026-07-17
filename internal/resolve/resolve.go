// Package resolve is muster's single canonical target-resolution module
// (CLAUDE.md's "one canonical module per concern" rule): the precedence a
// bare user- or agent-supplied target string is matched against the roster
// by — exact alias, then a project-qualified "proj:label", then a bare label
// scoped to the caller's own project. Both internal/humancli (the CLI's own
// resolver, used before nudge/inbox/tasks/send) and internal/daemon (the
// authoritative check send_message/task_create run for every caller,
// including MCP agents that never resolve client-side) share this one
// implementation so the two surfaces can never drift into different
// addressing rules — the drift that let an MCP agent address a label the
// daemon accepted literally, silently creating an undeliverable thread (the
// black-hole incident this package exists to close).
package resolve

import (
	"fmt"
	"sort"
	"strings"
)

// Candidate is the minimal per-agent view Target needs. The daemon builds
// these from store.Agent's STORED label/label_manual (internal/daemon is
// tmux-agnostic by rule — it never queries tmux itself); the CLI builds them
// from its tmux-refreshed enrichedAgent (a live session's label is re-read
// from tmux, a dead one falls back to the stored snapshot), so a
// manually-pinned label a human just typed in a live pane resolves
// immediately there without waiting for a re-register. Both feed the exact
// same precedence rules below.
type Candidate struct {
	Alias       string
	Project     string
	Label       string
	LabelManual bool
	// Departed marks a tombstoned (deregistered) agent (store.Agent.Departed):
	// its alias still resolves — mail may be waiting for it to return — but
	// its label does not. A label is a live, mutable "what are you working on
	// right now" signal; once an agent has departed, that label no longer
	// describes anyone reachable, so matching it would misdirect a NEW
	// message onto a dead thread instead of failing loudly.
	Departed bool
}

// Target resolves given to a unique alias against candidates, scoped to
// callerProject for a bare (unqualified) label. Precedence:
//  1. exact alias match (including departed rows — mail may be waiting)
//  2. qualified "proj:label" (non-departed, manually-pinned label only)
//  3. bare label, restricted to callerProject (same restriction)
//
// Unknown or ambiguous targets return a descriptive error listing
// candidates — Target must never resolve to anything but the single unique
// answer; every caller depends on that never-silent contract.
func Target(candidates []Candidate, given, callerProject string) (string, error) {
	if alias, ok := exactAlias(candidates, given); ok {
		return alias, nil
	}
	if proj, label, ok := strings.Cut(given, ":"); ok {
		var hits []string
		for _, c := range candidates {
			if !c.Departed && c.Project == proj && c.LabelManual && c.Label == label {
				hits = append(hits, c.Alias)
			}
		}
		return uniqueOrErr(hits, given)
	}
	var inProject, elsewhere []string
	for _, c := range candidates {
		if !c.Departed && c.LabelManual && c.Label == given {
			if c.Project == callerProject {
				inProject = append(inProject, c.Alias)
			} else {
				elsewhere = append(elsewhere, qualify(c.Project, given))
			}
		}
	}
	if len(inProject) > 0 {
		return uniqueOrErr(inProject, given)
	}
	if len(elsewhere) > 0 {
		sort.Strings(elsewhere)
		return "", fmt.Errorf("label %q is not in your project; qualify it: %s", given, strings.Join(elsewhere, ", "))
	}
	return "", fmt.Errorf("no agent or addressable label %q", given)
}

// AliasExact resolves given ONLY against exact aliases (including departed
// rows) — no label matching of any kind. This is the daemon's fallback for
// an UNREGISTERED sender (spec: "unregistered sender → alias-exact matching
// only"): without a registered sender there is no callerProject to scope a
// bare label against, and a qualified proj:label's project half is just as
// unauthenticated a claim as a bare one — so an unregistered actor can only
// ever address a target it names exactly.
func AliasExact(candidates []Candidate, given string) (string, error) {
	if alias, ok := exactAlias(candidates, given); ok {
		return alias, nil
	}
	return "", fmt.Errorf("no agent or addressable label %q", given)
}

func exactAlias(candidates []Candidate, given string) (string, bool) {
	for _, c := range candidates {
		if c.Alias == given {
			return c.Alias, true
		}
	}
	return "", false
}

func uniqueOrErr(hits []string, given string) (string, error) {
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("no agent or addressable label %q", given)
	default:
		sort.Strings(hits)
		return "", fmt.Errorf("%q is ambiguous: %s", given, strings.Join(hits, ", "))
	}
}

func qualify(project, label string) string {
	if project == "" {
		return label
	}
	return project + ":" + label
}
