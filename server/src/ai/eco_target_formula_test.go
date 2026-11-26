package ai

import "testing"

// TestWindowPolicyTargetDeductsTheOverhead pins the target formula:
//
//	target = (trigger - promptOverheadTokens) * 2/3 + promptOverheadTokens
//
// The 2/3 applies to the HISTORY share only. The system prompt (20-40K tokens,
// measured live) plus the tool schemas ride on every request and folding cannot
// shrink them, so charging them to history makes the ratio a fiction and folds
// far harder than intended.
//
// Worked example (the user's): a 150000-token trigger and a 30000-token system
// prompt give (150000-30000)*2/3 + 30000 = 110000 — the fold keeps 80000 tokens
// of history plus the 30000 the request costs by itself.
func TestWindowPolicyTargetDeductsTheOverhead(t *testing.T) {
	for _, trigger := range []int{60_000, 100_000, 150_000, 216_000, 258_048} {
		got := windowPolicyTargetTokens(trigger)
		want := (trigger-promptOverheadTokens)*2/3 + promptOverheadTokens
		if trigger <= promptOverheadTokens {
			want = trigger * 2 / 3
		}
		if got != want {
			t.Fatalf("trigger %d: target %d, want %d", trigger, got, want)
		}
		if got >= trigger {
			t.Fatalf("trigger %d: target %d sits at/above the trigger — the fold would have nothing to remove", trigger, got)
		}
		// The HISTORY share of the target is exactly 2/3 of the history share of
		// the trigger: the ratio applies to what compaction can shrink.
		histIn := trigger - promptOverheadTokens
		histOut := got - promptOverheadTokens
		if histIn > 0 {
			if d := histOut*3 - histIn*2; d > 3 || d < -3 {
				t.Fatalf("trigger %d: history share %d is not 2/3 of %d (delta %d)", trigger, histOut, histIn, d)
			}
		}
		// The target is NOT 2/3 of the trigger: the overhead is added back whole,
		// so it is strictly more than the naive form.
		if naive := trigger * 2 / 3; got <= naive {
			t.Fatalf("trigger %d: target %d <= naive 2/3 %d — the overhead must be added back, not taxed", trigger, got, naive)
		}
	}
	// Live numbers: 262144 window, overhead 40000, reserve 4096+guard.
	trig2, tgt2 := ecoWindowPolicy(150_000, 50_000, 262_144)
	if want := (trig2-promptOverheadTokens)*2/3 + promptOverheadTokens; tgt2 != want {
		t.Fatalf("262144 window: target %d, want %d from the trigger %d", tgt2, want, trig2)
	}
	t.Logf("262144 -> trigger %d target %d (naive 2/3 would be %d)", trig2, tgt2, trig2*2/3)
}

// TestTickTargetArmWhenTheFoldMissedTarget pins the second stage: a conversation
// that has a checkpoint pair fires the tick when its sent view is above
// compaction's target, even below the trigger — the target IS the bar, so
// "target not reached" is the trigger. Bounded by the fold's progress floor,
// below which a fold cannot pay for its write and the tick must stay quiet.
func TestTickTargetArmWhenTheFoldMissedTarget(t *testing.T) {
	const trigger, target = 216_000, 157_333
	floor := ultracaveMinFoldProgressTokens(target)

	// Below the trigger and at the bar: quiet.
	if tickNeedsFold(150_000, target, trigger, target) {
		t.Fatalf("a view exactly at the target must not fire the tick")
	}
	// Over the bar by the progress floor: fires, although the trigger is far away.
	if !tickNeedsFold(150_000, target+floor, trigger, target) {
		t.Fatalf("a view %d over the target (the fold's minimum progress) must fire the tick — "+
			"the last fold missed its target and nothing else in the gate notices", floor)
	}
	// Over the bar by less than the floor: the fold cannot pay for the write.
	if tickNeedsFold(150_000, target+floor/2, trigger, target) {
		t.Fatalf("a view under the progress floor must not fire the tick — the fold would be refused every turn")
	}
	// No pair (the caller passes no target): the trigger alone governs.
	if tickNeedsFold(150_000, target+floor, trigger) {
		t.Fatalf("without a checkpoint pair the target arm must not fire — the first fold is the bootstrap fold")
	}
	// The trigger arm still governs with a pair present.
	if !tickNeedsFold(trigger+1, 10, trigger, target) {
		t.Fatalf("lastPT over the trigger must fire regardless of the target")
	}
}
