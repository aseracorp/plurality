package ai

import "testing"

// eco_target_formula_test.go pins how the fold target is sized, and on WHICH
// AXIS.
//
// The rule: 2/3 of the trigger, DISCOUNTING THE FIXED PROMPT PART. The system
// prompt (20-30K tokens in this deployment) and the tool schemas ride every
// request and no fold can remove a single token of them, so multiplying the
// whole trigger by 2/3 charges the fold for the prompt: the history has to
// absorb the difference, and the fold keeps less history than the window
// affords.
//
// The axis matters as much as the arithmetic. The trigger gates on the
// provider's prompt_tokens (history + prompt overhead); the fold gates add a
// live tail to a roll-up (history only). So the history target is
// (trigger - fixed) * 2/3, and fixed + target is the operator's prompt number.
func TestFoldTargetExemptsTheFixedPromptPart(t *testing.T) {
	// The operator's arithmetic, on the prompt axis: a 150K trigger with a
	// 30K-token prompt folds toward ~110K, not 100K.
	const cfgTrigger = 150_000
	hist := foldTargetForTrigger(cfgTrigger)
	if want := (cfgTrigger - promptOverheadTokens) * 2 / 3; hist != want {
		t.Fatalf("history target for a %d trigger: got %d, want (trigger-fixed)*2/3 = %d",
			cfgTrigger, hist, want)
	}
	if prompt := promptOverheadTokens + hist; prompt <= cfgTrigger*2/3 {
		t.Fatalf("the post-fold prompt is %d, at/below the flat 2/3 (%d): the fixed part was discounted",
			prompt, cfgTrigger*2/3)
	}
	// The axis agreement, which is the point of the exemption: the NEXT fold
	// fires at prompt = trigger, i.e. history = trigger - fixed, so the headroom
	// one fold buys is (trigger - fixed)/3 on both axes. A target that leaves
	// less than that refires on the next turn and churns the pair.
	headroom := (cfgTrigger - promptOverheadTokens) - hist
	if want := (cfgTrigger - promptOverheadTokens) / 3; headroom < want-1 {
		t.Fatalf("a fold buys %d tokens of history headroom, want >= %d: the conversation refires immediately",
			headroom, want)
	}
	if old := cfgTrigger * 2 / 3; hist >= old {
		t.Fatalf("history target %d is at/above the old shape %d: the exemption did nothing", hist, old)
	}
	t.Logf("fold target: trigger %d -> history %d, post-fold prompt %d, headroom %d on both axes",
		cfgTrigger, hist, promptOverheadTokens+hist, headroom)
}

// TestFoldTargetStaysBelowTheFoldablePart: a history target at/above the
// foldable part (trigger - fixed) means a fold has nothing to remove on the
// prompt axis, so the hysteresis floor refuses it and the conversation churns
// the checkpoint pair every turn - rewriting the prompt head and zeroing the KV
// prefix cache.
func TestFoldTargetStaysBelowTheFoldablePart(t *testing.T) {
	for _, trigger := range []int{1_000, 8_000, 40_000, 60_000, 84_928, 124_928, 150_000, 216_000, 256_000} {
		tgt := foldTargetForTrigger(trigger)
		if tgt <= 0 {
			t.Fatalf("trigger %d: target %d: a fold aiming at zero writes nothing", trigger, tgt)
		}
		if tgt >= trigger {
			t.Fatalf("trigger %d: target %d is at/above it: the fold has nothing to remove", trigger, tgt)
		}
		if trigger <= promptOverheadTokens {
			// The window cannot host the prompt itself: there is no foldable part
			// to exempt, so the plain 2/3 shape is kept on purpose and folding
			// history alone cannot clear the gate. The overflow path owns that case.
			continue
		}
		// The post-fold prompt must sit at or under the trigger, so ONE fold is
		// enough to clear the gate. Aim higher and the next turn refires.
		prompt := promptOverheadTokens + tgt
		if prompt > trigger {
			t.Fatalf("trigger %d: a fold aiming at history %d lands the prompt at %d, ABOVE the trigger: "+
				"the next turn refires and churns the pair", trigger, tgt, prompt)
		}
		// And one fold must buy a real third of the foldable part, or the fold is
		// a no-op that rewrites the pair for nothing (cache_read pinned at 16384).
		headroom := (trigger - promptOverheadTokens) - tgt
		if want := (trigger - promptOverheadTokens) / 3; headroom < want-1 {
			t.Fatalf("trigger %d: a fold buys %d tokens of headroom, want >= %d", trigger, headroom, want)
		}
	}
	if foldTargetForTrigger(0) != 0 || foldTargetForTrigger(-5) != 0 {
		t.Fatal("no trigger must yield no target: a made-up number is worse than none")
	}
}

// TestPolicyTargetRaisesWithTheWindow: the policy target must move with the
// window, stay under the trigger, and keep the fixed part whole.
func TestPolicyTargetRaisesWithTheWindow(t *testing.T) {
	trigS, small := ecoWindowPolicy(150_000, 50_000, 131_072)
	trigB, big := ecoWindowPolicy(150_000, 50_000, 262_144)
	if big <= small {
		t.Fatalf("a doubled window must not shrink the fold target: 131072 -> %d, 262144 -> %d", small, big)
	}
	for _, tc := range []struct {
		win       int
		trig, tgt int
	}{
		{131_072, trigS, small},
		{262_144, trigB, big},
	} {
		if tc.tgt >= tc.trig {
			t.Fatalf("window %d: target %d >= trigger %d", tc.win, tc.tgt, tc.trig)
		}
		if prompt := promptOverheadTokens + tc.tgt; prompt > tc.trig {
			t.Fatalf("window %d: a fold toward history %d lands the prompt at %d, over the trigger %d: "+
				"refire and pair churn every turn", tc.win, tc.tgt, prompt, tc.trig)
		}
	}
	// A configured target bigger than the formula wins: it is a floor, not a
	// ceiling - and it is clamped below the trigger, which is what protects the
	// hysteresis floor from refusing every fold.
	if _, tgt := ecoWindowPolicy(150_000, 200_000, 131_072); tgt >= 150_000 {
		t.Fatalf("target %d must stay below the trigger 150000 even when configured above it", tgt)
	}
}
