package ai

// guard_shipped_measure_test.go — the preemptive guard must charge for the
// request that SHIPS, and its forecast must not end a turn the provider accepts.
//
// The production case, verbatim from the live log (conversation
// 6021687a5770b7afbb6d47f1, 2026-11-27):
//
//	13:38:26  Created new conversation ID: 6021687a5770b7afbb6d47f1
//	13:40:34  PERF msgs=2 tok_before=33421 cache_hit_pct=0
//	13:40:44  PERF msgs=4 tok_before=33645 cache_hit_pct=100
//	13:40:53  [Eco] compression-aware guard: est 523348 tokens / factor 1.02 = 510999
//	13:40:53  [ERROR] [LLMLoop] context length exceeded — compacting … ~510999 tokens vs 262144
//	13:40:53  [ERROR] [LLMLoop] context length exceeded and no durable shrink is available
//
// A 6-row, 27K-token conversation was told it does not fit a 262144-token window,
// and the turn died. The rows: user(443), assistant+call, tool(482),
// assistant+call, tool(4282658 ← a scraped page), tool(193). The 4.28MB row ships
// as an ATTACHMENT PLACEHOLDER (~110 chars); the guard's sent-view measure counts
// STORED row chars, so it charged 1.43M tokens for text the request never carries.
//
// Why the recovery then killed the turn: the fold's floor is the newest USER row,
// and this conversation's only user row is seq 0, so the fold refuses (0 rows);
// RefineRollupSummary needs a checkpoint pair, and a brand-new conversation has
// none. Nothing can shrink — and the pre-fix code treated that as a terminal
// failure for a FORECAST as much as for a measured provider overflow.

import (
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

func TestGuardMeasureIsTheShippedRequestNotTheStoredRows(t *testing.T) {
	ctx, _, convID := ucDB(t)
	model := ucSeedActionModel(t, "guard-shipped-model", window262k, map[string]string{"system_tools__shell_exec": "on"})

	// The production shape: user ask, a tool chain, and ONE huge tool row.
	big := strings.Repeat("<div class='product'>lorem ipsum dolor sit amet</div>", 100_000) // ~4.3M chars
	hist := []utils.Message{
		{Role: "user", Content: utils.NewTextContent("Please look at https://www.asera.ch/produkt/viveroo-free-ipad-wandhalterung/ and tell me if the buy box animates")},
	}
	hist = append(hist, ucTurn(0, "system_tools__shell_exec", `{"tasks":[{"id":"t1","title":"Read product page"}]}`)...)
	hist = append(hist, utils.Message{
		Role:       "tool",
		Name:       "playwright__browser_snapshot",
		ToolCallID: "call_guard_big",
		Content:    utils.NewTextContent(big),
	})
	ucPush(t, ctx, convID, hist...)
	view := ucLLMView(t, ctx, convID)
	conv := ucConvOf(t, ctx, convID, view, model)
	payload := ChatPayload{ConversationID: convID, ModelSelected: conv.ModelSelected}

	if len(big) < 4_000_000 {
		t.Fatalf("fixture is %d chars, want >4M to match the production row", len(big))
	}

	// The STORED view measure — what the guard used — counts the whole row.
	stored := SentViewTokens(view)
	if stored < 1_000_000 {
		t.Fatalf("the stored measure is %d tokens: the fixture must over-count like production (~1.43M)", stored)
	}

	parts, err := buildChatRequestParts(ctx, model, conv, payload)
	if err != nil {
		t.Fatalf("chat parts: %v", err)
	}
	shippedText, shippedImages := requestShippedTokens(parts.request.Messages)
	schema := len(marshalNoEscape(parts.request.Tools)) / summaryCharsPerToken

	// The wire is small: the huge row is a placeholder.
	if shippedText > 60_000 {
		t.Fatalf("the shipped measure is %d tokens — the request carries the 4.3M-char row, not a placeholder", shippedText)
	}
	if shippedImages != 0 {
		t.Fatalf("no images in this fixture, measured %d", shippedImages)
	}

	// The guard, measured on the wire: SILENT. This is the fix.
	window := parts.windowTokens
	if err := preemptiveContextGuard(model.Name, shippedText, shippedImages, parts.request.Tools, 0, window, len(parts.request.Messages)); err != nil {
		t.Fatalf("the guard fires on a request of %d text + %d schema tokens against a %d window: %v",
			shippedText, schema, window, err)
	}
	// The guard, measured on the stored view: FIRES — the production defect,
	// pinned so it cannot come back as the guard's measure.
	if err := preemptiveContextGuard(model.Name, stored, 0, parts.request.Tools, 0, window, len(parts.request.Messages)); err == nil {
		t.Fatal("the stored measure no longer over-counts: the fixture stopped reproducing the production case")
	}
}

// TestGuardForecastIsTaggedAndRecognised: the guard's error is a PROJECTION, and
// it still reads as a context-length error so the loop's durable fold gets its
// chance first.
func TestGuardForecastIsTaggedAndRecognised(t *testing.T) {
	err := preemptiveContextGuard("guard-tag-model", 900_000, 0, nil, 0, window262k, 10)
	if err == nil {
		t.Fatal("a 900K-token view on a 262144 window must fire the guard")
	}
	if !isGuardProjection(err) {
		t.Fatalf("the guard's error is not tagged as a projection: %v", err)
	}
	if !isContextLengthError(err) {
		t.Fatal("the guard's error stopped matching isContextLengthError: the loop would not compact at all")
	}
	// The provider's own overflow is NOT a projection: it is a measured fact.
	if isGuardProjection(errPlainContextLength()) {
		t.Fatal("a provider context-length error must not be tagged as a guard projection")
	}
}

// errPlainContextLength is the provider-shaped error the proxy returns.
func errPlainContextLength() error {
	return guardPlainErr("This model's maximum context length is 262144 tokens, prompt is 300000")
}

// guardPlainErr builds an untagged error (a provider response, not the guard).
func guardPlainErr(msg string) error { return &plainErr{s: msg} }

type plainErr struct{ s string }

func (e *plainErr) Error() string { return e.s }

// TestGuardForecastWithNothingToShrinkShipsTheRequest: the second half of the fix.
// The recovery site's decision, as a table — because the whole point is that a
// FORECAST and a MEASURED overflow must not be treated the same way.
func TestGuardForecastWithNothingToShrinkShipsTheRequest(t *testing.T) {
	forecast := preemptiveContextGuard("guard-ship-model", 900_000, 0, nil, 0, window262k, 10)
	overflow := errPlainContextLength()

	cases := []struct {
		name         string
		err          error
		overrideUsed bool
		wantShip     bool
	}{
		{"forecast, first refusal: ship it", forecast, false, true},
		{"forecast, already shipped once: stop", forecast, true, false},
		{"provider overflow: fold, then stop honestly", overflow, false, false},
		{"provider overflow after an override: stop honestly", overflow, true, false},
	}
	for _, tc := range cases {
		got := contextRecoveryExhausted(tc.err, tc.overrideUsed)
		if got != tc.wantShip {
			t.Fatalf("%s: contextRecoveryExhausted = %v, want %v", tc.name, got, tc.wantShip)
		}
	}
}

// TestRequestShippedTokensPricesImagesAsImages: base64 image bytes are not
// chars/3 — a vision encoder packs an image into tiles. The measure must price
// them the way the existing estimator does, and never fold them into text.
func TestRequestShippedTokensPricesImagesAsImages(t *testing.T) {
	b64 := "data:image/png;base64," + strings.Repeat("A", 4000)
	rows := []StandardMessageReq{
		{Role: "system", Content: strings.Repeat("s", 300)},
		{Role: "user", Content: []StandardContentReq{
			{Type: "text", Text: strings.Repeat("u", 900)},
			{Type: "image_url", ImageURL: &utils.ContentImageURL{URL: b64}},
		}},
	}
	text, images := requestShippedTokens(rows)
	if want := (300 + 900) / summaryCharsPerToken; text != want {
		t.Fatalf("text tokens = %d, want %d (images must not be counted as text)", text, want)
	}
	wantImg := (len(b64) - strings.Index(b64, ";base64,") - len(";base64,")) / 4
	if images != wantImg {
		t.Fatalf("image tokens = %d, want %d (the vision price)", images, wantImg)
	}
	// A base64 image at chars/3 would be ~3x its real price: that is the
	// over-count the separation exists to avoid.
	if at3 := len(b64) / summaryCharsPerToken; at3 <= images {
		t.Fatalf("the fixture must be big enough that bytes/3 (%d) over-prices the image (%d)", at3, images)
	}
}
