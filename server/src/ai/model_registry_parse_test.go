package ai

import (
	"encoding/json"
	"testing"
)

func TestParseCavemanSettingsBareTrue(t *testing.T) {
	// Bare `caveman: true` must mean "enabled with default stone settings",
	// so optimizationOwners hands caveman content ownership (read_less on).
	s := parseCavemanSettings(json.RawMessage(`true`))
	if !s.Enabled {
		t.Fatal("bare true: expected Enabled")
	}
	if !s.Stones.ReadLess || !s.Stones.SayLess {
		t.Fatal("bare true: expected default stones (read_less+say_less) on, got", s.Stones)
	}
}

func TestParseCavemanSettingsEnabledNoStones(t *testing.T) {
	// The proxy normalizes `caveman: true` to {"enabled": true} on older
	// images; that shape must still default the stones on (backwards compat).
	s := parseCavemanSettings(json.RawMessage(`{"enabled": true}`))
	if !s.Enabled {
		t.Fatal("enabled dict: expected Enabled")
	}
	if !s.Stones.ReadLess {
		t.Fatal("enabled dict without stones: expected default read_less on, got", s.Stones)
	}
}

func TestParseCavemanSettingsExplicitStones(t *testing.T) {
	s := parseCavemanSettings(json.RawMessage(`{"enabled": true, "stones": {"read_less": true, "say_less": false}}`))
	if !s.Enabled || !s.Stones.ReadLess {
		t.Fatal("explicit stones: expected read_less true")
	}
	if s.Stones.SayLess {
		t.Fatal("explicit stones: say_less should be false")
	}
	if s.Stones.Auto {
		t.Fatal("explicit stones: unspecified stones should stay off (auto)")
	}
}

func TestParseCavemanSettingsDisabled(t *testing.T) {
	s := parseCavemanSettings(json.RawMessage(`{"enabled": false}`))
	if s.Enabled {
		t.Fatal("disabled should stay disabled")
	}
	if s.Stones.ReadLess {
		t.Fatal("disabled: stones should not be force-enabled")
	}
}

func TestOptimizationOwnersCavemanDefaults(t *testing.T) {
	// caveman:true (enabled with default stones) must own content over headroom.
	Models.byName["t"] = ModelEntry{
		Name:     "t",
		Headroom: HeadroomSettings{Enabled: true},
		Caveman:  CavemanSettings{Enabled: true, Stones: CavemanStones{ReadLess: true}},
	}
	owner, _ := optimizationOwners("t")
	if owner != ownerCaveman {
		t.Fatalf("expected ownerCaveman, got %d", owner)
	}
	delete(Models.byName, "t")
}

// The default stone set is the answer to "what does `caveman: true` mean".
// Three places state it: DEFAULT_CAVEMAN_STONES in litellm_proxy.py,
// defaultCavemanStones() here, and the table in litellm.md. The proxy
// normalizes the operator's YAML and publishes /v1/models; this parser reads
// that, and must also cope with older images publishing a bare
// {"enabled": true}. If the two sets drift, which stones are live depends on
// which side normalized last — so both sides pin the same literal.
func TestDefaultCavemanStonesMatchesProxyDefault(t *testing.T) {
	want := CavemanStones{
		SayLess: true, ReadLess: true, Browse: true,
		Auto: false, Blocks: false, WasteFixes: false,
	}
	if got := defaultCavemanStones(); got != want {
		t.Fatalf("defaultCavemanStones() = %+v, must match DEFAULT_CAVEMAN_STONES in litellm_proxy.py: %+v", got, want)
	}

	// A bare true and an enabled dict without stones must both land on it:
	// these are the two shapes an operator's config can arrive as.
	for _, raw := range []string{`true`, `{"enabled": true}`} {
		s := parseCavemanSettings(json.RawMessage(raw))
		if !s.Enabled || s.Stones != want {
			t.Fatalf("parseCavemanSettings(%s) = %+v, want default set %+v", raw, s, want)
		}
	}

	// read_less must be in the default: it is the stone that grants caveman
	// content ownership (optimizationOwners), so a default without it would
	// silently leave headroom in charge of an "enabled" caveman.
	if !want.ReadLess {
		t.Fatal("default must include read_less")
	}
}

func TestExplicitStonesBlockIsAuthoritative(t *testing.T) {
	// A stones block on the wire is taken at face value: a key that is absent
	// stays off. Go deliberately does NOT re-fill defaults here, because the
	// proxy already filled them (see DEFAULT_CAVEMAN_STONES in
	// litellm_proxy.py) and filling again would mask a config that really did
	// turn a stone off.
	//
	// The invariant that makes this safe lives on the proxy side: it always
	// publishes a complete stones block, so "absent" only happens for
	// hand-written /v1/models payloads in tests.
	s := parseCavemanSettings(json.RawMessage(`{"enabled": true, "stones": {"auto": true}}`))
	if !s.Stones.Auto {
		t.Fatal("explicit auto: true must be honored")
	}
	for name, on := range map[string]bool{"read_less": s.Stones.ReadLess, "say_less": s.Stones.SayLess, "browse": s.Stones.Browse} {
		if on {
			t.Fatalf("%s unlisted in an explicit stones block must stay off, got on", name)
		}
	}

	// Consequence worth naming: a wire payload that names only `auto` hands
	// content ownership to headroom, because read_less is off and
	// optimizationOwners gates caveman on read_less. That is the operator's
	// stated config, not a bug — and it is why the proxy fills defaults, so a
	// partial YAML dict never lands here in this shape.
	owner, _ := optimizationOwners("any")
	_ = owner
}
