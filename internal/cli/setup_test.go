package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// setupHome points setup at a fresh temp HOME and a PATH with no `kempt` on
// it, so the direct-apply path is exercised by default. It returns the home
// dir. (This is not a unix-socket test, so t.TempDir's long path is fine.)
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Isolate PATH so exec.LookPath("kempt") fails unless a test opts in.
	t.Setenv("PATH", t.TempDir())
	return home
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func runSetup(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := cmdSetup(args, &buf); err != nil {
		t.Fatalf("cmdSetup(%v): %v", args, err)
	}
	return buf.String()
}

func TestSetupDetectClaudeOnly(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("claude configured: ~/.claude.json missing")
	}
	if dirExists(filepath.Join(home, ".codex")) || dirExists(filepath.Join(home, ".cursor")) {
		t.Fatal("undetected agents should not be created")
	}
}

func TestSetupApplyClaude(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	// MCP into ~/.claude.json
	claudeJSON := readJSON(t, filepath.Join(home, ".claude.json"))
	mcp, _ := claudeJSON["mcpServers"].(map[string]any)
	if mcp == nil {
		t.Fatalf("mcpServers missing: %v", claudeJSON)
	}
	wantMuster := map[string]any{
		"type": "stdio", "command": "muster",
		"args": []any{"mcp"}, "env": map[string]any{},
	}
	if got := mcp["muster"]; !reflect.DeepEqual(got, wantMuster) {
		t.Errorf("mcpServers.muster = %#v, want %#v", got, wantMuster)
	}
	wantChannel := map[string]any{
		"type": "stdio", "command": "muster",
		"args": []any{"channel"}, "env": map[string]any{},
	}
	if got := mcp["muster-channel"]; !reflect.DeepEqual(got, wantChannel) {
		t.Errorf("mcpServers.muster-channel = %#v, want %#v", got, wantChannel)
	}

	// Hooks + permission into ~/.claude/settings.json
	settings := readJSON(t, filepath.Join(home, ".claude", "settings.json"))
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil || !reflect.DeepEqual(perms["allow"], []any{"mcp__muster"}) {
		t.Errorf("permissions.allow = %#v", settings["permissions"])
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("hooks missing: %v", settings)
	}
	for _, ev := range []string{"SessionStart", "Stop", "SessionEnd"} {
		if _, ok := hooks[ev].([]any); !ok {
			t.Errorf("hooks.%s missing/not-array: %#v", ev, hooks[ev])
		}
	}
	// Command uses the ~/.local/bin/muster tilde form for the tilde-expanding
	// settings.json.
	ss := hooks["SessionStart"].([]any)[0].(map[string]any)
	inner := ss["hooks"].([]any)[0].(map[string]any)
	if cmd, _ := inner["command"].(string); !strings.HasPrefix(cmd, "~/.local/bin/muster hook SessionStart claude") {
		t.Errorf("SessionStart command = %q", cmd)
	}
}

func TestSetupIdempotent(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	files := []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".cursor", "mcp.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
	}
	first := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		first[f] = b
	}

	runSetup(t) // second run must not change anything

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !bytes.Equal(b, first[f]) {
			t.Errorf("%s changed on re-run:\n--- first ---\n%s\n--- second ---\n%s", f, first[f], b)
		}
	}
}

func TestSetupNoHooks(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t, "--no-hooks")

	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("MCP should still be written with --no-hooks")
	}
	if fileExists(filepath.Join(home, ".claude", "settings.json")) {
		t.Fatal("settings.json hooks should NOT be written with --no-hooks")
	}
}

func TestSetupCodexTOML(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)

	path := filepath.Join(home, ".codex", "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	srv, _ := m["mcp_servers"].(map[string]any)
	muster, _ := srv["muster"].(map[string]any)
	if muster == nil || muster["command"] != "muster" {
		t.Fatalf("mcp_servers.muster = %#v", srv)
	}
	if !reflect.DeepEqual(muster["args"], []any{"mcp"}) {
		t.Errorf("args = %#v", muster["args"])
	}

	// Re-run idempotent.
	runSetup(t)
	b2, _ := os.ReadFile(path)
	if !bytes.Equal(b, b2) {
		t.Errorf("codex config.toml changed on re-run:\n%s\nvs\n%s", b, b2)
	}
}

func TestSetupPrintKempt(t *testing.T) {
	setupHome(t)
	out := runSetup(t, "--print", "kempt")
	if out != setupKemptTOML {
		t.Errorf("--print kempt output does not equal the embedded template.\ngot:\n%s", out)
	}
}

func TestSetupPrintUnknown(t *testing.T) {
	setupHome(t)
	var buf bytes.Buffer
	if err := cmdSetup([]string{"--print", "bogus"}, &buf); err == nil {
		t.Fatal("expected error for unknown --print value")
	}
}

func TestSetupKemptPresent(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Put a fake `kempt` executable on PATH.
	bindir := t.TempDir()
	kempt := filepath.Join(bindir, "kempt")
	if err := os.WriteFile(kempt, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir)

	out := runSetup(t)
	if !strings.Contains(out, "kempt apply") {
		t.Errorf("expected kempt guidance, got:\n%s", out)
	}
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("no files should be written when kempt is present without --force")
	}

	// --force overrides.
	runSetup(t, "--force")
	if !fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("--force should configure directly even with kempt present")
	}
}

func TestSetupTmux(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t, "--tmux")

	path := filepath.Join(home, ".tmux.conf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range tmuxLines {
		if !strings.Contains(string(b), l) {
			t.Errorf("~/.tmux.conf missing line %q", l)
		}
	}

	runSetup(t, "--tmux") // re-run: no duplicates
	b2, _ := os.ReadFile(path)
	for _, l := range tmuxLines {
		if n := strings.Count(string(b2), l); n != 1 {
			t.Errorf("line %q appears %d times, want 1", l, n)
		}
	}
}

func TestSetupNoTmuxByDefault(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSetup(t)
	if fileExists(filepath.Join(home, ".tmux.conf")) {
		t.Fatal("tmux config should not be touched without --tmux")
	}
}

func TestSetupPreserveExisting(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"topLevelKey": "keepme",
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "other-tool"},
		},
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	runSetup(t)

	got := readJSON(t, filepath.Join(home, ".claude.json"))
	if got["topLevelKey"] != "keepme" {
		t.Errorf("top-level key not preserved: %#v", got)
	}
	mcp := got["mcpServers"].(map[string]any)
	if _, ok := mcp["other"]; !ok {
		t.Errorf("pre-existing mcpServers.other dropped: %#v", mcp)
	}
	if _, ok := mcp["muster"]; !ok {
		t.Errorf("mcpServers.muster not added: %#v", mcp)
	}
}

func TestSetupNoAgents(t *testing.T) {
	setupHome(t)
	out := runSetup(t)
	if !strings.Contains(out, "No supported agents found") {
		t.Errorf("expected no-agents message, got:\n%s", out)
	}
}

func TestSetupDryRun(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := runSetup(t, "--dry-run")
	if !strings.Contains(out, "would update") {
		t.Errorf("expected dry-run output, got:\n%s", out)
	}
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Fatal("--dry-run must not write files")
	}
}

func TestSetupAgentFilter(t *testing.T) {
	home := setupHome(t)
	for _, d := range []string{".claude", ".codex", ".cursor"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runSetup(t, "--agent", "codex")
	if fileExists(filepath.Join(home, ".claude.json")) {
		t.Error("claude configured despite --agent codex")
	}
	if !fileExists(filepath.Join(home, ".codex", "config.toml")) {
		t.Error("codex not configured with --agent codex")
	}
}
