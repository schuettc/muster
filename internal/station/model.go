// Package station implements `muster station`, the operator TUI. Iteration
// two (spec §5-REVISED) replaced the original three-pane dashboard with a
// project-first, two-column drill-down: projects → project (agent strip +
// conversations) → agent (their threads + activity) → conversation, Enter
// drills, Esc climbs. Like `muster watch`, station never streams — it polls
// the daemon on a tea.Tick, but the poll loop is owned by the Bubble Tea
// MODEL instead of a bare for-loop, so the event journal cursor advances
// only when the model actually applies an events page (see Update's
// eventsMsg branch) rather than whenever a fetch happens to complete.
package station

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/nudge"
	"github.com/schuettc/muster/internal/render"
)

// keyMap is station's canonical key vocabulary (spec §5-REVISED keys): Enter
// drills / focuses right · Esc climbs · Tab cycles the current screen's
// sub-targets · j/k move · End/G newest (conversation reader only) · s send
// (global, roster-filtered picker) · r reply (conversation reader) · n nudge
// (agent strip/page) · / filter the current left list · a aliases toggle ·
// ? help overlay · q quit. bubbles/key gives every binding a single named
// definition instead of a scattered string switch.
type keyMap struct {
	Tab, ShiftTab, Down, Up, Quit, Enter, Esc, End         key.Binding
	Send, Reply, Nudge, Filter, Aliases, CycleIntent, Help key.Binding
	MailJump                                               key.Binding
}

var keys = keyMap{
	Tab: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "cycle")),
	// ShiftTab cycles the SAME sub-targets as Tab, in reverse (iteration-4
	// queue item 2) — see cycleFocusBack, cycleFocus's mirror-image twin.
	ShiftTab: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "cycle back")),
	Down:     key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "move")),
	Up:       key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "move")),
	Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "drill")),
	Esc:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	// End snaps the focused conversation reader to its live tail (spec
	// §5-REVISED: "End/G newest") — a no-op everywhere else, since the
	// global feed pane it used to control is gone.
	End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end/G", "newest")),
	Send:        key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send")),
	Reply:       key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reply")),
	Nudge:       key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Aliases:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "aliases")),
	CycleIntent: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "intent")),
	Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	// MailJump implements 'm' (spec iteration-5: "m from ANY level jumps to
	// the FOR YOU section") — see handleMailJumpKey.
	MailJump: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "your mail")),
}

// Layout knobs. defaultRows/eventBacklog bound how much of the global events
// journal the model keeps — no longer rendered as its own pane (spec
// §5-REVISED removes the feed pane), but still the source buffer an agent
// page's activity list filters client-side (see conversationsForAgent's
// sibling, agentActivity, in views.go), and still the same cursor-disciplined
// buffer spec §5's data loop requires.
const (
	defaultRows  = 20
	eventBacklog = 500
)

// initialEventBacklog bounds the cold-start (and bootstrap-retry) backlog
// fetch.
const initialEventBacklog = defaultRows

// conversationReaderRows bounds the right pane's visible WRAPPED-line budget
// when a conversation is focused (spec §5 carried-over fix: render
// windowing).
const conversationReaderRows = defaultRows

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
	ActionCount int  // the session's action-requested unread count (spec §5-REVISED project rollup: "(+action)")
	// Attached is true when a human tmux client is currently attached to
	// this agent's session (spec iteration-5 Tier 1: the attach marker) —
	// checked only for a LIVE session, batched into the same per-agent tmux
	// query loop as Live/Label (see fetchAgents in poll.go), never in
	// View().
	Attached bool
}

// listThreadRow mirrors store.Thread's wire JSON for the conversation lists.
type listThreadRow struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	FromAgent string `json:"from_agent"`
	ToKind    string `json:"to_kind"`
	ToTarget  string `json:"to_target"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	Intent    string `json:"intent"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// OriginProject is the sender's registered project stamped at thread
	// creation time (iteration-4 orphan-thread fix, spec queue item 4) — ""
	// when unstamped (a pre-migration row whose sender no longer resolves,
	// or a genuinely unregistered sender). nav.go's threadProjects unions
	// this with the roster-derived participant projects so a thread whose
	// participants have ALL since deregistered still has a project home.
	OriginProject string `json:"origin_project"`
	LastFrom      string `json:"last_from"`
	LastAt        int64  `json:"last_at"`
	EntryCount    int    `json:"entry_count"`
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
	activity     *render.Renderer // renders agent-activity lines (spec §5-REVISED: "the render.Renderer stays — agent-activity lines reuse it")

	agents []agentEnriched
	labels map[string]string // alias → current label, shared by the activity view and every row renderer

	threads []listThreadRow

	// Navigation (spec §5-REVISED). screen names the current PAGE; focus
	// names which sub-list (or the right-pane reader) currently owns the
	// cursor — see nav.go's doc comment for why this pair stands in for a
	// literal []level stack. project/agent/conversation are PER-LEVEL
	// SELECTIONS by identity (name/alias/thread ID, never an index) that
	// persist across Esc — climbing back to a level restores its last
	// selection, and a poll's regroup never silently moves the operator's
	// cursor (spec §5 carried-over fix, extended to every list here).
	screen        screen
	focus         focusTarget
	singleProject bool // recomputed every applyAgents: gates L0 auto-skip and Esc-from-L1 no-op
	everNavigated bool // gates the ONE-TIME auto-skip so projects appearing/disappearing later never yanks the operator back to L0 mid-session
	project       string
	agent         string
	conversation  int64
	// forYou is the currently-selected FOR YOU row's thread ID (spec
	// iteration-5: the pinned L0 section listing station's own unread
	// threads) — its own identity-keyed selection, distinct from
	// m.conversation (a different underlying list).
	forYou int64

	ackedThreads map[int64]bool // thread IDs already open-to-acknowledged THIS run — focusing the same thread twice must not re-fire get_inbox

	// lastActive is alias -> the newest journal event's TS (ms epoch) where
	// that alias was the ACTOR (spec iteration-5 Tier 0a: "last active:
	// <relative>") — populated lazily by fetchLastActiveCmd, scoped each
	// poll tick to the current project's agent-strip membership (see
	// pollCmd), never fetched from View(). A missing entry (nil map, or no
	// key yet) simply renders no last-active text rather than "unknown".
	lastActive map[string]int64

	// Conversation reader (right pane): the currently loaded conversation's
	// page. Which MODE is active — L1's passive last-messages preview or L2's
	// interactive scroll/load-older/reply — is purely m.focus ==
	// focusConvRight, not a separate field; the pagination mechanics
	// themselves are unchanged from the pre-redesign thread view overlay.
	viewThreadID int64
	viewEntries  []threadEntryRow
	viewOffset   int64
	viewTotal    int64
	viewCursor   int
	viewLoading  bool
	viewGen      int64

	// composer implements the bottom-line composer (spec §5: 's' send via a
	// roster-filtered target picker anywhere; 'r' reply to the focused
	// conversation; intent cycled F/R/A, Enter submits, Esc cancels).
	composer composerState

	// nudgeConfirmAlias is "" (no pending confirmation) or the alias 'n' is
	// asking "nudge <label>? y/n" about — handleNudgeConfirmKey owns every
	// key while it's set. nudger is the send-keys seam (DI'd via
	// Options.Nudger; tests inject a fake so a model test never shells to
	// real tmux).
	nudgeConfirmAlias string
	nudger            nudger

	// filter implements '/' (spec §5-REVISED: "filter left list"): a
	// substring filter over the CURRENT left list's rendered row text,
	// selection-aware exactly like the pre-redesign roster/threads filters
	// (see nav.go's generic selectionVisible/snapSelection/moveSelection).
	filter filterState

	helpOpen bool // '?' overlay (spec §5-REVISED)

	status   string
	quitting bool

	// termWidth/termHeight are the last tea.WindowSizeMsg the program has
	// seen (0,0 before the first one arrives) — the ONLY inputs layout()
	// needs to size the two columns, and the sole input to the narrow-mode
	// (< ~110 cols) single-column collapse (spec §5-REVISED).
	termWidth  int
	termHeight int
}

// composerPhase is the composer's own little state machine: closed (no
// composer on screen), picking a target (the 's' roster-filtered picker,
// before the body input), or editing the body (both 's' after a target is
// picked, and 'r' — which skips straight here, its target is the focused
// conversation).
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

// filterState is the Model's '/' filter sub-state (see Model.filter): list
// names which left list the active query applies to (set when '/' is
// pressed); query is live-updated as the operator types; editing gates
// whether handleFilterKey currently owns the keys (Enter stops editing but
// leaves query applied; Esc clears the filter entirely).
type filterState struct {
	list    llList
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
		caller:   caller,
		opts:     opts,
		nudger:   n,
		activity: render.NewRenderer(nil, nil, opts.Aliases, false, opts.Width),
		screen:   screenProjects,
		focus:    focusProjectList,
		status:   "connecting…",
	}
}

// Init issues the first poll immediately (so the screen isn't blank for a
// full --interval) and schedules the first tick. Since a fresh Model is not
// yet bootstrapped, that first poll's events fetch is pollCmd's backlog
// branch, not follow — see pollCmd and applyEvents.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
}

// pollCmd issues the tick's three independent fetches (spec §5 data loop):
// events, list_agents, list_threads. Each yields its own message; none may be
// combined into a shared snapshot before Update sees them, so a failure in
// one can never block or corrupt the others, and no decision is ever derived
// from a mixed tick bundle.
//
// The events fetch's MODE depends on m.bootstrapped: until the cold-start
// backlog fetch has actually applied, every attempt — including retries
// after a failed backlog fetch — stays in BACKLOG mode.
func (m Model) pollCmd() tea.Cmd {
	eventsCmd := fetchEventsCmd(m.caller, m.cursor, m.pollGen)
	if !m.bootstrapped {
		eventsCmd = fetchBacklogEventsCmd(m.caller, initialEventBacklog, m.pollGen)
	}
	cmds := []tea.Cmd{
		eventsCmd,
		fetchAgentsCmd(m.caller),
		fetchThreadsCmd(m.caller),
	}
	// Tier 0a last-active enrichment (spec iteration-5): one small
	// list_events(agent=alias) lookup per CURRENT project's agent-strip
	// member, tagged with this tick's pollGen so a slow in-flight fetch from
	// an older tick is discarded exactly like eventsMsg (see applyLastActive)
	// — issued from the poll Cmd only, never from a keypress or View().
	for _, a := range m.agentStripRows() {
		cmds = append(cmds, fetchLastActiveCmd(m.caller, a.Alias, m.pollGen))
	}
	return tea.Batch(cmds...)
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
		m.pollGen++ // a new tick supersedes any still-in-flight fetch from the previous one
		return m, tea.Batch(m.pollCmd(), tickCmd(m.opts.Interval))
	case eventsMsg:
		if msg.gen != m.pollGen {
			return m, nil // stale: a newer tick has already superseded this in-flight fetch
		}
		return m.applyEvents(msg), nil
	case agentsMsg:
		return m.applyAgents(msg), nil
	case threadsMsg:
		return m.applyThreads(msg)
	case threadPageMsg:
		return m.applyThreadPage(msg)
	case inboxAckMsg:
		return m.applyInboxAck(msg), nil
	case composerSentMsg:
		return m.applyComposerSent(msg), nil
	case nudgeResultMsg:
		return m.applyNudgeResult(msg), nil
	case lastActiveMsg:
		return m.applyLastActive(msg), nil
	}
	return m, nil
}

// handleKey routes a keypress to whichever modal owns the keyboard right
// now, innermost first: the composer (spec §5's bottom-line send/reply),
// then the nudge y/n confirmation, then the '/' filter's own edit box, then
// the '?' help overlay (any key closes it) — each of these owns EVERY key
// while active. Only once none of them are active does a keypress reach the
// base navigation vocabulary.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.composer.phase != composerClosed:
		return m.handleComposerKey(msg)
	case m.nudgeConfirmAlias != "":
		return m.handleNudgeConfirmKey(msg)
	case m.filter.editing:
		return m.handleFilterKey(msg)
	case m.helpOpen:
		m.helpOpen = false
		return m, nil
	}
	switch {
	case key.Matches(msg, keys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, keys.Help):
		m.helpOpen = true
		return m, nil
	case key.Matches(msg, keys.Tab):
		m = m.cycleFocus()
		return m, nil
	case key.Matches(msg, keys.ShiftTab):
		m = m.cycleFocusBack()
		return m, nil
	case key.Matches(msg, keys.Down):
		return m.moveFocused(1)
	case key.Matches(msg, keys.Up):
		return m.moveFocused(-1)
	case key.Matches(msg, keys.End):
		return m.handleEndKey()
	case key.Matches(msg, keys.Enter):
		return m.handleEnterKey()
	case key.Matches(msg, keys.Esc):
		return m.handleEscKey(), nil
	case key.Matches(msg, keys.Send):
		return m.openComposerSend(), nil
	case key.Matches(msg, keys.Reply):
		if m.focus == focusConvRight {
			return m.openComposerReply(m.viewThreadID), nil
		}
		return m, nil
	case key.Matches(msg, keys.Nudge):
		return m.handleNudgeKey(), nil
	case key.Matches(msg, keys.MailJump):
		return m.handleMailJumpKey()
	case key.Matches(msg, keys.Filter):
		return m.openFilter(), nil
	case key.Matches(msg, keys.Aliases):
		m.opts.Aliases = !m.opts.Aliases
		m.activity.SetAliases(m.opts.Aliases)
		return m, nil
	}
	return m, nil
}

// currentLeftList identifies which left list '/' filters and Tab/j/k address
// right now (spec §5-REVISED: "/ filter left list") — screenProjects has
// only one list; screenProject has two (the agent strip and the
// conversations below it, switched by focus); screenAgent has one (that
// agent's threads). Valid even while focus == focusConvRight (a filter
// opened there still targets the underlying list the operator drilled
// through, exactly as if they'd pressed Esc first).
func (m Model) currentLeftList() llList {
	switch m.screen {
	case screenAgent:
		return llAgentThreads
	case screenProject:
		if m.focus == focusAgentStrip {
			return llAgentStrip
		}
		return llConvList
	default:
		if m.focus == focusForYou {
			return llForYou
		}
		return llProjects
	}
}

// filterQueryFor reports list's active '/' query, if any.
func (m Model) filterQueryFor(list llList) (query string, filtering bool) {
	if m.filter.list != list || m.filter.query == "" {
		return "", false
	}
	return m.filter.query, true
}

// containsFold reports whether s contains substr, case-insensitively.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// projectRows returns L0's rows (spec §5-REVISED project rollups).
func (m Model) projectRows() []projectSummary {
	return computeProjectSummaries(m.agents, m.threads)
}

// agentStripRows returns the active project's agent strip (spec §5-REVISED
// L1: "agent strip on top").
func (m Model) agentStripRows() []agentEnriched {
	return agentsForProject(m.agents, m.project)
}

// conversationRows returns the currently-showing conversation list — the
// active project's conversations (screenProject, also used while still at
// screenProjects since nothing renders it there) or the active agent's
// threads (screenAgent) — always grouped action-requested-first (spec
// §5-REVISED: "action-requested pinned").
func (m Model) conversationRows() []conversationRow {
	if m.screen == screenAgent {
		rows := conversationsForAgent(m.threads, m.agent)
		wrapped := make([]conversationRow, len(rows))
		for i, r := range rows {
			wrapped[i] = conversationRow{listThreadRow: r}
		}
		return groupConversationRows(wrapped)
	}
	aliasProj := aliasProjectMap(m.agents)
	return groupConversationRows(conversationsForProject(m.threads, aliasProj, m.project))
}

// plainProjectRow/plainConvRow are the FILTER/SELECTION predicate's plain
// (unpadded) row text for a project/conversation row — plainAgentRow is
// renderRosterRow itself (views.go), reused verbatim for the agent strip.
func (m Model) plainProjectRow(p projectSummary) string {
	return fmt.Sprintf("%s %d/%d live (%d unread, %d action)", projectDisplayName(p.Name), p.Live, p.Total, p.Unread, p.ActionUnread)
}

func (m Model) plainConvRow(c conversationRow) string {
	return m.renderThreadRow(c.listThreadRow)
}

// cycleFocus implements Tab (spec §5-REVISED keys): cycles the current
// screen's sub-targets. screenProjects has only one target (a no-op).
func (m Model) cycleFocus() Model {
	switch m.screen {
	case screenProjects:
		// Toggle onto/off the FOR YOU section (spec iteration-5) — only a
		// live target while the section is actually showing (station has
		// unread mail); otherwise the project list is the only target, same
		// no-op as before this feature.
		if total, _ := m.stationUnread(); total > 0 {
			if m.focus == focusForYou {
				m.focus = focusProjectList
			} else {
				m.focus = focusForYou
			}
		}
	case screenProject:
		switch m.focus {
		case focusAgentStrip:
			m.focus = focusConvList
		case focusConvList:
			m.focus = focusConvRight
		default:
			m.focus = focusAgentStrip
		}
	case screenAgent:
		if m.focus == focusAgentThreads {
			m.focus = focusConvRight
		} else {
			m.focus = focusAgentThreads
		}
	}
	return m
}

// cycleFocusBack implements Shift-Tab (iteration-4 queue item 2): cycles the
// current screen's sub-targets in the REVERSE order from cycleFocus. It is
// cycleFocus's exact mirror image — screenAgent's two-way toggle is its own
// reverse, so only screenProject's three-way order actually differs.
// screenProjects has only one target (a no-op).
func (m Model) cycleFocusBack() Model {
	switch m.screen {
	case screenProjects:
		// A two-way toggle is its own reverse (same as screenAgent below) —
		// see cycleFocus's identical case.
		if total, _ := m.stationUnread(); total > 0 {
			if m.focus == focusForYou {
				m.focus = focusProjectList
			} else {
				m.focus = focusForYou
			}
		}
	case screenProject:
		switch m.focus {
		case focusAgentStrip:
			m.focus = focusConvRight
		case focusConvList:
			m.focus = focusAgentStrip
		default: // focusConvRight
			m.focus = focusConvList
		}
	case screenAgent:
		if m.focus == focusAgentThreads {
			m.focus = focusConvRight
		} else {
			m.focus = focusAgentThreads
		}
	}
	return m
}

// moveFocused applies a j/k (delta=+1/-1) move to whichever list (or the
// conversation reader) currently has focus.
func (m Model) moveFocused(delta int) (tea.Model, tea.Cmd) {
	switch m.focus {
	case focusProjectList:
		rows := m.projectRows()
		q, f := m.filterQueryFor(llProjects)
		m.project = moveSelection(rows, projKey, m.project, delta, m.plainProjectRow, q, f, "")
		return m, nil
	case focusForYou:
		rows := m.forYouRows()
		q, f := m.filterQueryFor(llForYou)
		m.forYou = moveSelection(rows, forYouKey, m.forYou, delta, m.plainForYouRow, q, f, 0)
		return m, nil
	case focusAgentStrip:
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llAgentStrip)
		m.agent = moveSelection(rows, agentKey, m.agent, delta, m.renderRosterRow, q, f, "")
		return m, nil
	case focusConvList, focusAgentThreads:
		rows := m.conversationRows()
		q, f := m.filterQueryFor(m.currentLeftList())
		newSel := moveSelection(rows, convKey, m.conversation, delta, m.plainConvRow, q, f, 0)
		if newSel == m.conversation {
			return m, nil
		}
		m.conversation = newSel
		var cmd tea.Cmd
		m, cmd = m.conversationPreviewCmd(m.conversation, false)
		return m, cmd
	case focusConvRight:
		return m.scrollConversation(delta)
	}
	return m, nil
}

// scrollConversation applies a j/k move within the FOCUSED conversation
// reader (spec §5-REVISED L2): k/up scrolls toward older entries, lazily
// fetching the next older get_thread page when the loaded window's top is
// reached; j/down scrolls toward newer entries within what's already loaded.
func (m Model) scrollConversation(delta int) (Model, tea.Cmd) {
	if delta < 0 {
		if m.viewCursor > 0 {
			m.viewCursor--
			return m, nil
		}
		if m.viewOffset > 0 && !m.viewLoading {
			m.viewLoading = true
			return m, fetchThreadPageCmd(m.caller, m.viewThreadID, 0, m.viewOffset, true, false, m.viewGen)
		}
		return m, nil
	}
	if m.viewCursor < len(m.viewEntries)-1 {
		m.viewCursor++
	}
	return m, nil
}

// handleEndKey implements End/G (spec §5-REVISED: "End/G newest") — only
// meaningful once a conversation is FOCUSED (focusConvRight): fetches the
// tail when newer entries have arrived since this window loaded
// (viewNewerCount, spec §5 carried-over fix: the newest-entries gap).
// Everywhere else it's a no-op — there's no global feed left to "follow".
func (m Model) handleEndKey() (tea.Model, tea.Cmd) {
	if m.focus != focusConvRight {
		return m, nil
	}
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
}

// viewNewerCount returns how many entries exist beyond the currently loaded
// tail, per the freshest list_threads poll's entry_count for this thread
// (spec §5 carried-over fix: the newest-entries gap) — 0 while nothing has
// loaded yet, or once nothing is known to be missing.
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

// handleEnterKey implements Enter (spec §5-REVISED: "Enter drills /
// focuses right"). At each list target, a selection currently hidden by an
// active '/' filter is only SNAPPED to the nearest visible row rather than
// acted on (spec §5 carried-over fix: filter/selection desync) — the next
// Enter (now landing on a visible row) drills normally.
func (m Model) handleEnterKey() (tea.Model, tea.Cmd) {
	switch {
	case m.screen == screenProjects && m.focus == focusForYou:
		rows := m.forYouRows()
		q, f := m.filterQueryFor(llForYou)
		if !anyRowVisible(rows, m.plainForYouRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, forYouKey, m.forYou, m.plainForYouRow, q, f) {
			m.forYou = snapSelection(rows, forYouKey, m.forYou, m.plainForYouRow, q, f, 0)
			return m, nil
		}
		return m.openForYouThread(m.forYou)

	case m.screen == screenProjects:
		rows := m.projectRows()
		q, f := m.filterQueryFor(llProjects)
		if !anyRowVisible(rows, m.plainProjectRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, projKey, m.project, m.plainProjectRow, q, f) {
			m.project = snapSelection(rows, projKey, m.project, m.plainProjectRow, q, f, "")
			return m, nil
		}
		m.screen = screenProject
		// Default focus on project entry is the AGENTS box (iteration-4 queue
		// item 1: "the who first") — not the conversation/thread list.
		m.focus = focusAgentStrip
		m.everNavigated = true
		m = m.refreshAgentStripSelection()
		return m.refreshConversationSelection()

	case m.focus == focusAgentStrip:
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llAgentStrip)
		if !anyRowVisible(rows, m.renderRosterRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			return m, nil
		}
		m.screen = screenAgent
		m.focus = focusAgentThreads
		m.everNavigated = true
		return m.refreshConversationSelection()

	case m.focus == focusConvList || m.focus == focusAgentThreads:
		rows := m.conversationRows()
		q, f := m.filterQueryFor(m.currentLeftList())
		if !anyRowVisible(rows, m.plainConvRow, q, f) {
			return m, nil
		}
		if !selectionVisible(rows, convKey, m.conversation, m.plainConvRow, q, f) {
			m.conversation = snapSelection(rows, convKey, m.conversation, m.plainConvRow, q, f, 0)
			return m, nil
		}
		return m.focusConversation()
	}
	return m, nil // focusConvRight: already the deepest level
}

// handleEscKey implements Esc (spec §5-REVISED: "Esc climbs"): un-focuses
// the conversation reader back to its list, then climbs screenAgent →
// screenProject → screenProjects — a no-op at screenProjects, and at
// screenProject on a single-project bus (spec §5-REVISED: "L0 skipped
// entirely" — there is no L0 to climb back to).
func (m Model) handleEscKey() Model {
	switch {
	case m.focus == focusConvRight:
		if m.screen == screenAgent {
			m.focus = focusAgentThreads
		} else {
			m.focus = focusConvList
		}
	case m.screen == screenAgent:
		m.screen = screenProject
		m.focus = focusAgentStrip
	case m.screen == screenProject:
		if !m.singleProject {
			m.screen = screenProjects
			m.focus = focusProjectList
		}
	}
	return m
}

// refreshAgentStripSelection re-derives the active project's agent-strip
// selection, defaulting to its first row when the previous selection
// doesn't belong to THIS project — needed on every L0→L1 drill, since
// applyAgents only recomputes this against whichever project was active the
// last time a list_agents poll landed, not against a project switch that
// just happened via keys alone.
func (m Model) refreshAgentStripSelection() Model {
	strip := m.agentStripRows()
	switch {
	case len(strip) == 0:
		m.agent = ""
	case keyIndex(strip, agentKey, m.agent) < 0:
		m.agent = strip[0].Alias
	}
	return m
}

// refreshConversationSelection re-derives the current screen's conversation
// list and defaults the selection to its first row when the previous
// selection no longer applies (a fresh drill into a project/agent whose
// conversation list the operator hasn't looked at before) — then kicks off
// that conversation's preview fetch.
func (m Model) refreshConversationSelection() (Model, tea.Cmd) {
	rows := m.conversationRows()
	if len(rows) == 0 {
		m.conversation = 0
		return m, nil
	}
	if keyIndex(rows, convKey, m.conversation) < 0 {
		m.conversation = rows[0].ID
	}
	return m.conversationPreviewCmd(m.conversation, false)
}

// focusConversation promotes focus to focusConvRight (spec §5-REVISED L2)
// and fires the ONE side-effecting read anywhere in station: open-to-
// acknowledge. If the focused thread is addressed to station's OWN
// registered alias, this issues exactly one get_inbox for that alias — never
// on selection, preview, or a later poll — and never twice for the same
// thread within this run (ackedThreads dedupes a repeated Enter/Esc/Enter on
// the same conversation).
func (m Model) focusConversation() (Model, tea.Cmd) {
	m.focus = focusConvRight
	idx := indexOfThread(m.threads, m.conversation)
	if idx < 0 {
		return m, nil
	}
	row := m.threads[idx]
	if row.ToTarget != m.opts.Alias || m.ackedThreads[row.ID] {
		return m, nil
	}
	if m.ackedThreads == nil {
		m.ackedThreads = map[int64]bool{}
	}
	m.ackedThreads[row.ID] = true
	return m, fetchInboxAckCmd(m.caller, m.opts.Alias)
}

// stationUnread returns station's OWN session tuple's unread total/action
// flag (spec iteration-5: the 📬 header badge and the FOR YOU section's
// visibility gate) — reusing the SAME session_unread result fetchAgents
// already computes for every distinct (socket_path, session_id) tuple in the
// roster (station registers itself like any other agent, so its own row
// carries it), never get_inbox: side-effect-free, exactly the count the
// operator would see without disturbing it.
func (m Model) stationUnread() (total int, action bool) {
	idx := keyIndex(m.agents, agentKey, m.opts.Alias)
	if idx < 0 {
		return 0, false
	}
	return m.agents[idx].Unread, m.agents[idx].Action
}

// forYouKey is the FOR YOU list's identity-key extractor (a thread ID),
// matching projKey/agentKey/convKey's role for the other three keyed lists.
func forYouKey(r listThreadRow) int64 { return r.ID }

// forYouRows returns station's own pinned-section rows: threads addressed
// DIRECTLY to station's alias (mirroring focusConversation's own
// open-to-acknowledge eligibility check) whose last entry wasn't written by
// station itself (spec iteration-5 Tier 0b's same display-only "waiting on
// me" proxy — see unreadThreadsFor in nav.go), excluding anything already
// acknowledged this run. This is a best-effort RECONSTRUCTION of which
// threads make up stationUnread's exact count (per-thread read watermarks
// aren't obtainable without get_inbox, which this feature must never call
// outside the acknowledge path) — the header badge's count is authoritative;
// this is display content for the section that count gates.
func (m Model) forYouRows() []listThreadRow {
	var out []listThreadRow
	for _, row := range m.threads {
		if row.ToKind != "agent" || row.ToTarget != m.opts.Alias {
			continue
		}
		if row.LastFrom == "" || row.LastFrom == m.opts.Alias {
			continue
		}
		if m.ackedThreads[row.ID] {
			continue
		}
		out = append(out, row)
	}
	return out
}

// plainForYouRow is the FOR YOU list's filter/selection predicate text (spec
// §5 carried-over discipline: the SAME text every list's
// filter/move/selection helpers resolve through).
func (m Model) plainForYouRow(row listThreadRow) string {
	return fmt.Sprintf("%s %s %s", row.Subject, m.dispLabel(row.FromAgent), relativeAge(time.Now(), row.LastAt))
}

// openForYouThread jumps directly into threadID's conversation (spec
// iteration-5: "Selecting + Enter opens the thread directly (this IS the
// acknowledge, existing rules)", and 'm's single-unread-thread shortcut) —
// resolves the thread's project, lands on screenProject with the
// conversation FOCUSED (L2), and reuses focusConversation's existing
// open-to-acknowledge check verbatim (a FOR YOU row is, by construction,
// always addressed directly to station, so the ack fires exactly like any
// other station-addressed thread).
func (m Model) openForYouThread(threadID int64) (Model, tea.Cmd) {
	idx := indexOfThread(m.threads, threadID)
	if idx < 0 {
		return m, nil
	}
	row := m.threads[idx]
	projs := threadProjectsOrUnassigned(row, aliasProjectMap(m.agents))
	m.project = projs[0]
	m.screen = screenProject
	m.everNavigated = true
	m = m.refreshAgentStripSelection()
	m.conversation = threadID
	var previewCmd, ackCmd tea.Cmd
	m, previewCmd = m.conversationPreviewCmd(threadID, false)
	m, ackCmd = m.focusConversation()
	return m, tea.Batch(previewCmd, ackCmd)
}

// handleMailJumpKey implements 'm' (spec iteration-5: "m from ANY level
// jumps to the FOR YOU section (or straight into the single unread thread
// when there is exactly one)"). Gated on stationUnread's count (the same
// side-effect-free source the header badge uses) rather than len(forYouRows())
// alone, so a real mismatch between the exact count and the best-effort row
// reconstruction still lets the operator jump to look, rather than silently
// doing nothing.
func (m Model) handleMailJumpKey() (tea.Model, tea.Cmd) {
	total, _ := m.stationUnread()
	if total <= 0 {
		m.status = "no mail for you"
		return m, nil
	}
	rows := m.forYouRows()
	if len(rows) == 1 {
		return m.openForYouThread(rows[0].ID)
	}
	m.screen = screenProjects
	m.focus = focusForYou
	if len(rows) > 0 && keyIndex(rows, forYouKey, m.forYou) < 0 {
		m.forYou = rows[0].ID
	}
	return m, nil
}

// agentAttached reports whether alias's tmux session currently has a human
// client attached (spec iteration-5 Tier 1b: the attach marker) — "" /
// unknown alias reads as not attached.
func (m Model) agentAttached(alias string) bool {
	idx := keyIndex(m.agents, agentKey, alias)
	if idx < 0 {
		return false
	}
	return m.agents[idx].Attached
}

// applyLastActive applies one fetchLastActiveCmd result (spec iteration-5
// Tier 0a). A stale generation (an in-flight fetch from an older tick,
// superseded by a newer one — same discipline as eventsMsg/pollGen) or a
// fetch error is discarded without touching the cache; ts==0 (no actor event
// found within the fetch's small window) also leaves any previously-cached
// value alone rather than blanking it, since "not found in this window"
// isn't the same claim as "never active".
func (m Model) applyLastActive(msg lastActiveMsg) Model {
	if msg.gen != m.pollGen || msg.err != nil || msg.ts <= 0 {
		return m
	}
	if m.lastActive == nil {
		m.lastActive = map[string]int64{}
	}
	m.lastActive[msg.alias] = msg.ts
	return m
}

// conversationPreviewCmd loads threadID's most recent page into the right
// pane (spec §5-REVISED: "Right pane previews the selected conversation's
// last messages"), NEVER acknowledging it (only focusConversation's
// Enter-path does that). forceRefresh re-fetches even if threadID is already
// loaded (used for the "keep the live preview fresh" tick refresh in
// applyThreads); otherwise a call for the already-loaded thread is a no-op.
// A fresh thread (switching away from whatever was previously loaded) clears
// the old entries immediately so the pane never shows a stale thread's
// content while the new one loads.
func (m Model) conversationPreviewCmd(threadID int64, forceRefresh bool) (Model, tea.Cmd) {
	if threadID == 0 {
		return m, nil
	}
	if !forceRefresh && m.viewThreadID == threadID {
		return m, nil
	}
	idx := indexOfThread(m.threads, threadID)
	var entryCount int
	if idx >= 0 {
		entryCount = m.threads[idx].EntryCount
	}
	limit := int64(threadViewPageSize)
	offset := int64(entryCount) - limit
	if offset < 0 {
		offset = 0
	}
	freshThread := m.viewThreadID != threadID
	m.viewThreadID = threadID
	m.viewGen++
	m.viewLoading = true
	if freshThread {
		m.viewEntries = nil
		m.viewOffset = offset
		m.viewTotal = 0
		m.viewCursor = 0
	}
	return m, fetchThreadPageCmd(m.caller, threadID, offset, limit, false, false, m.viewGen)
}

// handleNudgeKey implements 'n' (spec §5-REVISED: "n nudge on agent
// strip/page"): valid when the agent strip has focus (nudges the strip's
// selected row) or when a whole agent page (screenAgent) is showing (nudges
// that page's own agent, regardless of its sub-focus) — a no-op anywhere
// else. Two guards mirror the pre-redesign roster's (spec §5 carried-over
// fixes): filter/selection desync (a hidden selection is only snapped, never
// acted on) and self-nudge (station's own row never opens the confirm gate —
// nudging it would tmux send-keys INTO station's own pane).
func (m Model) handleNudgeKey() Model {
	switch {
	case m.focus == focusAgentStrip:
		rows := m.agentStripRows()
		q, f := m.filterQueryFor(llAgentStrip)
		if !selectionVisible(rows, agentKey, m.agent, m.renderRosterRow, q, f) {
			m.agent = snapSelection(rows, agentKey, m.agent, m.renderRosterRow, q, f, "")
			if m.agent == "" {
				m.status = "no agent visible — adjust or clear the filter"
			}
			return m
		}
	case m.screen == screenAgent:
		if m.agent == "" {
			return m
		}
	default:
		return m
	}
	if m.agent == m.opts.Alias {
		m.status = "that's you — can't nudge yourself"
		return m
	}
	m.nudgeConfirmAlias = m.agent
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

// openFilter opens '/' editing for the current left list (spec §5-REVISED):
// re-opening the SAME list's filter preserves its existing query for
// editing; switching to a different list starts blank.
func (m Model) openFilter() Model {
	list := m.currentLeftList()
	ti := textinput.New()
	ti.Placeholder = "filter…"
	ti.Focus()
	if m.filter.list == list {
		ti.SetValue(m.filter.query)
	}
	m.filter = filterState{list: list, query: ti.Value(), editing: true, input: ti}
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

// openComposerSend opens the 's' composer: the roster-filtered target picker
// phase (spec §5), before the body input. Valid from anywhere (spec
// §5-REVISED: "s send global from anywhere with picker").
func (m Model) openComposerSend() Model {
	ti := textinput.New()
	ti.Placeholder = "filter roster (label or alias)…"
	ti.Focus()
	m.composer = composerState{phase: composerPickingTarget, kind: composerKindSend, filter: ti}
	return m
}

// openComposerReply opens the 'r' composer straight into body-editing (spec
// §5-REVISED: "r reply in conversation") — no target picker, the target is
// the focused conversation.
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

// applyEvents is the ONLY place the cursor advances, and only once a page is
// actually applied: a failed fetch (err != nil) leaves the cursor and the
// buffered events untouched, so a dropped/failed poll can never skip events —
// the next successful poll retries in the same mode (backlog stays backlog
// until it succeeds; follow picks up from exactly the same after_id).
//
// msg.backlog is the cold-start (or bootstrap-retry) case: the daemon
// returns backlog rows NEWEST-first, so they're reversed into the
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

// applyAgents refreshes the roster and re-derives the L0/L1 selection
// defaults (spec §5-REVISED). A failed fetch only updates the status line —
// the roster keeps showing its last-known state rather than blanking (spec:
// "roster/threads failures don't block the feed", and by the same principle
// don't erase themselves either).
func (m Model) applyAgents(msg agentsMsg) Model {
	if msg.err != nil {
		m.status = fmt.Sprintf("agents: poll failed, retrying: %v", msg.err)
		return m
	}
	m.agents = msg.rows
	labels := make(map[string]string, len(m.agents))
	for _, a := range m.agents {
		labels[a.Alias] = a.Label
	}
	m.labels = labels
	m.activity.SetLabels(labels)

	projects := m.projectRows()
	m.singleProject = len(projects) == 1
	switch {
	case len(projects) == 0:
		m.project = ""
	case keyIndex(projects, projKey, m.project) < 0:
		m.project = projects[0].Name
	}
	// Single-project auto-skip (spec §5-REVISED: "L0 skipped entirely"),
	// fired exactly once so a project appearing/disappearing later never
	// yanks the operator back to L0 mid-session. Lands on the agent strip
	// (iteration-4 queue item 1), same default as a manual L0 drill-in.
	if !m.everNavigated && m.screen == screenProjects && len(projects) == 1 {
		m.screen = screenProject
		m.focus = focusAgentStrip
	}

	return m.refreshAgentStripSelection()
}

// applyThreads refreshes the conversation-list data and re-derives the
// current screen's conversation selection default (spec §5 carried-over
// fix: selection preserved BY THREAD ID across a poll's regroup, never an
// index). When the operator isn't currently focused on the conversation
// reader (focusConvRight), it also re-fetches the selected conversation's
// preview so the right pane's "last messages" stay live without disturbing
// a focused reader's scroll position (which instead uses End/G, spec §5
// carried-over fix: the newest-entries gap).
func (m Model) applyThreads(msg threadsMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.status = fmt.Sprintf("threads: poll failed, retrying: %v", msg.err)
		return m, nil
	}
	m.threads = msg.threads
	rows := m.conversationRows()
	switch {
	case len(rows) == 0:
		m.conversation = 0
	case keyIndex(rows, convKey, m.conversation) < 0:
		m.conversation = rows[0].ID
	}
	// FOR YOU selection default (spec iteration-5): the SAME courtesy as the
	// conversation list's own default above — an operator who Tabs onto the
	// section lands on a valid row instead of needing an extra keypress just
	// to snap the cursor onto one.
	switch fyRows := m.forYouRows(); {
	case len(fyRows) == 0:
		m.forYou = 0
	case keyIndex(fyRows, forYouKey, m.forYou) < 0:
		m.forYou = fyRows[0].ID
	}
	if m.conversation != 0 && m.focus != focusConvRight && !m.viewLoading {
		return m.conversationPreviewCmd(m.conversation, true)
	}
	return m, nil
}

// applyThreadPage applies one get_thread page. A page that resolves after
// the reader moved on to a different thread, OR that belongs to a PREVIOUS
// load of the very same thread ID (msg.gen != m.viewGen — spec §5
// carried-over fix: threadPageMsg staleness, mirrors pollGen), is discarded.
// Older pages are PREPENDED (the lazy "load older" fetch) rather than
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
	if msg.threadID != m.viewThreadID || msg.gen != m.viewGen {
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
		if m.viewCursor > len(m.viewEntries)-1 {
			m.viewCursor = len(m.viewEntries) - 1
		}
		if m.viewCursor < 0 {
			m.viewCursor = 0
		}
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

// dispLabel resolves an alias for display exactly like render.Renderer.disp
// (the station-local peer copy: render's disp/dispTarget are unexported, so
// panes outside the activity view re-derive the same alias-fallback rule
// from the labels map applyAgents already builds).
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

// intentRowTag maps a thread's effective intent to a conversation row's tag
// — the station-local peer of render's unexported intentTag, same
// vocabulary.
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

// relativeAge renders the gap between now and atMillis (a ms-epoch
// timestamp) as a short duration ("Ns"/"Nm"/"Nh"/"Nd") for a conversation
// row's "last speaker + age" column. atMillis <= 0 (no last entry yet)
// renders "".
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

// agentActivity filters the global events buffer down to rows concerning
// alias (spec §5-REVISED L1.5: "their recent journal activity (list_events
// agent filter)" — implemented as a client-side filter over the SAME
// cursor-disciplined buffer the data loop already maintains, rather than a
// second fetch/cursor scheme, so "activity is always scoped to a WHO" reuses
// spec §5's reviewed plumbing instead of duplicating it).
func agentActivity(events []render.EventRow, alias string) []render.EventRow {
	var out []render.EventRow
	for _, e := range events {
		if e.Agent == alias || e.Target == "agent:"+alias {
			out = append(out, e)
		}
	}
	return out
}
