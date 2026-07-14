package humancli

import "testing"

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
