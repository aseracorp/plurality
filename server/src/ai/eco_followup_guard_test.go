package ai

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// eco_followup_guard_test.go — the PREEMPTIVE CONTEXT GUARD measures the SENT
// VIEW, not the marshalled body.
//
// Measured live, 2026-11-20, prod conversation c363b2b3 (/app/data/server.log):
// the guard said 859818 tokens for a 262144-token window while the eco view it
// was about to send was 41135 tokens — a ~20x false positive. It fired a
// context-length error on EVERY turn, so the recovery path folded every turn
// (live rows 1830 -> 1855, UP), and every fold rewrote the checkpoint pair at
// the head of the prompt, pinning cache_read at 16384. The number the guard
// measured was len(jsonData)/3 over the WHOLE body: history + tool schemas +
// re-inflated base64 attachments, and base64 image bytes are not ~3 chars/token
// at a provider (a vision encoder packs an image into a fixed tile count).
//
// A fold can never shrink a number the attachments dominate, so the whole-body
// measure makes compaction unfalsifiable: the loop churns forever. The history
// part of the estimate must be SentViewTokens; the tool schemas keep their own
// allowance (they ARE rendered into the prompt prefix by the chat template).

// prodBodyBytes is the prod body measure: 859818 tokens at /3 => ~2.58 MB of
// marshalled bytes, the number that false-positived on a 262144-token window.
const prodBodyBytes = 859818 * summaryCharsPerToken

// prodViewTokens is the ACTUAL sent view of the same request.
const prodViewTokens = 41135

// window262k is the prod model's context window.
const window262k = 262_144

// bigSchemaTools builds tool schemas whose marshalled bytes are ~`tokens`
// tokens (bytes = tokens*3): the shape of a deployment with dozens of MCP tools.
func bigSchemaTools(tokens int) []utils.ToolsRequest {
	desc := strings.Repeat("z", tokens*summaryCharsPerToken)
	return []utils.ToolsRequest{{
		Type: "function",
		Function: utils.FunctionToolsRequest{
			Name:        "filesystem_server__fs_read",
			Description: desc,
		},
	}}
}

// TestPreemptiveGuardIgnoresMarshalledBodyInflationIsTheProdShape: the prod
// request (body ~2.6 MB / ~860k tokens at /3, sent view ~41k tokens, window
// 262144) must NOT fire. The old measure fired every turn on exactly this shape.
func TestPreemptiveGuardProdShapeDoesNotFire(t *testing.T) {
	// Pin the defect: the whole-body measure IS over the window, so this fixture
	// is the shape that false-positived.
	bodyMeasure := prodBodyBytes / summaryCharsPerToken
	if bodyMeasure <= window262k-guardReplyReserve {
		t.Fatalf("fixture is not the prod shape: body measure %d should blow a %d window", bodyMeasure, window262k)
	}
	if got := SentViewTokens(nil); got != 0 {
		t.Fatalf("SentViewTokens(nil) = %d, want 0", got)
	}

	err := preemptiveContextGuard("eco-guard-test-prod", prodViewTokens, 0, nil, prodBodyBytes, window262k, 1855)
	if err != nil {
		t.Fatalf("guard fired on the prod shape (view %d tokens, window %d): %v — a fold can never shrink a body dominated by attachments",
			prodViewTokens, window262k, err)
	}
}

// TestPreemptiveGuardFiresWhenTheViewIsGenuinelyOverWindow: the guard's purpose
// survives — a sent view that really fills the window must return the error the
// loop's context-length recovery recognises, with the same string shape.
func TestPreemptiveGuardFiresWhenTheViewIsGenuinelyOverWindow(t *testing.T) {
	// The view alone is 60k tokens over the usable ceiling.
	over := window262k - guardReplyReserve + 60_000
	err := preemptiveContextGuard("eco-guard-test-over", over, 0, nil, 1_000, window262k, 1855)
	if err == nil {
		t.Fatalf("guard did not fire on a %d-token view against a %d window", over, window262k)
	}
	if !isContextLengthError(err) {
		t.Fatalf("guard error is not matched by isContextLengthError, the loop will not compact: %v", err)
	}
	// The string shape the log scraping and the loop both key on.
	for _, needle := range []string{"context window", "~", "tokens vs", "max"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("guard error lost %q: %v", needle, err)
		}
	}

	// Just inside the ceiling (room left for the 1-token schema allowance of a
	// nil tools array): no fire, so the guard does not fold a request that fits
	// with room for the reply. The ceiling is window - guardReserve, where
	// guardReserve is the reply reservation PLUS the measurement margin
	// (guard_measure_margin_test.go): the guard's chars/3 estimate is a floor, so
	// the reservation is bigger than the reply alone. Keyed on guardReserve, not
	// a literal, so this stays correct as the margin is retuned.
	fits := window262k - guardReserve(window262k) - 1
	if err := preemptiveContextGuard("eco-guard-test-fits", fits, 0, nil, 1_000, window262k, 10); err != nil {
		t.Fatalf("guard fired just under the ceiling (%d tokens, window %d): %v", fits, window262k, err)
	}
}

// TestPreemptiveGuardCountsBigToolSchemasAgainstASmallView: the schema allowance
// is what the whole-body measure used to provide by accident. A deployment with
// huge MCP tool schemas and a small view must still be bounded — schemas cost
// window because the chat template renders them into the prompt prefix.
func TestPreemptiveGuardCountsBigToolSchemasAgainstASmallView(t *testing.T) {
	// Schemas alone: over the usable ceiling. View is tiny.
	tools := bigSchemaTools(window262k - guardReplyReserve + 5_000)
	schemaTokens := len(marshalNoEscape(tools)) / summaryCharsPerToken
	if schemaTokens <= window262k-guardReplyReserve {
		t.Fatalf("fixture schemas measure %d tokens, want over %d", schemaTokens, window262k-guardReplyReserve)
	}
	err := preemptiveContextGuard("eco-guard-test-schemas", 1_000, 0, tools, 1_000, window262k, 10)
	if err == nil {
		t.Fatalf("guard ignored %d tokens of tool schemas (view 1000, window %d) — schemas must count", schemaTokens, window262k)
	}
	if !isContextLengthError(err) {
		t.Fatalf("schema-driven guard error is not matched by isContextLengthError: %v", err)
	}
	// Same view, NO schemas: fits. So the schemas are what fired the guard.
	if err := preemptiveContextGuard("eco-guard-test-schemas", 1_000, 0, nil, 1_000, window262k, 10); err != nil {
		t.Fatalf("guard fired on a 1000-token view with no schemas (window %d): %v", window262k, err)
	}
}

// TestPreemptiveGuardFallsBackToBodyWhenTheViewIsGone: viewTokens == 0 (no
// visible rows measured) falls back to the whole-body bytes, and the fallback
// can only WIDEN the estimate — never narrow it. A missing view must never let
// an oversized request ship.
func TestPreemptiveGuardFallsBackToBodyWhenTheViewIsGone(t *testing.T) {
	// Body over the window, no view -> fires (conservative).
	if err := preemptiveContextGuard("eco-guard-test-fallback", 0, 0, nil, prodBodyBytes, window262k, 0); err == nil {
		t.Fatal("no-view fallback did not fire on a body over the window — the fallback must never narrow")
	}
	// Small body, no view -> fits.
	if err := preemptiveContextGuard("eco-guard-test-fallback", 0, 0, nil, 30_000, window262k, 0); err != nil {
		t.Fatalf("no-view fallback fired on a %d-byte body against a %d window: %v", 30_000, window262k, err)
	}
	// A window of 0 (unknown) disables the guard entirely.
	if err := preemptiveContextGuard("eco-guard-test-fallback", prodViewTokens, 0, bigSchemaTools(500_000), prodBodyBytes, 0, 1855); err != nil {
		t.Fatalf("guard must not fire when the window is unknown: %v", err)
	}
}

// TestPreemptiveGuardEstimateAppliesTheCompressionFactor: the compression-aware
// discount is applied to the VIEW-based estimate exactly as before.
func TestPreemptiveGuardEstimateAppliesTheCompressionFactor(t *testing.T) {
	const model = "eco-guard-test-compression-model"
	RecordCompression(model, 20_000, 5_000) // 4x, inside the accepted band
	factor := CompressionFactor(model)
	if factor <= 1 {
		t.Fatalf("test model factor %v, want >1", factor)
	}
	est := preemptiveGuardEstimate(prodViewTokens, 0, nil, model, prodBodyBytes)
	want := int(float64(prodViewTokens) / factor)
	if est < want-2 || est > want+2 {
		t.Fatalf("estimate with factor: got %d, want ~%d (view %d / %.2f)", est, want, prodViewTokens, factor)
	}
	// The whole-body number must NOT survive the discount: prod shape, 4x
	// compressed, still fits — and it fits because of the VIEW, not the body.
	if err := preemptiveContextGuard(model, prodViewTokens, 0, nil, prodBodyBytes, window262k, 1855); err != nil {
		t.Fatalf("compressed prod model still false-positives: %v", err)
	}
}

// TestSummaryBudgetIsSizedFromTheView: the fold's summary budget comes from the
// sent view, not the marshalled body. On prod the body measure collapsed every
// fold to minSummaryOutputTokens ("summary budget collapsed (max_tokens=1024
// for a 262144-token window)") and forced machine-only folds.
func TestSummaryBudgetIsSizedFromTheView(t *testing.T) {
	// Registered in the model registry with the prod window, the same seeding
	// style the eco summariser tests use (ucSeedActionModel).
	const model = "eco-guard-test-summary-model"
	ucSeedActionModel(t, model, window262k, map[string]string{"system_tools__shell_exec": "on"})

	budget := summaryOutputTokens(model, prodViewTokens, nil)
	if budget <= minSummaryOutputTokens {
		t.Fatalf("summary budget collapsed to %d for a %d-token window with a %d-token view — the prefix estimate is not view-based",
			budget, window262k, prodViewTokens)
	}
	if budget > maxSummaryOutputTokens {
		t.Fatalf("summary budget %d exceeds the ceiling %d", budget, maxSummaryOutputTokens)
	}

	// The clamp floor is still reachable for the GENUINE case: a view that
	// really fills the window.
	collapsed := summaryOutputTokens(model, window262k+10_000, nil)
	if collapsed != minSummaryOutputTokens {
		t.Fatalf("a view that fills the window must clamp to the floor %d, got %d", minSummaryOutputTokens, collapsed)
	}

	// A view with room to spare: the budget is window-minus-prefix (the nil
	// tools array still costs a token, "null"), capped at the ceiling.
	mid := window262k - maxSummaryOutputTokens/2
	wantMid := window262k - mid - len(marshalNoEscape([]utils.ToolsRequest(nil)))/summaryCharsPerToken
	if got := summaryOutputTokens(model, mid, nil); got != wantMid {
		t.Fatalf("budget for a %d-token view = %d, want window-minus-prefix %d", mid, got, wantMid)
	}

	// Schemas cost the summary budget too (they render into the prefix): big
	// enough to pull the budget under the ceiling, it drops by the schemas.
	tools := bigSchemaTools(window262k - prodViewTokens - maxSummaryOutputTokens/2)
	withSchemas := summaryOutputTokens(model, prodViewTokens, tools)
	if withSchemas >= budget {
		t.Fatalf("schemas did not reduce the summary budget: %d vs %d", withSchemas, budget)
	}
	// The schemas' JSON envelope costs a few bytes over the description, so the
	// budget lands just under the exact window-view-schemas number.
	if got, want := withSchemas, maxSummaryOutputTokens/2; got >= want || got < want-512 {
		t.Fatalf("schema-sized budget = %d, want just under %d (window - view - schemas)", got, want)
	}
}
