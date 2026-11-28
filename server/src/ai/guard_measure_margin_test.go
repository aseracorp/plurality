package ai

// guard_measure_margin_test.go — the guard's estimate is a FLOOR, and it must
// keep headroom for that.
//
// MEASURED, 2026-11-27, model strata-swift, window 262144. The guard's own
// compared number, from the live log, against what the backend measured for the
// SAME request:
//
//	76c17afbb288581be0d23812  guard compared 224193  backend measured 262705  REFUSED
//	9f0ab2352bf2bdc40e77384b  guard compared 114823  proxy  measured 112235  served, cache 100%
//
// The refused request is the freeze. The guard priced it 38512 tokens (14.7%)
// under the window, so it shipped; the backend rejected it as an overflow; and
// the retry/slot chain froze two conversations for 23 minutes (see
// providerRejectionIsOverflow and backoffOffSlot in utils for that chain).
//
// These tests reproduce the comparison END TO END — the estimate, the 1.08
// compression discount, and the margin — rather than feeding a post-discount
// number into a process where no compression was ever observed. That distinction
// matters: the first version of this file did the latter, passed, and proved
// nothing.

import (
	"testing"
)

const (
	// prodRefusedEstimate is the guard's estimate BEFORE the compression
	// discount — shipped rows + schemas at chars/3 — from the log line
	// "est 242493 tokens / factor 1.08 = 224193 post-compression".
	prodRefusedEstimate = 242_493
	// prodRefusedMeasured is what the backend charged for that same request.
	prodRefusedMeasured = 262_705

	// prodServedEstimate / prodServedMeasured: the request 11 minutes earlier,
	// served at 100% cache hit. The guard's post-discount 114823 against a proxy
	// measurement of 112235 — it OVER-counted by 2.3% and stayed silent.
	prodServedEstimate = 124_196
	prodServedMeasured = 112_235

	// prodFactorBefore / prodFactorAfter are the paired proxy counts that
	// produce the 1.08 factor in those log lines.
	prodFactorBefore = 1080
	prodFactorAfter  = 1000
)

// withProdFactor registers the deployment's observed compression factor under a
// test-private model name, so the guard applies the SAME discount production
// applied. Unique names keep the shared per-model map clean between tests.
func withProdFactor(t *testing.T, name string) string {
	t.Helper()
	RecordCompression(name, prodFactorBefore, prodFactorAfter)
	if f := CompressionFactor(name); f < 1.079 || f > 1.081 {
		t.Fatalf("fixture: compression factor = %v, want 1.08", f)
	}
	return name
}

// nilSchemaTokens is the schema allowance a nil tools array contributes, so a
// test can sit exactly on a ceiling without tripping over it.
func nilSchemaTokens() int { return len(marshalNoEscape(nil)) / summaryCharsPerToken }

// applyFactor is the guard's discount: estimate / factor, as the log prints it.
func applyFactor(estimate int, factor float64) int {
	return int(float64(estimate) / factor)
}

// TestGuardMarginFiresOnTheRequestTheBackendRefused: the guard must fold the
// request whose estimate sat ~15% under the backend's measurement, with the 1.08
// discount applied exactly as production applied it.
func TestGuardMarginFiresOnTheRequestTheBackendRefused(t *testing.T) {
	if prodRefusedMeasured <= window262k-guardReplyReserve {
		t.Fatalf("fixture drift: measured %d against window %d is no longer an overflow",
			prodRefusedMeasured, window262k)
	}
	// Pin the production shape: the estimate is UNDER the window and the discount
	// pushes it further under. That is the whole defect.
	post := applyFactor(prodRefusedEstimate, 1.08)
	if post >= window262k-guardReplyReserve {
		t.Fatalf("fixture drift: post-discount %d is not under the %d ceiling", post, window262k-guardReplyReserve)
	}
	deficit := prodRefusedMeasured - post
	if pct := deficit * 100 / prodRefusedMeasured; pct < 14 {
		t.Fatalf("fixture drift: the deficit is %d%%; this file is pinned to ~15%%", pct)
	}

	model := withProdFactor(t, "margin-refused-model")
	err := preemptiveContextGuard(model, prodRefusedEstimate, 0, nil, 0, window262k, 391)
	if err == nil {
		t.Fatalf("the guard is silent on the exact request the backend refused (est %d, factor 1.08, measured %d, window %d): "+
			"the request ships, is rejected, and the retry/slot chain freezes every serialized conversation on the box",
			prodRefusedEstimate, prodRefusedMeasured, window262k)
	}
	if !isContextLengthError(err) {
		t.Fatalf("margin-driven guard error is not matched by isContextLengthError, the loop will not compact: %v", err)
	}
	if !isGuardProjection(err) {
		t.Fatalf("the guard's error lost its projection tag: %v", err)
	}
}

// TestGuardMarginLeavesTheRequestTheBackendServedAlone: the margin must NOT fold
// a request the backend served. That request sits at 43% of the window, so a
// margin that fires there is a sledgehammer, not a fix.
func TestGuardMarginLeavesTheRequestTheBackendServedAlone(t *testing.T) {
	if prodServedMeasured > window262k-guardReplyReserve {
		t.Fatal("fixture drift: the served request is no longer a fit")
	}
	model := withProdFactor(t, "margin-served-model")
	if err := preemptiveContextGuard(model, prodServedEstimate, 0, nil, 0, window262k, 120); err != nil {
		t.Fatalf("the guard folds the request the backend served at 100%% cache hit (est %d, measured %d): %v",
			prodServedEstimate, prodServedMeasured, err)
	}
}

// TestGuardMarginIsTheMeasuredDeficit: the constant is derived from the log, so
// the test derives it too. The margin must cover the deficit the guard made on
// the refused request, and must stay well under half the window.
func TestGuardMarginIsTheMeasuredDeficit(t *testing.T) {
	post := applyFactor(prodRefusedEstimate, 1.08)
	deficitPct := (prodRefusedMeasured - post) * 100 / prodRefusedMeasured
	if guardMeasureMarginPercent < deficitPct {
		t.Fatalf("margin %d%% is below the measured deficit %d%%: the refused request ships again",
			guardMeasureMarginPercent, deficitPct)
	}
	if guardMeasureMarginPercent > 30 {
		t.Fatalf("margin %d%% eats a third of the window before a reply is allowed", guardMeasureMarginPercent)
	}
	t.Logf("margin %d%% covers a %d%% measured deficit (est %d, post-discount %d, backend %d)",
		guardMeasureMarginPercent, deficitPct, prodRefusedEstimate, post, prodRefusedMeasured)
}

// TestGuardReserveIsProportionalToTheWindow: the deficit is a fraction of the
// prompt, so a fixed token count is right for one model size only.
func TestGuardReserveIsProportionalToTheWindow(t *testing.T) {
	if big, small := guardReserve(1_048_576), guardReserve(32_768); big <= small {
		t.Fatalf("reserve does not scale: 32k window -> %d, 1M window -> %d", small, big)
	}
	for _, w := range []int{32_768, 131_072, 262_144, 1_048_576} {
		margin := guardReserve(w) - guardReplyReserve
		// Integer division floors the margin, so on a small window the
		// recomputed percentage is the constant minus the rounding (32768*15/100
		// = 4915 -> 14%). Accept the constant, allow 1 point of floor.
		if got := margin * 100 / w; got < guardMeasureMarginPercent-1 || got > guardMeasureMarginPercent {
			t.Fatalf("window %d: margin is %d%%, want %d%% (±1 for integer floor)", w, got, guardMeasureMarginPercent)
		}
		if r := guardReserve(w); r >= w {
			t.Fatalf("window %d: reserve %d swallows the whole window", w, r)
		}
	}
}

// TestGuardMarginFiresInsideTheBandTheOldCeilingAccepted: the band between the
// old ceiling and the new one is where the backend's tokenizer over-count lives.
// Folding inside the band is folding while a fold is still buildable and
// shippable, instead of after the provider has refused with no foldable rows
// left — the recovery path that produced the 23-minute freeze.
func TestGuardMarginFiresInsideTheBandTheOldCeilingAccepted(t *testing.T) {
	newCeiling := window262k - guardReserve(window262k)
	oldCeiling := window262k - guardReplyReserve
	if newCeiling >= oldCeiling {
		t.Fatalf("the margin did not lower the ceiling: %d vs %d", newCeiling, oldCeiling)
	}
	schema := nilSchemaTokens()
	inBand := (newCeiling + oldCeiling) / 2
	if inBand <= newCeiling {
		t.Fatalf("fixture: %d is not inside the band (ceiling %d)", inBand, newCeiling)
	}
	// A model with no compression observed: the margin alone decides.
	plain := "margin-band-model"
	if f := CompressionFactor(plain); f != 1.0 {
		t.Fatalf("fixture expects an undiscounted model, factor = %v", f)
	}
	if err := preemptiveContextGuard(plain, inBand, 0, nil, 0, window262k, 400); err == nil {
		t.Fatalf("a %d-token view inside the band must fold (new ceiling %d, old %d)", inBand, newCeiling, oldCeiling)
	}
	// The old ceiling sat in the band and accepted it: that is the shape that
	// shipped and was refused.
	if oldCeiling <= inBand {
		t.Fatalf("fixture: the band is above the old ceiling (%d <= %d)", oldCeiling, inBand)
	}
	// Exactly at the new ceiling: no fold. The margin reserves its band and no more.
	if err := preemptiveContextGuard(plain, newCeiling-schema, 0, nil, 0, window262k, 400); err != nil {
		t.Fatalf("the guard folds at exactly the ceiling %d: %v", newCeiling, err)
	}
}
