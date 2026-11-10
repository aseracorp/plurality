package utils

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// clock is an injectable time source: real time plus a settable offset, so TTL
// transitions can be simulated instantly while gap waits still elapse in
// wall-clock terms. A clock that does not advance is exactly the pathological
// case the scheduler must fail open on — see TestFrozenClockFailsOpen.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) now() time.Time { return time.Now().Add(c.get()) }

func (c *clock) get() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

func (c *clock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func newTestScheduler(gap, ttl time.Duration) *llmScheduler {
	return &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: gap,
		defaultTTL: ttl,
		enabled:    true,
		now:        (&clock{}).now,
	}
}

func lastOf(t *testing.T, s *llmScheduler, key string) time.Time {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.perModel[key]
	if !ok {
		t.Fatalf("no state recorded for model %q", key)
	}
	return st.lastDispatch
}

// The gap must engage for warm requests: two dispatches for the same model may
// not leave closer together than the configured spacing, because that burst
// shape is what the providers' cache docs name as the eviction trigger.
func TestScheduleSpacesWarmRequests(t *testing.T) {
	gap := 60 * time.Millisecond
	s := newTestScheduler(gap, time.Hour)
	ctx := context.Background()

	start := time.Now()
	if err := s.schedule(ctx, "Strata-Swift"); err != nil {
		t.Fatal(err)
	}
	if err := s.schedule(ctx, "Strata-Swift"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < gap {
		t.Fatalf("second dispatch left after %v, want >= the %v gap", elapsed, gap)
	}
}

// Model names must be matched case-insensitively: LiteLLM deployment names
// differ in case between config and request payload, and a case-sensitive lane
// would silently give each spelling its own pacing (i.e. no pacing at all).
func TestScheduleIsCaseInsensitivePerModel(t *testing.T) {
	gap := 60 * time.Millisecond
	s := newTestScheduler(gap, time.Hour)
	ctx := context.Background()

	if err := s.schedule(ctx, "Strata-Swift"); err != nil {
		t.Fatal(err)
	}
	first := lastOf(t, s, "strata-swift")
	if err := s.schedule(ctx, "STRATA-SWIFT"); err != nil {
		t.Fatal(err)
	}
	second := lastOf(t, s, "strata-swift")

	if d := second.Sub(first); d < gap {
		t.Fatalf("same model under two spellings dispatched %v apart, want >= %v (one shared lane expected)", d, gap)
	}
}

// Distinct models must not serialize behind each other's per-model gap: only
// the small global floor applies, so an eco run's title/summary calls on other
// models are not delayed by a local conversation's pacing.
func TestScheduleDoesNotSerializeAcrossModels(t *testing.T) {
	s := newTestScheduler(5*time.Second, time.Hour)
	ctx := context.Background()

	start := time.Now()
	for _, m := range []string{"m1", "m2", "m3"} {
		if err := s.schedule(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed >= 5*time.Second {
		t.Fatalf("cross-model fan-out serialized behind a 5s per-model gap: %v", elapsed)
	}
	if want := 2 * globalDispatchFloor; elapsed < want {
		t.Fatalf("3 cross-model dispatches took %v, want >= %v of floor spacing", elapsed, want)
	}
}

// TTL coalescing: once a model has been idle past the provider's cache TTL the
// prefix is gone, so the next request dispatches IMMEDIATELY (it pays the
// prefill either way) and the queue behind it follows at once, sharing that one
// prefill. The clock jump makes the transition instantaneous, so any pacing
// regression shows up as wall-clock delay.
func TestScheduleDispatchesImmediatelyAfterTTLExpiry(t *testing.T) {
	c := &clock{}
	s := &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: 5 * time.Second,
		defaultTTL: 300 * time.Second,
		enabled:    true,
		now:        c.now,
	}
	ctx := context.Background()

	if err := s.schedule(ctx, "strata-swift"); err != nil {
		t.Fatal(err)
	}
	c.jump(301 * time.Second)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		if err := s.schedule(ctx, "strata-swift"); err != nil {
			t.Error(err)
		}
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		if d > 500*time.Millisecond {
			t.Fatalf("cold dispatch waited %v; an evicted prefix must dispatch at once", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cold dispatch never returned")
	}
}

// ctx cancellation must abort a paced wait — a cancelled workflow may never
// park behind the scheduler (same contract as the LLM semaphores).
func TestScheduleHonoursContextCancel(t *testing.T) {
	s := newTestScheduler(30*time.Second, time.Hour)
	ctx := context.Background()

	if err := s.schedule(ctx, "strata-swift"); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.schedule(cctx, "strata-swift"); err == nil {
		t.Fatal("expected ctx.Err() from a paced wait on a cancelled context")
	}
}

// Disabled scheduling is a pure passthrough (the escape hatch).
func TestScheduleDisabledIsPassthrough(t *testing.T) {
	s := newTestScheduler(30*time.Second, time.Hour)
	s.enabled = false
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.schedule(context.Background(), "strata-swift"); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("disabled scheduler waited %v", d)
	}
}

// A clock that does not advance must NOT livelock the scheduler: it dispatches
// rather than holding a turn forever.
func TestFrozenClockFailsOpen(t *testing.T) {
	frozen := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	s := &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: 30 * time.Second,
		defaultTTL: time.Hour,
		enabled:    true,
		now:        func() time.Time { return frozen },
	}
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := s.schedule(context.Background(), "strata-swift"); err != nil {
			t.Fatal(err)
		}
	}
	// A stalled clock must cost NOTHING: the first dispatch records the
	// instant, every later call sees the clock has not moved and fails open.
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("stalled clock stalled the scheduler for %v", d)
	}
}

// Per-model params from config must override the defaults: that is the knob for
// a backend whose cache re-keys on every request (gap 0) or whose TTL is short.
func TestRegisterModelSchedulingOverridesDefaults(t *testing.T) {
	original := scheduler
	defer func() { scheduler = original }()

	c := &clock{}
	scheduler = &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: 30 * time.Second,
		defaultTTL: time.Hour,
		enabled:    true,
		now:        c.now,
	}

	RegisterModelScheduling(Model{Name: "Strata-Swift", Params: map[string]string{"cache_gap_ms": "0"}})

	start := time.Now()
	if err := scheduleModelSlot(context.Background(), "strata-swift"); err != nil {
		t.Fatal(err)
	}
	if err := scheduleModelSlot(context.Background(), "strata-swift"); err != nil {
		t.Fatal(err)
	}
	d := time.Since(start)
	if d >= 5*time.Second {
		t.Fatalf("cache_gap_ms=0 model waited %v; 0 must disable per-model spacing", d)
	}
	if d < globalDispatchFloor {
		t.Fatalf("pair dispatched %v apart, below the global floor %v", d, globalDispatchFloor)
	}
}

// Unparseable params must be ignored, not applied as a 0/negative rule.
func TestRegisterModelSchedulingIgnoresGarbage(t *testing.T) {
	original := scheduler
	defer func() { scheduler = original }()

	scheduler = &llmScheduler{
		perModel:   map[string]*modelSchedState{},
		defaultGap: 60 * time.Millisecond,
		defaultTTL: time.Hour,
		enabled:    true,
		now:        (&clock{}).now,
	}

	RegisterModelScheduling(Model{Name: "m", Params: map[string]string{"cache_gap_ms": "not-a-number"}})
	RegisterModelScheduling(Model{Name: "m", Params: map[string]string{"cache_gap_ms": "-5"}})
	RegisterModelScheduling(Model{Name: "", Params: map[string]string{"cache_gap_ms": "0"}})

	start := time.Now()
	scheduleModelSlot(context.Background(), "m")
	scheduleModelSlot(context.Background(), "m")
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("garbage params shortened pacing to %v; expected the %v default", d, 60*time.Millisecond)
	}
}

func TestModelFromLLMRequest(t *testing.T) {
	req, err := http.NewRequest("POST", "http://x/v1/chat/completions", bytes.NewBufferString(`{"model":"Strata-Swift","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	ensureRetryableBody(req)
	if got := modelFromLLMRequest(req); got != "Strata-Swift" {
		t.Fatalf("got %q", got)
	}

	bad, _ := http.NewRequest("POST", "http://x", bytes.NewBufferString("not json"))
	ensureRetryableBody(bad)
	if got := modelFromLLMRequest(bad); got != "" {
		t.Fatalf("unparseable body should yield \"\", got %q", got)
	}

	// A request with no readable body must not panic.
	if got := modelFromLLMRequest(&http.Request{Method: "GET"}); got != "" {
		t.Fatalf("bodyless request should yield \"\", got %q", got)
	}
}

// The scheduler must be safe under concurrent use: eco runs fan out several
// calls at once and sub-agents add more. Run with -race in CI.
func TestScheduleConcurrent(t *testing.T) {
	s := newTestScheduler(5*time.Millisecond, 50*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.schedule(context.Background(), "strata-swift"); err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
