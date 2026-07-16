package station

import (
	"encoding/json"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/schuettc/muster/internal/render"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// tickMsg fires every --interval; Update's tickMsg branch re-issues the
// three fetch Cmds and reschedules the next tick.
type tickMsg time.Time

// eventsMsg carries one list_events follow-mode page (or a fetch error).
type eventsMsg struct {
	page render.EventsPage
	err  error
}

// agentsMsg carries one enriched roster snapshot (or a fetch error).
type agentsMsg struct {
	rows []agentEnriched
	err  error
}

// threadsMsg carries one list_threads snapshot (or a fetch error).
type threadsMsg struct {
	threads []listThreadRow
	err     error
}

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// eventFetchLimit mirrors `muster watch`'s follow-mode call (limit 0 = the
// daemon's own cap) — station's tick fetch is a follow-mode poll exactly
// like watch's, just issued from a tea.Cmd instead of a for-loop.
const eventFetchLimit = 0

func fetchEventsCmd(caller render.Caller, cursor int64) tea.Cmd {
	return func() tea.Msg {
		page, err := render.FetchEvents(caller, "", "", 0, cursor, eventFetchLimit)
		return eventsMsg{page: page, err: err}
	}
}

// agentRow mirrors the list_agents wire JSON — station is a peer client of
// the daemon, like humancli (see internal/humancli.agentRow); it decodes its
// own copy rather than importing internal/humancli or internal/store.
type agentRow struct {
	Alias       string `json:"alias"`
	Role        string `json:"role"`
	ModelType   string `json:"model_type"`
	SocketPath  string `json:"socket_path"`
	SessionID   string `json:"session_id"`
	Project     string `json:"project"`
	Label       string `json:"label"`
	LabelManual bool   `json:"label_manual"`
}

func fetchAgentsCmd(caller render.Caller) tea.Cmd {
	return func() tea.Msg {
		rows, err := fetchAgents(caller)
		return agentsMsg{rows: rows, err: err}
	}
}

// sessionUnreadCount caches one (socket_path, session_id) tuple's
// session_unread result across the agent rows that share it (spec §5:
// "per-tuple unread" — a session with several sibling aliases must not be
// queried once per alias).
type sessionUnreadCount struct{ total, action int }

// fetchAgents lists agents, overlays live tmux state (liveness + current
// label, exactly like `muster agents`), and looks up each distinct live
// session tuple's unread count once.
func fetchAgents(caller render.Caller) ([]agentEnriched, error) {
	raw, err := caller.Call("list_agents", nil)
	if err != nil {
		return nil, err
	}
	var rows []agentRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	cache := map[[2]string]sessionUnreadCount{}
	out := make([]agentEnriched, 0, len(rows))
	for _, a := range rows {
		e := agentEnriched{
			Alias: a.Alias, Project: a.Project, ModelType: a.ModelType,
			Label: a.Label, LabelManual: a.LabelManual,
			SocketPath: a.SocketPath, SessionID: a.SessionID,
		}
		e.Live = tmuxenv.IsSessionAlive(a.SocketPath, a.SessionID)
		if e.Live {
			e.Label, e.LabelManual = tmuxenv.SessionLabel(a.SocketPath, a.SessionID)
		}
		if a.SocketPath != "" && a.SessionID != "" {
			key := [2]string{a.SocketPath, a.SessionID}
			u, ok := cache[key]
			if !ok {
				if total, action, uerr := sessionUnread(caller, a.SocketPath, a.SessionID); uerr == nil {
					u = sessionUnreadCount{total, action}
					cache[key] = u
				}
			}
			e.Unread, e.Action = u.total, u.action > 0
		}
		out = append(out, e)
	}
	return out, nil
}

func sessionUnread(caller render.Caller, socketPath, sessionID string) (total, action int, err error) {
	raw, err := caller.Call("session_unread", map[string]any{"socket_path": socketPath, "session_id": sessionID})
	if err != nil {
		return 0, 0, err
	}
	var res struct {
		Total  int `json:"total"`
		Action int `json:"action"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, 0, err
	}
	return res.Total, res.Action, nil
}

// threadListLimit is the page size station's tick fetch passes to
// list_threads — generous enough for one screen's worth of rows without
// pulling the whole table every second.
const threadListLimit = 200

func fetchThreadsCmd(caller render.Caller) tea.Cmd {
	return func() tea.Msg {
		threads, err := fetchThreads(caller)
		return threadsMsg{threads: threads, err: err}
	}
}

func fetchThreads(caller render.Caller) ([]listThreadRow, error) {
	raw, err := caller.Call("list_threads", map[string]any{"limit": threadListLimit})
	if err != nil {
		return nil, err
	}
	var res struct {
		Threads []listThreadRow `json:"threads"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Threads, nil
}
