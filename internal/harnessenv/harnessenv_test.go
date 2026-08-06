package harnessenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFromHookPayloadPrecedence(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "env-uuid")
	c := FromHookPayload([]byte(`{"session_id":"payload-uuid","cwd":"/tmp/payload-dir"}`))
	if c.SessionID != "payload-uuid" || c.CWD != "/tmp/payload-dir" {
		t.Fatalf("payload must win over env, got %+v", c)
	}
}

func TestFromHookPayloadFallsBackToEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "env-uuid")
	for _, payload := range []string{"", "not json", "{}"} {
		c := FromHookPayload([]byte(payload))
		if c.SessionID != "env-uuid" {
			t.Fatalf("payload %q: expected env fallback, got %+v", payload, c)
		}
	}
}

func TestAlias(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		{"/Users/x/GitHub/muster", "muster"},
		{"/Users/x/repo/.claude/worktrees/paneless-agents", "paneless-agents"},
		{"/", ""},
		{"", ""},
	} {
		if got := (Capture{CWD: tc.cwd}).Alias(); got != tc.want {
			t.Fatalf("Alias(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

func TestProjectMainCheckout(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "myproj")
	sub := filepath.Join(main, "internal", "deep")
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := (Capture{CWD: sub}).Project(); got != "myproj" {
		t.Fatalf("Project from subdir = %q, want myproj", got)
	}
	if got := (Capture{CWD: root}).Project(); got != "" {
		t.Fatalf("Project outside any checkout = %q, want empty", got)
	}
}

func TestProjectLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "myproj")
	wt := filepath.Join(main, ".claude", "worktrees", "feat-x")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := "gitdir: " + filepath.Join(main, ".git", "worktrees", "feat-x") + "\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(gitdir), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (Capture{CWD: wt}).Project(); got != "myproj" {
		t.Fatalf("Project from linked worktree = %q, want myproj", got)
	}
}

func TestFromHookPayloadCapturesTranscriptPath(t *testing.T) {
	c := FromHookPayload([]byte(`{"session_id":"u1","cwd":"/w","transcript_path":"/tmp/t.jsonl"}`))
	if c.TranscriptPath != "/tmp/t.jsonl" {
		t.Fatalf("TranscriptPath = %q, want /tmp/t.jsonl", c.TranscriptPath)
	}
}

func TestCustomTitleLastRecordWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"type":"custom-title","customTitle":"old-name","sessionId":"u1"}`,
		`{"type":"user","message":{"role":"user","content":"body mentioning custom-title"}}`,
		`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u1"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CustomTitle(path); got != "nfl-3" {
		t.Fatalf("CustomTitle = %q, want nfl-3", got)
	}
}

func TestCustomTitleAbsentOrUnreadable(t *testing.T) {
	if got := CustomTitle(""); got != "" {
		t.Fatalf("empty path: got %q", got)
	}
	if got := CustomTitle(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Fatalf("missing file: got %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "no-title.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CustomTitle(path); got != "" {
		t.Fatalf("no record: got %q", got)
	}
}

func TestIsTeammateDetectsMemberTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "member.jsonl")
	lines := []string{
		`{"type":"mode","mode":"normal","sessionId":"u1"}`,
		`{"type":"permission-mode","permissionMode":"auto","sessionId":"u1"}`,
		`{"parentUuid":null,"isSidechain":false,"teamName":"session-b41c21dd","agentName":"l5-mlb-measure","type":"user","message":{"role":"user","content":"go"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsTeammate(path) {
		t.Fatal("teamName-bearing transcript must read as teammate")
	}
}

func TestIsTeammateFalseForPrimariesAndLeads(t *testing.T) {
	dir := t.TempDir()
	// a lead/primary: custom-title + agent-name records but NO teamName —
	// the spec's verified shape (an agent-name record alone must not match)
	path := filepath.Join(dir, "primary.jsonl")
	lines := []string{
		`{"type":"custom-title","customTitle":"nfl-3","sessionId":"u2"}`,
		`{"type":"agent-name","agentName":"nfl-3","sessionId":"u2"}`,
		`{"type":"user","message":{"role":"user","content":"body mentioning teamName in prose"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsTeammate(path) {
		t.Fatal("primary transcript must not read as teammate")
	}
}

func TestIsTeammateFailOpenAndBounded(t *testing.T) {
	if IsTeammate("") {
		t.Fatal("empty path must be false")
	}
	if IsTeammate(filepath.Join(t.TempDir(), "missing.jsonl")) {
		t.Fatal("missing file must be false")
	}
	// teamName appearing only AFTER line 30 does not match: the signal
	// sits in the first few lines by construction, and an unbounded scan
	// would false-positive on conversation text echoing transcripts.
	dir := t.TempDir()
	path := filepath.Join(dir, "late.jsonl")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"x"}}`)
	}
	lines = append(lines, `{"teamName":"t","agentName":"a","type":"user"}`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsTeammate(path) {
		t.Fatal("teamName beyond line 30 must not match (bounded scan)")
	}
}
