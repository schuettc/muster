package harnessenv

import (
	"os"
	"path/filepath"
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
