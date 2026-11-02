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
