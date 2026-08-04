package daemon

import (
	"fmt"
	"os"
	"time"

	"github.com/schuettc/muster/internal/proto"
	"github.com/schuettc/muster/internal/store"
)

// DefaultPollInterval is how often a remote-mode daemon asks the bus whether
// another device has sent mail to an agent on this one. It is the knob
// MUSTER_POLL_INTERVAL sets (see cmd/muster), not a constant to reach for.
//
// Ten seconds is the tradeoff: same-device traffic never waits for a tick (a
// local write reconciles inline, see forward), so this only bounds how late a
// CROSS-device wake can be — and a badge arriving up to ten seconds after the
// message is invisible next to the time an agent takes to notice it.
const DefaultPollInterval = 10 * time.Second

// maxPollInterval caps the quiet-time backoff. A device that has heard nothing
// for a long while still checks in about once a minute, so a woken laptop's
// first cross-device message is never more than that late.
const maxPollInterval = time.Minute

// StartPoller begins polling upstream for mail addressed to this device's
// agents, reconciling local session badges when any arrives. It is the wake
// path for traffic originating on OTHER devices; same-device writes reconcile
// inline (see forward) and never wait for a tick.
//
// The poller does nothing while this device has no live local agents — an idle
// device costs nothing. On error it logs to stderr and backs off; it never
// takes down the daemon and never blocks the unix socket.
//
// The goroutine is owned by Close, exactly as the reconcile loop is: it joins
// the same WaitGroup under the same lock, so "Close returned" keeps meaning
// "nothing is still calling upstream or writing tmux options". Calling this on
// a local-mode or already-closed daemon is a no-op.
func (d *Daemon) StartPoller(base time.Duration) {
	if base <= 0 {
		base = DefaultPollInterval
	}
	d.recMu.Lock()
	if d.recClosed || d.up == nil {
		d.recMu.Unlock()
		return
	}
	d.recWG.Add(1)
	d.recMu.Unlock()

	go func() {
		defer d.recWG.Done()
		d.pollLoop(base)
	}()
}

// pollLoop is the poller proper: sleep, ask, reconcile, repeat.
//
// The interval widens while the bus is quiet and snaps back to base the moment
// a tick returns mail, so an idle pair of laptops settles at one call a minute
// while a live conversation stays at the operator-visible cadence.
//
// sinceEntryID is the resume floor the server hands back, and this loop treats
// it as opaque. It moves over every entry the server CONSIDERED, not only the
// ones that concerned this device, which is what stops a busy bus from
// re-reading the same entries for ever — but it may deliberately lag the newest
// entry the server saw, and a floor that stops advancing means the server is
// holding back over a gap it has not seen filled yet (see
// store.DevicePollResult). Both look identical from here: ask, reconcile what
// comes back, pass the floor on unchanged.
func (d *Daemon) pollLoop(base time.Duration) {
	interval := base
	var sinceEntryID int64
	for {
		if !d.pollSleep(interval) {
			return // Close latched the daemon shut
		}

		// Nobody local to wake: skip the round trip entirely. This is checked
		// every tick rather than once, because agents come and go over the
		// life of a daemon.
		if !d.hasLocalAgents() {
			interval = nextPollInterval(interval, base, false)
			continue
		}

		res, err := d.devicePoll(sinceEntryID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "muster: poll:", err)
			interval = nextPollInterval(interval, base, false)
			continue
		}
		// Only ever forward. A server that answered with a lower watermark
		// than this device already holds must not make it re-read history.
		if res.MaxEntryID > sinceEntryID {
			sinceEntryID = res.MaxEntryID
		}
		if len(res.Sessions) == 0 {
			interval = nextPollInterval(interval, base, false)
			continue
		}
		interval = nextPollInterval(interval, base, true)
		d.reconcileSessions(res.Sessions)
	}
}

// pollSleep waits for interval, returning false if the daemon was closed
// first. Parking on recStop rather than only on the timer is what keeps Close
// from blocking for up to a whole poll interval.
func (d *Daemon) pollSleep(interval time.Duration) bool {
	t := time.NewTimer(interval)
	defer t.Stop()
	select {
	case <-d.recStop:
		return false
	case <-t.C:
		return !d.reconcileStopped()
	}
}

// nextPollInterval is the backoff schedule: double the interval (capped) after
// a quiet tick, and drop straight back to base after one that found mail —
// cross-device traffic arrives in conversations, so the tick that found mail
// is the best predictor of the next one.
func nextPollInterval(current, base time.Duration, foundMail bool) time.Duration {
	if foundMail {
		return base
	}
	next := current * 2
	if next > maxPollInterval {
		next = maxPollInterval
	}
	if next < base {
		next = base
	}
	return next
}

// devicePoll asks the bus which of this device's sessions have mail newer than
// sinceEntryID. The server does the filtering — it holds both the roster and
// the entries — so this is one round trip that returns exactly the sessions to
// reconcile, never a page of entries to sift through here.
func (d *Daemon) devicePoll(sinceEntryID int64) (store.DevicePollResult, error) {
	resp, err := d.callUpstream(proto.Request{Op: "device_poll", Args: map[string]any{
		"device_id": d.deviceID, "since_entry_id": sinceEntryID,
	}})
	if err != nil {
		return store.DevicePollResult{}, err
	}
	var out store.DevicePollResult
	if err := decodeData(resp.Data, &out); err != nil {
		return store.DevicePollResult{}, fmt.Errorf("device_poll: %w", err)
	}
	return out, nil
}
