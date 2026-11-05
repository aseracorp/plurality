package ai

import "testing"

// TestEcoTriggerFor pins the documented contract in src/auth/config.go:
// "the effective trigger is min(TriggerTokens, round(ContextFraction * window))".
// That sentence had no code behind it — ContextFraction was read nowhere — which
// is why an operator lowering eco.context_fraction changed nothing while a live
// strata conversation grew past the engine prompt-cache cliff (reuse 89.7%
// under 200k tokens -> 25.3% over, 15s -> 154s prefill per turn).
func TestEcoTriggerFor(t *testing.T) {
	cases := []struct {
		name     string
		static   int
		window   int
		fraction float64
		want     int
	}{
		{"fraction scales trigger below static", 200_000, 100_000, 0.5, 50_000},
		{"min wins when fraction exceeds static", 100_000, 262_144, 0.8, 100_000},
		{"fraction 0 disables dynamic part", 100_000, 262_144, 0, 100_000},
		{"negative fraction disables dynamic part", 100_000, 64_000, -1, 64_000},
		{"unknown window falls back to static", 100_000, 0, 0.8, 100_000},
		{"window always caps the trigger", 500_000, 64_000, 0.9, 57_600},
		{"rounds to nearest token", 200_000, 33_000, 0.5, 16_500},
		// The live strata shape: 0.8*262144 = 209715, and trigger_tokens was
		// already 209715 — so a fraction of 0.8 alone is a no-op there and the
		// operator must lower the fraction to get under the cache cliff.
		{"live strata numbers are a no-op at 0.8", 209_715, 262_144, 0.8, 209_715},
		{"lowering the fraction moves the ceiling", 209_715, 262_144, 0.45, 117_965},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ecoTriggerFor(tc.static, tc.window, tc.fraction); got != tc.want {
				t.Fatalf("ecoTriggerFor(static=%d, window=%d, fraction=%.2f) = %d, want %d",
					tc.static, tc.window, tc.fraction, got, tc.want)
			}
		})
	}
}

// TestEcoTriggerForNeverExceedsWindow is the safety property: the trigger must
// never be above the model window, or compaction cannot keep the request inside
// the context at all.
func TestEcoTriggerForNeverExceedsWindow(t *testing.T) {
	for _, fraction := range []float64{0, 0.1, 0.5, 0.8, 1, 1.5, 4} {
		for _, window := range []int{1, 1024, 65_536, 131_072, 262_144} {
			for _, static := range []int{1, 50_000, 100_000, 1_000_000} {
				got := ecoTriggerFor(static, window, fraction)
				if got > window {
					t.Fatalf("trigger %d exceeds window %d (static=%d fraction=%.2f)", got, window, static, fraction)
				}
				if got > static {
					t.Fatalf("trigger %d exceeds static ceiling %d (window=%d fraction=%.2f)", got, static, window, fraction)
				}
				if got <= 0 {
					t.Fatalf("trigger %d must be positive (static=%d window=%d fraction=%.2f)", got, static, window, fraction)
				}
			}
		}
	}
}
