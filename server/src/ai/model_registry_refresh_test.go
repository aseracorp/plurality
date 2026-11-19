package ai

// Tests for the registry half of the per-stone toggle path.
//
// The stones an operator flips in litellm_config.yaml reach the Go server only
// through /v1/models -> ModelRegistry. Two things made a toggle a restart
// ritual rather than a feature, and both are pinned here:
//
//  1. registryEqual must treat a stone change as a change. A refresh that
//     compared only names/modes would log "nothing happened" while a real
//     toggle sat in the payload, and the polling log would be blind.
//  2. StartRegistryRefresh must pick a toggle up with no restart, and must
//     survive a proxy that is down (the previous cache keeps serving).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// body is a payload the test can swap while a test server is serving it.
// Plain string sharing would be a data race under -race (the handler runs on
// its own goroutine), so access goes through the mutex.
type body struct {
	mu sync.Mutex
	s  string
}

func (b *body) set(s string) {
	b.mu.Lock()
	b.s = s
	b.mu.Unlock()
}

func (b *body) get() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.s
}

// modelListJSON builds a /v1/models body with one chat model whose caveman
// stones are the given values.
func modelListJSON(browse, readLess, sayLess bool) string {
	return `{"object":"list","data":[{
		"id":"toggle-probe","object":"model","owned_by":"plurality","mode":"chat",
		"supports_function_calling":true,"context_window":8192,
		"caveman":{"enabled":true,"stones":{"say_less":` + boolStr(sayLess) +
		`,"read_less":` + boolStr(readLess) +
		`,"auto":false,"blocks":false,"waste_fixes":false,"browse":` + boolStr(browse) + `}}
	}]}`
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// pointRegistryAt redirects the package-level litellm base URL at a test
// server and restores it after the test.
func pointRegistryAt(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	prev := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	t.Cleanup(func() { LiteLLMBaseURL = prev })
}

func TestRegistryEqualSeesStoneToggles(t *testing.T) {
	base := []ModelEntry{{
		Name:    "m",
		Mode:    "chat",
		Caveman: CavemanSettings{Enabled: true, Stones: CavemanStones{ReadLess: true, Browse: true}},
	}}

	// Identical: no change.
	same := []ModelEntry{{
		Name:    "m",
		Mode:    "chat",
		Caveman: CavemanSettings{Enabled: true, Stones: CavemanStones{ReadLess: true, Browse: true}},
	}}
	if !registryEqual(base, same) {
		t.Fatal("identical lists must compare equal — a poll would log every time")
	}

	// The exact edit an operator makes in litellm_config.yaml.
	off := []ModelEntry{{
		Name:    "m",
		Mode:    "chat",
		Caveman: CavemanSettings{Enabled: true, Stones: CavemanStones{ReadLess: true, Browse: false}},
	}}
	if registryEqual(base, off) {
		t.Fatal("a browse stone toggle must be detected as a change")
	}

	// Whole-proxy opt-out must be visible too.
	disabled := []ModelEntry{{Name: "m", Mode: "chat"}}
	if registryEqual(base, disabled) {
		t.Fatal("disabling caveman must be detected as a change")
	}

	// Length difference.
	if registryEqual(base, nil) {
		t.Fatal("differing lengths must compare unequal")
	}
}

func TestRegistryRefreshPicksUpStoneToggle(t *testing.T) {
	pointRegistryAt(t, modelListJSON(true, true, true))
	if err := Models.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	entry, ok := Models.Get("toggle-probe")
	if !ok {
		t.Fatal("probe model missing from registry")
	}
	if !entry.Caveman.Stones.Browse {
		t.Fatal("browse should be enabled by the first payload")
	}

	// Now point the same registry at a payload with browse OFF — the proxy
	// half of the toggle (config reload) is tested in test_litellm_proxy.py;
	// this proves the Go half follows without a restart.
	pointRegistryAt(t, modelListJSON(false, true, true))
	if err := Models.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh after toggle: %v", err)
	}
	entry, _ = Models.Get("toggle-probe")
	if entry.Caveman.Stones.Browse {
		t.Fatal("browse toggle did not reach the registry")
	}
	// read_less is untouched: the toggle must not disturb other stones, since
	// it is what decides content ownership (optimizationOwners).
	if !entry.Caveman.Stones.ReadLess {
		t.Fatal("read_less must survive a browse toggle — it gates content ownership")
	}
}

func TestRegistryRefreshKeepsLastGoodOnServerError(t *testing.T) {
	pointRegistryAt(t, modelListJSON(true, true, true))
	if err := Models.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := Models.Get("toggle-probe"); !ok {
		t.Fatal("probe model missing after first refresh")
	}

	// A 500 must leave the previous cache intact (the polling loop runs
	// against a live proxy that can restart; an empty registry would take
	// every model out of the picker).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "proxy restarting", http.StatusInternalServerError)
	}))
	defer srv.Close()
	prev := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	defer func() { LiteLLMBaseURL = prev }()

	if err := Models.Refresh(context.Background()); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if _, ok := Models.Get("toggle-probe"); !ok {
		t.Fatal("a failed refresh must preserve the previous registry")
	}
}

func TestStartRegistryRefreshAppliesToggleWithoutRestart(t *testing.T) {
	// Toggleable payload: the server reads the current body from a var the
	// test mutates, so one endpoint can change shape mid-test.
	cur := &body{s: modelListJSON(true, true, true)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cur.get()))
	}))
	defer srv.Close()

	prev := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	defer func() { LiteLLMBaseURL = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Snapshot what the registry held before the loop, and restore it, so the
	// test leaves no residue: Refresh swaps the whole registry, not a key.
	defer func(before []ModelEntry) {
		Models.mu.Lock()
		Models.list = before
		Models.byName = map[string]ModelEntry{}
		for _, e := range before {
			Models.byName[e.Name] = e
		}
		Models.mu.Unlock()
	}(Models.list)

	StartRegistryRefresh(ctx, 10*time.Millisecond)

	// The first fire happens one interval in, so wait for the loop to load
	// the model before testing that it can also change it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := Models.Get("toggle-probe"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe model never loaded by the refresh loop")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Flip the stone; the loop must notice on its own.
	cur.set(modelListJSON(false, true, true))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := Models.Get("toggle-probe"); ok && !e.Caveman.Stones.Browse {
			return // toggle observed, no restart involved
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("StartRegistryRefresh never applied the browse toggle")
}

func TestStartRegistryRefreshStopsOnContextCancel(t *testing.T) {
	calls := make(chan struct{}, 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelListJSON(true, true, true)))
	}))
	defer srv.Close()

	prev := LiteLLMBaseURL
	LiteLLMBaseURL = srv.URL
	defer func() { LiteLLMBaseURL = prev }()

	ctx, cancel := context.WithCancel(context.Background())
	StartRegistryRefresh(ctx, 10*time.Millisecond)
	<-calls
	cancel()

	// Drain, then confirm the loop stops polling. A leaked goroutine polling
	// every 10ms forever is exactly the kind of thing this pins down.
	time.Sleep(100 * time.Millisecond)
	for {
		select {
		case <-calls:
			continue
		default:
		}
		break
	}
	before := len(calls)
	time.Sleep(200 * time.Millisecond)
	if len(calls) != before {
		t.Fatalf("refresh loop kept polling after ctx cancel (%d -> %d calls)", before, len(calls))
	}
}
