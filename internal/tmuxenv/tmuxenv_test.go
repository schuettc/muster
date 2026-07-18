package tmuxenv

import (
	"fmt"
	"testing"
)

func withRun(t *testing.T, fn func(args ...string) (string, error)) {
	t.Helper()
	prev := Run
	Run = fn
	t.Cleanup(func() { Run = prev })
}

func TestProjectFromSocket(t *testing.T) {
	cases := map[string]string{
		"/private/tmp/tmux-501/proj-muster": "muster",
		"/tmp/tmux-0/proj-foo-bar":          "foo-bar",
		"/tmp/tmux-0/default":               "",
		"":                                  "",
	}
	for in, want := range cases {
		if got := ProjectFromSocket(in); got != want {
			t.Errorf("ProjectFromSocket(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsSessionAlive(t *testing.T) {
	withRun(t, func(_ ...string) (string, error) { return "", nil })
	if !IsSessionAlive("/s", "$1") {
		t.Fatal("want alive when has-session exits 0")
	}
	withRun(t, func(_ ...string) (string, error) { return "", fmt.Errorf("no such session") })
	if IsSessionAlive("/s", "$1") {
		t.Fatal("want dead when has-session errors")
	}
	if IsSessionAlive("", "$1") || IsSessionAlive("/s", "") {
		t.Fatal("empty socket/session must be dead")
	}
}

func TestIsPaneAlive(t *testing.T) {
	withRun(t, func(_ ...string) (string, error) { return "%5", nil })
	if !IsPaneAlive("/s", "%5") {
		t.Fatal("want alive when display-message returns a pane id")
	}
	withRun(t, func(_ ...string) (string, error) { return "", fmt.Errorf("no such pane") })
	if IsPaneAlive("/s", "%5") {
		t.Fatal("want dead when the query errors")
	}
	if IsPaneAlive("", "%5") || IsPaneAlive("/s", "") {
		t.Fatal("empty socket/pane must be dead without calling Run")
	}
}

func TestSessionLabelManualVsAuto(t *testing.T) {
	withRun(t, func(_ ...string) (string, error) { return "frontend\x1f1", nil })
	if l, m := SessionLabel("/s", "$1"); l != "frontend" || !m {
		t.Fatalf("manual: got (%q,%v)", l, m)
	}
	withRun(t, func(_ ...string) (string, error) { return "some auto topic\x1f", nil })
	if l, m := SessionLabel("/s", "$1"); l != "some auto topic" || m {
		t.Fatalf("auto: got (%q,%v)", l, m)
	}
}

func TestSessionAttached(t *testing.T) {
	withRun(t, func(_ ...string) (string, error) { return "1", nil })
	if !SessionAttached("/s", "$1") {
		t.Fatal("want attached when #{session_attached} reports 1")
	}
	withRun(t, func(_ ...string) (string, error) { return "2", nil })
	if !SessionAttached("/s", "$1") {
		t.Fatal("want attached when #{session_attached} reports 2 (multiple clients)")
	}
	withRun(t, func(_ ...string) (string, error) { return "0", nil })
	if SessionAttached("/s", "$1") {
		t.Fatal("want not-attached when #{session_attached} reports 0")
	}
	withRun(t, func(_ ...string) (string, error) { return "", fmt.Errorf("no such session") })
	if SessionAttached("/s", "$1") {
		t.Fatal("want not-attached when the query fails (e.g. dead session)")
	}
	if SessionAttached("", "$1") || SessionAttached("/s", "") {
		t.Fatal("empty socket/session must never report attached")
	}
}

func TestCaptureEnvNoTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	c := CaptureEnv()
	if c.SocketPath != "" || c.Project != "" || c.SessionID != "" {
		t.Fatalf("no-tmux capture should be empty, got %+v", c)
	}
}

func TestCaptureEnvPopulated(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux-501/proj-muster,123,0")
	t.Setenv("TMUX_PANE", "%0")
	withRun(t, func(args ...string) (string, error) {
		last := args[len(args)-1]
		switch last {
		case "#{session_id}":
			return "$7", nil
		case "#{session_name}":
			return "muster-2", nil
		default: // label format
			return "backend\x1f1", nil
		}
	})
	c := CaptureEnv()
	if c.Project != "muster" || c.SessionID != "$7" || c.SessionName != "muster-2" ||
		c.Label != "backend" || !c.LabelManual {
		t.Fatalf("capture=%+v", c)
	}
}
