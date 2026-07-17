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
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/render"
)

// keyMap is station's canonical key vocabulary (spec §5 keys, the subset
// this task wires: Tab focus · j/k move · q quit — send/reply/nudge/filter/
// aliases-toggle land in task 9). bubbles/key gives every binding a single
// named definition instead of a scattered string switch, and is ready to
// feed a bubbles/help view once the composer/nudge keys land.
type keyMap struct {
	Tab, Down, Up, Quit key.Binding
}

var keys = keyMap{
	Tab:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
	Down: key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move")),
	Up:   key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move")),
	Quit: key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
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

// listThreadRow mirrors store.Thread's wire JSON for the threads pane
// (placeholder this task; populated properly in task 8).
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

	threads []listThreadRow

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
		caller: caller,
		opts:   opts,
		feed:   render.NewRenderer(nil, nil, opts.Aliases, false, opts.Width),
		status: "connecting…",
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
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, keys.Tab):
		m.focus = (m.focus + 1) % paneCount
		return m, nil
	case key.Matches(msg, keys.Down):
		if m.focus == paneRoster && m.rosterIdx < len(m.agents)-1 {
			m.rosterIdx++
		}
		return m, nil
	case key.Matches(msg, keys.Up):
		if m.focus == paneRoster && m.rosterIdx > 0 {
			m.rosterIdx--
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
			m.events = m.events[n-eventBacklog:]
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
	return m
}

// applyThreads refreshes the (placeholder, task 8) threads pane data. A
// failed fetch only updates the status line and never touches the cursor or
// the roster.
func (m Model) applyThreads(msg threadsMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("threads: poll failed, retrying: %v", msg.err)
		return m
	}
	m.threads = msg.threads
	return m
}

// View implements tea.Model.
func (m Model) View() string {
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
	start := 0
	if n := len(m.events); n > defaultRows {
		start = n - defaultRows
	}
	for _, e := range m.events[start:] {
		m.feed.Line(&b, e)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderThreads is the task-8 placeholder: the pane exists (focusable via
// Tab) but shows no thread data yet.
func (m Model) renderThreads() string {
	return paneTitle("THREADS", m.focus == paneThreads) + "\n(threads — coming in task 8)"
}

func (m Model) renderStatus() string {
	if m.status != "" {
		return m.status
	}
	return fmt.Sprintf("%s focus · %s/%s move · %s quit",
		keys.Tab.Help().Key, keys.Down.Help().Key, keys.Up.Help().Key, keys.Quit.Help().Key)
}

func paneTitle(name string, focused bool) string {
	if focused {
		return "» " + name
	}
	return "  " + name
}
