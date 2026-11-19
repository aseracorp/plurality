package ai

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// ultracave_llm_guard_test.go — the "compaction must NEVER call an LLM"
// assertion, checked on the code itself.
//
// GenerateCheckpointSummary is a plain function, not an injectable var, so it
// cannot be swapped out at runtime. The load-bearing guarantee is structural:
// no compaction entry point reaches an LLM entry point. That is exactly what
// the AST walk below proves, and it is stronger than a runtime counter (a
// runtime counter only shows the branch this fixture happened to take).
//
// The runtime side is corroborated by the fold tests: LiteLLM is not ready
// in tests, so any LLM hop would return "AI proxy is not ready" and the fold
// would abort — yet they fold rows and store a non-empty megacave roll-up.

// llmEntryPoints are the functions that put bytes on the wire to a model.
var llmEntryPoints = map[string]bool{
	"GenerateCheckpointSummary": true,
	"SendChatCompletion":        true,
	"fastShortcutModels":        true,
}

// llmGuard is the result of one static sweep of the compaction path.
type llmGuard struct {
	hits   []string // "funcName -> callee"
	checks []string // db.ReplaceCheckpoint / time.Now hits
}

func (g *llmGuard) llmHits() int          { return len(g.hits) }
func (g *llmGuard) destructive() []string { return g.checks }

// installLLMGuard sweeps eco.go: every function reachable from the compaction
// entry points, following calls to functions defined in that same file.
func installLLMGuard(t *testing.T) *llmGuard {
	t.Helper()
	src := "eco.go"
	if _, err := os.Stat(src); err != nil {
		src = "ai/eco.go"
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name != nil {
			funcs[fd.Name.Name] = fd
		}
	}

	roots := []string{"runEcoSummary", "durableFold", "CompactConversationMachineOnly",
		"logFoldResult", "UltracaveOverflowFoldFrom", "SentViewTokens", "filterCheckpointsForRequest"}
	seen := map[string]bool{}
	g := &llmGuard{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		fd := funcs[name]
		if fd == nil || fd.Body == nil {
			return
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := ce.Fun.(type) {
			case *ast.Ident:
				if llmEntryPoints[fn.Name] {
					g.hits = append(g.hits, name+" -> "+fn.Name)
				}
				walk(fn.Name)
			case *ast.SelectorExpr:
				sel := fn.Sel.Name
				if llmEntryPoints[sel] {
					g.hits = append(g.hits, name+" -> "+sel)
				}
				// The destructive checkpoint shape: it DELETES the old pair
				// and renumbers seq, which rewrites the head of every prompt.
				if sel == "ReplaceCheckpoint" {
					g.checks = append(g.checks, name+" -> db."+sel)
				}
				// A clock in the stored summary = a new head on every fold.
				if fn.X != nil && fn.Sel != nil && fn.Sel.Name == "Now" {
					g.checks = append(g.checks, name+" -> "+fn.X.(*ast.Ident).Name+".Now")
				}
			}
			return true
		})
	}
	for _, r := range roots {
		walk(r)
	}
	return g
}

// TestUltracaveCompactionPathHasNoLLMCall pins the guarantee: the compaction
// path (eco trigger, machine fold, boundary arithmetic, request filter) is
// free of LLM entry points, of the destructive ReplaceCheckpoint shape, and
// of time.Now.
func TestUltracaveCompactionPathHasNoLLMCall(t *testing.T) {
	g := installLLMGuard(t)
	if len(g.hits) != 0 {
		t.Fatalf("compaction path reaches an LLM entry point: %v", g.hits)
	}
	if d := g.destructive(); len(d) != 0 {
		t.Fatalf("compaction path reaches a prompt-head mover: %v", d)
	}
}
