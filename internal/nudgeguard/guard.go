// Package nudgeguard proves a roster row still names the local, live tmux
// target before an operator surface asks internal/nudge to type into it.
package nudgeguard

import (
	"fmt"

	"github.com/schuettc/muster/internal/device"
	"github.com/schuettc/muster/internal/store"
	"github.com/schuettc/muster/internal/tmuxenv"
)

// Checker supplies the identity and tmux liveness probes used by Check. It is
// injectable so the guard can be tested without a tmux server.
type Checker struct {
	DeviceID     func() (string, error)
	SessionAlive func(socket, sessionID string, created int64) bool
	PaneAlive    func(socket, paneID string) bool
}

// Check proves that agent belongs to this device and still names one live tmux
// incarnation and pane. displayName is the caller's human-facing alias.
func Check(agent store.Agent, displayName string) error {
	return Checker{
		DeviceID:     device.ID,
		SessionAlive: tmuxenv.IsSessionAlive,
		PaneAlive:    tmuxenv.IsPaneAlive,
	}.Check(agent, displayName)
}

// Check applies the target proof in the only safe order: machine ownership,
// immutable tmux session incarnation, then pane existence.
func (c Checker) Check(agent store.Agent, displayName string) error {
	if displayName == "" {
		displayName = agent.Alias
	}
	if agent.DeviceID != "" {
		deviceID, err := c.DeviceID()
		if err != nil {
			return fmt.Errorf("nudge %s: device id: %w", displayName, err)
		}
		if deviceID != agent.DeviceID {
			deviceName := agent.DeviceName
			if deviceName == "" {
				deviceName = "another device"
			}
			return fmt.Errorf("nudge %s: registered on %s", displayName, deviceName)
		}
	}
	if !c.SessionAlive(agent.SocketPath, agent.SessionID, agent.SessionCreated) {
		return fmt.Errorf("nudge %s: stored session %s is not alive — refusing to type into a stale target", displayName, agent.SessionID)
	}
	if !c.PaneAlive(agent.SocketPath, agent.PaneID) {
		return fmt.Errorf("nudge %s: stored pane %s is gone but its session is alive — the row heals at the session's next start/resume (or re-register from the live pane); refusing to type into a guessed pane", displayName, agent.PaneID)
	}
	return nil
}
