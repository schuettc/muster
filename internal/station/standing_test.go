package station

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// standingModel is a station reading one thread full-width — the simplest
// screen where 'R' has a target (replyTargetThreadID's screenRead branch).
func standingModel(caller fakeCaller, row listThreadRow) Model {
	m := NewModel(caller, Options{Alias: "station"})
	m.threads = []listThreadRow{row}
	m.screen = screenRead
	m.viewThreadID = row.ID
	return m
}

func TestStationRetractStandingConfirmationAndAction(t *testing.T) {
	var calls []string
	caller := fakeCaller{fn: func(op string, args map[string]any) (json.RawMessage, error) {
		calls = append(calls, op)
		if op == "standing_retract_thread" {
			if args["thread_id"] != int64(519) || args["from"] != "station" {
				t.Fatalf("standing_retract_thread args = %+v", args)
			}
			return json.RawMessage(`{"changed":true}`), nil
		}
		return json.RawMessage(`[]`), nil
	}}
	m := standingModel(caller, listThreadRow{ID: 519, Kind: "message", FromAgent: "ci-audit", ToKind: "broadcast", ToTarget: "tools-workspace", Subject: "Planned: muda", Standing: true})

	next, _ := m.Update(keyMsg("R"))
	m = mustModel(t, next)
	confirmation := m.renderBottomLine()
	for _, want := range []string{"retract", "#519", "tools-workspace", "y/n"} {
		if !strings.Contains(confirmation, want) {
			t.Fatalf("confirmation missing %q: %q", want, confirmation)
		}
	}
	next, cmd := m.Update(keyMsg("n"))
	m = mustModel(t, next)
	if cmd != nil || len(calls) != 0 || m.retractConfirmThread != 0 {
		t.Fatalf("cancel changed state: cmd=%v calls=%v confirm=%d", cmd != nil, calls, m.retractConfirmThread)
	}

	next, _ = m.Update(keyMsg("R"))
	m = mustModel(t, next)
	next, cmd = m.Update(keyMsg("y"))
	m = mustModel(t, next)
	msg, ok := cmd().(retractStandingResultMsg)
	if !ok || msg.err != nil || msg.threadID != 519 || !msg.changed || len(calls) != 1 {
		t.Fatalf("result=%+v calls=%v", msg, calls)
	}
	next, refresh := m.Update(msg)
	m = mustModel(t, next)
	if refresh == nil || !strings.Contains(m.status, "retracted") {
		t.Fatalf("success status=%q refresh=%v", m.status, refresh != nil)
	}
}

// 'R' opens no confirmation on anything that is not a LIVE standing broadcast
// — a plain broadcast, a direct message, or an already-retracted one — and
// says why instead.
func TestStationRetractStandingIgnoresOtherThreads(t *testing.T) {
	for name, row := range map[string]listThreadRow{
		"plain broadcast": {ID: 1, Kind: "message", ToKind: "broadcast", ToTarget: "p"},
		"direct":          {ID: 2, Kind: "message", ToKind: "agent", ToTarget: "x"},
		"retracted":       {ID: 3, Kind: "message", ToKind: "broadcast", ToTarget: "p", Standing: true, StandingRetracted: true},
	} {
		t.Run(name, func(t *testing.T) {
			m := standingModel(fakeCaller{}, row)
			next, cmd := m.Update(keyMsg("R"))
			m = mustModel(t, next)
			if m.retractConfirmThread != 0 || cmd != nil {
				t.Fatalf("opened confirmation on %s: %d", name, m.retractConfirmThread)
			}
			if !strings.Contains(m.status, "not a live standing broadcast") {
				t.Fatalf("status = %q", m.status)
			}
		})
	}
}

func TestStationRetractStandingFailureReported(t *testing.T) {
	caller := fakeCaller{fn: func(string, map[string]any) (json.RawMessage, error) {
		return nil, errors.New("boom")
	}}
	m := standingModel(caller, listThreadRow{ID: 7, Kind: "message", ToKind: "broadcast", ToTarget: "p", Standing: true})
	next, _ := m.Update(keyMsg("R"))
	m = mustModel(t, next)
	next, cmd := m.Update(keyMsg("y"))
	m = mustModel(t, next)
	next, _ = m.Update(cmd())
	m = mustModel(t, next)
	if !strings.Contains(m.status, "failed") || !strings.Contains(m.status, "boom") {
		t.Fatalf("status = %q", m.status)
	}
}

// A live standing broadcast is marked in the plain row text (so '/standing'
// finds it) and the columnized thread line; a retracted one is not.
func TestStationMarksLiveStandingThreads(t *testing.T) {
	m := NewModel(fakeCaller{}, Options{Alias: "station"})
	live := listThreadRow{ID: 1, Kind: "message", ToKind: "broadcast", ToTarget: "p", Subject: "rules", Standing: true}
	gone := listThreadRow{ID: 2, Kind: "message", ToKind: "broadcast", ToTarget: "p", Subject: "old", Standing: true, StandingRetracted: true}
	if !strings.Contains(m.renderThreadRow(live), "◆ standing") {
		t.Fatalf("live row not marked: %q", m.renderThreadRow(live))
	}
	if strings.Contains(m.renderThreadRow(gone), "◆ standing") {
		t.Fatalf("retracted row marked: %q", m.renderThreadRow(gone))
	}
	line := m.renderConversationLine(conversationRow{listThreadRow: live}, 160, 20)
	if !strings.Contains(line, "◆ standing") {
		t.Fatalf("conversation line not marked: %q", line)
	}
}
