package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMain builds the shared binary lazily (see builtBinary) and removes its
// temp directory once every test in this package has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

// builtBinary lazily builds the real muster binary once per test process
// (not per test) and returns its path, so exit-code/output behavior is
// verified end-to-end through main() itself — the layer unit tests inside
// internal/humancli can't reach, since the exit-code split (UsageError → 2,
// everything else → 1) and the bare-invocation/serve-mcp-debug help
// interception all live here.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func builtBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "muster-cli-test-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "muster")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = "."
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("go build output:\n%s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building muster binary: %v", buildErr)
	}
	return binPath
}

// run execs the built binary with args and returns stdout, stderr, and the
// process exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	bin := builtBinary(t)
	cmd := exec.Command(bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

func TestBareInvocationExitsTwoOnStdout(t *testing.T) {
	out, _, code := run(t)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out, "muster — local multi-agent coordination bus") {
		t.Errorf("stdout missing grouped usage banner:\n%s", out)
	}
}

func TestHelpExitsZero(t *testing.T) {
	out, _, code := run(t, "help")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Talk:") {
		t.Errorf("stdout missing grouped usage:\n%s", out)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	_, errOut, code := run(t, "bogus")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.HasPrefix(errOut, "muster: unknown command") {
		t.Errorf("stderr = %q, want a \"muster: unknown command\" prefix", errOut)
	}
}

func TestUnknownHelpCommandExitsTwo(t *testing.T) {
	_, errOut, code := run(t, "help", "bogus")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "valid commands:") {
		t.Errorf("stderr missing valid-commands listing: %q", errOut)
	}
}

func TestVersionExitsZero(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		out, _, code := run(t, args...)
		if code != 0 {
			t.Errorf("%v: exit code = %d, want 0", args, code)
		}
		if !strings.HasPrefix(out, "muster ") {
			t.Errorf("%v: stdout = %q, want it to start with \"muster \"", args, out)
		}
	}
}

func TestServeMcpDebugHelpExitZero(t *testing.T) {
	for _, name := range []string{"serve", "mcp", "debug"} {
		for _, flag := range []string{"-h", "--help"} {
			out, _, code := run(t, name, flag)
			if code != 0 {
				t.Errorf("%s %s: exit code = %d, want 0", name, flag, code)
			}
			if !strings.Contains(out, "muster "+name+" — ") {
				t.Errorf("%s %s: stdout missing command help banner:\n%s", name, flag, out)
			}
		}
	}
}

func TestDebugMissingOpStillExitsNonzero(t *testing.T) {
	// Sanity check that fixing debug's -h handling didn't disturb its
	// existing "no args" error path.
	_, errOut, code := run(t, "debug")
	if code == 0 {
		t.Error("expected non-zero exit for `muster debug` with no op")
	}
	if !strings.Contains(errOut, "usage: muster debug") {
		t.Errorf("stderr = %q, want the debug usage message", errOut)
	}
}
