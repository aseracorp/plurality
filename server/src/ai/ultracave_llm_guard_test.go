package ai

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ultracave_llm_guard_test.go — the compaction path's structural invariants,
// checked on the code itself.
//
// THE INVARIANT CHANGED. It used to be "compaction must NEVER call an LLM".
// That is no longer the design: the LLM summarisation call is back, as the LAST
// THING that touches the context a durable fold is about to evict, on the
// conversation's CHAT model, on a request that replays the KV-cached prefix one
// final time. What replaced it, and what this file enforces:
//
//  1. THE LLM IS THE FIRST ATTEMPT, ON THE CHAT MODEL. The fold path reaches
//     exactly ONE entry point that puts bytes on the wire to a model:
//     GenerateCheckpointSummaryFromRequest (fed by buildSummaryRequestFromView,
//     which replays the production chat request builder). It must NOT reach
//     SendChatCompletion (a fold must never take a chat turn, and that path
//     streams), and must NOT reach fastShortcutModels (the fast model's KV
//     prefix is not warm for this conversation and it never saw the history).
//
//  2. ITS FAILURE CAN NEVER LOSE A FOLD. Asserted structurally, not with a
//     runtime counter: the summariser call site sits in a branch, and that
//     branch's failure path calls the machine-only renderer
//     (machineOnlyFoldRenderer, the deterministic megacave render) and folds. A
//     runtime counter only shows the branch one fixture happened to take; the
//     AST shows the shape of every fold.
//
//  3. NO PROMPT-HEAD MOVER ON THE FOLD PATH. db.ReplaceCheckpoint (deletes the
//     pair and renumbers seq) stays at zero. The clock rule is the one rule that
//     had to be refined, and the refinement is explicit: the replay builds the
//     clock (buildChatRequestParts -> appendCurrentTimeTail, which reads
//     time.Now) ONLY to take it back off with stripCurrentTimeTail, the
//     documented inverse — the clock is the only per-turn mutation of the chat
//     prefix, and a timestamp must never be narrated into the stored roll-up. So
//     a time.Now hit on the fold path is excused ONLY while the same sweep also
//     reaches stripCurrentTimeTail; a fold path that reads the clock without the
//     strip fails.
//
//  4. THE STORED ROLL-UP IS A CHAIN: the LLM's text is megacaved before it is
//     stored (llmFoldSummaryRenderer), the prior is carried verbatim, and the
//     escape valve (MegacaveSummaryChain, registered with db) compresses the
//     prior — never the newest section — when the chain outgrows the bound.

// summarisationWireEntries are the functions that put bytes on the wire to a
// model. Exactly ONE of these may be reachable from the fold path.
var summarisationWireEntries = map[string]bool{
	"GenerateCheckpointSummaryFromRequest": true,
	"GenerateCheckpointSummary":            true,
	"SendChatCompletion":                   true,
	"GenerateTitleForMessage":              true,
	"GenerateImage":                        true,
}

// forbiddenFoldCalls may NEVER be reachable from the fold path:
// SendChatCompletion would make the fold take a chat turn; fastShortcutModels
// would route the summary to the FAST model (the requirement is the chat
// model); the legacy GenerateCheckpointSummary wrapper posts to LiteLLMBaseURL
// directly, which is a different cache than the chat request's route, so the
// fold must use the request-taking form instead.
var forbiddenFoldCalls = map[string]bool{
	"SendChatCompletion":        true,
	"fastShortcutModels":        true,
	"GenerateCheckpointSummary": true,
}

// llmGuard is the result of one static sweep of the compaction path.
type llmGuard struct {
	hits    []string        // "funcName -> callee" for summarisation wire entries
	forbids []string        // "funcName -> callee" for forbiddenFoldCalls
	checks  []string        // prompt-head movers: db.ReplaceCheckpoint / *.Now
	reached map[string]bool // every function reachable from the roots
	funcs   map[string]*ast.FuncDecl
}

func (g *llmGuard) llmHits() int          { return len(g.hits) }
func (g *llmGuard) destructive() []string { return g.checks }

// ucGuardSources: every non-test .go file of package ai. The fold path now
// crosses files (eco.go -> eco_summarize.go -> index.go -> clock_tail.go), so a
// single-file sweep silently under-counts reachability — which is exactly how a
// "no LLM" guard can look green while the path calls one two hops away.
func ucGuardSources(t *testing.T) []string {
	t.Helper()
	dir := "."
	if _, err := os.Stat("eco.go"); err != nil {
		dir = "ai"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, dir+"/"+n)
		}
	}
	if len(out) == 0 {
		t.Fatal("no sources found for the guard sweep")
	}
	return out
}

// installLLMGuard sweeps every function of package ai reachable from the
// compaction entry points, following calls to functions defined in this package.
func installLLMGuard(t *testing.T) *llmGuard {
	t.Helper()
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, src := range ucGuardSources(t) {
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name != nil {
				if _, dup := funcs[fd.Name.Name]; dup {
					continue
				}
				funcs[fd.Name.Name] = fd
			}
		}
	}

	roots := []string{
		"runEcoSummary", "durableFold", "CompactConversationMachineOnly",
		"ultracaveDurableFoldForLoop", "logFoldResult", "UltracaveOverflowFoldFrom",
		"SentViewTokens", "filterCheckpointsForRequest",
	}
	// PASS 1: the full transitive closure of the roots, with no recording. The
	// clock excuse (rule 3) must be judged against the WHOLE fold path, not
	// against whatever has been visited so far: a depth-first walk reaches
	// buildChatRequestParts (which builds the clock) BEFORE stripCurrentTimeTail
	// in source order, so a single pass would report the clock as a prompt-head
	// mover on a path that strips it.
	closure := map[string]bool{}
	var reach func(name string)
	reach = func(name string) {
		if closure[name] {
			return
		}
		closure[name] = true
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
				reach(fn.Name)
			case *ast.SelectorExpr:
				// Package-qualified calls are outside package ai: not walkable.
				_ = fn
			}
			return true
		})
	}
	for _, r := range roots {
		reach(r)
	}

	seen := map[string]bool{}
	g := &llmGuard{funcs: funcs, reached: closure}
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
				if summarisationWireEntries[fn.Name] {
					g.hits = append(g.hits, name+" -> "+fn.Name)
				}
				if forbiddenFoldCalls[fn.Name] {
					g.forbids = append(g.forbids, name+" -> "+fn.Name)
				}
				walk(fn.Name)
			case *ast.SelectorExpr:
				sel := fn.Sel.Name
				if summarisationWireEntries[sel] {
					g.hits = append(g.hits, name+" -> "+sel)
				}
				if forbiddenFoldCalls[sel] {
					g.forbids = append(g.forbids, name+" -> "+sel)
				}
				// The destructive checkpoint shape: it DELETES the old pair and
				// renumbers seq, which rewrites the head of every prompt.
				if sel == "ReplaceCheckpoint" {
					g.checks = append(g.checks, name+" -> db."+sel)
				}
				// A clock in the STORED summary = a new head on every fold. The
				// replay may build the clock only to strip it, so the hit is
				// excused ONLY while stripCurrentTimeTail is reachable on the
				// same sweep (doc comment, rule 3).
				if sel == "Now" && fn.X != nil && !closure["stripCurrentTimeTail"] {
					g.checks = append(g.checks, name+" -> "+identName(fn.X)+".Now")
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

// identName is a defensive printer for the receiver of a *.Now call.
func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// TestUltracaveCompactionPathUsesExactlyOneChatModelSummariser replaces the old
// TestUltracaveCompactionPathHasNoLLMCall.
//
// WHY THE OLD INVARIANT NO LONGER HOLDS: the owner's design puts the LLM
// summarisation call back as the LAST reader of the context a fold is about to
// evict, reusing the KV-cached prefix. "No LLM anywhere in compaction" would
// forbid the feature outright. What the old test protected against — a model
// call in the request/visibility path, a fold that takes a chat turn, the fast
// model, and anything that moves the prompt head — is still enforced, by name.
func TestUltracaveCompactionPathUsesExactlyOneChatModelSummariser(t *testing.T) {
	g := installLLMGuard(t)

	// (1) The fold path reaches a summariser at all — the feature is wired.
	if len(g.hits) == 0 {
		t.Fatal("the fold path reaches NO summarisation entry point: durableFold does not attempt the LLM (the design requires LLM-FIRST)")
	}
	// (2) EXACTLY ONE wire entry, and it is the chat-model summariser.
	seen := map[string]bool{}
	for _, h := range g.hits {
		parts := strings.Split(h, " -> ")
		seen[parts[len(parts)-1]] = true
	}
	if len(seen) != 1 {
		t.Fatalf("the fold path reaches %d summarisation entry points %v, want exactly 1 (GenerateCheckpointSummaryFromRequest)", len(seen), seen)
	}
	for name := range seen {
		if name != "GenerateCheckpointSummaryFromRequest" {
			t.Fatalf("the fold path summarises via %q, want GenerateCheckpointSummaryFromRequest (the request-taking form, which posts to the chat model's own route)", name)
		}
	}
	// (3) The chat-model requirement: SendChatCompletion (a chat turn) and
	// fastShortcutModels (the fast model) must be unreachable.
	if len(g.forbids) != 0 {
		t.Fatalf("the fold path reaches a forbidden call: %v", g.forbids)
	}
	// (4) No prompt-head mover (clock hits are excused only by the strip pairing,
	// which TestFoldClockIsAlwaysStripped pins).
	if d := g.destructive(); len(d) != 0 {
		t.Fatalf("the fold path reaches a prompt-head mover: %v", d)
	}
}

// TestFoldClockIsAlwaysStripped pins rule 3: the fold path builds the clock (the
// replayed builder attaches it) ONLY to strip it. If the path reaches
// appendCurrentTimeTail it MUST also reach stripCurrentTimeTail; the strip is
// proven the exact inverse by TestStripCurrentTimeTailIsTheInverseOfAppend.
func TestFoldClockIsAlwaysStripped(t *testing.T) {
	g := installLLMGuard(t)
	if !g.reached["appendCurrentTimeTail"] {
		t.Fatal("the fold replay no longer goes through the production request builder — the summary prefix is no longer the chat prefix (the cache-reuse guarantee is gone)")
	}
	if !g.reached["stripCurrentTimeTail"] {
		t.Fatal("the fold path reaches the clock WITHOUT stripCurrentTimeTail: a timestamp would enter the stored roll-up and the replay would not be byte-identical")
	}
	if !g.reached["buildSummaryRequestFromView"] {
		t.Fatal("the fold does not replay the chat request (buildSummaryRequestFromView): the summariser is hand-rolling a payload and paying a full prefill")
	}
	if !g.reached["buildChatRequestParts"] {
		t.Fatal("the fold summary does not use the SHARED request builder, so its prompt can drift from the chat request's")
	}
}

// TestFoldSummariserFailureFallsBackToMachineRenderer is the AST proof of "the
// LLM's failure can never lose a fold": durableFold's summariser call site sits
// in a branch, the success branch renders the megacaved LLM text, and the
// failure path calls the machine-only renderer AND folds.
func TestFoldSummariserFailureFallsBackToMachineRenderer(t *testing.T) {
	g := installLLMGuard(t)
	fd := g.funcs["durableFold"]
	if fd == nil || fd.Body == nil {
		t.Fatal("durableFold not found")
	}

	var summariserIf *ast.IfStmt
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok || summariserIf != nil {
			return true
		}
		// The summariser call site: in the if's Init (the
		// `if x, err := attempt(...); err == nil` shape) or in its Cond.
		if callsSummariser(is.Init) || callsSummariser(is.Cond) {
			summariserIf = is
		}
		return true
	})
	if summariserIf == nil {
		t.Fatal("durableFold has no branch on a summariser call: the LLM is not the FIRST attempt with a failure branch")
	}

	// The success branch must render through the LLM renderer (which megacaves
	// the LLM text), so the stored section is a megacaved LLM summary.
	if !callsNamed(summariserIf.Body, "llmFoldSummaryRenderer") {
		t.Fatal("the summariser's success branch does not use llmFoldSummaryRenderer: the LLM text would be stored un-megacaved, breaking the chain contract")
	}

	// THE FAILURE PATH: an explicit else-branch, or the fall-through after the
	// if (the `if ok { return ... }` shape). The machine-only renderer must be
	// reachable on it, and it must be a FOLD, not just a log line.
	after := statementsAfter(fd.Body.List, summariserIf)
	onFailure := callsNamed(summariserIf.Else, "machineOnlyFoldRenderer")
	folds := callsSelectorNamed(summariserIf.Else, "CompactConversationDurable")
	for _, s := range after {
		if callsNamed(s, "machineOnlyFoldRenderer") {
			onFailure = true
		}
		if callsSelectorNamed(s, "CompactConversationDurable") {
			folds = true
		}
	}
	if !onFailure {
		t.Fatal("durableFold's failure path does not call machineOnlyFoldRenderer: an LLM failure has no machine-only fallback, so a fold CAN be lost to the LLM")
	}
	if !folds {
		t.Fatal("the machine-only fallback path does not call db.CompactConversationDurable: the fallback is not a fold")
	}
}

// callsSummariser reports whether the node contains a call to the fold's
// summariser entry points.
func callsSummariser(n ast.Node) bool {
	return callsAny(n, "attemptFoldSummary", "buildSummaryRequestFromView", "GenerateCheckpointSummaryFromRequest")
}

// callsAny reports whether the node contains a call to any of `names`, in bare
// or package-qualified form.
func callsAny(n ast.Node, names ...string) bool {
	if n == nil {
		return false
	}
	want := map[string]bool{}
	for _, s := range names {
		want[s] = true
	}
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		ce, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := ce.Fun.(type) {
		case *ast.Ident:
			if want[fn.Name] {
				found = true
			}
		case *ast.SelectorExpr:
			if want[fn.Sel.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

// callsNamed reports whether the node contains a call to the named function.
func callsNamed(n ast.Node, name string) bool { return callsAny(n, name) }

// callsSelectorNamed is callsNamed specialised to a package-qualified call.
func callsSelectorNamed(n ast.Node, name string) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		ce, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ce.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}

// statementsAfter returns the statements of `list` that follow `target` at the
// same nesting level (the fall-through of an `if { return }` shape).
func statementsAfter(list []ast.Stmt, target ast.Stmt) []ast.Stmt {
	for i, s := range list {
		if s == target {
			return list[i+1:]
		}
	}
	return nil
}

// TestFoldGuardNamesAreNotDeadCode: the assertions above name specific
// functions, so those functions must exist and be wired the way the guard
// describes — a rename silently defeats the guard.
func TestFoldGuardNamesAreNotDeadCode(t *testing.T) {
	g := installLLMGuard(t)
	for _, name := range []string{
		"machineOnlyFoldRenderer", "llmFoldSummaryRenderer", "attemptFoldSummary",
		"buildSummaryRequestFromView", "GenerateCheckpointSummaryFromRequest",
		"stripCurrentTimeTail", "MegacaveSummaryChain",
	} {
		if g.funcs[name] == nil {
			t.Fatalf("%s no longer exists: the guard's assertions about it are vacuous", name)
		}
	}
	fd := g.funcs["durableFold"]
	if !callsNamed(fd.Body, "machineOnlyFoldRenderer") || !callsNamed(fd.Body, "llmFoldSummaryRenderer") {
		t.Fatal("durableFold does not use both renderers: the LLM-first + megacave + fallback shape is not what the guard describes")
	}
	// The chain compressor must be registered with the storage layer, or the
	// escape valve in db.composeDurableSummary is inert.
	registered := false
	for name, fn := range g.funcs {
		if name == "init" && fn.Recv == nil && callsSelectorNamed(fn.Body, "SetSummaryChainCompressor") {
			registered = true
		}
	}
	if !registered {
		t.Fatal("MegacaveSummaryChain is not registered as db's summary chain compressor: the roll-up chain's escape valve cannot fire")
	}
}
