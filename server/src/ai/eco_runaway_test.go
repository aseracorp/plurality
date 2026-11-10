package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// The pasted-logfile shape (conversation 08611c509032e6831ba6031f,
// "System Status Check"). A user pasted a 991440-char logfile. The eco trigger
// then claimed compaction on EVERY message and prefill caching collapsed to
// ~24k reuse, in a loop that ran the full compaction path every turn:
//
//	[Eco] content ≈388917 tokens > trigger 150000 — staged megacave compaction
//	[Eco] stage D: budget 143344 tokens below the live tail (~332531)
//	[Eco] resolved staged compaction (stage 4 ids, 17 messages, ≈334587 / budget 113984, folding 2 of 17)
//	[Eco] durable compaction folded 0 messages at boundary 2
//	[Eco] preemptive context guard: estimated 375541 tokens vs window 262144 — ... compacts and retries
//	[Eco] content ≈95754 tokens ≤ trigger 150000 — sending unchanged
//
// Two defects made that permanent:
//
//  1. The trigger estimate counted RAW wire bytes, with no per-message bound,
//     while the request path truncates every outgoing message to
//     maxCompactMessageChars. The gate charged ~330k est tokens for a single
//     row of which at most ~33k is ever sent — so estTotalTokens could never
//     return under the 150000 trigger, on any turn, ever.
//  2. Stages A-D fold OLDER history. The oversized row is the NEWEST user
//     message, which the fold floor protects, so no stage can shrink it. Stage
//     D kept it verbatim, computed a negative excerpt room, and returned a
//     ~334k-token view against a ~114k budget — which the preemptive guard
//     rejects, which triggers compact (folds 0) + retry, forever.
//
// The fix bounds the SENT view: the estimator and the resolver must both
// measure/emit what is actually sendable.

const logfileChars = 991440

// mkLogfileConv builds the live shape: a bounded history plus one user message
// holding a pasted logfile, as the newest user turn.
func mkLogfileConv(turns int) []utils.Message {
	msgs := mkConv(turns, 1200)
	msgs = append(msgs,
		mkMsg("user", strings.Repeat("E", logfileChars)),
		mkMsg("assistant", "The stack is clear enough to name a suspect already."),
		mkTool("system_tools__shell_exec", strings.Repeat("o", 6428)),
	)
	return msgs
}

// TestSentViewTokensBoundsOversizedMessage: the trigger estimate must not
// charge the request for bytes the provider never receives.
func TestSentViewTokensBoundsOversizedMessage(t *testing.T) {
	msgs := mkLogfileConv(20)

	raw := 0
	for i := range msgs {
		raw += messageWireChars(msgs[i])
	}
	if raw/3 <= 150000 {
		t.Fatalf("setup: expected the raw count to blow the 150000 trigger, got ~%d", raw/3)
	}

	got := SentViewTokens(msgs)
	// The whole sent view is bounded by (bounded messages + overhead); the
	// single logfile row alone may not exceed the per-message send cap.
	big := msgs[indexOfLastUser(msgs)]
	if w := boundedWireChars(big); w > ecoSentMessageCapChars {
		t.Fatalf("oversized row counted as %d wire chars, want <= %d (it is truncated before send)",
			w, ecoSentMessageCapChars)
	}
	if got > 150000 {
		t.Fatalf("trigger estimate ~%d tokens exceeds the 150000 trigger — the gate would demand compaction on EVERY message (raw ~%d)",
			got, raw/3)
	}
	t.Logf("logfile conv: raw ~%d est tokens vs bounded ~%d (trigger 150000)", raw/3, got)
}

// TestResolver_BoundsOversizedNewestMessage: the staged resolver must return a
// view that FITS. Keeping an oversized newest user message verbatim is what
// made the resolver hand the preemptive context guard an oversized request on
// every turn (guard -> compact(folds 0) -> retry -> send).
func TestResolver_BoundsOversizedNewestMessage(t *testing.T) {
	msgs := mkLogfileConv(20)

	// The live numbers from the log.
	window := 262144
	budget := 113984

	out, foldFrom := resolveEcoContextCompaction(msgs, window, budget)

	if approxTokens(out) > budget {
		t.Fatalf("staged view ≈%d tokens does not fit the %d budget — the preemptive guard rejects it and the compaction/retry cycle repeats every message",
			approxTokens(out), budget)
	}
	for i := range out {
		if w := messageWireChars(out[i]) - imageWireChars(out[i].Content); w > maxCompactMessageChars {
			t.Fatalf("staged view message %d (%s) carries %d non-image wire chars > the %d send cap",
				i, out[i].Role, w, maxCompactMessageChars)
		}
	}
	if indexOfLastUser(out) < 0 {
		t.Fatal("staged view has no user message; the chat template will 400")
	}
	// The bounded view FITS, so the resolver must report "nothing to fold".
	// Demanding a fold here is the wedge that ran the full compaction path on
	// every message: the oversized row is the protected newest user turn, so the
	// fold always removed 0 rows, and removing 0 rows used to rewrite the
	// checkpoint pair at the HEAD of the prompt (prefix cache busted, ~24k reuse
	// of a ~95k prompt).
	if foldFrom != 0 {
		t.Fatalf("bounded history fits the %d budget — expected foldFrom 0, got %d (a fold here can only remove 0 rows and churns the prompt head)",
			budget, foldFrom)
	}
	t.Logf("staged view: %d messages ≈%d tokens (budget %d), foldFrom %d",
		len(out), approxTokens(out), budget, foldFrom)
}

// The send path (index.go) applies TruncateOversizedMessages to EVERY request,
// so the helper — not just the overflow-retry branch — must guarantee the
// invariant the eco trigger estimate relies on:
// messageWireChars(m) <= maxCompactMessageChars.

// TestTruncateOversizedMessages_EnforcesWireCapOnEveryRow: the trigger estimator
// clamps each row to the cap; the sender must enforce the same cap, including
// the tool_call ARGUMENT bytes the estimator counts.
func TestTruncateOversizedMessages_EnforcesWireCapOnEveryRow(t *testing.T) {
	bigArgs := strings.Repeat("a", 400_000)
	msgs := []utils.Message{
		// Oversized TEXT (the pasted-logfile shape).
		mkMsg("user", strings.Repeat("E", logfileChars)),
		// Oversized tool_call ARGUMENTS with a small text body — the estimator
		// counts these bytes, so text-only truncation would leave the row
		// oversized on the wire and the trigger/guard estimates would disagree.
		{
			Role:    "assistant",
			Content: utils.NewTextContent("writing the file"),
			ToolCalls: []utils.ToolCall{{
				ID: "call_1", Type: "function",
				Function: utils.FunctionCall{Name: "filesystem_server__fs_write", Arguments: bigArgs},
			}},
		},
		mkTool("shell", strings.Repeat("o", 6_000)),
	}

	out := TruncateOversizedMessages(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("messages dropped: %d -> %d (role/turn boundaries must be preserved)", len(msgs), len(out))
	}
	for i := range out {
		if w := messageWireChars(out[i]) - imageWireChars(out[i].Content); w > maxCompactMessageChars {
			t.Fatalf("message %d (role=%s) still carries %d wire chars > the %d cap — the trigger estimate and the preemptive guard disagree, which is the runaway loop",
				i, out[i].Role, w, maxCompactMessageChars)
		}
	}

	// tool_calls arguments must stay VALID JSON after elision: providers
	// validate them and answer 400 on malformed tool_calls.
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(out[1].ToolCalls[0].Function.Arguments), &probe); err != nil {
		t.Fatalf("elided tool_call arguments are not valid JSON: %v", err)
	}
	// Roles preserved.
	for i := range msgs {
		if out[i].Role != msgs[i].Role {
			t.Fatalf("role changed at %d: %s -> %s", i, msgs[i].Role, out[i].Role)
		}
	}
}

// TestTruncateOversizedMessages_PreservesAttachments: the helper now guards the
// normal send path, so it must not silently delete image parts — the previous
// implementation rebuilt oversized content as a single text part.
func TestTruncateOversizedMessages_PreservesAttachments(t *testing.T) {
	dataURI := "data:image/png;base64," + strings.Repeat("A", 200_000)
	msgs := []utils.Message{{
		Role:    "user",
		Content: utils.NewPartsContent([]utils.ContentPart{{Type: "text", Text: strings.Repeat("t", 300_000)}}),
	}, {
		Role: "user",
		Content: utils.NewPartsContent([]utils.ContentPart{
			{Type: "text", Text: strings.Repeat("t", 200_000)},
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: dataURI}, Filename: "shot.png"},
		}),
	}}

	out := TruncateOversizedMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("messages dropped: %d -> %d", len(msgs), len(out))
	}
	kept := false
	for _, p := range out[1].Content.ContentParts() {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL == dataURI {
			kept = true
			if p.Filename != "shot.png" {
				t.Fatalf("attachment filename lost: %q", p.Filename)
			}
		}
	}
	if !kept {
		t.Fatal("an oversized message's image part was DELETED — attachments must survive the send cap")
	}
	if !strings.Contains(out[0].TextContent(), "truncated by Plurality") {
		t.Fatal("oversized text was not marked as truncated")
	}
}

// TestTruncateOversizedMessages_IsIdempotent: the send path applies the cap on
// every request. A second pass must not change anything, or the request bytes
// would move between turns and the backend prefix cache would be busted.
func TestTruncateOversizedMessages_IsIdempotent(t *testing.T) {
	once := TruncateOversizedMessages(mkLogfileConv(6))
	twice := TruncateOversizedMessages(once)
	if len(once) != len(twice) {
		t.Fatalf("second pass changed the message count: %d -> %d", len(once), len(twice))
	}
	for i := range once {
		if once[i].TextContent() != twice[i].TextContent() {
			t.Fatalf("second pass changed message %d (%s) — the cap is not deterministic, so the prompt prefix moves every turn",
				i, once[i].Role)
		}
	}
}
