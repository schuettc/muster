package station

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// markdownThreadFake builds a fakeCaller whose get_thread response is the
// given entries — the same shape TestConversationReaderWindowingBounds... in
// threads_test.go uses, factored out here since several tests below need it.
func markdownThreadFake(entries []threadEntryRow, total int) fakeCaller {
	return fakeCaller{fn: func(op string, _ map[string]any) (json.RawMessage, error) {
		if op != "get_thread" {
			return json.RawMessage(`{}`), nil
		}
		b, _ := json.Marshal(map[string]any{"thread": map[string]any{}, "entries": entries, "total": total})
		return b, nil
	}}
}

// TestConversationLinesMarkdownBodyRendersWithStructure is the core
// iteration-three regression test: a message body written the way agents
// actually write them — a paragraph, a blank line, a "- " bullet list, bold
// and inline code — must come back from conversationLines as SEPARATE
// structured lines (each bullet its own entry, a blank line preserved)
// rather than the pre-fix flattened brick, and the rendered text must never
// contain the raw "**"/"`" markdown punctuation the operator's screenshot
// showed littering the flattened output.
func TestConversationLinesMarkdownBodyRendersWithStructure(t *testing.T) {
	body := "Please review:\n\n- fix the **critical** auth bug\n- update the `README`\n\nThanks!"
	fake := markdownThreadFake([]threadEntryRow{{ID: 1, ThreadID: 1, FromAgent: "a", Body: body, CreatedAt: 0}}, 1)

	m := focusConversationList(t, NewModel(fake, Options{}), "a")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "a", EntryCount: 1}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.viewEntries) != 1 {
		t.Fatalf("loaded %d entries, want 1", len(m.viewEntries))
	}

	const width = 50
	lines, _ := m.conversationLines(width)

	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "**") {
		t.Fatalf("rendered conversation lines still contain literal **:\n%s", joined)
	}
	if strings.Contains(joined, "`") {
		t.Fatalf("rendered conversation lines still contain a literal backtick:\n%s", joined)
	}

	var bulletLines []string
	var blankLines int
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "- ") {
			bulletLines = append(bulletLines, trimmed)
		}
		if trimmed == "" {
			blankLines++
		}
		if w := lipgloss.Width(l); w > width {
			t.Fatalf("line %q is %d display columns wide, want <= %d", l, w, width)
		}
	}
	if len(bulletLines) != 2 {
		t.Fatalf("expected 2 separate bullet lines, got %d: %q\nfull render:\n%s", len(bulletLines), bulletLines, joined)
	}
	if !strings.Contains(bulletLines[0], "critical") || !strings.Contains(bulletLines[0], "auth bug") {
		t.Fatalf("first bullet line lost its text: %q", bulletLines[0])
	}
	if !strings.Contains(bulletLines[1], "README") {
		t.Fatalf("second bullet line lost its text: %q", bulletLines[1])
	}
	// At least the blank line the source markdown itself has (between the
	// intro paragraph and the list, and again before "Thanks!") must survive
	// as a genuinely blank line rather than being swallowed.
	if blankLines < 2 {
		t.Fatalf("expected at least 2 blank separator lines (structure preserved), got %d:\n%s", blankLines, joined)
	}
	for _, want := range []string{"Please review:", "fix the", "update the", "Thanks!"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered lines missing %q:\n%s", want, joined)
		}
	}
}

// TestRenderConversationBoxMarkdownBodyExactWidth is
// TestBoxLinesAreExactlyOuterWidth's companion for the FOCUSED reader with a
// markdown body in play: every rendered box row (borders included) must
// come out at EXACTLY outerW display columns even once a body line carries
// inline lipgloss ANSI from bold/code styling — the exact miscount hazard
// renderConversationBox's rewrite (no longer re-Sanitizing/re-padding
// conversationLines' already-styled output) exists to avoid.
func TestRenderConversationBoxMarkdownBodyExactWidth(t *testing.T) {
	body := "- **bold** item one\n- plain item two with `a code span` inside it"
	fake := markdownThreadFake([]threadEntryRow{{ID: 1, ThreadID: 1, FromAgent: "reviewer", Body: body, CreatedAt: 0}}, 1)

	m := focusConversationList(t, NewModel(fake, Options{}), "reviewer")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "reviewer", EntryCount: 1}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	m = enterThreadsSection(t, m)
	next, _ = m.Update(keyMsg("enter")) // focus the reader (L2)
	m = mustModel(t, next)

	const outerW, outerH = 30, 12 // deliberately narrow, so the bullet wraps
	view := m.renderConversationBox(outerW, outerH, true)
	for i, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w != outerW {
			t.Fatalf("box row %d is %d display columns wide, want exactly %d:\n%q\nfull view:\n%s", i, w, outerW, l, view)
		}
	}
	if strings.Contains(view, "**") || strings.Contains(view, "`") {
		t.Fatalf("rendered box still contains raw markdown punctuation:\n%s", view)
	}
}

// TestConversationPreviewStartsAtMessageBoundary is spec item 4: the
// PASSIVE (non-focused) right-pane preview shows the tail of the
// conversation, but must start rendering at a message's own header line —
// never mid-body/mid-word — even when a straight tail-height slice would
// otherwise land inside a message's wrapped body.
func TestConversationPreviewStartsAtMessageBoundary(t *testing.T) {
	const n = 10
	entries := make([]threadEntryRow, n)
	for i := 0; i < n; i++ {
		entries[i] = threadEntryRow{ID: int64(i), ThreadID: 1, FromAgent: fmt.Sprintf("author%d", i), Body: fmt.Sprintf("BODYLINE-%d", i), CreatedAt: int64(i)}
	}
	fake := markdownThreadFake(entries, n)

	m := focusConversationList(t, NewModel(fake, Options{}), "author0")
	next, cmd := m.Update(threadsMsg{threads: []listThreadRow{{ID: 1, FromAgent: "author0", EntryCount: n}}})
	m = mustModel(t, next)
	m = drainCmd(t, m, cmd)
	if len(m.viewEntries) != n {
		t.Fatalf("loaded %d entries, want %d", len(m.viewEntries), n)
	}

	// Each entry renders as exactly 3 lines (header + 1 body line + blank
	// separator) at this width, so a straight tail slice of 7 content rows
	// out of 30 total would land at line 23 — mid-entry-8's body, NOT its
	// header (entry boundaries sit at 0,3,6,...,27).
	const outerW = 40
	const outerH = 9 // content height = outerH - boxBorderRows(2) = 7
	view := m.renderConversationBox(outerW, outerH, false)
	lines := strings.Split(view, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected a bordered box with at least one content row, got %q", view)
	}
	first := lines[1] // lines[0] is the top border
	if !strings.Contains(first, " · ") {
		t.Fatalf("preview's first content row must be a message HEADER (author · age), not a mid-body fragment: %q\nfull view:\n%s", first, view)
	}
	if strings.Contains(first, "BODYLINE") {
		t.Fatalf("preview's first content row must not be body text: %q\nfull view:\n%s", first, view)
	}
}
