package cli

import (
	"strings"
	"testing"
)

func agents() []enrichedAgent {
	return []enrichedAgent{
		{agentRow: agentRow{Alias: "muster", Project: "muster"}, Live: true, EffLabel: "backend", EffManual: true},
		{agentRow: agentRow{Alias: "muster-2", Project: "muster"}, Live: true, EffLabel: "frontend", EffManual: true},
		{agentRow: agentRow{Alias: "timewalk", Project: "timewalk"}, Live: true, EffLabel: "frontend", EffManual: true},
		{agentRow: agentRow{Alias: "auto1", Project: "muster"}, Live: true, EffLabel: "some topic", EffManual: false},
	}
}

func TestResolveExactAliasWins(t *testing.T) {
	got, err := ResolveTarget(agents(), "timewalk", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveBareLabelInCallerProject(t *testing.T) {
	got, err := ResolveTarget(agents(), "frontend", "muster")
	if err != nil || got != "muster-2" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveBareLabelCrossProjectIsError(t *testing.T) {
	// caller in "scratch": "frontend" exists only in muster & timewalk → must error, not guess
	if _, err := ResolveTarget(agents(), "frontend", "scratch"); err == nil {
		t.Fatal("want error for cross-project bare label")
	}
}

func TestResolveQualified(t *testing.T) {
	got, err := ResolveTarget(agents(), "timewalk:frontend", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveAutoTopicNotAddressable(t *testing.T) {
	if _, err := ResolveTarget(agents(), "some topic", "muster"); err == nil {
		t.Fatal("auto (non-manual) labels must not be addressable")
	}
}

func TestResolveUnknown(t *testing.T) {
	if _, err := ResolveTarget(agents(), "nope", "muster"); err == nil {
		t.Fatal("want unknown-agent error")
	}
}

// TestResolveViaExpandsLocalFirstAgainstARoster is the resolveVia-level round
// trip closing the gap the task brief describes: `muster register` prints a
// short hint like `muster inbox 'dotfiles/main'`, and this proves that short
// name actually resolves — to the LOCAL row, not a foreign one of the same
// bare name sitting right next to it in the same roster. This is the
// resolveVia counterpart of TestExpandAliasPrefersTheLocalRow, exercised
// through the real callData → daemon → list_agents path rather than a fake
// exists func.
func TestResolveViaExpandsLocalFirstAgainstARoster(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	for _, a := range []string{"personal-dotfiles/main", "dotfiles/main"} {
		if _, err := callData("register_agent", map[string]any{"alias": a}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveVia("dotfiles/main")
	if err != nil {
		t.Fatal(err)
	}
	if got != "personal-dotfiles/main" {
		t.Fatalf("resolveVia(%q) = %q, want the local row %q", "dotfiles/main", got, "personal-dotfiles/main")
	}
}

// TestResolveViaUnknownNameSurfacesTheOperatorsOwnInput: a typo that expands
// to nothing must still error naming what was ACTUALLY typed, not a
// device-prefixed string the operator never wrote — expandAlias's unknown
// name returns unchanged, and resolve.Target's error interpolates given
// directly (see internal/resolve.go's "no agent or addressable label %q").
func TestResolveViaUnknownNameSurfacesTheOperatorsOwnInput(t *testing.T) {
	startTestDaemon(t)
	t.Setenv("MUSTER_DEVICE_NAME", "personal")
	if _, err := callData("register_agent", map[string]any{"alias": "personal-dotfiles/main"}); err != nil {
		t.Fatal(err)
	}
	_, err := resolveVia("dotfiles/typo")
	if err == nil {
		t.Fatal("want unknown-agent error")
	}
	if !strings.Contains(err.Error(), `"dotfiles/typo"`) {
		t.Fatalf("error must name the operator's own typed input, got %v", err)
	}
	if strings.Contains(err.Error(), "personal-dotfiles/typo") {
		t.Fatalf("error must not surface a prefixed string the operator never wrote, got %v", err)
	}
}
