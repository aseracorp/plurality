package ai

import (
	"os"
	"strings"
	"testing"
)

// The compaction path must be governed ONLY by a context window the proxy
// DECLARED (model_info.context_window). Two assumptions existed and are
// deleted:
//
//   - knownModelContextWindows: a built-in table matched by longest prefix of
//     the model NAME, so "qwen2.5-coder-32b-my-fork" inherited the family's
//     advertised size, not the deployment's.
//   - defaultContextWindow = 131072, returned for anything unrecognised, so a
//     32K local model was governed as a 128K one — and a window that is too
//     LARGE is the direction that ships a request the provider rejects.
//
// These tests pin that the accessor reports declarations and nothing else, and
// that the window policy degrades to the operator's configuration on unknown.

func registerDeclTestModel(name string, window int) {
	Models.mu.Lock()
	defer Models.mu.Unlock()
	Models.byName[name] = ModelEntry{Name: name, Mode: "chat", MaxContextTokens: window}
}

// readSourceFile reads a source file in this package's directory. Go tests run
// with the package directory as the working directory, so the name alone is
// enough. It lets a test pin a source-level rule (which accessor a file may
// call) where a runtime test cannot see it.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("cannot read %s: %v", name, err)
	}
	return string(b)
}

func TestDeclaredContextWindowIsDeclarationOnly(t *testing.T) {
	registerDeclTestModel("decl-small", 32_768)
	registerDeclTestModel("decl-family-variant:all", 262_144)
	// Declared ZERO: an entry that exists but declares no window.
	Models.mu.Lock()
	Models.byName["decl-zero"] = ModelEntry{Name: "decl-zero", Mode: "chat", MaxContextTokens: 0}
	Models.mu.Unlock()

	if got := Models.DeclaredContextWindow("decl-small"); got != 32_768 {
		t.Fatalf("declared window = %d, want the declared 32768", got)
	}
	// The ":suffix" variant is matched on the bare name too.
	if got := Models.DeclaredContextWindow("decl-family-variant"); got != 262_144 {
		t.Fatalf("bare-name lookup = %d, want the declared 262144", got)
	}
	// A model the table used to guess for must report 0, not a family number.
	for _, name := range []string{"gpt-4o", "gpt-4o-mini:all", "deepseek-v4-flash-0731", "llama-3.3-70b", "claude-3-5-sonnet"} {
		if got := Models.DeclaredContextWindow(name); got != 0 {
			t.Fatalf("%s: got %d, want 0 — the built-in family table is deleted, "+
				"a NAME-based guess is not a declaration", name, got)
		}
	}
	// An entry with no declaration is 0, not 131072.
	if got := Models.DeclaredContextWindow("decl-zero"); got != 0 {
		t.Fatalf("undeclared entry = %d, want 0 (defaultContextWindow is deleted)", got)
	}
	if got := Models.DeclaredContextWindow(""); got != 0 {
		t.Fatalf("empty name = %d, want 0", got)
	}
}

// TestWindowPolicyFallsBackToConfigNotAGuess: window 0 (unknown model) must
// leave the configured pair untouched. Raising the trigger from an assumed
// 128k is exactly what folded a 148K conversation toward 50K on a 262144-token
// model — the "sent 27K tokens, not containing any real history" failure.
func TestWindowPolicyFallsBackToConfigNotAGuess(t *testing.T) {
	trig, tgt := ecoWindowPolicy(100_000, 50_000, 0)
	if trig != 100_000 || tgt != 50_000 {
		t.Fatalf("unknown window: got trigger=%d target=%d, want the configured 100000/50000 verbatim",
			trig, tgt)
	}
	// A declared window below the configured trigger must cap the trigger: the
	// fold has to fire before the provider does.
	trig, tgt = ecoWindowPolicy(200_000, 50_000, 64_000)
	if trig > 64_000 {
		t.Fatalf("declared window 64000: trigger %d exceeds the window", trig)
	}
	if tgt >= trig {
		t.Fatalf("target %d >= trigger %d: a fold with nothing to remove churns the pair every turn", tgt, trig)
	}
	// target >= trigger is refused in both branches, including the unknown one.
	trig, tgt = ecoWindowPolicy(100_000, 150_000, 0)
	if tgt >= trig {
		t.Fatalf("config target above trigger: got trigger=%d target=%d, want target forced below the trigger", trig, tgt)
	}
}

// TestCompactionPathUsesDeclaredWindowOnly: no compaction-path file may reach
// for an assumed window. Pinned at the source level so a future helper that
// re-introduces a default is caught here, where the incident class is.
func TestCompactionPathUsesDeclaredWindowOnly(t *testing.T) {
	for _, f := range []string{"eco.go", "eco_summarize.go", "index.go", "model_registry.go", "context_compaction.go"} {
		src := readSourceFile(t, f)
		for _, banned := range []string{"MaxContextTokensForPrefixed", "builtinContextWindow", "knownModelContextWindows", "defaultContextWindow"} {
			// Only a comment mentioning the deleted name is fine: a real call is not.
			for _, line := range strings.Split(src, "\n") {
				tl := strings.TrimSpace(line)
				if strings.HasPrefix(tl, "//") {
					continue
				}
				if strings.Contains(tl, banned) {
					t.Errorf("%s: %q is present in code — the compaction path must read a DECLARED window only: %s",
						f, banned, tl)
				}
			}
		}
	}
}
