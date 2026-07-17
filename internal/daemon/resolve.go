package daemon

import "github.com/schuettc/muster/internal/resolve"

// resolveAgentTarget resolves to_target for a send_message/task_create op
// whose to_kind=="agent" against the CURRENT roster (spec: the black-hole
// fix — an MCP caller passes to_target straight through, with no client-side
// resolution to catch a mistyped or stale label before it reaches the wire,
// so this daemon-side check is the ONLY backstop before a thread gets
// created addressed to nobody). from is the sending agent's alias: its
// CURRENTLY registered project scopes a bare label exactly as the CLI's own
// resolver scopes one by the caller's tmux project. An unregistered sender
// has no project to scope a label against, so it falls back to
// resolve.AliasExact — an exact alias still resolves (spec: "unregistered
// sender → alias-exact matching only"), but no label of any form does.
//
// The daemon builds resolve.Candidate from store.Agent's STORED
// label/label_manual, never a live tmux re-read — internal/daemon is
// tmux-agnostic by rule (CLAUDE.md), so a label change only takes effect
// here once the owning agent's next register_agent upsert lands it in the
// store, exactly as every other daemon-side view of label already works.
func (d *Daemon) resolveAgentTarget(from, given string) (string, error) {
	agents, err := d.s.ListAgents()
	if err != nil {
		return "", err
	}
	candidates := make([]resolve.Candidate, len(agents))
	for i, ag := range agents {
		candidates[i] = resolve.Candidate{
			Alias: ag.Alias, Project: ag.Project,
			Label: ag.Label, LabelManual: ag.LabelManual, Departed: ag.Departed,
		}
	}
	sender, found, err := d.s.GetAgent(from)
	if err != nil {
		return "", err
	}
	if !found {
		return resolve.AliasExact(candidates, given)
	}
	return resolve.Target(candidates, given, sender.Project)
}
