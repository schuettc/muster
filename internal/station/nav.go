package station

import "sort"

// This file is the IA-redesign's own concern (spec §5-REVISED): the
// project-first, two-column drill-down's navigation state machine and the
// client-side data derivations it needs (project rollups, project/agent
// conversation membership, cross-project marking). Nothing here touches
// polling/cursor discipline (poll.go) or rendering (views.go/layout.go).
//
// The drill-down is a fixed-depth hierarchy (projects → project → agent? →
// conversation), so rather than a literal []level stack, Model tracks one
// selection per level (project/agent/conversation, see model.go) plus a
// screen+focusTarget pair that together encode exactly where in the
// hierarchy the operator currently is. screen names the current PAGE;
// focusTarget names which of that page's sub-lists (or the right-pane
// reader) currently owns the cursor. Climbing (Esc) never clears a
// level's selection — only screen/focus move — so re-entering a level
// lands back on the same row (spec: "per-level selection").
type screen int

const (
	screenProjects screen = iota // L0: the project list (auto-skipped on a single-project bus)
	screenProject                // L1 (+ L2 when focus is focusConvRight): agent strip + conversations
	screenAgent                  // L1.5 (+ L2 when focus is focusConvRight): one agent's threads + activity
)

// focusTarget is which sub-list (or the right-pane reader) currently owns
// the cursor within the current screen.
type focusTarget int

const (
	focusProjectList  focusTarget = iota // screenProjects' only list
	focusForYou                          // screenProjects' pinned FOR YOU section (spec iteration-5), only a live target while it's showing
	focusAgentStrip                      // screenProject's top strip
	focusConvList                        // screenProject's conversation list
	focusAgentThreads                    // screenAgent's thread list
	focusConvRight                       // L2: the right pane is focused (read/load-older/reply)
)

// llList identifies which LEFT list a '/' filter is scoped to (spec §5-REVISED
// "/ filter left list") — the direct successor of the old three-pane `pane`
// filter target, now parameterized over the drill-down's lists instead of
// three fixed panes.
type llList int

const (
	llProjects llList = iota
	llForYou
	llAgentStrip
	llConvList
	llAgentThreads
)

// unassignedProject is the synthetic project key for the "(unassigned)"
// bucket (spec iteration-4 orphan-thread fix, queue item 4d): the home for a
// thread whose participants have ALL deregistered AND whose origin_project
// never got stamped (an unresolvable historical row, or a genuinely
// unregistered sender) — never a real agent's Project value, so no agent
// ever lands here; only threadProjects' fallback assigns it. Chosen with a
// NUL prefix so it can never collide with an operator-chosen project name,
// and sorts before every real project name (including "").
const unassignedProject = "\x00unassigned"

// projectDisplayName renders a raw project string for display — "" (the
// v0.5.1 default bucket) shows as "(default)", matching the roster's
// existing "(none)" convention's spirit but the term the rest of the CLI
// (`muster agents`) already uses for an unset project; unassignedProject
// shows as "(unassigned)" (spec iteration-4: threads with no known project).
func projectDisplayName(p string) string {
	switch p {
	case "":
		return "(default)"
	case unassignedProject:
		return "(unassigned)"
	default:
		return p
	}
}

// projectSummary is one L0 row (spec §5-REVISED): name, live/total agents,
// unread rollup (+action), last-activity age.
type projectSummary struct {
	Name         string
	Live, Total  int
	Unread       int
	ActionUnread int
	LastAt       int64
}

// aliasProjectMap builds the alias→project lookup conversation membership
// (threadProjects) and rollups need — the roster is the single source of
// truth for which project an alias belongs to.
func aliasProjectMap(agents []agentEnriched) map[string]string {
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		m[a.Alias] = a.Project
	}
	return m
}

// computeProjectSummaries groups the roster by project and rolls up
// unread/action counts by DISTINCT SESSION TUPLE within each project — never
// summed per-alias, mirroring spec §3's "no summing of per-alias counts"
// principle for the exact same reason: sibling aliases of one session must
// not double-count that session's unread. Last-activity age is derived
// separately from threads (a project's rollup reflects its conversations'
// actual traffic, not just agent registration time).
func computeProjectSummaries(agents []agentEnriched, threads []listThreadRow) []projectSummary {
	byProject := map[string]*projectSummary{}
	seenTuple := map[string]map[[2]string]bool{}
	order := []string{}

	// ensureProject returns name's rollup row, creating an empty one (0
	// agents, 0 unread) the first time a THREAD references a project no
	// agent currently belongs to — the "(unassigned)" bucket (and any other
	// project known only via an origin_project stamp) needs an L0 row of its
	// own, not just a silently-dropped LastAt update (spec iteration-4: "so
	// nothing is ever invisible").
	ensureProject := func(name string) *projectSummary {
		p, ok := byProject[name]
		if !ok {
			p = &projectSummary{Name: name}
			byProject[name] = p
			seenTuple[name] = map[[2]string]bool{}
			order = append(order, name)
		}
		return p
	}

	for _, a := range agents {
		p := ensureProject(a.Project)
		p.Total++
		if a.Live {
			p.Live++
		}
		if a.SocketPath != "" && a.SessionID != "" {
			tup := [2]string{a.SocketPath, a.SessionID}
			if !seenTuple[a.Project][tup] {
				seenTuple[a.Project][tup] = true
				p.Unread += a.Unread
				p.ActionUnread += a.ActionCount
			}
		}
	}
	aliasProj := aliasProjectMap(agents)
	for _, row := range threads {
		for _, proj := range threadProjectsOrUnassigned(row, aliasProj) {
			p := ensureProject(proj)
			if row.LastAt > p.LastAt {
				p.LastAt = row.LastAt
			}
		}
	}
	out := make([]projectSummary, 0, len(order))
	for _, name := range order {
		out = append(out, *byProject[name])
	}
	sort.Slice(out, func(i, j int) bool {
		return projectDisplayName(out[i].Name) < projectDisplayName(out[j].Name)
	})
	return out
}

// dedupeStrings returns ss with duplicates removed, order preserved.
func dedupeStrings(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// participantAliases returns a thread row's known participant aliases
// (spec §5-REVISED: "a thread belongs to every project having a
// participant") — the sender, the recipient when addressed directly to an
// agent (role/broadcast targets have no single alias to attribute), and the
// last-speaker (so a reply from a THIRD alias also counts as participating,
// not just the original from/to pair).
func participantAliases(row listThreadRow) []string {
	out := []string{row.FromAgent}
	if row.ToKind == "agent" {
		out = append(out, row.ToTarget)
	}
	out = append(out, row.LastFrom)
	return dedupeStrings(out)
}

// threadProjects returns the sorted, deduped set of projects a thread
// touches: every project of its CURRENTLY-REGISTERED participants (via the
// roster) UNION its origin_project (spec iteration-4 orphan-thread fix,
// queue item 4d) — the origin stamp is what keeps a thread reachable once
// every participant has since deregistered. An alias with no roster entry
// contributes nothing on its own; an empty origin_project (never stamped)
// contributes nothing either. A thread with NEITHER kind of project ends up
// with an empty result here — see threadProjectsOrUnassigned, which is what
// every caller that needs "a project to file this thread under" (as opposed
// to raw membership testing) actually uses.
func threadProjects(row listThreadRow, aliasProject map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, alias := range participantAliases(row) {
		proj, ok := aliasProject[alias]
		if !ok || seen[proj] {
			continue
		}
		seen[proj] = true
		out = append(out, proj)
	}
	if row.OriginProject != "" && !seen[row.OriginProject] {
		out = append(out, row.OriginProject)
	}
	sort.Strings(out)
	return out
}

// threadProjectsOrUnassigned is threadProjects, falling back to
// unassignedProject when a thread maps to no known project at all (spec
// iteration-4: "so nothing is ever invisible") — the form every filing
// decision (L0 rollups, L1 conversation-list membership) needs, since a
// thread must always land SOMEWHERE.
func threadProjectsOrUnassigned(row listThreadRow, aliasProject map[string]string) []string {
	projs := threadProjects(row, aliasProject)
	if len(projs) == 0 {
		return []string{unassignedProject}
	}
	return projs
}

// conversationRow is one L1 conversation-list row: the wire thread row plus
// OTHER projects it also touches (spec §5-REVISED: "cross-project threads
// marked '↔ otherproj' and shown in both projects") — empty when the
// thread's participants are all in the ONE project this row is being shown
// for.
type conversationRow struct {
	listThreadRow
	OtherProjects []string
}

// conversationsForProject filters threads to those touching project (any
// participant's roster project matches, or project is unassignedProject and
// the thread maps to no known project at all — spec iteration-4),
// annotating each with whichever OTHER projects it also touches.
func conversationsForProject(threads []listThreadRow, aliasProject map[string]string, project string) []conversationRow {
	var out []conversationRow
	for _, row := range threads {
		projs := threadProjectsOrUnassigned(row, aliasProject)
		touches := false
		for _, p := range projs {
			if p == project {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}
		var other []string
		for _, p := range projs {
			if p != project {
				other = append(other, p)
			}
		}
		out = append(out, conversationRow{listThreadRow: row, OtherProjects: other})
	}
	return out
}

// conversationsForAgent filters threads to those alias participates in
// (spec §5-REVISED L1.5: "their threads").
func conversationsForAgent(threads []listThreadRow, alias string) []listThreadRow {
	var out []listThreadRow
	for _, row := range threads {
		for _, a := range participantAliases(row) {
			if a == alias {
				out = append(out, row)
				break
			}
		}
	}
	return out
}

// unreadThreadsFor returns the threads alias participates in (spec iteration-5
// Tier 0b: unread AGE) where the last entry was NOT written by alias — the
// display-only proxy for "waiting on alias" this feature uses everywhere a
// per-alias unread age is needed, since a real per-thread read watermark
// isn't obtainable without get_inbox (which nothing outside the
// acknowledge path may call). A thread alias itself spoke last is excluded:
// it's the OTHER party's turn, not alias's.
func unreadThreadsFor(threads []listThreadRow, alias string) []listThreadRow {
	var out []listThreadRow
	for _, row := range conversationsForAgent(threads, alias) {
		if row.LastFrom != "" && row.LastFrom != alias {
			out = append(out, row)
		}
	}
	return out
}

// unreadThreadsForProject is unreadThreadsFor's project-rollup counterpart
// (spec iteration-5 Tier 0b, project rollups): a project's conversations
// whose last speaker is NOT a currently-registered member of that SAME
// project — i.e. the last word came from outside the project (or from an
// alias the roster no longer knows), so the project itself is "waiting".
func unreadThreadsForProject(threads []listThreadRow, aliasProject map[string]string, project string) []listThreadRow {
	var out []listThreadRow
	for _, row := range conversationsForProject(threads, aliasProject, project) {
		if row.LastFrom == "" {
			continue
		}
		if proj, ok := aliasProject[row.LastFrom]; ok && proj == project {
			continue
		}
		out = append(out, row.listThreadRow)
	}
	return out
}

// oldestUnreadAt returns the smallest (oldest) LastAt among rows, or 0 when
// rows is empty or every row's LastAt is unset — the raw ms-epoch value;
// views.go's relativeAge renders it for display.
func oldestUnreadAt(rows []listThreadRow) int64 {
	var oldest int64
	for _, r := range rows {
		if r.LastAt <= 0 {
			continue
		}
		if oldest == 0 || r.LastAt < oldest {
			oldest = r.LastAt
		}
	}
	return oldest
}

// agentsForProject returns the roster rows belonging to project, sorted by
// alias — the L1 agent strip's row order.
func agentsForProject(agents []agentEnriched, project string) []agentEnriched {
	var out []agentEnriched
	for _, a := range agents {
		if a.Project == project {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// projKey/agentKey/convKey are the identity-key extractors the generic
// selection helpers below use for the three keyed lists (projects by name,
// agent strip by alias, conversations by thread ID) — the zero value of
// each key type ("" / "" / 0) doubles as "nothing selected", exactly like
// the pre-redesign code's threadSelected==0 sentinel.
func projKey(p projectSummary) string { return p.Name }
func agentKey(a agentEnriched) string { return a.Alias }
func convKey(c conversationRow) int64 { return c.ID }

// The generic trio below (visible/snap/move) is the ONE shared visible-rows
// filter/selection-stability predicate discipline the current (pre-redesign)
// code applied separately to the roster and threads panes (rosterRowVisible/
// moveRosterSelection and threadRowVisible/moveThreadSelection) — carried
// over per the task brief, now parameterized over row type T and a
// comparable identity key K so the SAME logic serves all four of the
// redesign's lists (projects by name, agent strip by alias, conversations by
// thread ID) instead of four hand-copied variants.

// keyIndex returns rows' index whose key(row) == sel, or -1.
func keyIndex[T any, K comparable](rows []T, key func(T) K, sel K) int {
	for i, r := range rows {
		if key(r) == sel {
			return i
		}
	}
	return -1
}

// rowVisible reports whether row's rendered text passes an active filter —
// always true when filtering is false.
func rowVisible[T any](row T, renderText func(T) string, query string, filtering bool) bool {
	return !filtering || containsFold(renderText(row), query)
}

// anyRowVisible reports whether any row in rows passes the active filter —
// callers (Enter/n) use this to distinguish "nothing to act on" from a real
// selection, mirroring the old code's rosterIdx<0/threadSelected==0 guards.
func anyRowVisible[T any](rows []T, renderText func(T) string, query string, filtering bool) bool {
	for _, r := range rows {
		if rowVisible(r, renderText, query, filtering) {
			return true
		}
	}
	return false
}

// selectionVisible reports whether sel currently points at a VISIBLE row.
func selectionVisible[T any, K comparable](rows []T, key func(T) K, sel K, renderText func(T) string, query string, filtering bool) bool {
	idx := keyIndex(rows, key, sel)
	if idx < 0 {
		return false
	}
	return rowVisible(rows[idx], renderText, query, filtering)
}

// snapSelection corrects sel to the first row visible under the active
// filter when it isn't already visible (spec §5 carried-over fix: filter/
// selection desync) — to zero ("nothing selected") when nothing is visible.
// A no-op when sel is already visible.
func snapSelection[T any, K comparable](rows []T, key func(T) K, sel K, renderText func(T) string, query string, filtering bool, zero K) K {
	if selectionVisible(rows, key, sel, renderText, query, filtering) {
		return sel
	}
	for _, r := range rows {
		if rowVisible(r, renderText, query, filtering) {
			return key(r)
		}
	}
	return zero
}

// moveSelection applies a j/k (delta=+1/-1) move to sel within rows' VISIBLE
// order, snapping first so a move starting from a filtered-out selection
// lands relative to the nearest visible row.
func moveSelection[T any, K comparable](rows []T, key func(T) K, sel K, delta int, renderText func(T) string, query string, filtering bool, zero K) K {
	sel = snapSelection(rows, key, sel, renderText, query, filtering, zero)
	var visible []int
	for i, r := range rows {
		if rowVisible(r, renderText, query, filtering) {
			visible = append(visible, i)
		}
	}
	if len(visible) == 0 {
		return zero
	}
	pos := -1
	for p, i := range visible {
		if key(rows[i]) == sel {
			pos = p
			break
		}
	}
	if pos < 0 {
		pos = 0
	} else {
		pos += delta
		if pos < 0 {
			pos = 0
		}
		if pos > len(visible)-1 {
			pos = len(visible) - 1
		}
	}
	return key(rows[visible[pos]])
}

// groupConversationRows applies groupThreads' action/reply/rest partition
// (spec §5-REVISED: "action-requested pinned") to a []conversationRow
// without losing the OtherProjects annotation — groupThreads itself stays
// the []listThreadRow-only primitive both this and the plain agent-page
// list (which has no cross-project annotation to carry) share.
func groupConversationRows(rows []conversationRow) []conversationRow {
	byID := make(map[int64]conversationRow, len(rows))
	plain := make([]listThreadRow, len(rows))
	for i, r := range rows {
		plain[i] = r.listThreadRow
		byID[r.ID] = r
	}
	grouped := groupThreads(plain)
	out := make([]conversationRow, len(grouped))
	for i, r := range grouped {
		out[i] = byID[r.ID]
	}
	return out
}
