//go:build e2e

package ai

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/azukaar/plurality/src/utils"
)

// mkRealConv is mkConv with a realistic tool chain: every tool result carries
// a tool_call_id and is preceded by an assistant message whose tool_calls
// declare that id. mkConv (from eco_resolver_test.go) builds bare tool rows,
// which no provider accepts; the emitter must send wire the request path would
// really produce, so the harness validator can assert pairing.
func mkRealConv(turns, bigToolChars int) []utils.Message {
	var msgs []utils.Message
	for i := 0; i < turns; i++ {
		n := itoa2(i)
		msgs = append(msgs, mkMsg("user", "user request "+n))
		id := "call_" + n
		msgs = append(msgs, utils.Message{
			Role:    "assistant",
			Content: utils.NewTextContent(""),
			ToolCalls: []utils.ToolCall{{
				ID:       id,
				Type:     "function",
				Function: utils.FunctionCall{Name: "system_tools__shell_exec", Arguments: "{\"command\":\"scan\"}"},
			}},
		})
		msgs = append(msgs, utils.Message{
			Role:       "tool",
			Name:       "system_tools__shell_exec",
			ToolCallID: id,
			Content:    utils.NewTextContent(strings.Repeat("x", bigToolChars)),
		})
		msgs = append(msgs, mkMsg("assistant", "assistant reply "+n))
	}
	return msgs
}

func itoa3(i int) string { return strconv.Itoa(i) }

func itoa2(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestEmitE2EScenarios emits, for every eco stage, the EXACT wire message array
// the request path would send to the backend: resolveEcoContextCompaction ->
// convertMessagesToOpenAI. Driven by the python strata harness (build tag e2e
// keeps it out of the normal suite).
//
// Stages are forced with budgets derived from the megacave excerpt cap, the
// same way eco_resolver_test.go does it, so the escalation chain
// fits -> A -> B -> C -> D is exercised deterministically.
func TestEmitE2EScenarios(t *testing.T) {
	out := os.Getenv("PL_E2E_OUT")
	if out == "" {
		t.Skip("PL_E2E_OUT not set")
	}
	const capTokens = maxMegacaveExcerptChars / 3 // 20000 est tokens

	type scen struct {
		Name   string
		Budget int
		Turns  int
		Chars  int
	}
	small := scen{Name: "fits", Budget: 60000, Turns: 3, Chars: 100}
	big := scen{Name: "A", Budget: capTokens + 931/3 + 1000, Turns: 200, Chars: 900}
	bigB := scen{Name: "B", Budget: capTokens + 200, Turns: 200, Chars: 900}
	bigC := scen{Name: "C", Budget: 5000, Turns: 200, Chars: 900}
	bigD := scen{Name: "D", Budget: 200, Turns: 200, Chars: 900}
	bigD2 := scen{Name: "D_shrink", Budget: 1000, Turns: 200, Chars: 900}

	res := map[string]any{}
	for _, s := range []scen{small, big, bigB, bigC, bigD, bigD2} {
		conv := mkRealConv(s.Turns, s.Chars)
		// The system prompt is prepended by the caller, exactly as index.go does.
		view, foldFrom := resolveEcoContextCompaction(conv, 65536, s.Budget)
		full := append([]utils.Message{mkMsg("system", "You are a terse assistant. Answer briefly.")}, view...)
		wire, _ := convertMessagesToOpenAI(full, utils.Model{Name: "strata-swift"})

		// Which stage did it land on? The artifact marker tells us.
		stage := "none"
		if id, _, ok := findMegacaveArtifact(view); ok {
			stage = strings.TrimPrefix(id, "megacave_")
		}
		b, err := json.Marshal(map[string]any{
			"scenario": s.Name,
			"stage":    stage,
			"budget":   s.Budget,
			"approx":   approxTokens(view),
			"foldFrom": foldFrom,
			"inMsgs":   len(conv),
			"outMsgs":  len(view),
			"wire":     wire,
		})
		if err != nil {
			t.Fatalf("%s: marshal: %v", s.Name, err)
		}
		res[s.Name] = json.RawMessage(b)
		t.Logf("%-9s stage=%-4s %4d msgs -> %3d msgs, ~%d tok / budget %d, foldFrom=%d",
			s.Name, stage, len(conv), len(view), approxTokens(view), s.Budget, foldFrom)
	}

	// Growth sweep: ONE production-shaped budget, conversation growing turn by
	// turn, printing which stage the resolver lands on. This is the real
	// escalation the request path takes: fits -> A -> B -> C -> D as content
	// outgrows the budget, on the same budget (no per-scenario tuning).
	sweepBudget := ecoContentBudget("strata-sweep-unknown-model") // default window - reserve
	prevStage := ""
	for _, turns := range []int{1, 4, 8, 16, 32, 64, 96, 128, 160, 200, 300, 500, 800} {
		conv := mkRealConv(turns, 900)
		view, ff := resolveEcoContextCompaction(conv, 65536, sweepBudget)
		stage := "fits"
		if id, _, ok := findMegacaveArtifact(view); ok {
			stage = "stage-" + strings.TrimPrefix(id, "megacave_")
		}
		var users int
		for _, m := range view {
			if m.Role == "user" {
				users++
			}
		}
		mark := ""
		if stage != prevStage {
			mark = "  <== transition"
		}
		prevStage = stage
		t.Logf("SWEEP turns=%3d budget=%d content≈%7d tok -> %-9s view=%2d msgs users=%d foldFrom=%d%s",
			turns, sweepBudget, approxTokens(conv), stage, len(view), users, ff, mark)
	}

	// Stage-band probe: which stage does a FIXED 200-turn conversation land on
	// as the budget tightens, and which model context windows produce those
	// budgets (budget = window - 6656). This localises the blast radius of a
	// broken stage: stage C sits in a narrow band, so only small-window models
	// ever reach it.
	conv200 := mkRealConv(200, 900)
	prev := ""
	for _, b := range []int{60000, 30000, 22000, 21000, 20400, 15000, 10000, 6000, 5000, 3500, 2500, 1500, 800, 200} {
		view, ff := resolveEcoContextCompaction(conv200, 65536, b)
		stage := "fits"
		if id, _, ok := findMegacaveArtifact(view); ok {
			stage = strings.TrimPrefix(id, "megacave_")
		}
		users := 0
		for _, m := range view {
			if m.Role == "user" {
				users++
			}
		}
		mark := ""
		if stage != prev {
			mark = "  <-- band starts (window ~" + itoa3(b+6656) + " tokens)"
		}
		prev = stage
		t.Logf("BAND budget=%6d -> stage %s  view=%2d msgs users=%d foldFrom=%d%s", b, stage, len(view), users, ff, mark)
	}

	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, name := range []string{"fits", "A", "B", "C", "D", "D_shrink"} {
		if err := enc.Encode(res[name]); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
