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
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/schuettc/muster/internal/display"
	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/render"
)

// keyMap is station's canonical key vocabulary (spec §5 keys): Tab focus ·
// j/k move · Enter open · Esc back/cancel · End/G live (feed) or load-tail
// (thread view) · s send · r reply (thread view/composer) · n nudge ·
// CycleIntent (composer, F/R/A) · / filter · a aliases toggle · q quit.
// bubbles/key gives every binding a single named definition instead of a
// scattered string switch.
type keyMap struct {
	Tab, Down, Up, Quit, Enter, Esc, End             key.Binding
	Send, Reply, Nudge, Filter, Aliases, CycleIntent key.Binding
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
	// rune keypress like j/k. Inside the thread view overlay, the SAME
	// binding instead fetches the tail when newer entries exist (see
	// viewNewerCount/handleThreadViewKey).
	End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end/G", "live")),
	Send:        key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send")),
	Reply:       key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reply")),
	Nudge:       key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Aliases:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "aliases")),
	CycleIntent: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "intent")),
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

// Layout knobs. rosterWidth (spec §5's "~30/34 cols" left column) lives in
// layout.go now, alongside the rest of the box math it's part of.
// defaultRows/eventBacklog bound how much feed history the model keeps and
// shows before a real terminal size arrives — defaultRows in particular is
// also the FALLBACK feed/threads pane row count layout.go's
// fallbackTermHeight is derived from, so an unsized caller (no
// tea.WindowSizeMsg yet, including every test that never sends one) sees
// exactly the same amount of content it always did.
const (
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

// threadViewRows bounds the thread view overlay's visible WRAPPED-line
// budget (spec §5 carried-over fix: render windowing) — mirrors defaultRows'
// role for the feed pane: a fixed knob-style constant rather than a flag,
// since the overlay has no more dynamic terminal-size wiring than the feed
// pane does today.
const threadViewRows = defaultRows

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
	Nudger   nudger        // test seam for the 'n' nudge action; nil (default) uses nudge.TmuxNudger{} (real tmux)
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
	// of the topmost highlighted entry within viewEntries. viewTotal is the
	// live entry count the last get_thread page carried (spec §5
	// carried-over fix: the newest-entries gap) — used both to self-correct
	// a stale-entry_count offset guess on open and to detect newer entries
	// that arrived after the window was loaded (viewNewerCount). viewGen is
	// bumped each time a thread is (re)opened (openSelectedThread) so a
	// threadPageMsg left over from a PREVIOUS opening of the SAME thread ID
	// is discarded rather than applied (spec §5 carried-over fix:
	// threadPageMsg staleness — mirrors pollGen's role).
	viewOpen     bool
	viewThreadID int64
	viewEntries  []threadEntryRow
	viewOffset   int64
	viewTotal    int64
	viewCursor   int
	viewLoading  bool
	viewGen      int64

	// composer implements the bottom-line composer (spec §5: 's' send via a
	// roster-filtered target picker, 'r' reply to the open thread view,
	// intent cycled F/R/A, Enter submits, Esc cancels).
	composer composerState

	// nudgeConfirmAlias is "" (no pending confirmation) or the alias 'n' is
	// asking "nudge <label>? y/n" about (spec §5) — handleNudgeConfirmKey
	// owns every key while it's set. nudger is the send-keys seam (DI'd via
	// Options.Nudger; tests inject a fake so nudgeCmd never shells to tmux).
	nudgeConfirmAlias string
	nudger            nudger

	// filter implements '/' (spec §5): a substring filter over one pane's
	// RENDERED row text. For the feed it's a pure display-time skip. For the
	// roster and threads panes it's ALSO selection-aware (spec §5 carried-over
	// fix: filter/selection desync): j/k walks only visible rows
	// (moveRosterSelection/moveThreadSelection), and an action key (n/Enter)
	// whose stored selection currently points at a filtered-out row only
	// corrects that selection (snapRosterSelection/snapThreadSelection) rather
	// than firing on a row the operator was never shown — see
	// rosterRowVisible/threadRowVisible, the one predicate render, movement,
	// and action lookup all share.
	filter filterState

	focus    pane
	status   string
	quitting bool

	// termWidth/termHeight are the last tea.WindowSizeMsg the program has
	// seen (0,0 before the first one arrives) — the ONLY inputs layout()
	// needs to size every pane box (spec §5 layout item 7: "respect
	// tea.WindowSizeMsg for all box math"). Purely a rendering input: no
	// data/keys/polling decision anywhere in Update ever reads these.
	termWidth  int
	termHeight int
}

// composerPhase is the composer's own little state machine: closed (no
// composer on screen), picking a target (the 's' roster-filtered picker,
// before the body input), or editing the body (both 's' after a target is
// picked, and 'r' — which skips straight here, its target is the open
// thread).
type composerPhase int

const (
	composerClosed composerPhase = iota
	composerPickingTarget
	composerEditingBody
)

// composerKind distinguishes the composer's two submit ops: send_message
// ('s') vs reply ('r') — see submitComposer/actions.go's sendMessageCmd and
// replyCmd.
type composerKind int

const (
	composerKindSend composerKind = iota
	composerKindReply
)

// composerState is the Model's composer sub-state (see Model.composer).
type composerState struct {
	phase     composerPhase
	kind      composerKind
	intent    intentState
	target    string          // resolved send target alias (composerKindSend)
	threadID  int64           // reply target thread (composerKindReply)
	filter    textinput.Model // target-picker's filter field (composerKindSend only)
	pickerIdx int
	input     textinput.Model // body input
	err       string          // op error, shown alongside the composer rather than crashing (spec §5)
}

// intentState is the composer's F/R/A cycle (spec §5: "--intent cycled with
// a keystroke (F/R/A indicator)").
type intentState int

const (
	intentFYI intentState = iota
	intentReplyRequested
	intentActionRequested
)

// next cycles F → R → A → F.
func (i intentState) next() intentState { return (i + 1) % 3 }

// tag renders the composer's F/R/A indicator letter.
func (i intentState) tag() string {
	switch i {
	case intentReplyRequested:
		return "R"
	case intentActionRequested:
		return "A"
	default:
		return "F"
	}
}

// wire renders the intent's send_message wire value.
func (i intentState) wire() string {
	switch i {
	case intentReplyRequested:
		return "reply-requested"
	case intentActionRequested:
		return "action-requested"
	default:
		return "fyi"
	}
}

// filterState is the Model's '/' filter sub-state (see Model.filter):
// pane names which pane query applies to (set when '/' is pressed); query is
// live-updated as the operator types; editing gates whether handleFilterKey
// currently owns the keys (Enter stops editing but leaves query applied;
// Esc clears the filter entirely).
type filterState struct {
	pane    pane
	query   string
	editing bool
	input   textinput.Model
}

// NewModel constructs a station Model against caller (the daemon-transport
// seam — production wraps internal/client.Call via daemonCaller, tests hand
// in a fake).
func NewModel(caller render.Caller, opts Options) Model {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	n := opts.Nudger
	if n == nil {
		n = nudge.TmuxNudger{} // production default: real tmux send-keys
	}
	return Model{
		caller:     caller,
		opts:       opts,
		nudger:     n,
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
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		return m, nil
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
		return m.applyThreadPage(msg)
	case inboxAckMsg:
		return m.applyInboxAck(msg), nil
	case composerSentMsg:
		return m.applyComposerSent(msg), nil
	case nudgeResultMsg:
		return m.applyNudgeResult(msg), nil
	}
	return m, nil
}

// handleKey routes a keypress to whichever modal owns the keyboard right
// now, innermost first: the composer (spec §5's bottom-line send/reply),
// then the nudge y/n confirmation, then the '/' filter's own edit box, then
// the thread view overlay — each of these owns EVERY key while active, none
// leaking through to the panes underneath. Only once none of them are active
// does a keypress reach the base pane vocabulary.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.composer.phase != composerClosed:
		return m.handleComposerKey(msg)
	case m.nudgeConfirmAlias != "":
		return m.handleNudgeConfirmKey(msg)
	case m.filter.editing:
		return m.handleFilterKey(msg)
	case m.viewOpen:
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
	case key.Matches(msg, keys.Send):
		return m.openComposerSend(), nil
	case key.Matches(msg, keys.Nudge):
		if m.focus == paneRoster {
			m = m.handleNudgeKey()
		}
		return m, nil
	case key.Matches(msg, keys.Filter):
		return m.openFilter(), nil
	case key.Matches(msg, keys.Aliases):
		m.opts.Aliases = !m.opts.Aliases
		m.feed.SetAliases(m.opts.Aliases)
		return m, nil
	}
	return m, nil
}

// rosterRowVisible reports whether a passes the roster pane's active '/'
// filter (spec §5 carried-over fix: filter/selection desync) — true always
// when the roster isn't currently filtered. This is the ONE predicate
// renderRoster, moveRosterSelection, and handleNudgeKey all resolve through,
// so they can never disagree about which rows are visible or selected.
func (m Model) rosterRowVisible(a agentEnriched) bool {
	q, filtering := m.filterActiveFor(paneRoster)
	if !filtering {
		return true
	}
	return containsFold(m.renderRosterRow(a), q)
}

// rosterSelectionVisible reports whether m.rosterIdx currently points at a
// row visible under the roster's active filter (rows in order, rosterOrder's
// order — the SAME order renderRoster walks).
func (m Model) rosterSelectionVisible(order []agentEnriched) bool {
	if m.rosterIdx < 0 || m.rosterIdx >= len(order) {
		return false
	}
	return m.rosterRowVisible(order[m.rosterIdx])
}

// snapRosterSelection corrects m.rosterIdx to the first row visible under
// the active filter, when the current selection isn't visible (spec §5
// carried-over fix: filter/selection desync) — to -1 ("no selection", the
// same sentinel handleNudgeKey/moveRosterSelection already treat as "nothing
// to act on") when nothing at all is visible. A no-op when the current
// selection is already visible, so moveRosterSelection can call it
// unconditionally without disturbing an already-valid position.
func (m Model) snapRosterSelection(order []agentEnriched) Model {
	if m.rosterSelectionVisible(order) {
		return m
	}
	for i, a := range order {
		if m.rosterRowVisible(a) {
			m.rosterIdx = i
			return m
		}
	}
	m.rosterIdx = -1
	return m
}

// visibleRosterIndices returns order's indices whose row passes the active
// filter, in order — moveRosterSelection's walk space.
func (m Model) visibleRosterIndices(order []agentEnriched) []int {
	out := make([]int, 0, len(order))
	for i, a := range order {
		if m.rosterRowVisible(a) {
			out = append(out, i)
		}
	}
	return out
}

// handleNudgeKey implements 'n' on the roster pane (spec §5's "nudge
// <label>? y/n" gate). Two guards run before it opens the confirm gate
// (spec §5 carried-over fixes):
//
//   - filter/selection desync: if the stored selection isn't currently
//     VISIBLE under an active '/' filter, this keypress only corrects it
//     (snapRosterSelection) rather than confirming a nudge against a row the
//     operator was never shown — the next 'n' press (now landing on a
//     visible, marked row) behaves normally.
//   - self-nudge: station's own row appears in the roster: nudging it would
//     tmux send-keys INTO station's own pane, whose quit key is literally
//     'q' (part of the nudge text) — so selecting station's own alias shows
//     a status note instead of ever entering the confirm state.
func (m Model) handleNudgeKey() Model {
	order := m.rosterOrder()
	if !m.rosterSelectionVisible(order) {
		m = m.snapRosterSelection(order)
		if m.rosterIdx < 0 {
			m.status = "no agent visible — adjust or clear the filter"
		}
		return m
	}
	a := order[m.rosterIdx]
	if a.Alias == m.opts.Alias {
		m.status = "that's you — can't nudge yourself"
		return m
	}
	m.nudgeConfirmAlias = a.Alias
	return m
}

// handleNudgeConfirmKey owns every key while nudgeConfirmAlias is pending
// (spec §5: "nudge <label>? y/n"): 'y' confirms and issues the nudge;
// anything else (including 'n' itself, and Esc) cancels without nudging.
func (m Model) handleNudgeConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	alias := m.nudgeConfirmAlias
	m.nudgeConfirmAlias = ""
	if msg.String() != "y" {
		return m, nil
	}
	m.status = fmt.Sprintf("nudging %s…", m.dispLabel(alias))
	return m, nudgeCmd(m.caller, m.nudger, alias)
}

// applyNudgeResult applies one nudgeCmd outcome to the status line — success
// and failure both land there (spec §5: nudge errors never crash).
func (m Model) applyNudgeResult(msg nudgeResultMsg) Model {
	label := m.dispLabel(msg.alias)
	if msg.err != nil {
		m.status = fmt.Sprintf("nudge %s failed: %v", label, msg.err)
		return m
	}
	word := "typed"
	if msg.submitted {
		word = "submitted"
	}
	m.status = fmt.Sprintf("nudged %s (%s)", label, word)
	return m
}

// openFilter opens '/' editing for the currently focused pane (spec §5):
// re-opening the SAME pane's filter preserves its existing query for
// editing; switching to a different pane starts blank (that pane had no
// filter of its own before now).
func (m Model) openFilter() Model {
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Focus()
	if m.filter.pane == m.focus {
		ti.SetValue(m.filter.query)
	}
	m.filter = filterState{pane: m.focus, query: ti.Value(), editing: true, input: ti}
	return m
}

// handleFilterKey owns every key while the '/' filter box is being edited:
// Esc clears the filter entirely (back to showing everything); Enter stops
// editing but leaves the last-typed query applied; anything else is handed
// to the textinput, and the live query is synced from it on every keystroke
// so filtering updates as the operator types.
func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.filter = filterState{}
		return m, nil
	case tea.KeyEnter:
		m.filter.editing = false
		return m, nil
	}
	var cmd tea.Cmd
	m.filter.input, cmd = m.filter.input.Update(msg)
	m.filter.query = m.filter.input.Value()
	return m, cmd
}

// filterActiveFor reports p's active filter query, if any (spec §5's '/':
// "substring over rendered row text"). Purely a rendering-time skip — it
// never touches selection/cursor state, so a filtered-out selected row just
// shows no cursor mark rather than the cursor silently jumping elsewhere.
func (m Model) filterActiveFor(p pane) (string, bool) {
	if m.filter.pane != p || m.filter.query == "" {
		return "", false
	}
	return m.filter.query, true
}

// containsFold reports whether s contains substr, case-insensitively.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// openComposerSend opens the 's' composer: the roster-filtered target picker
// phase (spec §5), before the body input.
func (m Model) openComposerSend() Model {
	ti := textinput.New()
	ti.Placeholder = "filter roster (label or alias)…"
	ti.Focus()
	m.composer = composerState{phase: composerPickingTarget, kind: composerKindSend, filter: ti}
	return m
}

// openComposerReply opens the 'r' composer straight into body-editing (spec
// §5: "r in thread view replies to that thread") — no target picker, the
// target is the thread already open.
func (m Model) openComposerReply(threadID int64) Model {
	ti := textinput.New()
	ti.Placeholder = "reply…"
	ti.Focus()
	m.composer = composerState{phase: composerEditingBody, kind: composerKindReply, threadID: threadID, input: ti}
	return m
}

// composerCandidates returns the roster rows matching the target picker's
// current filter text (label or alias substring, spec §5), excluding
// station's own alias (it can't message itself).
func (m Model) composerCandidates() []agentEnriched {
	q := strings.ToLower(strings.TrimSpace(m.composer.filter.Value()))
	var out []agentEnriched
	for _, a := range m.agents {
		if a.Alias == m.opts.Alias {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(a.Alias), q) || strings.Contains(strings.ToLower(m.dispLabel(a.Alias)), q) {
			out = append(out, a)
		}
	}
	return out
}

// handleComposerKey routes to whichever composer phase is active.
func (m Model) handleComposerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.composer.phase {
	case composerPickingTarget:
		return m.handleComposerPickerKey(msg)
	case composerEditingBody:
		return m.handleComposerBodyKey(msg)
	}
	return m, nil
}

// handleComposerPickerKey owns the keys during the 's' target-picker phase:
// Esc cancels the whole composer; Up/Down move the highlighted candidate;
// Enter selects it and advances to body-editing; every other key is handed
// to the filter textinput, narrowing the candidate list live.
func (m Model) handleComposerPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.composer = composerState{}
		return m, nil
	case tea.KeyEnter:
		cands := m.composerCandidates()
		if len(cands) == 0 {
			return m, nil
		}
		idx := m.composer.pickerIdx
		if idx < 0 || idx >= len(cands) {
			idx = 0
		}
		m.composer.target = cands[idx].Alias
		m.composer.phase = composerEditingBody
		ti := textinput.New()
		ti.Placeholder = "message…"
		ti.Focus()
		m.composer.input = ti
		return m, nil
	case tea.KeyUp:
		if m.composer.pickerIdx > 0 {
			m.composer.pickerIdx--
		}
		return m, nil
	case tea.KeyDown:
		if n := len(m.composerCandidates()); m.composer.pickerIdx < n-1 {
			m.composer.pickerIdx++
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.composer.filter, cmd = m.composer.filter.Update(msg)
	if n := len(m.composerCandidates()); m.composer.pickerIdx >= n {
		m.composer.pickerIdx = max(n-1, 0)
	}
	return m, cmd
}

// handleComposerBodyKey owns the keys during body-editing (both composer
// kinds): Esc cancels; Enter submits (submitComposer); CycleIntent (tab)
// advances the F/R/A indicator; everything else is handed to the body
// textinput.
func (m Model) handleComposerBodyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Type == tea.KeyEsc:
		m.composer = composerState{}
		return m, nil
	case msg.Type == tea.KeyEnter:
		return m.submitComposer()
	case key.Matches(msg, keys.CycleIntent):
		m.composer.intent = m.composer.intent.next()
		return m, nil
	}
	var cmd tea.Cmd
	m.composer.input, cmd = m.composer.input.Update(msg)
	return m, cmd
}

// submitComposer sends the composer's body via the same send_message/reply
// ops the CLI uses (spec §5), closing the composer immediately (op errors —
// applyComposerSent — land on the status line, never leaving the operator
// stuck re-typing into a composer whose op already fired).
func (m Model) submitComposer() (Model, tea.Cmd) {
	body := strings.TrimSpace(m.composer.input.Value())
	if body == "" {
		m.composer.err = "cannot send an empty message"
		return m, nil
	}
	from := m.opts.Alias
	kind := m.composer.kind
	target := m.composer.target
	threadID := m.composer.threadID
	intentWire := m.composer.intent.wire()
	label := m.dispLabel(target)
	m.composer = composerState{}
	switch kind {
	case composerKindReply:
		m.status = fmt.Sprintf("replying on #%d…", threadID)
		return m, replyCmd(m.caller, from, threadID, body)
	default: // composerKindSend
		m.status = fmt.Sprintf("sending to %s…", label)
		return m, sendMessageCmd(m.caller, from, target, body, intentWire)
	}
}

// applyComposerSent applies one composer submit's outcome to the status
// line (spec §5: "errors land in the status line, never crash").
func (m Model) applyComposerSent(msg composerSentMsg) Model {
	verb := "sent"
	if msg.kind == composerKindReply {
		verb = "reply sent"
	}
	if msg.err != nil {
		m.status = fmt.Sprintf("%s failed: %v", verb, msg.err)
		return m
	}
	m.status = verb
	return m
}

// moveFocused applies a j/k (delta=+1/-1) move to whichever pane currently
// has focus: the roster cursor (see moveRosterSelection), the threads
// selection (by ID, see moveThreadSelection), or the feed scroll position
// (see scrollFeed).
func (m Model) moveFocused(delta int) Model {
	switch m.focus {
	case paneRoster:
		m = m.moveRosterSelection(delta)
	case paneThreads:
		m = m.moveThreadSelection(delta)
	case paneFeed:
		m = m.scrollFeed(delta)
	}
	return m
}

// moveRosterSelection applies a j/k (delta=+1/-1) move to the roster cursor,
// walking only rows visible under the roster's active '/' filter (spec §5
// carried-over fix: filter/selection desync) — snapping first
// (snapRosterSelection) so a move starting from a filtered-out selection
// lands relative to the nearest visible row rather than an invisible one.
func (m Model) moveRosterSelection(delta int) Model {
	order := m.rosterOrder()
	m = m.snapRosterSelection(order)
	visible := m.visibleRosterIndices(order)
	if len(visible) == 0 {
		return m
	}
	pos := indexOfInt(visible, m.rosterIdx)
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
	m.rosterIdx = visible[pos]
	return m
}

// handleThreadViewKey is the thread view overlay's own key vocabulary: Esc
// closes it; k/up scrolls toward older entries, lazily fetching the next
// older get_thread page when the loaded window's top is reached (spec §5);
// j/down scrolls toward newer entries within what's already loaded; `r`
// opens the composer as a reply to this thread; End/G fetches the tail when
// newer entries have arrived since this window loaded (viewNewerCount, spec
// §5 carried-over fix: the newest-entries gap) — every other key is a no-op
// rather than falling through to the panes underneath.
func (m Model) handleThreadViewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Esc):
		m.viewOpen = false
		return m, nil
	case key.Matches(msg, keys.Reply):
		return m.openComposerReply(m.viewThreadID), nil
	case key.Matches(msg, keys.End):
		if n := m.viewNewerCount(); n > 0 && !m.viewLoading {
			limit := int64(threadViewPageSize)
			offset := m.viewTotal + n - limit
			if offset < 0 {
				offset = 0
			}
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, offset, limit, false, false, m.viewGen)
		}
		return m, nil
	case key.Matches(msg, keys.Up):
		if m.viewCursor > 0 {
			m.viewCursor--
			return m, nil
		}
		if m.viewOffset > 0 && !m.viewLoading {
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, 0, m.viewOffset, true, false, m.viewGen)
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

// viewNewerCount returns how many entries exist beyond the currently loaded
// tail, per the freshest list_threads poll's entry_count for this thread
// (spec §5 carried-over fix: the newest-entries gap) — 0 while the initial
// page hasn't landed yet, or once nothing is known to be missing.
func (m Model) viewNewerCount() int64 {
	if m.viewLoading || len(m.viewEntries) == 0 {
		return 0
	}
	idx := indexOfThread(m.threads, m.viewThreadID)
	if idx < 0 {
		return 0
	}
	n := int64(m.threads[idx].EntryCount) - m.viewTotal
	if n < 0 {
		return 0
	}
	return n
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

// indexOfInt returns needle's position in haystack, or -1 if absent —
// moveRosterSelection's index-space counterpart to indexOfThread.
func indexOfInt(haystack []int, needle int) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

// threadRowVisible reports whether row passes the threads pane's active '/'
// filter (spec §5 carried-over fix: filter/selection desync) — true always
// when the threads pane isn't currently filtered. This is the ONE predicate
// renderThreads, moveThreadSelection, and openSelectedThread all resolve
// through, so they can never disagree about which rows are visible or
// selected.
func (m Model) threadRowVisible(row listThreadRow) bool {
	q, filtering := m.filterActiveFor(paneThreads)
	if !filtering {
		return true
	}
	return containsFold(m.renderThreadRow(row), q)
}

// visibleGroupedThreads returns groupThreads' order filtered down to rows
// visible under the active filter — moveThreadSelection's walk space.
func (m Model) visibleGroupedThreads() []listThreadRow {
	grouped := groupThreads(m.threads)
	out := make([]listThreadRow, 0, len(grouped))
	for _, r := range grouped {
		if m.threadRowVisible(r) {
			out = append(out, r)
		}
	}
	return out
}

// threadSelectionVisible reports whether m.threadSelected currently points
// at a row visible under the threads pane's active filter.
func (m Model) threadSelectionVisible() bool {
	grouped := groupThreads(m.threads)
	idx := indexOfThread(grouped, m.threadSelected)
	if idx < 0 {
		return false
	}
	return m.threadRowVisible(grouped[idx])
}

// snapThreadSelection corrects m.threadSelected to the first row visible
// under the active filter, when the current selection isn't visible (spec §5
// carried-over fix: filter/selection desync) — to 0 ("no selection", mirrors
// applyThreads' own "nothing left" sentinel) when nothing at all is visible.
// A no-op when the current selection is already visible, so
// moveThreadSelection can call it unconditionally without disturbing an
// already-valid position.
func (m Model) snapThreadSelection() Model {
	if m.threadSelectionVisible() {
		return m
	}
	for _, r := range groupThreads(m.threads) {
		if m.threadRowVisible(r) {
			m.threadSelected = r.ID
			return m
		}
	}
	m.threadSelected = 0
	return m
}

// moveThreadSelection moves the threads pane's selection by delta (+1/-1)
// within the VISIBLE grouped order (spec §5 carried-over fix: filter/
// selection desync — j/k walks only rows visible under the active '/'
// filter), re-deriving the index from threadSelected's ID each time (never a
// stored index) — so a selection that survives a regroup between keystrokes
// still moves relative to where it actually is now, not some stale position.
// Snaps first (snapThreadSelection) so a move starting from a filtered-out
// selection lands relative to the nearest visible row rather than an
// invisible one.
func (m Model) moveThreadSelection(delta int) Model {
	m = m.snapThreadSelection()
	visible := m.visibleGroupedThreads()
	if len(visible) == 0 {
		return m
	}
	idx := indexOfThread(visible, m.threadSelected)
	if idx < 0 {
		idx = 0
	} else {
		idx += delta
		if idx < 0 {
			idx = 0
		}
		if idx > len(visible)-1 {
			idx = len(visible) - 1
		}
	}
	m.threadSelected = visible[idx].ID
	return m
}

// openSelectedThread opens the thread view overlay on the currently selected
// thread (spec §5's Enter-to-open): the initial get_thread fetch requests
// the newest threadViewPageSize entries, offset computed from the thread's
// cached entry_count — a best-effort guess that applyThreadPage self-corrects
// once the response's own LIVE total lands, if entry_count was stale (spec
// §5 carried-over fix: the newest-entries gap). viewGen is bumped so a page
// left over from a previous opening of this SAME thread ID never applies
// (spec §5 carried-over fix: threadPageMsg staleness). Open-to-acknowledge
// (spec §5, the ONE side-effecting read anywhere in station): if the opened
// thread's to_target is station's OWN registered alias, this also issues
// exactly one get_inbox for that alias — never on selection, focus, or a
// later poll.
//
// Filter/selection desync guard (spec §5 carried-over fix): if the stored
// selection isn't currently VISIBLE under an active '/' filter, Enter only
// corrects it (snapThreadSelection) rather than opening — and firing
// open-to-acknowledge's get_inbox on — a thread the operator was never
// shown. The next Enter press (now landing on a visible, marked row) opens
// normally.
func (m Model) openSelectedThread() (Model, tea.Cmd) {
	if !m.threadSelectionVisible() {
		m = m.snapThreadSelection()
		if m.threadSelected == 0 {
			m.status = "no thread visible — adjust or clear the filter"
		}
		return m, nil
	}
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
	m.viewTotal = 0
	m.viewCursor = 0
	m.viewLoading = true
	m.viewGen++

	cmds := []tea.Cmd{fetchThreadPageCmd(m.caller, row.ID, offset, limit, false, false, m.viewGen)}
	if row.ToTarget == m.opts.Alias {
		cmds = append(cmds, fetchInboxAckCmd(m.caller, m.opts.Alias))
	}
	return m, tea.Batch(cmds...)
}

// applyThreadPage applies one get_thread page. A page that resolves after
// the view moved on to a different thread (or closed), OR that belongs to a
// PREVIOUS opening of the very same thread ID (msg.gen != m.viewGen — spec §5
// carried-over fix: threadPageMsg staleness, mirrors pollGen), is discarded.
// older pages are PREPENDED (the lazy "load older" fetch) rather than
// replacing the loaded window; viewCursor advances by the number of
// newly-prepended entries so the previously-topmost (still-visible) entry
// keeps its highlighted position instead of the view jumping.
//
// The newest-entries gap (spec §5 carried-over fix): a non-older page's live
// total can reveal that this fetch's offset guess (from a stale cached
// entry_count) undershot the true tail — total > offset+len(entries) means
// there are more entries beyond this window that should have been included.
// When that happens, issue exactly ONE corrective re-fetch at the corrected
// offset (msg.corrected on the correction's own response prevents this from
// ever chaining into a second correction).
func (m Model) applyThreadPage(msg threadPageMsg) (Model, tea.Cmd) {
	if !m.viewOpen || msg.threadID != m.viewThreadID || msg.gen != m.viewGen {
		return m, nil
	}
	if msg.err != nil {
		m.status = fmt.Sprintf("thread: poll failed, retrying: %v", msg.err)
		m.viewLoading = false
		return m, nil
	}
	if msg.older {
		m.viewEntries = append(msg.entries, m.viewEntries...)
		m.viewCursor += len(msg.entries)
	} else {
		m.viewEntries = msg.entries
		m.viewCursor = 0
	}
	m.viewOffset = msg.offset
	m.viewTotal = msg.total
	m.viewLoading = false
	m.status = ""

	if !msg.older && !msg.corrected {
		limit := int64(len(msg.entries))
		if limit > 0 && msg.total > msg.offset+limit {
			wantOffset := msg.total - limit
			if wantOffset < 0 {
				wantOffset = 0
			}
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, wantOffset, limit, false, true, m.viewGen)
		}
	}
	return m, nil
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
	maxTop := len(m.events) - m.feedContentRows()
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

// feedContentRows is the feed pane's visible EVENT-line budget (excluding
// its own header row) — layout()'s feedH minus border rows minus the header
// row, so the feed's scroll math (scrollFeed/feedWindowStart) and its
// actual rendered row count (renderFeedBox) can never disagree about how
// many events fit. Before a real tea.WindowSizeMsg arrives, layout() falls
// back to exactly defaultRows here (see fallbackTermHeight's derivation),
// so this is a pure display-window size, not a data/polling decision.
func (m Model) feedContentRows() int {
	n := m.layout().feedH - boxBorderRows - 1
	if n < 1 {
		n = 1
	}
	return n
}

// feedWindowStart returns the index into m.events the feed pane should start
// rendering from: the live tail while following, or feedTop (clamped to the
// current valid range, in case the buffer shrank) while scrolled up.
func (m Model) feedWindowStart() int {
	maxTop := len(m.events) - m.feedContentRows()
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
		return m.renderThreadView() + "\n" + m.renderBottomLine()
	}
	dims := m.layout()
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		m.renderRosterBox(dims),
		lipgloss.JoinVertical(lipgloss.Left, m.renderFeedBox(dims), m.renderThreadsBox(dims)),
	)
	return body + "\n" + m.renderBottomLine()
}

// rosterOrder returns the roster's rows in the exact order renderRoster walks
// them — grouped by project alphabetically, then by alias within a project —
// so rosterIdx indexes into the SAME order the roster actually draws,
// regardless of any '/' filter applied on top of it (rosterRowVisible is the
// filter itself; this is just the walk order it filters over).
func (m Model) rosterOrder() []agentEnriched {
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

	out := make([]agentEnriched, 0, len(m.agents))
	for _, p := range projects {
		rows := byProject[p]
		sort.Slice(rows, func(i, j int) bool { return rows[i].Alias < rows[j].Alias })
		out = append(out, rows...)
	}
	return out
}

// renderRosterRow renders one roster row's text: live dot, label (resolved
// via dispLabel, so the 'a' aliases toggle affects the roster exactly like
// the feed and threads panes), and per-session unread count — "!" marks a
// session whose unread includes an action-requested thread.
func (m Model) renderRosterRow(a agentEnriched) string {
	dot := "✗"
	if a.Live {
		dot = "●"
	}
	line := fmt.Sprintf("%s %s", dot, m.dispLabel(a.Alias))
	if a.Unread > 0 {
		marker := ""
		if a.Action {
			marker = "!"
		}
		line += fmt.Sprintf(" (%d%s)", a.Unread, marker)
	}
	return line
}

// renderRosterLine renders one roster row's VISIBLE text — a SINGLE line,
// clipped (never wrapped) to innerW display columns via display.Sanitize,
// with the unread-count suffix (if any) preserved intact rather than being
// the part that gets cut when a long label doesn't fit (spec §5 layout item
// 2: "unread count right-aligned or as a suffix... truncated with … to the
// pane width — NEVER wrapped"). This is the pane's VISUAL row; the filter/
// selection predicate (rosterRowVisible) still resolves through the plain,
// unpadded renderRosterRow above, so truncation here can never change which
// rows a '/' filter or j/k walk considers.
func (m Model) renderRosterLine(cursorMark string, a agentEnriched, innerW int) string {
	dot := "✗"
	if a.Live {
		dot = "●"
	}
	suffix := ""
	if a.Unread > 0 {
		marker := ""
		if a.Action {
			marker = "!"
		}
		suffix = fmt.Sprintf(" (%d%s)", a.Unread, marker)
	}
	prefix := cursorMark + dot + " "
	avail := innerW - display.Width(prefix) - display.Width(suffix)
	if avail < 1 {
		avail = 1
	}
	label := display.Sanitize(m.dispLabel(a.Alias), avail)
	return render.PadDisplay(prefix+label+suffix, innerW)
}

// renderRosterBox builds the roster pane's bordered box: project-grouped
// agent rows (rosterOrder), skipping rows the roster's active '/' filter
// hides via rosterRowVisible — the SAME predicate moveRosterSelection and
// handleNudgeKey resolve through (spec §5 carried-over fix: filter/selection
// desync) — vertically windowed (windowLines) around the focused selection
// so a selection scrolled past the visible rows is still on screen (spec §5
// layout item 1).
func (m Model) renderRosterBox(dims layoutDims) string {
	innerW := dims.rosterW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	innerH := dims.rosterH - boxBorderRows
	if innerH < 0 {
		innerH = 0
	}

	var lines []string
	selectedLine := -1
	lastProject := "\x00" // sentinel unequal to any real project name/"(none)"
	for i, a := range m.rosterOrder() {
		if !m.rosterRowVisible(a) {
			continue
		}
		p := a.Project
		if p == "" {
			p = "(none)"
		}
		if p != lastProject {
			lines = append(lines, projectHeaderStyle.Render(render.PadDisplay(display.Sanitize(p, innerW), innerW)))
			lastProject = p
		}
		cursorMark := "  "
		if m.focus == paneRoster && i == m.rosterIdx {
			cursorMark = "> "
			selectedLine = len(lines)
		}
		lines = append(lines, m.renderRosterLine(cursorMark, a, innerW))
	}
	lines = windowLines(lines, innerH, selectedLine)
	return renderBox("ROSTER", m.focus == paneRoster, dims.rosterW, dims.rosterH, lines)
}

// renderFeedBox builds the feed pane's bordered box: the journal tail
// rendered with render.Renderer verbatim — the same WHO arrows, labels, and
// width-capped WHAT the CLI's events/watch commands print — applying the
// '/' filter (spec §5) over each rendered line's own text. m.feed.SetWidth
// tracks the PANE's inner width every render (spec §5 layout item 4: the
// feed's width budget is the pane's, not the terminal's), and every line is
// still re-clipped via display.Sanitize as a defensive floor against
// Renderer.Line's own minimum WHAT-budget (10 cols) ever exceeding a very
// narrow pane.
func (m Model) renderFeedBox(dims layoutDims) string {
	innerW := dims.rightW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rowsHeight := dims.feedH - boxBorderRows - 1 // -1 for the header row below
	if rowsHeight < 0 {
		rowsHeight = 0
	}

	m.feed.SetWidth(innerW)
	var hb bytes.Buffer
	m.feed.Header(&hb)
	header := render.PadDisplay(display.Sanitize(strings.TrimRight(hb.String(), "\n"), innerW), innerW)

	start := m.feedWindowStart()
	end := start + rowsHeight
	if end > len(m.events) {
		end = len(m.events)
	}
	if start > end {
		start = end
	}
	q, filtering := m.filterActiveFor(paneFeed)
	lines := []string{header}
	for _, e := range m.events[start:end] {
		var lb bytes.Buffer
		m.feed.Line(&lb, e)
		line := strings.TrimRight(lb.String(), "\n")
		if filtering && !containsFold(line, q) {
			continue
		}
		lines = append(lines, render.PadDisplay(display.Sanitize(line, innerW), innerW))
	}
	return renderBox("FEED", m.focus == paneFeed, dims.rightW, dims.feedH, lines)
}

// renderThreadLine renders one threads-pane row COLUMNIZED like the feed
// (spec §5 layout item 3): `#ID  [tag]  who → who  last-from  AGE  subject`,
// fixed-width columns via render.PadDisplay, subject width-capped to
// whatever's left of innerW. The intent tag is colored (colorIntentTag)
// AFTER its column is already padded, so the color never perturbs the
// column-width accounting the subject budget below depends on.
func (m Model) renderThreadLine(row listThreadRow, innerW int) string {
	marker := "  "
	if row.ID == m.threadSelected {
		marker = "> "
	}
	idCol := render.PadDisplay(display.Sanitize(fmt.Sprintf("#%d", row.ID), threadIDWidth), threadIDWidth)
	tagPlain := intentRowTag(row.Intent)
	tagCol := colorIntentTag(row.Intent, render.PadDisplay(tagPlain, threadTagWidth))
	who := fmt.Sprintf("%s → %s", m.dispLabel(row.FromAgent), m.dispToTarget(row))
	whoCol := render.PadDisplay(display.Sanitize(who, threadWhoWidth), threadWhoWidth)
	lastCol := render.PadDisplay(display.Sanitize(m.dispLabel(row.LastFrom), threadLastWidth), threadLastWidth)
	ageCol := render.PadDisplay(relativeAge(time.Now(), row.LastAt), threadAgeWidth)

	// threadsPlainFixedWidth already accounts for the marker column (see its
	// own doc comment) — don't subtract display.Width(marker) again here, or
	// every row comes out len(marker) columns SHORTER than innerW (exactly
	// the kind of bug that lets a row bleed/misalign against its box).
	subjectBudget := innerW - threadsPlainFixedWidth
	if subjectBudget < 0 {
		subjectBudget = 0
	}
	subjectCol := render.PadDisplay(display.Sanitize(row.Subject, subjectBudget), subjectBudget)

	return marker + idCol + "  " + tagCol + "  " + whoCol + "  " + lastCol + "  " + ageCol + "  " + subjectCol
}

// renderThreadsBox builds the threads pane's bordered box (spec §5):
// action-requested pinned first, then reply-requested, then the rest — see
// groupThreads — each row columnized (renderThreadLine). The selection
// marker follows threadSelected by ID, so it always lands on the right row
// regardless of this poll's grouping. Rows the threads pane's active '/'
// filter hides are skipped via threadRowVisible — the SAME predicate
// moveThreadSelection and openSelectedThread resolve through (spec §5
// carried-over fix: filter/selection desync) — and the visible rows are
// vertically windowed (windowLines) around the selection, same as the
// roster pane.
func (m Model) renderThreadsBox(dims layoutDims) string {
	innerW := dims.rightW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}
	rowsHeight := dims.threadsH - boxBorderRows - 1 // -1 for the header row below
	if rowsHeight < 0 {
		rowsHeight = 0
	}

	header := render.PadDisplay(display.Sanitize(threadsHeaderLine(), innerW), innerW)

	var rows []string
	selectedLine := -1
	for _, row := range groupThreads(m.threads) {
		if !m.threadRowVisible(row) {
			continue
		}
		if row.ID == m.threadSelected {
			selectedLine = len(rows)
		}
		rows = append(rows, m.renderThreadLine(row, innerW))
	}
	rows = windowLines(rows, rowsHeight, selectedLine)
	lines := append([]string{header}, rows...)
	return renderBox("THREADS", m.focus == paneThreads, dims.rightW, dims.threadsH, lines)
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

// threadViewLines renders every loaded entry into flat, already-wrapped
// display lines (header + wrapped body + a blank separator), and returns
// entryStart[i]..entryStart[i+1] as entry i's line range within lines —
// entryStart has len(viewEntries)+1 elements, the last one the total line
// count. Pure: both renderThreadView and threadViewWindowTop call this so
// windowing and rendering never compute two different notions of "where is
// entry i on screen."
func (m Model) threadViewLines() (lines []string, entryStart []int) {
	width := m.threadViewBoxWidth() - boxBorderCols
	if width < 1 {
		width = 1
	}
	entryStart = make([]int, len(m.viewEntries)+1)
	for i, e := range m.viewEntries {
		entryStart[i] = len(lines)
		marker := "  "
		if i == m.viewCursor {
			marker = "> "
		}
		ts := time.UnixMilli(e.CreatedAt).Format("15:04:05")
		lines = append(lines, fmt.Sprintf("%s%s %s", marker, ts, m.dispLabel(e.FromAgent)))
		body := lipgloss.NewStyle().Width(width).Render(display.Sanitize(e.Body, threadViewBodySanitizeWidth))
		lines = append(lines, strings.Split(body, "\n")...)
		lines = append(lines, "")
	}
	entryStart[len(m.viewEntries)] = len(lines)
	return lines, entryStart
}

// threadViewWindowTop picks which line to start rendering from (spec §5
// carried-over fix: render windowing, mirroring the feed's feedWindowStart
// pattern but over variable-height wrapped entries): the window always ends
// right at the cursor entry's own lines — i.e. the cursor's entry is the
// last one fully visible — clamped so it never scrolls past either end.
// Stateless by design: no persisted scroll position is needed, since the
// cursor's own index is enough to re-derive the correct window on every
// render (j/k "scrolls" simply by moving the cursor and letting this
// recompute).
func (m Model) threadViewWindowTop(lines []string, entryStart []int) int {
	height := threadViewRows
	if height <= 0 || len(lines) <= height || len(entryStart) <= 1 {
		return 0
	}
	cursor := m.viewCursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(entryStart)-2 {
		cursor = len(entryStart) - 2
	}
	top := entryStart[cursor+1] - height
	if top < 0 {
		top = 0
	}
	if maxTop := len(lines) - height; top > maxTop {
		top = maxTop
	}
	return top
}

// threadViewBoxWidth is the thread view overlay's outer box width (spec §5
// layout item 5: a centered/full-right bordered box) — capped to the
// terminal's own last-known width once a real tea.WindowSizeMsg has landed
// (spec §5 layout item 7: "never render wider than the terminal"), otherwise
// threadViewDefaultWidth's existing body-wrap fallback (plus border), so an
// unsized caller's body wrap width is unchanged from before this box even
// existed.
func (m Model) threadViewBoxWidth() int {
	bodyWidth := m.opts.Width
	if bodyWidth <= 0 {
		bodyWidth = threadViewDefaultWidth
	}
	outer := bodyWidth + boxBorderCols
	if m.termWidth > 0 && m.termWidth < outer {
		outer = m.termWidth
	}
	if outer < minPaneOuter {
		outer = minPaneOuter
	}
	return outer
}

// renderThreadView renders the thread view overlay in a bordered box titled
// with the thread's ID (spec §5 layout item 5): entries oldest-first, body
// text sanitized and wrapped to the box's own inner width, WINDOWED to
// threadViewRows lines (spec §5 carried-over fix — an unbounded render of a
// long thread would blow well past any real pane height). A "load older"
// hint shows while more entries remain above the loaded window (viewOffset >
// 0); a "N newer — press G" hint shows when viewNewerCount reports entries
// that arrived after this window loaded (spec §5 carried-over fix: the
// newest-entries gap). The box is sized to fit its content exactly (no
// filler rows) rather than a fixed pane height, since the overlay isn't part
// of the three-pane grid layout() sizes.
func (m Model) renderThreadView() string {
	outerW := m.threadViewBoxWidth()
	innerW := outerW - boxBorderCols
	if innerW < 1 {
		innerW = 1
	}

	var content []string
	if m.viewOffset > 0 {
		content = append(content, "↑ more above — k/↑ to load older")
	}
	lines, entryStart := m.threadViewLines()
	top := m.threadViewWindowTop(lines, entryStart)
	end := top + threadViewRows
	if end > len(lines) || threadViewRows <= 0 {
		end = len(lines)
	}
	content = append(content, lines[top:end]...)
	if n := m.viewNewerCount(); n > 0 {
		content = append(content, fmt.Sprintf("↓ %d newer — press G to load", n))
	}

	padded := make([]string, len(content))
	for i, l := range content {
		padded[i] = render.PadDisplay(display.Sanitize(l, innerW), innerW)
	}
	outerH := len(padded) + boxBorderRows
	return renderBox("THREAD #"+strconv.FormatInt(m.viewThreadID, 10), true, outerW, outerH, padded)
}

// paneName renders a pane value for display (the '/' filter's "filter
// (roster): …" prompt).
func paneName(p pane) string {
	switch p {
	case paneRoster:
		return "roster"
	case paneFeed:
		return "feed"
	case paneThreads:
		return "threads"
	default:
		return ""
	}
}

// renderBottomLine renders whichever of the composer, the nudge y/n
// confirmation, the '/' filter edit box, or the plain status line currently
// owns the bottom of the screen — mirroring handleKey's same modal-priority
// order (composer > nudge confirm > filter > plain status).
func (m Model) renderBottomLine() string {
	switch {
	case m.composer.phase == composerPickingTarget:
		return m.renderComposerPicker()
	case m.composer.phase == composerEditingBody:
		return m.renderComposerBody()
	case m.nudgeConfirmAlias != "":
		return fmt.Sprintf("nudge %s? y/n", m.dispLabel(m.nudgeConfirmAlias))
	case m.filter.editing:
		return fmt.Sprintf("filter (%s): %s", paneName(m.filter.pane), m.filter.input.View())
	default:
		return m.renderStatus()
	}
}

// renderComposerPicker renders the 's' target-picker line: the filter input
// plus the (label-resolved) candidates it currently matches, the highlighted
// one marked. Two candidates sharing the same display label are ambiguous on
// screen (spec §5 carried-over fix: picker ambiguity), so any label with more
// than one hit is disambiguated with its project prefixed ("project:label"),
// mirroring the CLI resolver's own qualify cue (internal/humancli.qualify).
func (m Model) renderComposerPicker() string {
	cands := m.composerCandidates()
	labels := make([]string, len(cands))
	counts := make(map[string]int, len(cands))
	for i, a := range cands {
		l := m.dispLabel(a.Alias)
		labels[i] = l
		counts[l]++
	}
	names := make([]string, 0, len(cands))
	for i, a := range cands {
		marker := ""
		if i == m.composer.pickerIdx {
			marker = ">"
		}
		label := labels[i]
		if counts[label] > 1 && a.Project != "" {
			label = a.Project + ":" + label
		}
		names = append(names, marker+label)
	}
	line := "to: " + m.composer.filter.View()
	if len(names) == 0 {
		return line + "  (no match)"
	}
	return line + "  [" + strings.Join(names, " ") + "]"
}

// renderComposerBody renders the body-editing line: the F/R/A intent
// indicator, the resolved target (send) or thread (reply), the input, and
// any op error from a previous submit attempt (spec §5: errors never crash,
// they land right here).
func (m Model) renderComposerBody() string {
	var target string
	switch m.composer.kind {
	case composerKindReply:
		target = fmt.Sprintf("reply #%d", m.composer.threadID)
	default:
		target = "to " + m.dispLabel(m.composer.target)
	}
	line := fmt.Sprintf("[%s] %s: %s", m.composer.intent.tag(), target, m.composer.input.View())
	if m.composer.err != "" {
		line += "  (" + m.composer.err + ")"
	}
	return line
}

// renderStatus renders the single bottom status line (spec §5 layout item
// 6): the operator's status/error text on the left, key hints right-aligned
// against the terminal's own width — errors get a distinct prefix + style
// (statusIsError) rather than reading identically to routine status notes
// like "sending to backend…".
func (m Model) renderStatus() string {
	width := m.termWidth
	if width <= 0 {
		width = fallbackTermWidth
	}

	left := m.status
	if statusIsError(left) {
		left = statusErrStyle.Render("✗ " + left)
	}

	right := keysHintBase
	if m.viewOpen {
		right = fmt.Sprintf("%s scroll · %s reply · %s close", keys.Down.Help().Key, keys.Reply.Help().Key, keys.Esc.Help().Key)
	}
	return joinStatusLine(left, right, width)
}
