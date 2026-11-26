package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The spec: the tail of the outgoing request is ONLY the tokens the newest turn
// adds. Attachment content belongs to the row that produced it.
//
// WHY. Measured on the deployed build after #217 (which bounded the trailing
// row to 32 KB): per-turn prompt growth is 149-1715 tokens, while the uncached
// cost is pinned at 10693-12361 tokens. 32521 bytes of trailing row / 3.0
// bytes-per-token = ~10.8K tokens: the cache prefix ends just before the
// trailing row, so every turn re-prefills the row AND the new turn. Bounding
// the row capped the damage at ~10K/turn; it did not move the row into the
// cached prefix, because a row appended at the END of the array is behind the
// append point by construction.
//
// Upstream (azukaar/Plurality) has no trailing row: content ships in place at
// its own row. The fork added the row because upstream's staleness window is an
// AGE term (user messages since), which rewrites history rows between turns and
// busts the prefix. The fix is to drop the age term, not to relocate the
// content: content stays in place permanently, and durable compaction replaces
// it with a fetch placeholder. Then the in-place view is a pure function of
// stored state — the property #216 relies on — and the tail holds only new
// bytes.

// renderRows renders and marshals the outgoing rows, joined: the byte string
// the provider sees for the messages array.
func renderRows(t *testing.T, msgs []utils.Message, model utils.Model) string {
	t.Helper()
	out, _, _ := PrepareMessagesForAI(msgs, model)
	var b strings.Builder
	for i, m := range out {
		b.WriteString(attBytes(t, m))
		if i < len(out)-1 {
			b.WriteString(",")
		}
	}
	return b.String()
}

// agenticRun: one user request, then `tools` assistant/tool pairs, each tool
// result carrying one large text attachment. This is the deployed shape: 33
// attachments / 279 KB in one conversation.
func agenticRun(tools, size int) []utils.Message {
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		attUserText("run the task"),
	}
	for i := 0; i < tools; i++ {
		msgs = append(msgs,
			attAssistantCall("c"+itoaTest(i), "system_tools__shell_exec"),
			attToolResult("c"+itoaTest(i), "system_tools__shell_exec", attSnippet(i, size)),
		)
	}
	return msgs
}

// TestRenderIsAppendOnlyAcrossTurns is the cache property the spec exists to
// get: rendering the same conversation one turn later must produce a string
// that has the PREVIOUS render as a byte prefix. Anything that changes a byte
// in the middle (an age window) or appends content at the end and then moves it
// (a trailing row) breaks it, and everything after the break is re-prefilled.
func TestRenderIsAppendOnlyAcrossTurns(t *testing.T) {
	model := attActionModel(t, "inplace-append-only")

	prev := renderRows(t, agenticRun(3, 20*1024), model)
	for tools := 4; tools <= 9; tools++ {
		next := renderRows(t, agenticRun(tools, 20*1024), model)
		if !strings.HasPrefix(next, prev) {
			div := 0
			for div < len(prev) && div < len(next) && prev[div] == next[div] {
				div++
			}
			t.Errorf("turn %d: render diverges at byte %d of the previous %d-byte render "+
				"(%d bytes re-prefilled). With %d attachments of 20 KB each in place, the "+
				"prefix must extend monotonically; a trailing row appended at the END breaks "+
				"that, because the next turn's rows land where the row was.\nprev: %q\nnext: %q",
				tools, div, len(prev), len(prev)-div, tools-1,
				attSnip(prev, div), attSnip(next, div))
			return
		}
		prev = next
	}
}

// TestAttachmentContentIsAtItsOwnRow pins the placement the spec asks for: the
// content is in the row that produced it, not in a separate row appended after
// the whole history.
func TestAttachmentContentIsAtItsOwnRow(t *testing.T) {
	model := attActionModel(t, "inplace-placement")

	out, has, _ := PrepareMessagesForAI(agenticRun(4, 20*1024), model)
	if !has {
		t.Fatalf("attachments must be detected")
	}
	if row := trailingAttachmentRow(out); row != nil {
		t.Errorf("a trailing attachment row exists (%d bytes at the end of the array): attachment "+
			"content must live at the row that produced it, so the tail carries only the newest "+
			"turn's tokens (deployed: uncached 10693-12361 tokens/turn vs 149-1715 added)",
			len(attBytes(t, *row)))
	}
	for i, m := range out {
		if m.Role == "tool" && strings.Contains(attBytes(t, m), "S0001S0001S0001") {
			return // content is in the tool row that produced it
		}
		_ = i
	}
	t.Errorf("no tool row carries its own attachment content: the content is nowhere in place")
}

// TestFoldLeavesAFetchPointer pins the compaction half of the spec. Content
// ships in place and stays there; the durable fold is what ages it out, so the
// fold must leave behind the address the fetch tools accept — otherwise the
// summary says "it is gone" and the retrieval promise is void.
//
// The pointer is ONE bounded block per fold, not a line per row. Emitting it per
// row is what this replaces: on a 1467-row fold it added ~264 KB, the fold
// stopped converging (62810 tokens left against a 50000 target), stage 1
// escalated to the chat model, and the truncated roll-up lost the file paths it
// exists to preserve.
func TestFoldLeavesAFetchPointer(t *testing.T) {
	// Rows carry DB ids: that is what makes the attachment name stable for the
	// life of the row, and therefore safe to write into a durable summary.
	msgs := []utils.Message{
		{Role: "system", Content: utils.NewTextContent("SYSTEM PROMPT")},
		attID(attUserText("run the task"), "101"),
		attID(attToolResult("c0", "system_tools__shell_exec", attSnippet(0, 20*1024)), "102"),
		attID(attToolResult("c1", "system_tools__shell_exec", attSnippet(1, 20*1024)), "103"),
	}

	folded := MachineExcerptMegacaveCapped(msgs[2:], 200, 0)
	if !strings.Contains(folded, "conversation_attachments") {
		t.Errorf("the fold carries no retrieval instruction; the folded rows' content is unreachable: %s",
			attSnip(folded, 0))
	}
	for _, want := range []string{"att_102_1", "att_103_1"} {
		if !strings.Contains(folded, want) {
			t.Errorf("the fold must name each folded attachment by the id the fetch tools resolve "+
				"(row id 102, part 1 = att_102_1); missing %s from: %s", want, attSnip(folded, 0))
		}
	}
}

// TestAttachmentIDSurvivesFold: the pointer is only worth writing if it keeps
// naming the same content. IDs come from the row, so removing earlier rows must
// not shift them — the property the positional scheme fails (measured: att_0
// meant "S0000S" before a fold and "S0003S" after).
func TestAttachmentIDSurvivesFold(t *testing.T) {
	full := []utils.Message{
		attID(attUserText("q0"), "101"),
		attID(attToolResult("c0", "t__a", attSnippet(0, 9000)), "102"),
		attID(attToolResult("c1", "t__b", attSnippet(1, 9000)), "103"),
		attID(attToolResult("c2", "t__c", attSnippet(2, 9000)), "104"),
	}
	// The LLM-facing view after a fold: the first two attachment rows are gone.
	foldedView := []utils.Message{full[0], full[3]}

	byID := func(ms []utils.Message) map[string]string {
		idx := utils.BuildAttachmentIndex(ms, nil)
		out := map[string]string{}
		for _, it := range idx.Items {
			head := idx.ContentMap[it.ID]
			if len(head) > 6 {
				head = head[:6]
			}
			out[it.ID] = head
		}
		return out
	}
	before, after := byID(full), byID(foldedView)

	survivor := "att_104_1"
	got, ok := after[survivor]
	if !ok {
		t.Fatalf("the surviving attachment lost its id after the fold: %v", keysOf(after))
	}
	if before[survivor] != got {
		t.Errorf("%s names %q before the fold and %q after: the pointer written into a folded row "+
			"resolves to the wrong content", survivor, before[survivor], got)
	}
	// The positional scheme's failure, pinned as a positive assertion: ids must
	// not depend on scan order.
	if _, shifted := after["att_0"]; shifted {
		t.Errorf("ids are still positional: att_0 exists in the folded view, which means it was "+
			"reassigned to the row that is now first (it was %q, it is now %q)",
			before["att_0"], after["att_0"])
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func attID(m utils.Message, id string) utils.Message {
	m.ID = id
	return m
}

func attSnip(s string, at int) string {
	lo := at - 40
	if lo < 0 {
		lo = 0
	}
	hi := at + 90
	if hi > len(s) {
		hi = len(s)
	}
	return strings.ReplaceAll(s[lo:hi], "\n", "\\n")
}
