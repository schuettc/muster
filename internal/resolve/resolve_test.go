package resolve

import (
	"strings"
	"testing"
)

func candidates() []Candidate {
	return []Candidate{
		{Alias: "muster", Project: "muster", Label: "backend", LabelManual: true},
		{Alias: "muster-2", Project: "muster", Label: "frontend", LabelManual: true},
		{Alias: "timewalk", Project: "timewalk", Label: "frontend", LabelManual: true},
		{Alias: "auto1", Project: "muster", Label: "some topic", LabelManual: false},
		{Alias: "gone-1", Project: "muster", Label: "datalake", LabelManual: true, Departed: true},
	}
}

func TestExactAliasWins(t *testing.T) {
	got, err := Target(candidates(), "timewalk", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestBareLabelInCallerProject(t *testing.T) {
	got, err := Target(candidates(), "frontend", "muster")
	if err != nil || got != "muster-2" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestBareLabelCrossProjectIsError(t *testing.T) {
	if _, err := Target(candidates(), "frontend", "scratch"); err == nil {
		t.Fatal("want error for cross-project bare label")
	}
}

func TestQualified(t *testing.T) {
	got, err := Target(candidates(), "timewalk:frontend", "muster")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestAutoTopicNotAddressable(t *testing.T) {
	if _, err := Target(candidates(), "some topic", "muster"); err == nil {
		t.Fatal("auto (non-manual) labels must not be addressable")
	}
}

func TestUnknown(t *testing.T) {
	if _, err := Target(candidates(), "nope", "muster"); err == nil {
		t.Fatal("want unknown-target error")
	}
}

// TestDepartedAliasStillResolves: a departed row's ALIAS remains addressable
// — mail may be waiting for it to come back.
func TestDepartedAliasStillResolves(t *testing.T) {
	got, err := Target(candidates(), "gone-1", "muster")
	if err != nil || got != "gone-1" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// TestDepartedLabelNotAddressable: a departed row's LABEL must NOT resolve,
// even though the exact same string would resolve for a live agent —
// otherwise a brand-new message could land on a thread nobody will ever
// collect.
func TestDepartedLabelNotAddressable(t *testing.T) {
	if _, err := Target(candidates(), "datalake", "muster"); err == nil {
		t.Fatal("a departed agent's label must not be addressable")
	}
}

// TestDepartedLabelExcludedFromQualifiedMatch mirrors the above for the
// proj:label form.
func TestDepartedLabelExcludedFromQualifiedMatch(t *testing.T) {
	if _, err := Target(candidates(), "muster:datalake", "muster"); err == nil {
		t.Fatal("a departed agent's qualified label must not be addressable")
	}
}

func TestAmbiguityListsCandidates(t *testing.T) {
	cs := append(candidates(), Candidate{Alias: "muster-3", Project: "muster", Label: "frontend", LabelManual: true})
	_, err := Target(cs, "frontend", "muster")
	if err == nil {
		t.Fatal("want ambiguity error")
	}
	if !strings.Contains(err.Error(), "muster-2") || !strings.Contains(err.Error(), "muster-3") {
		t.Fatalf("expected ambiguity error to list both candidates, got %q", err.Error())
	}
}

func TestAliasExactResolvesRegisteredAliasOnly(t *testing.T) {
	got, err := AliasExact(candidates(), "timewalk")
	if err != nil || got != "timewalk" {
		t.Fatalf("got %q err %v", got, err)
	}
	// A departed alias still resolves under AliasExact too.
	if got, err := AliasExact(candidates(), "gone-1"); err != nil || got != "gone-1" {
		t.Fatalf("got %q err %v", got, err)
	}
	// But a label — even a manually-pinned live one — never does.
	if _, err := AliasExact(candidates(), "frontend"); err == nil {
		t.Fatal("AliasExact must not match labels")
	}
}
