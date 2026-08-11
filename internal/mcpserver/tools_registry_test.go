package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// TestMCPRegistrationSeedsTheModelSuppliedAlias pins device.SeedMinted's own
// contract in isolation — it calls SeedMinted directly and would pass even if
// registerAgentHandler never called it. The wiring into the two MCP mint
// sites is what actually closes the hole this task exists for (a model
// registering the same name from two machines taking the other machine's row
// and its inbox with it); that wiring is covered separately by
// TestRegisterAgentFreshPaneRegisters (register_agent),
// TestRegisterAgentBecomeClaimsThroughPaneGuard (become), and
// TestRegisterAgentBareAliasMatchesSeededRow (the already-registered guard).
func TestMCPRegistrationSeedsTheModelSuppliedAlias(t *testing.T) {
	t.Setenv("MUSTER_HOME", t.TempDir())
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if got, want := device.SeedMinted("researcher"), "personal-researcher"; got != want {
		t.Fatalf("mint seeding = %q, want %q", got, want)
	}
}

func TestRegisterAgentCapturesTmuxEnv(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the mint site's device.Adopt() from this machine's real config
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$5", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "backend\x1f1", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(_ string, args map[string]any) (json.RawMessage, error) {
		got = args
		return []byte(`{}`), nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, _, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{Alias: "backend", Role: "producer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["socket_path"] != "/private/tmp/tmux-501/proj-muster" || got["session_id"] != "$5" ||
		got["project"] != "muster" || got["label"] != "backend" || got["label_manual"] != true {
		t.Fatalf("captured args = %+v", got)
	}
}

func TestRegisterAgentIdempotentForRegisteredPane(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the guard's seeded comparison from this machine's real config
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "500", nil // same incarnation as the roster row below
		}
		return "$1", nil // session-id probe (and other queries)
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":500,"label":"standard 2000","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2002", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if registered {
		t.Fatal("must NOT mint a second alias for an already-registered pane")
	}
	if !out.OK || !strings.Contains(out.Detail, "already registered as 'timewalk-2'") || !strings.Contains(out.Detail, "standard 2000") {
		t.Fatalf("expected identity-bearing detail, got %+v", out)
	}
}

// TestRegisterAgentGhostSessionCreatedRegisters covers the ghost-guard: a
// roster row can match the calling pane's (socket_path, session_id, pane_id)
// tuple yet be a leftover from a DEAD server incarnation — tmux recycles
// session IDs from $0 after a server restart. When the row's recorded
// session_created disagrees with the caller's live one, paneRegistration must
// NOT treat it as this pane's own registration: the handler must register
// fresh, not short-circuit as "already registered" under the ghost's alias.
func TestRegisterAgentGhostSessionCreatedRegisters(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the mint site's device.Adopt() from this machine's real config
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "222", nil // caller's LIVE session incarnation
		}
		return "$1", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			// same tuple as the caller, but a STALE incarnation (111 != 222):
			// a ghost left by a session ID tmux recycled after a restart.
			return json.RawMessage(`[{"alias":"ghost-agent","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":111,"label":"old label","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "fresh-agent", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !registered {
		t.Fatal("a tuple match with a mismatched session_created is a ghost — must register fresh, not short-circuit")
	}
	if strings.Contains(out.Detail, "already registered") {
		t.Fatalf("must not report the ghost row's alias as this session's identity, got %+v", out)
	}
}

func TestRegisterAgentSameAliasStillUpserts(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the mint site's device.Adopt() from this machine's real config
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			// The stored row's alias is the SEEDED form — the only form the
			// MCP mint path can ever write. A bare fixture here would no
			// longer cover the production case: see
			// TestRegisterAgentBareAliasMatchesSeededRow for that guard.
			return json.RawMessage(`[{"alias":"personal-timewalk-2","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "timewalk-2", ModelType: "claude"})
	if err != nil || !out.OK {
		t.Fatalf("same-alias re-register must succeed: %+v %v", out, err)
	}
	if !registered {
		t.Fatal("same-alias call must still upsert (refresh)")
	}
}

// TestRegisterAgentBareAliasMatchesSeededRow covers the realistic already-
// registered case the MCP mint path actually produces: the stored row's alias
// is SEEDED ("personal-researcher"), but a model re-registering quotes the
// same bare name it originally supplied ("researcher") — which is exactly
// what the tool's own description invites ("Calling it from an
// already-registered pane returns your existing identity"). The guard must
// compare against the seeded form of in.Alias, not the bare form, or every
// row this path creates becomes permanently unmatchable by its own pane: the
// call falls through to the refusal branch instead of the upsert/refresh
// path, losing the revival/unread report.
func TestRegisterAgentBareAliasMatchesSeededRow(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var registered bool
	var becomeCalled bool
	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[{"alias":"personal-researcher","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%6","departed":false}]`), nil
		case "register_agent":
			registered = true
			return json.RawMessage(`{"outcome":"revived","unread":2}`), nil
		case "become":
			becomeCalled = true
			return json.RawMessage(`{}`), nil
		}
		t.Fatalf("unexpected op %s", op)
		return nil, nil
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{Alias: "researcher", Role: "producer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if becomeCalled {
		t.Fatal("bare alias matching the seeded stored row must take the upsert path, not become")
	}
	if !registered {
		t.Fatal("bare alias matching the seeded stored row must upsert/refresh via register_agent, not refuse")
	}
	if strings.Contains(out.Detail, "already registered") {
		t.Fatalf("Detail = %q, must not refuse a pane re-registering its own seeded alias by its bare form", out.Detail)
	}
	if !strings.Contains(out.Detail, "revived") || !strings.Contains(out.Detail, "2 unread") {
		t.Fatalf("Detail = %q, want revival + unread notice reported for the existing identity", out.Detail)
	}
}

// TestRegisterAgentStampsHarnessLinkAndReportsRevival covers the durable-alias
// spec's MCP surface: the handler forwards the ambient harness session UUID
// (so hook reclaim can find this row after a resume), and folds the daemon's
// outcome/unread into the Detail so a re-registering agent learns its backlog.
func TestRegisterAgentStampsHarnessLinkAndReportsRevival(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "uuid-7")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the mint site's device.Adopt() from this machine's real config
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		switch args[len(args)-1] {
		case "#{session_id}":
			return "$5", nil
		case "#{session_name}":
			return "muster-2", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var got map[string]any
	prevDaemon := callDaemon
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		if op == "register_agent" {
			got = args
			return []byte(`{"outcome":"revived","unread":3}`), nil
		}
		return []byte(`[]`), nil // paneRegistration's list_agents probe
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{Alias: "backend", Role: "peer", ModelType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got["harness_session_id"] != "uuid-7" {
		t.Fatalf("harness_session_id = %v, want uuid-7", got["harness_session_id"])
	}
	if !strings.Contains(out.Detail, "revived") || !strings.Contains(out.Detail, "3 unread") {
		t.Fatalf("Detail = %q, want revival + unread notice", out.Detail)
	}
	// Both the revived-outcome and unread-suffix branches of the Detail must
	// name the SEEDED alias, not the bare model-supplied one — a model
	// quoting either back (e.g. into get_inbox) must land on the stored row.
	if !strings.Contains(out.Detail, "reconnected as 'personal-backend'") {
		t.Fatalf("Detail = %q, want the revived branch to report the seeded alias 'personal-backend'", out.Detail)
	}
	if !strings.Contains(out.Detail, "call get_inbox with alias 'personal-backend'") {
		t.Fatalf("Detail = %q, want the unread branch to report the seeded alias 'personal-backend'", out.Detail)
	}
}

func TestRegisterAgentFreshPaneRegisters(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the mint site's device.Adopt() from this machine's real config
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(_ ...string) (string, error) { return "$1", nil }
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	var registered bool
	var got map[string]any
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "list_agents":
			return json.RawMessage(`[]`), nil
		case "register_agent":
			registered = true
			got = args
			return json.RawMessage(`null`), nil
		}
		return nil, nil
	}
	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "fresh", ModelType: "claude"})
	if err != nil || !out.OK || !registered {
		t.Fatalf("fresh pane must register: registered=%v out=%+v err=%v", registered, out, err)
	}
	// register_agent is the second MCP mint site: the model-supplied alias
	// must be seeded with the device name before it reaches the daemon, and
	// the reply text must report that same seeded alias — not the bare one —
	// so a model quoting it back on another machine addresses the right row.
	if got["alias"] != "personal-fresh" {
		t.Fatalf("register_agent alias = %v, want seeded 'personal-fresh'", got["alias"])
	}
	if !strings.Contains(out.Detail, "personal-fresh") {
		t.Fatalf("Detail = %q, want the seeded alias 'personal-fresh'", out.Detail)
	}
}

// TestRegisterAgentBecomeClaimsThroughPaneGuard: an already-registered pane
// calling register_agent with become:true issues the become op instead of
// the refusal, and the Detail reports the trade.
//
// It also covers the become path's mint site: the model supplies a bare
// alias ("alias-routing"), which must be seeded with the device name before
// it reaches the daemon's "to" argument and before it appears in the reply
// text — the same rule TestRegisterAgentFreshPaneRegisters pins for the
// register_agent mint site.
func TestRegisterAgentBecomeClaimsThroughPaneGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	var becomeArgs map[string]any
	prevDaemon := callDaemon
	callDaemon = func(op string, args map[string]any) (json.RawMessage, error) {
		switch op {
		case "become":
			becomeArgs = args
			return []byte(`{"from":"muster-2","to":"personal-alias-routing","unread":2}`), nil
		default: // paneRegistration's roster probe: this pane already owns muster-2
			return []byte(`[{"alias":"muster-2","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%6"}]`), nil
		}
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{
		Alias: "alias-routing", Role: "peer", ModelType: "claude", Become: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if becomeArgs["from"] != "muster-2" || becomeArgs["to"] != "personal-alias-routing" {
		t.Fatalf("become args = %+v, want to = seeded 'personal-alias-routing'", becomeArgs)
	}
	if !strings.Contains(out.Detail, "you are now 'personal-alias-routing' (was 'muster-2')") ||
		!strings.Contains(out.Detail, "2 unread") {
		t.Fatalf("Detail = %q", out.Detail)
	}
}

// TestRegisterAgentRefusalAdvertisesBecome: the become:false refusal now
// tells the agent how to claim instead of dead-ending. The advertised alias
// must be the SEEDED form, not the model's bare input: it is what the model
// is told to pass as become:true's target, and the become path itself mints
// through device.SeedMinted — advertising the bare form would send the model
// back with an alias that mismatches what become actually claims.
func TestRegisterAgentRefusalAdvertisesBecome(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("MUSTER_DEVICE_NAME", "personal") // isolate the refusal's seeded quoting from this machine's real config
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "become":
			t.Fatalf("must not issue become op when become:false")
			return nil, nil
		default: // paneRegistration's roster probe: this pane already owns muster-2
			return []byte(`[{"alias":"muster-2","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%6"}]`), nil
		}
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{
		Alias: "alias-routing", Role: "peer", ModelType: "claude", Become: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Detail, "pass become:true to claim 'personal-alias-routing'") {
		t.Fatalf("Detail = %q, want become-advertisement with the seeded alias", out.Detail)
	}
}

// TestListAgentsCarriesAddressableLabel pins the roster against the resolver
// (internal/resolve): an address is BUILT from project + label — the
// "proj:label" and bare-label rungs below exact-alias — and a label is
// addressable only while it is manually pinned on a live row. list_agents
// must therefore carry all four fields, or an MCP agent sees aliases alone
// and concludes a working address does not exist. That is not hypothetical:
// a live session whose operator had labeled it "nfl-3" reported to that
// operator that "nfl-3" was a dead address and offered to retire its durable
// alias to fix it, because list_agents could not show the label that was
// already routing mail to it.
func TestListAgentsCarriesAddressableLabel(t *testing.T) {
	startTestDaemon(t)
	for _, a := range []map[string]any{
		{"alias": "bettor-help-workspace-2", "project": "bettor-help-workspace", "label": "nfl-3", "label_manual": true},
		{"alias": "bettor-help-workspace-4", "project": "bettor-help-workspace", "label": "debug alarms", "label_manual": false},
		{"alias": "bettor-help-workspace-5", "project": "bettor-help-workspace", "label": "corpus-rebuild", "label_manual": true},
	} {
		if _, err := callDaemon("register_agent", a); err != nil {
			t.Fatalf("register %v: %v", a["alias"], err)
		}
	}
	if _, err := callDaemon("deregister_agent", map[string]any{"alias": "bettor-help-workspace-5"}); err != nil {
		t.Fatalf("deregister: %v", err)
	}

	_, out, err := listAgentsHandler(context.TODO(), nil, ListAgentsIn{})
	if err != nil {
		t.Fatalf("list_agents: %v", err)
	}
	byAlias := map[string]AgentView{}
	for _, ag := range out.Agents {
		byAlias[ag.Alias] = ag
	}

	// The addressable one: every field a caller needs to write
	// "bettor-help-workspace:nfl-3" (or bare "nfl-3" from inside the project).
	live := byAlias["bettor-help-workspace-2"]
	if live.Project != "bettor-help-workspace" || live.Label != "nfl-3" || !live.LabelManual || live.Departed {
		t.Fatalf("addressable row = %+v, want project/label carried with label_manual set and not departed", live)
	}
	// An auto-generated label is display-only — the resolver skips it, so the
	// roster must say so rather than let a caller address it.
	auto := byAlias["bettor-help-workspace-4"]
	if auto.Label != "debug alarms" || auto.LabelManual {
		t.Fatalf("auto-labeled row = %+v, want label carried with label_manual false", auto)
	}
	// A tombstone keeps its alias addressable (mail waits) but not its label;
	// without Departed the caller cannot tell the two apart.
	gone := byAlias["bettor-help-workspace-5"]
	if !gone.Departed || gone.Label != "corpus-rebuild" {
		t.Fatalf("departed row = %+v, want departed true with its label still visible", gone)
	}
}

// TestRegisterAgentAlreadyRegisteredDetailKeepsFullAlias guards the
// "already registered as '%s'" detail string registerAgentHandler builds
// from row.Alias — the pane's existing registration. This string tells a
// model what alias to address itself as, so it is subject to the same
// human/model asymmetry as list_agents and get_inbox/get_thread (see
// TestModelSurfacesKeepTheFullAlias): a short form here would be echoed by
// the model and could re-resolve against a different device's own prefix
// when read elsewhere. Human CLI/TUI surfaces strip; this MCP tool detail
// must not — deliberately, not by oversight.
func TestRegisterAgentAlreadyRegisteredDetailKeepsFullAlias(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%14")
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	prevCall := callDaemon
	t.Cleanup(func() { callDaemon = prevCall })
	prevRun := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_created}" {
			return "500", nil // same incarnation as the roster row below
		}
		return "$1", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prevRun })

	const fullAlias = "personal-work/backend"
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "list_agents" {
			t.Fatalf("unexpected op %s", op)
		}
		return json.RawMessage(`[{"alias":"` + fullAlias + `","model_type":"claude","socket_path":"/tmp/sock","pane_id":"%14","session_id":"$1","session_created":500,"departed":false}]`), nil
	}

	_, out, err := registerAgentHandler(context.Background(), nil, RegisterAgentIn{Alias: "personal-work/frontend", ModelType: "claude"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(out.Detail, "already registered as '"+fullAlias+"'") {
		t.Fatalf("Detail = %q, want the full stored alias %q", out.Detail, fullAlias)
	}
	short := device.Strip("personal", fullAlias)
	if strings.Contains(out.Detail, "'"+short+"'") {
		t.Fatalf("Detail = %q, was device-stripped to %q; model surfaces must carry the full alias", out.Detail, short)
	}
}

// TestRegisterAgentBecomeDetailKeepsFullAlias guards the "you are now '%s'
// (was '%s'); ... call get_inbox with alias '%s'" detail string the become
// path builds from trade.To/trade.From. Same rationale as
// TestRegisterAgentAlreadyRegisteredDetailKeepsFullAlias: this instructs the
// model what alias to use going forward, including in a follow-up tool call,
// so it must carry the full device-prefixed alias even though the human
// surfaces for the same identity render short.
func TestRegisterAgentBecomeDetailKeepsFullAlias(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("TMUX_PANE", "%6")
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	prev := tmuxenv.Run
	tmuxenv.Run = func(args ...string) (string, error) {
		if args[len(args)-1] == "#{session_id}" {
			return "$1", nil
		}
		return "", nil
	}
	t.Cleanup(func() { tmuxenv.Run = prev })

	const fromFull, toFull = "personal-work/backend", "personal-work/frontend"
	prevDaemon := callDaemon
	callDaemon = func(op string, _ map[string]any) (json.RawMessage, error) {
		switch op {
		case "become":
			return []byte(`{"from":"` + fromFull + `","to":"` + toFull + `","unread":2}`), nil
		default: // paneRegistration's roster probe: this pane already owns fromFull
			return []byte(`[{"alias":"` + fromFull + `","socket_path":"/tmp/sock","session_id":"$1","pane_id":"%6"}]`), nil
		}
	}
	t.Cleanup(func() { callDaemon = prevDaemon })

	_, out, err := registerAgentHandler(context.TODO(), nil, RegisterAgentIn{
		Alias: toFull, Role: "peer", ModelType: "claude", Become: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Detail, "you are now '"+toFull+"' (was '"+fromFull+"')") {
		t.Fatalf("Detail = %q, want full 'you are now'/'was' aliases", out.Detail)
	}
	if !strings.Contains(out.Detail, "call get_inbox with alias '"+toFull+"'") {
		t.Fatalf("Detail = %q, want the full alias in the get_inbox instruction", out.Detail)
	}
	shortFrom, shortTo := device.Strip("personal", fromFull), device.Strip("personal", toFull)
	if strings.Contains(out.Detail, "'"+shortFrom+"'") || strings.Contains(out.Detail, "'"+shortTo+"'") {
		t.Fatalf("Detail = %q, was device-stripped; model surfaces must carry the full alias", out.Detail)
	}
}
