package nudgeguard

import (
	"strings"
	"testing"

	"github.com/schuettc/muster/internal/store"
)

func TestCheckerRefusesForeignDevice(t *testing.T) {
	c := Checker{
		DeviceID:     func() (string, error) { return "this-device", nil },
		SessionAlive: func(string, string, int64) bool { t.Fatal("session check after foreign-device refusal"); return false },
		PaneAlive:    func(string, string) bool { t.Fatal("pane check after foreign-device refusal"); return false },
	}
	err := c.Check(store.Agent{Alias: "review", DeviceID: "other-device", DeviceName: "work-laptop"}, "review")
	if err == nil || !strings.Contains(err.Error(), "registered on work-laptop") {
		t.Fatalf("foreign-device error = %v, want registered-on-device refusal", err)
	}
}

func TestCheckerRequiresTheRecordedSessionIncarnation(t *testing.T) {
	c := Checker{
		DeviceID:     func() (string, error) { return "this-device", nil },
		SessionAlive: func(string, string, int64) bool { return false },
		PaneAlive:    func(string, string) bool { t.Fatal("pane check after dead-session refusal"); return false },
	}
	err := c.Check(store.Agent{Alias: "review", DeviceID: "this-device", SocketPath: "/s", SessionID: "$1", SessionCreated: 0}, "review")
	if err == nil || !strings.Contains(err.Error(), "stored session $1 is not alive") {
		t.Fatalf("dead-session error = %v", err)
	}
}

func TestCheckerRefusesDeadPaneWithTheExistingRemedy(t *testing.T) {
	c := Checker{
		DeviceID:     func() (string, error) { return "this-device", nil },
		SessionAlive: func(string, string, int64) bool { return true },
		PaneAlive:    func(string, string) bool { return false },
	}
	err := c.Check(store.Agent{Alias: "review", DeviceID: "this-device", SocketPath: "/s", SessionID: "$1", SessionCreated: 12, PaneID: "%9"}, "review")
	if err == nil || !strings.Contains(err.Error(), "stored pane %9 is gone but its session is alive") {
		t.Fatalf("dead-pane error = %v", err)
	}
}

func TestCheckerAllowsTheLiveLocalTarget(t *testing.T) {
	c := Checker{
		DeviceID: func() (string, error) { return "this-device", nil },
		SessionAlive: func(socket, session string, created int64) bool {
			return socket == "/s" && session == "$1" && created == 12
		},
		PaneAlive: func(socket, pane string) bool { return socket == "/s" && pane == "%9" },
	}
	if err := c.Check(store.Agent{Alias: "review", DeviceID: "this-device", SocketPath: "/s", SessionID: "$1", SessionCreated: 12, PaneID: "%9"}, "review"); err != nil {
		t.Fatalf("live local target refused: %v", err)
	}
}
