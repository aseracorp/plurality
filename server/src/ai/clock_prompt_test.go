package ai

import (
	"strings"
	"testing"
)

// The system prompt is built ONCE per request and sits at the very head of the
// token stream — the part the KV/prompt cache matches on. It must therefore be
// byte-stable across turns, and it must not dangle.
//
// Upstream shipped these prompts ending in the literal clause "The time is "
// with nothing after it, because the timestamp that used to be filled in there
// was moved out (a timestamp in the head invalidates the whole prefix every
// turn). The clause was never removed, so every system prompt ended with a
// sentence fragment, and the miniApp variants ended with
// "\n The time is: \n". The clock now rides on the tail of the newest
// text-bearing message (see clock_tail.go), so the head stays stable.
func TestSystemPromptsHaveNoDanglingTimeClause(t *testing.T) {
	prompts := map[string]string{
		"baseSystemPrompt":   baseSystemPrompt(),
		"callSystemPrompt":   callSystemPrompt,
		"initialAppPrompt":   initialAppPrompt,
		"initialAppForCalls": initialAppPromptForCalls,
	}
	for name, p := range prompts {
		if strings.Contains(p, "The time is") {
			t.Fatalf("%s: system prompt still mentions the time; the clock belongs on the request tail: %q", name, tail(p))
		}
		if strings.HasSuffix(strings.TrimRight(p, " \n"), ":") {
			t.Fatalf("%s: system prompt ends on a dangling colon: %q", name, tail(p))
		}
	}
}

// The clock is injected per request; a static system prompt is the whole point.
func TestSystemPromptIsByteStableAcrossCalls(t *testing.T) {
	first := baseSystemPrompt() + webhookExtSuffix() + portExtSuffix()
	for i := 0; i < 50; i++ {
		if got := baseSystemPrompt() + webhookExtSuffix() + portExtSuffix(); got != first {
			t.Fatalf("system prompt changed on call %d: %q != %q", i, tail(got), tail(first))
		}
	}
}

func tail(s string) string {
	if len(s) > 120 {
		return "..." + s[len(s)-120:]
	}
	return s
}
