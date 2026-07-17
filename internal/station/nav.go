package station

import (
	"sort"
)

// This file is the station navigation/data-derivation concern (spec
// §5-LOCK, "operator-approved, locked design"): the pure-STACK navigation
// state machine (see navFrame/Model.stack in model.go) and the client-side
// data derivations it needs (project rollups, project/agent conversation
// membership, mailbox rows, cross-project marking, label-collision
// disambiguation). Nothing here touches polling/cursor discipline (poll.go)
// or rendering (views.go/layout.go).
//
// §5-LOCK's decision B locks navigation to a PURE STACK: Enter pushes a
// frame (drill/open), Esc pops exactly one frame everywhere, g pops all the
// way home. The chain is exactly four content levels — projects (L0) → a
// project's agents (L1) → an agent's threads (L2) → a full-width thread
// read (L3) — plus the mailbox, reached via 'm' from any screen and popped
// back to the exact origin, mirroring mail (threads live UNDER agents like
// messages in a mailbox). The ONE deliberate exception: the synthetic
// "(unassigned)" bucket has no living agent for an orphaned thread to live
// under, so its L1 is a thread list directly (see l1IsOrphaned) — Enter
// there reads full-width with no agent step in between.
type screen int

const (
	screenProjects screen = iota // L0: the project list (auto-skipped on a single-project bus)
	screenProject                // L1: a project's AGENTS ONLY — except the "(unassigned)" bucket's ORPHANED THREADS exception, see l1IsOrphaned
	screenAgent                  // L2: one agent's own thread list (+ header band, spec §5-LOCK screen 4)
	screenMailbox                // the mailbox overlay page (spec §5-LOCK screen 2) — every thread addressed to station, read and unread
	screenRead                   // L3: full-width thread reading (spec §5-LOCK screen 5)
)

// navFrame is one entry on Model.stack (spec §5-LOCK decision B: "introduce
// an explicit navigation stack"). project/agent are snapshotted AT PUSH TIME
// for screenProject/screenAgent frames — the breadcrumb's own per-frame
// label (frameCrumb in views.go) — never re-derived from the model's live
// (possibly since-moved-on) selection fields, so an intervening excursion
// (e.g. 'm' to the mailbox and back) can never corrupt a shallower frame's
// remembered path. screenRead/screenMailbox/screenProjects frames need
// neither field: a Read frame's own crumb text comes from the live
// m.viewThreadID (there is never more than one Read frame on the stack at a
// time — Enter is a no-op at screenRead, the deepest level), and Mailbox/
// Projects both render fixed text.
type navFrame struct {
	screen  screen
	project string
	agent   string
}

// llList identifies which LEFT list a '/' filter is scoped to.
type llList int

const (
	llProjects llList = iota
	llProjectItems
	llAgentThreads
	llMailbox
)

// unassignedProject is the synthetic project key for the "(unassigned)"
// bucket: the home for a thread whose participants have ALL deregistered AND
// whose origin_project never got stamped (an unresolvable historical row, or
// a genuinely unregistered sender) — never a real agent's Project value, so
// no agent ever lands here; only threadProjects' fallback assigns it. Chosen
// with a NUL prefix so it can never collide with an operator-chosen project
// name, and sorts before every real project name (including "").
const unassignedProject = "\x00unassigned"

// projectDisplayName renders a raw project string for display — "" (the
// v0.5.1 default bucket) shows as "(default)"; unassignedProject shows as
// "(unassigned)".
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

// l1IsOrphaned reports whether screenProject's L1 list is currently the
// "(unassigned)" bucket's ORPHANED THREADS exception rather than the normal
// agents-only list every other project shows.
func (m Model) l1IsOrphaned() bool {
	return m.project == unassignedProject
}

// projectSummary is one L0 row: name, live/total agents, unread rollup
// (+action), last-activity age.
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

// participantAliases returns a thread row's known participant aliases — the
// sender, the recipient when addressed directly to an agent (role/broadcast
// targets have no single alias to attribute), and the last-speaker (so a
// reply from a THIRD alias also counts as participating, not just the
// original from/to pair).
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
// roster) UNION its origin_project — the origin stamp is what keeps a
// thread reachable once every participant has since deregistered.
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
// unassignedProject when a thread maps to no known project at all.
func threadProjectsOrUnassigned(row listThreadRow, aliasProject map[string]string) []string {
	projs := threadProjects(row, aliasProject)
	if len(projs) == 0 {
		return []string{unassignedProject}
	}
	return projs
}

// conversationRow is one thread-list row: the wire thread row plus OTHER
// projects it also touches (spec: "cross-project threads marked '↔
// otherproj'") — empty when the thread's participants are all in the ONE
// project/agent this row is being shown for.
type conversationRow struct {
	listThreadRow
	OtherProjects []string
}

// conversationsForProject filters threads to those touching project (any
// participant's roster project matches, or project is unassignedProject and
// the thread maps to no known project at all), annotating each with
// whichever OTHER projects it also touches.
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

// conversationsForAgent filters threads to those alias participates in.
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

// conversationsForAgentAnnotated is conversationsForAgent, additionally
// marking each row with whichever OTHER projects it touches beyond alias's
// own home project — the "↔ otherproj" cross-project marker computed
// against the agent viewing it: a thread's home agent still sees it, marked
// whenever the thread ALSO touches a different project than the agent's own.
func conversationsForAgentAnnotated(threads []listThreadRow, aliasProject map[string]string, alias string) []conversationRow {
	home := aliasProject[alias]
	rows := conversationsForAgent(threads, alias)
	out := make([]conversationRow, 0, len(rows))
	for _, row := range rows {
		var other []string
		for _, p := range threadProjectsOrUnassigned(row, aliasProject) {
			if p != home {
				other = append(other, p)
			}
		}
		out = append(out, conversationRow{listThreadRow: row, OtherProjects: other})
	}
	return out
}

// unreadThreadsFor returns the threads alias participates in where the last
// entry was NOT written by alias — the display-only proxy for "waiting on
// alias" used everywhere a per-alias unread age is needed, since a real
// per-thread read watermark isn't obtainable without get_inbox.
func unreadThreadsFor(threads []listThreadRow, alias string) []listThreadRow {
	var out []listThreadRow
	for _, row := range conversationsForAgent(threads, alias) {
		if row.LastFrom != "" && row.LastFrom != alias {
			out = append(out, row)
		}
	}
	return out
}

// unreadThreadsForProject is unreadThreadsFor's project-rollup counterpart: a
// project's conversations whose last speaker is NOT a currently-registered
// member of that SAME project.
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
// rows is empty or every row's LastAt is unset.
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

// lastActivityAt returns the newest LastAt among threads alias participates
// in — a departed agent's "thread-count + age" row (spec §5-LOCK screen 3)
// needs this even though it has no live journal activity of its own to
// derive an age from.
func lastActivityAt(threads []listThreadRow, alias string) int64 {
	var latest int64
	for _, r := range conversationsForAgent(threads, alias) {
		if r.LastAt > latest {
			latest = r.LastAt
		}
	}
	return latest
}

// agentsForProject returns the roster rows belonging to project, LIVE agents
// first (each group alphabetical by alias) — spec §5-LOCK screen 3: "live
// agents on top... then quit agents", the same order both cursor movement
// and rendering must agree on.
func agentsForProject(agents []agentEnriched, project string) []agentEnriched {
	var out []agentEnriched
	for _, a := range agents {
		if a.Project == project {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		return out[i].Alias < out[j].Alias
	})
	return out
}

// computeLabelCollisions reports, for every agent, whether its CURRENT
// display label needs its alias appended to stay unambiguous (spec §5-LOCK
// item 7): the label collides with another agent's label, or equals another
// agent's actual alias — "who → who" must never read like nonsense. The one
// shared helper feeding Model.dispLabel, which every label-rendering call
// site in the package goes through — never a second vocabulary.
func computeLabelCollisions(agents []agentEnriched) map[string]bool {
	labelCount := map[string]int{}
	for _, a := range agents {
		label := a.Label
		if label == "" {
			label = a.Alias
		}
		labelCount[label]++
	}
	out := map[string]bool{}
	for _, a := range agents {
		label := a.Label
		if label == "" {
			label = a.Alias
		}
		collide := labelCount[label] > 1
		if !collide && label != a.Alias {
			for _, b := range agents {
				if b.Alias != a.Alias && b.Alias == label {
					collide = true
					break
				}
			}
		}
		if collide {
			out[a.Alias] = true
		}
	}
	return out
}

// projKey/agentKey/convKey/mailKey are the identity-key extractors the
// generic selection helpers below use for the four keyed lists (projects by
// name, agent strip by alias, thread lists by thread ID, mailbox by thread
// ID) — the zero value of each key type doubles as "nothing selected".
func projKey(p projectSummary) string { return p.Name }
func agentKey(a agentEnriched) string { return a.Alias }
func convKey(c conversationRow) int64 { return c.ID }
func mailKey(r listThreadRow) int64   { return r.ID }

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

// anyRowVisible reports whether any row in rows passes the active filter.
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
// filter when it isn't already visible — to zero ("nothing selected") when
// nothing is visible. A no-op when sel is already visible.
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

// groupConversationRows applies groupThreads' action/reply/rest partition to
// a []conversationRow without losing the OtherProjects annotation —
// groupThreads itself stays the []listThreadRow-only primitive both this and
// the mailbox list (which has no cross-project annotation to carry) share.
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
