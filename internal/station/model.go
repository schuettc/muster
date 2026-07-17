// Package station implements `muster station`, the operator TUI: a
// full-screen Bubble Tea program showing the live agent roster and the bus
// journal feed (threads + composer land in later tasks). Like `muster
// watch`, station never streams — it polls the daemon on a tea.Tick, but the
// poll loop is owned by the Bubble Tea MODEL instead of a bare for-loop, so
// the event journal cursor advances only when the model actually applies an
// events page (see Update's eventsMsg branch) rather than whenever a fetch
// happens to complete.
package station

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/render"
)

// keyMap is station's canonical key vocabulary (spec §5 keys, the subset
// this task wires: Tab focus · j/k move · q quit — send/reply/nudge/filter/
// aliases-toggle land in task 9). bubbles/key gives every binding a single
// named definition instead of a scattered string switch, and is ready to
// feed a bubbles/help view once the composer/nudge keys land.
type keyMap struct {
	Tab, Down, Up, Quit, Enter, Esc, End key.Binding
}

var keys = keyMap{
	Tab:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
	Down:  key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move")),
	Up:    key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move")),
	Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Enter: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Esc:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	// End snaps the feed back to live-follow (spec §5's "End/G snaps back to
	// live") — "G" (shift+g) is the vim-ish alternative, matched as a plain
	// rune keypress like j/k.
	End: key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end/G", "live")),
}

// pane identifies which of the three panes currently has focus, for Tab
// cycling and j/k roster movement.
type pane int

const (
	paneRoster pane = iota
	paneFeed
	paneThreads
	paneCount
)

// Layout knobs. rosterWidth mirrors spec §5's "~30 cols" — there is no
// --roster-width flag in the spec's flag list, so it stays an internal
// constant rather than a knob; defaultRows/eventBacklog bound how much feed
// history the model keeps and shows before a real terminal size arrives.
const (
	rosterWidth  = 32
	defaultRows  = 20
	eventBacklog = 500
)

// initialEventBacklog bounds the cold-start (and bootstrap-retry) backlog
// fetch: sized to the feed pane's visible height (defaultRows) since that's
// all the first screen can show anyway. Like rosterWidth, this isn't one of
// spec §5's named flags, so it's a knob-style constant rather than an
// --flag.
const initialEventBacklog = defaultRows

// threadViewDefaultWidth is the thread view overlay's body-wrap width when
// --width wasn't given (Options.Width == 0) — the overlay doesn't get
// $COLUMNS detection the way render.NewRenderer does, since it's wrapping
// prose rather than aligning table columns.
const threadViewDefaultWidth = 100

// threadViewBodySanitizeWidth bounds display.Sanitize's width cap for a
// thread view entry's body before lipgloss wraps it to the pane width —
// generously large so sanitize's own truncation never fires ahead of the
// real wrap (mirrors render.rawFieldWidth's role for the feed).
const threadViewBodySanitizeWidth = 4096

// agentEnriched is one roster row: the wire agent row plus tmux-live state
// and its session's unread count (spec §5: "per-tuple unread via
// session_unread").
type agentEnriched struct {
	Alias       string
	Project     string
	ModelType   string
	Label       string
	LabelManual bool
	SocketPath  string
	SessionID   string
	Live        bool
	Unread      int
	Action      bool // true when the session's unread includes an action-requested thread
}

// listThreadRow mirrors store.Thread's wire JSON for the threads pane.
type listThreadRow struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	FromAgent  string `json:"from_agent"`
	ToKind     string `json:"to_kind"`
	ToTarget   string `json:"to_target"`
	Subject    string `json:"subject"`
	Status     string `json:"status"`
	Intent     string `json:"intent"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	LastFrom   string `json:"last_from"`
	LastAt     int64  `json:"last_at"`
	EntryCount int    `json:"entry_count"`
}

// Options configures a Model — station.Run's flags, or whatever a test wants
// to fix directly.
type Options struct {
	Interval time.Duration // poll interval (--interval, default 1s)
	Aliases  bool          // show raw aliases instead of labels (--aliases)
	Width    int           // total line budget (--width; 0 = default)
	Alias    string        // this station's own registered alias
}

// Model is the station Bubble Tea model. It owns the event journal cursor —
// Update's eventsMsg branch is the ONLY place it advances, and only after a
// page is actually applied (spec §5 data loop).
type Model struct {
	caller render.Caller
	opts   Options

	cursor       int64 // event journal read cursor; advances only on applied event pages
	bootstrapped bool  // false until the cold-start BACKLOG fetch has applied; gates pollCmd's mode
	pollGen      int64 // current poll generation; bumped per tick, stamped onto each fetch's msg so a stale in-flight fetch from an older tick is discarded rather than applied (Update's eventsMsg case)
	events       []render.EventRow
	feed         *render.Renderer

	agents    []agentEnriched
	rosterIdx int
	labels    map[string]string // alias → current label, shared by the feed and threads panes

	threads        []listThreadRow
	threadSelected int64 // selected thread's ID (0 = none) — PRESERVED BY ID across a poll's regroup, never an index

	// feedFollow/feedTop implement the feed's scroll-lock (spec §5): the feed
	// follows the live tail unless the operator scrolls up, and End/G snaps
	// back to following. feedTop is an absolute index into m.events (not a
	// line count), so appending new events never moves an already-scrolled
	// viewport; applyEvents compensates it when the backlog trim drops events
	// off the front.
	feedFollow bool
	feedTop    int

	// Thread view overlay (Enter on the threads pane opens it; Esc closes).
	// viewEntries is the currently loaded, oldest-first window of
	// viewThreadID's entries; viewOffset is that window's offset within the
	// FULL entry list, so "load older" (pressing k/up at the top of the
	// loaded window) knows exactly what's still missing (spec §5: get_thread
	// {offset,limit} pagination, lazy load older). viewCursor is the index
	// of the topmost highlighted entry within viewEntries.
	viewOpen     bool
	viewThreadID int64
	viewEntries  []threadEntryRow
	viewOffset   int64
	viewCursor   int
	viewLoading  bool

	focus    pane
	status   string
	quitting bool
}

// NewModel constructs a station Model against caller (the daemon-transport
// seam — production wraps internal/client.Call via daemonCaller, tests hand
// in a fake).
func NewModel(caller render.Caller, opts Options) Model {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	return Model{
		caller:     caller,
		opts:       opts,
		feed:       render.NewRenderer(nil, nil, opts.Aliases, false, opts.Width),
		feedFollow: true,
		status:     "connecting…",
	}
}

// Init issues the first poll immediately (so the screen isn't blank for a
// full --interval) and schedules the first tick. Since a fresh Model is not
// yet bootstrapped, that first poll's events fetch is pollCmd's backlog
// branch, not follow — see pollCmd and applyEvents (Finding 1).
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
}

// pollCmd issues the tick's three independent fetches (spec §5 data loop):
// events, list_agents, list_threads. Each yields its own message; none may
// be combined into a shared snapshot before Update sees them, so a failure
// in one can never block or corrupt the others, and no decision is ever
// derived from a mixed tick bundle.
//
// The events fetch's MODE depends on m.bootstrapped (Finding 1): until the
// cold-start backlog fetch has actually applied, every attempt — including
// retries after a failed backlog fetch — stays in BACKLOG mode. The model
// only ever issues a follow-mode after_id=cursor fetch once it has a cursor
// seeded from a real backlog response; it never follows from the implicit
// zero value.
func (m Model) pollCmd() tea.Cmd {
	eventsCmd := fetchEventsCmd(m.caller, m.cursor, m.pollGen)
	if !m.bootstrapped {
		eventsCmd = fetchBacklogEventsCmd(m.caller, initialEventBacklog, m.pollGen)
	}
	return tea.Batch(
		eventsCmd,
		fetchAgentsCmd(m.caller),
		fetchThreadsCmd(m.caller),
	)
}

// Update implements tea.Model. See eventsMsg/agentsMsg/threadsMsg handling
// below for the cursor discipline (spec §5, identical to watch: max_id,
// regression reset, errors → status line + retry, never exit).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		m.pollGen++ // a new tick supersedes any still-in-flight fetch from the previous one (Finding 2)
		return m, tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
	case eventsMsg:
		if msg.gen != m.pollGen {
			return m, nil // stale: a newer tick has already superseded this in-flight fetch
		}
		return m.applyEvents(msg), nil
	case agentsMsg:
		return m.applyAgents(msg), nil
	case threadsMsg:
		return m.applyThreads(msg), nil
	case threadPageMsg:
		return m.applyThreadPage(msg), nil
	case inboxAckMsg:
		return m.applyInboxAck(msg), nil
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The thread view is a modal overlay: while open, it owns every key
	// (Esc closes it, j/k scroll/lazy-load) — Tab/roster/threads movement
	// never leaks through underneath it.
	if m.viewOpen {
		return m.handleThreadViewKey(msg)
	}
	switch {
	case key.Matches(msg, keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, keys.Tab):
		m.focus = (m.focus + 1) % paneCount
		return m, nil
	case key.Matches(msg, keys.Down):
		return m.moveFocused(1), nil
	case key.Matches(msg, keys.Up):
		return m.moveFocused(-1), nil
	case key.Matches(msg, keys.End):
		if m.focus == paneFeed {
			m.feedFollow = true
		}
		return m, nil
	case key.Matches(msg, keys.Enter):
		if m.focus == paneThreads {
			return m.openSelectedThread()
		}
		return m, nil
	}
	return m, nil
}

// moveFocused applies a j/k (delta=+1/-1) move to whichever pane currently
// has focus: the roster cursor, the threads selection (by ID, see
// moveThreadSelection), or the feed scroll position (see scrollFeed).
func (m Model) moveFocused(delta int) Model {
	switch m.focus {
	case paneRoster:
		m.rosterIdx += delta
		if m.rosterIdx < 0 {
			m.rosterIdx = 0
		}
		if n := len(m.agents); m.rosterIdx > n-1 {
			m.rosterIdx = max(n-1, 0)
		}
	case paneThreads:
		m = m.moveThreadSelection(delta)
	case paneFeed:
		m = m.scrollFeed(delta)
	}
	return m
}

// handleThreadViewKey is the thread view overlay's own key vocabulary: Esc
// closes it; k/up scrolls toward older entries, lazily fetching the next
// older get_thread page when the loaded window's top is reached (spec §5);
// j/down scrolls toward newer entries within what's already loaded. `r`
// (reply) is Task 9 — every other key is a no-op rather than falling through
// to the panes underneath.
func (m Model) handleThreadViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Esc):
		m.viewOpen = false
		return m, nil
	case key.Matches(msg, keys.Up):
		if m.viewCursor > 0 {
			m.viewCursor--
			return m, nil
		}
		if m.viewOffset > 0 && !m.viewLoading {
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, 0, m.viewOffset, true)
		}
		return m, nil
	case key.Matches(msg, keys.Down):
		if m.viewCursor < len(m.viewEntries)-1 {
			m.viewCursor++
		}
		return m, nil
	}
	return m, nil
}

// applyEvents is the ONLY place the cursor advances, and only once a page is
// actually applied: a failed fetch (err != nil) leaves the cursor and the
// buffered events untouched, so a dropped/failed poll can never skip events —
// the next successful poll retries in the same mode (backlog stays backlog
// until it succeeds; follow picks up from exactly the same after_id).
//
// msg.backlog (Finding 1) is the cold-start (or bootstrap-retry) case: the
// daemon returns backlog rows NEWEST-first, so they're reversed into the
// oldest-first order the buffer and every follow-mode page use, the cursor
// is seeded straight from max_id (there is no prior cursor to regress
// against), and bootstrapped flips true so every subsequent pollCmd follows
// instead of re-fetching backlog.
//
// Otherwise this is a follow-mode page: a regression (max_id < cursor — the
// DB was replaced) resets the cursor to the new tail without applying the
// (stale) page, exactly like watch.
func (m Model) applyEvents(msg eventsMsg) Model {
	if msg.err != nil {
		what := "events"
		if msg.backlog {
			what = "events backlog"
		}
		m.status = fmt.Sprintf("%s: poll failed, retrying: %v", what, msg.err)
		return m
	}
	if msg.backlog {
		events := make([]render.EventRow, len(msg.page.Events))
		for i, e := range msg.page.Events {
			events[len(events)-1-i] = e
		}
		m.events = events
		m.cursor = msg.page.MaxID
		m.bootstrapped = true
		m.status = ""
		return m
	}
	if msg.page.MaxID < m.cursor {
		m.status = fmt.Sprintf("journal reset (max id %d < cursor %d) — following from the new tail", msg.page.MaxID, m.cursor)
		m.cursor = msg.page.MaxID
		return m
	}
	if len(msg.page.Events) > 0 {
		m.events = append(m.events, msg.page.Events...) // follow mode: oldest-first
		if n := len(m.events); n > eventBacklog {
			trimmed := n - eventBacklog
			m.events = m.events[trimmed:]
			// feedTop is an index into m.events; trimming off the front
			// shifts every index down by `trimmed`, so a scrolled-up
			// viewport (spec §5 scroll-lock) must shift with it or it'd
			// silently jump to different (later) content.
			if !m.feedFollow {
				m.feedTop -= trimmed
				if m.feedTop < 0 {
					m.feedTop = 0
				}
			}
		}
		m.status = ""
	}
	m.cursor = msg.page.MaxID
	return m
}

// applyAgents refreshes the roster and the feed's label map. A failed fetch
// only updates the status line — the roster keeps showing its last-known
// state rather than blanking (spec: "roster/threads failures don't block the
// feed", and by the same principle don't erase themselves either).
func (m Model) applyAgents(msg agentsMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("agents: poll failed, retrying: %v", msg.err)
		return m
	}
	m.agents = msg.rows
	if m.rosterIdx >= len(m.agents) {
		m.rosterIdx = len(m.agents) - 1
	}
	if m.rosterIdx < 0 {
		m.rosterIdx = 0
	}
	labels := make(map[string]string, len(m.agents))
	for _, a := range m.agents {
		labels[a.Alias] = a.Label
	}
	m.feed.SetLabels(labels)
	m.labels = labels
	return m
}

// applyThreads refreshes the threads pane data. A failed fetch only updates
// the status line and never touches the cursor or the roster.
//
// Selection is PRESERVED BY THREAD ID (spec §5): threadSelected holds an ID,
// never an index, so re-grouping (a thread moving between the action/reply/
// rest buckets as its intent or status changes) can never silently move the
// operator's cursor onto a different thread. It's re-pointed only when the
// previously-selected ID is no longer present at all (defaults to the first
// row of the new grouped order).
func (m Model) applyThreads(msg threadsMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("threads: poll failed, retrying: %v", msg.err)
		return m
	}
	m.threads = msg.threads
	grouped := groupThreads(m.threads)
	if len(grouped) == 0 {
		m.threadSelected = 0
		return m
	}
	if indexOfThread(grouped, m.threadSelected) < 0 {
		m.threadSelected = grouped[0].ID
	}
	return m
}

// groupThreads partitions rows into the three spec §5 buckets — action-
// requested pinned first, then reply-requested, then everything else — via a
// STABLE three-way partition: each bucket keeps rows in the order list_threads
// already returned them (updated_at DESC, id DESC), so grouping never
// re-sorts within a bucket, only re-buckets.
func groupThreads(rows []listThreadRow) []listThreadRow {
	var action, reply, rest []listThreadRow
	for _, r := range rows {
		switch r.Intent {
		case "action-requested":
			action = append(action, r)
		case "reply-requested":
			reply = append(reply, r)
		default:
			rest = append(rest, r)
		}
	}
	out := make([]listThreadRow, 0, len(rows))
	out = append(out, action...)
	out = append(out, reply...)
	out = append(out, rest...)
	return out
}

// indexOfThread returns id's position in rows, or -1 if absent.
func indexOfThread(rows []listThreadRow, id int64) int {
	for i, r := range rows {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// moveThreadSelection moves the threads pane's selection by delta (+1/-1)
// within the current grouped order, re-deriving the index from
// threadSelected's ID each time (never a stored index) — so a selection that
// survives a regroup between keystrokes still moves relative to where it
// actually is now, not some stale position.
func (m Model) moveThreadSelection(delta int) Model {
	grouped := groupThreads(m.threads)
	if len(grouped) == 0 {
		return m
	}
	idx := indexOfThread(grouped, m.threadSelected)
	if idx < 0 {
		idx = 0
	} else {
		idx += delta
		if idx < 0 {
			idx = 0
		}
		if idx > len(grouped)-1 {
			idx = len(grouped) - 1
		}
	}
	m.threadSelected = grouped[idx].ID
	return m
}

// openSelectedThread opens the thread view overlay on the currently selected
// thread (spec §5's Enter-to-open): the initial get_thread fetch requests
// the newest threadViewPageSize entries (offset computed from the thread's
// cached entry_count, since GetThread's own response carries no running
// total — see threadViewPageSize's doc). Open-to-acknowledge (spec §5, the
// ONE side-effecting read anywhere in station): if the opened thread's
// to_target is station's OWN registered alias, this also issues exactly one
// get_inbox for that alias — never on selection, focus, or a later poll.
func (m Model) openSelectedThread() (Model, tea.Cmd) {
	idx := indexOfThread(m.threads, m.threadSelected)
	if idx < 0 {
		return m, nil
	}
	row := m.threads[idx]

	limit := int64(threadViewPageSize)
	offset := int64(row.EntryCount) - limit
	if offset < 0 {
		offset = 0
	}

	m.viewOpen = true
	m.viewThreadID = row.ID
	m.viewEntries = nil
	m.viewOffset = offset
	m.viewCursor = 0
	m.viewLoading = true

	cmds := []tea.Cmd{fetchThreadPageCmd(m.caller, row.ID, offset, limit, false)}
	if row.ToTarget == m.opts.Alias {
		cmds = append(cmds, fetchInboxAckCmd(m.caller, m.opts.Alias))
	}
	return m, tea.Batch(cmds...)
}

// applyThreadPage applies one get_thread page. A page that resolves after
// the view moved on to a different thread (or closed) is discarded — msg's
// threadID no longer matches what's on screen. older pages are PREPENDED
// (the lazy "load older" fetch) rather than replacing the loaded window;
// viewCursor advances by the number of newly-prepended entries so the
// previously-topmost (still-visible) entry keeps its highlighted position
// instead of the view jumping.
func (m Model) applyThreadPage(msg threadPageMsg) Model {
	if !m.viewOpen || msg.threadID != m.viewThreadID {
		return m
	}
	if msg.err != nil {
		m.status = fmt.Sprintf("thread: poll failed, retrying: %v", msg.err)
		m.viewLoading = false
		return m
	}
	if msg.older {
		m.viewEntries = append(msg.entries, m.viewEntries...)
		m.viewCursor += len(msg.entries)
	} else {
		m.viewEntries = msg.entries
		m.viewCursor = 0
	}
	m.viewOffset = msg.offset
	m.viewLoading = false
	m.status = ""
	return m
}

// applyInboxAck applies the open-to-acknowledge get_inbox result: success is
// silent (the read already happened; there's nothing further to show), a
// failure lands on the status line like every other poll error.
func (m Model) applyInboxAck(msg inboxAckMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("inbox ack: %v", msg.err)
	}
	return m
}

// scrollFeed applies a j/k (delta=+1/-1) move to the feed's scroll window
// (spec §5's scroll-lock): delta<0 (k/up) scrolls toward older events,
// dropping out of live-follow; delta>0 (j/down) scrolls toward newer events
// and re-enters live-follow once it reaches the tail. feedTop is an absolute
// index into m.events, so it stays correct across new events appending at
// the far end (see applyEvents' backlog-trim compensation).
func (m Model) scrollFeed(delta int) Model {
	maxTop := len(m.events) - defaultRows
	if maxTop < 0 {
		maxTop = 0
	}
	top := m.feedWindowStart() + delta
	if top >= maxTop {
		m.feedFollow = true
		m.feedTop = maxTop
		return m
	}
	if top < 0 {
		top = 0
	}
	m.feedFollow = false
	m.feedTop = top
	return m
}

// feedWindowStart returns the index into m.events the feed pane should start
// rendering from: the live tail while following, or feedTop (clamped to the
// current valid range, in case the buffer shrank) while scrolled up.
func (m Model) feedWindowStart() int {
	maxTop := len(m.events) - defaultRows
	if maxTop < 0 {
		maxTop = 0
	}
	if m.feedFollow {
		return maxTop
	}
	top := m.feedTop
	if top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// dispLabel resolves an alias for display exactly like render.Renderer.disp
// (the station-local peer copy: render's disp/dispTarget are unexported, so
// panes outside the feed re-derive the same alias-fallback rule from the
// labels map applyAgents already builds).
func (m Model) dispLabel(alias string) string {
	if !m.opts.Aliases {
		if l := m.labels[alias]; l != "" {
			return l
		}
	}
	return alias
}

// dispToTarget renders a thread's (to_kind, to_target) pair for display:
// "agent" resolves through dispLabel, "role"/"broadcast" show as-is (they
// have no per-alias label to resolve).
func (m Model) dispToTarget(row listThreadRow) string {
	switch row.ToKind {
	case "agent":
		return m.dispLabel(row.ToTarget)
	case "role":
		return "role:" + row.ToTarget
	case "broadcast":
		return "broadcast"
	default:
		return row.ToTarget
	}
}

// intentRowTag maps a thread's effective intent to the threads pane's tag —
// the station-local peer of render's unexported intentTag, same vocabulary.
func intentRowTag(intent string) string {
	switch intent {
	case "action-requested":
		return "[action]"
	case "reply-requested":
		return "[reply?]"
	case "fyi":
		return "[fyi]"
	default:
		return ""
	}
}

// relativeAge renders the gap between now and atMillis (a ms-epoch
// timestamp) as a short duration ("Ns"/"Nm"/"Nh"/"Nd") for the threads pane's
// "last speaker + age" column. atMillis <= 0 (no last entry yet) renders "".
func relativeAge(now time.Time, atMillis int64) string {
	if atMillis <= 0 {
		return ""
	}
	d := now.Sub(time.UnixMilli(atMillis))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// View implements tea.Model.
func (m Model) View() string {
	if m.viewOpen {
		return m.renderThreadView() + "\n" + m.renderStatus()
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderRoster(),
		lipgloss.JoinVertical(lipgloss.Left, m.renderFeed(), m.renderThreads()),
	)
	return body + "\n" + m.renderStatus()
}

// renderRoster renders the project-grouped agent list: live dot, label
// (alias fallback), and per-session unread count — "!" marks a session whose
// unread includes an action-requested thread.
func (m Model) renderRoster() string {
	var b strings.Builder
	b.WriteString(paneTitle("ROSTER", m.focus == paneRoster) + "\n")

	byProject := map[string][]agentEnriched{}
	for _, a := range m.agents {
		p := a.Project
		if p == "" {
			p = "(none)"
		}
		byProject[p] = append(byProject[p], a)
	}
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	idx := 0
	for _, p := range projects {
		rows := byProject[p]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Alias < rows[j].Alias })
		b.WriteString(p + "\n")
		for _, a := range rows {
			dot := "✗"
			if a.Live {
				dot = "●"
			}
			label := a.Label
			if label == "" {
				label = a.Alias
			}
			line := fmt.Sprintf("%s %s", dot, label)
			if a.Unread > 0 {
				marker := ""
				if a.Action {
					marker = "!"
				}
				line += fmt.Sprintf(" (%d%s)", a.Unread, marker)
			}
			cursorMark := "  "
			if m.focus == paneRoster && idx == m.rosterIdx {
				cursorMark = "> "
			}
			b.WriteString(cursorMark + line + "\n")
			idx++
		}
	}
	return lipgloss.NewStyle().Width(rosterWidth).Render(strings.TrimRight(b.String(), "\n"))
}

// renderFeed renders the journal tail with render.Renderer verbatim — the
// same WHO arrows, labels, and width-capped WHAT the CLI's events/watch
// commands print.
func (m Model) renderFeed() string {
	var b bytes.Buffer
	b.WriteString(paneTitle("FEED", m.focus == paneFeed) + "\n")
	m.feed.Header(&b)
	start := m.feedWindowStart()
	end := start + defaultRows
	if end > len(m.events) {
		end = len(m.events)
	}
	for _, e := range m.events[start:end] {
		m.feed.Line(&b, e)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderThreads renders the threads pane (spec §5): action-requested pinned
// first, then reply-requested, then the rest — see groupThreads — each row
// showing its id, intent tag, participants, last speaker + relative age, and
// sanitized subject. The selection marker follows threadSelected by ID, so
// it always lands on the right row regardless of this poll's grouping.
func (m Model) renderThreads() string {
	var b strings.Builder
	b.WriteString(paneTitle("THREADS", m.focus == paneThreads) + "\n")
	for _, row := range groupThreads(m.threads) {
		b.WriteString(m.renderThreadRow(row) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderThreadRow renders one threads-pane row.
func (m Model) renderThreadRow(row listThreadRow) string {
	marker := "  "
	if row.ID == m.threadSelected {
		marker = "> "
	}
	tag := intentRowTag(row.Intent)
	if tag != "" {
		tag = " " + tag
	}
	participants := fmt.Sprintf("%s → %s", m.dispLabel(row.FromAgent), m.dispToTarget(row))
	last := m.dispLabel(row.LastFrom)
	age := relativeAge(time.Now(), row.LastAt)
	subject := display.Sanitize(row.Subject, 200)
	return fmt.Sprintf("%s#%d%s %s | %s %s | %s", marker, row.ID, tag, participants, last, age, subject)
}

// renderThreadView renders the thread view overlay (spec §5): entries
// oldest-first, body text sanitized and wrapped to the pane width. A
// "load older" hint shows while more entries remain above the loaded window
// (viewOffset > 0).
func (m Model) renderThreadView() string {
	var b strings.Builder
	b.WriteString(paneTitle("THREAD #"+strconv.FormatInt(m.viewThreadID, 10), true) + "\n")
	if m.viewOffset > 0 {
		b.WriteString("↑ more above — k/↑ to load older\n")
	}
	width := m.opts.Width
	if width <= 0 {
		width = threadViewDefaultWidth
	}
	for i, e := range m.viewEntries {
		marker := "  "
		if i == m.viewCursor {
			marker = "> "
		}
		ts := time.UnixMilli(e.CreatedAt).Format("15:04:05")
		header := fmt.Sprintf("%s%s %s", marker, ts, m.dispLabel(e.FromAgent))
		body := lipgloss.NewStyle().Width(width).Render(display.Sanitize(e.Body, threadViewBodySanitizeWidth))
		b.WriteString(header + "\n" + body + "\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) renderStatus() string {
	if m.status != "" {
		return m.status
	}
	if m.viewOpen {
		return fmt.Sprintf("%s scroll · %s close", keys.Down.Help().Key, keys.Esc.Help().Key)
	}
	return fmt.Sprintf("%s focus · %s/%s move · %s open · %s live · %s quit",
		keys.Tab.Help().Key, keys.Down.Help().Key, keys.Up.Help().Key,
		keys.Enter.Help().Key, keys.End.Help().Key, keys.Quit.Help().Key)
}

func paneTitle(name string, focused bool) string {
	if focused {
		return "» " + name
	}
	return "  " + name
}
