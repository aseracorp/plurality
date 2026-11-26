package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

type ModelEntry struct {
	Name                    string
	Mode                    string // chat | image_generation | image_edit | audio_speech | audio_transcription | embedding
	SupportsVision          bool
	SupportsFunctionCalling bool
	// Headroom routes this model's chat completions through the in-container
	// Headroom optimization proxy. Set per model via model_info.headroom in
	// litellm_config.yaml — either `headroom: true` (enabled, defaults) or a
	// dict of sub-settings. Defaults to disabled — every other model talks to
	// LiteLLM directly, byte-for-byte untouched.
	Headroom HeadroomSettings

	// Caveman routes this model's chat completions through the in-container
	// Caveman compression proxy. model_info.caveman may be `true` or a dict
	// with the six "stones" (say_less, read_less, auto, blocks, waste_fixes,
	// browse). When both Headroom and Caveman are enabled the optimization
	// ownership resolver (OptimizationOwners) picks a SINGLE content
	// compressor so no category is ever optimized twice (see caveman.go).
	Caveman CavemanSettings

	// MaxContextTokens is the model's context window in tokens, as declared
	// in litellm_config.yaml model_info.context_window and surfaced by the
	// proxy on /v1/models. Zero means "unknown" — callers should fall back
	// to the configured eco trigger rather than assume a window.
	MaxContextTokens int
}

// ModelRegistry caches the list of models the litellm proxy knows about.
// It's the single source of truth for what models the server exposes and
// what they support. All capability gates in the codebase read from here.
type ModelRegistry struct {
	mu     sync.RWMutex
	byName map[string]ModelEntry
	list   []ModelEntry
}

// Models is the package-level singleton, populated at startup by InitModels.
var Models = &ModelRegistry{byName: map[string]ModelEntry{}}

type litellmModelEntry struct {
	ID                      string          `json:"id"`
	Mode                    string          `json:"mode"`
	SupportsVision          bool            `json:"supports_vision"`
	SupportsFunctionCalling bool            `json:"supports_function_calling"`
	Headroom                json.RawMessage `json:"headroom"`
	Caveman                 json.RawMessage `json:"caveman"`
	MaxContextTokens        int             `json:"context_window"`
}

// HeadroomSettings are the per-model sub-settings for the Headroom proxy.
type HeadroomSettings struct {
	Enabled bool
	// keepLastTurns trims history before optimization (headroom's
	// x-headroom-keep-last-turns). 0 = keep everything.
	KeepLastTurns int
}

// CavemanStones mirror caveman's six "rocks" (the README's "six rocks, one
// install"). Each is independently toggleable.
type CavemanStones struct {
	SayLess    bool
	ReadLess   bool
	Auto       bool
	Blocks     bool
	WasteFixes bool
	Browse     bool
}

// CavemanSettings are the per-model sub-settings for the Caveman proxy.
type CavemanSettings struct {
	Enabled bool
	Stones  CavemanStones
	// Proxy routes this model's chat completions THROUGH the caveman binary
	// (plurality -> caveman -> LiteLLM). Off by default: the stones that
	// Plurality implements (say_less, read_less, browse) all run in Go, and
	// the binary's compress mode forwards input unchanged when it has no
	// recovery tool bound — measured on this deployment at identical provider
	// prompt_tokens through the proxy and direct LiteLLM (651==651, 951==951)
	// at 10.6x the latency (3367ms vs 318ms). Turning this on is for the
	// stones Go does not implement (auto, blocks, waste_fixes), which need
	// the binary in the request path.
	Proxy bool
}

type litellmModelList struct {
	Data []litellmModelEntry `json:"data"`
}

// InitModels fetches the model list from the litellm proxy. Call once after
// InitLiteLLM() succeeds. Safe to call again to refresh.
func InitModels(ctx context.Context) error {
	return Models.Refresh(ctx)
}

// Refresh GETs /v1/models from the litellm proxy and atomically swaps the
// cached registry. On error the previous cache is preserved.
func (r *ModelRegistry) Refresh(ctx context.Context) error {
	url := LiteLLMBaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching litellm /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("litellm /v1/models returned %d", resp.StatusCode)
	}

	var parsed litellmModelList
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return fmt.Errorf("decoding litellm /v1/models: %w", err)
	}

	byName := make(map[string]ModelEntry, len(parsed.Data))
	list := make([]ModelEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		entry := ModelEntry{
			Name:                    m.ID,
			Mode:                    m.Mode,
			SupportsVision:          m.SupportsVision,
			SupportsFunctionCalling: m.SupportsFunctionCalling,
			Headroom:                parseHeadroomSettings(m.Headroom),
			Caveman:                 parseCavemanSettings(m.Caveman),
			MaxContextTokens:        m.MaxContextTokens,
		}
		byName[m.ID] = entry
		list = append(list, entry)
	}

	r.mu.Lock()
	changed := !registryEqual(r.list, list)
	r.byName = byName
	r.list = list
	r.mu.Unlock()

	// The registry is polled (see StartRegistryRefresh), so this must stay
	// quiet while nothing changes: an unconditional log here is one line per
	// poll, forever.
	if changed {
		utils.Log("[Models] Loaded %d models from litellm proxy", len(list))
	}
	return nil
}

// Get returns the entry for a model name and whether it's known.
func (r *ModelRegistry) Get(name string) (ModelEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byName[name]
	return entry, ok
}

// All returns a snapshot of every known model entry.
func (r *ModelRegistry) All() []ModelEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelEntry, len(r.list))
	copy(out, r.list)
	return out
}

// IsKnown reports whether the model exists in the registry.
func (r *ModelRegistry) IsKnown(name string) bool {
	_, ok := r.Get(name)
	return ok
}

// DeclaredContextWindow returns the context window the proxy DECLARED for a
// model (`model_info.context_window` in litellm_config.yaml`), and 0 when no
// such declaration exists.
//
// This is the ONLY window accessor the compaction path may use. What it
// replaces all assumed a size:
//   - a built-in table of well-known model families (longest-prefix match on
//     the model NAME), so a fine-tune named after a family inherited that
//     family's advertised number rather than its own deployment's;
//   - defaultContextWindow = 131072 for anything the table missed, so a 32K
//     local model was governed as a 128K one.
//
// Neither is recoverable at runtime: the trigger, the fold's roll-up bound,
// the preemptive guard and the summariser's max_tokens are all denominated in
// this number, and a number that is too LARGE ships a request the provider
// rejects. 0 means "unknown", and every caller treats 0 as "use the operator's
// configured trigger/target verbatim", so an undeclared model is governed by
// configuration, never by a guess.
func (r *ModelRegistry) DeclaredContextWindow(name string) int {
	if name == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.byName[name]; ok {
		return entry.MaxContextTokens
	}
	// ":suffix" variant matching, so a declaration is found for the bare name
	// too (entries carry e.g. "deepseek-v4-flash-0731:all").
	base := name
	if i := strings.IndexByte(name, ':'); i >= 0 {
		base = name[:i]
	}
	for n, entry := range r.byName {
		nbase := n
		if i := strings.IndexByte(n, ':'); i >= 0 {
			nbase = n[:i]
		}
		if nbase == base && entry.MaxContextTokens > 0 {
			return entry.MaxContextTokens
		}
	}
	return 0
}

// IsImageGenModel reports whether the model produces images.

// IsActionModel reports whether the model is a chat model that supports tool calls.
// This is the gate for sending `tools` to the LLM and injecting skill prompts.
func (r *ModelRegistry) IsActionModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "chat" && entry.SupportsFunctionCalling
}

// IsVisionModel reports whether the model can ingest images.
func (r *ModelRegistry) IsVisionModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.SupportsVision
}

// IsImageGenModel reports whether the model produces images.
func (r *ModelRegistry) IsImageGenModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "image_generation"
}

// IsImageEditModel reports whether the model edits an input image.
func (r *ModelRegistry) IsImageEditModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return entry.Mode == "image_edit"
}

// IsAudioModel reports whether the model is an audio (TTS or STT) model.
func (r *ModelRegistry) IsAudioModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return strings.HasPrefix(entry.Mode, "audio_")
}

// headroomFlag is the on-disk shape of model_info.headroom after the litellm
// proxy normalizes it: {"enabled": bool, ...sub-settings}.
type headroomFlag struct {
	Enabled       *bool `json:"enabled"`
	KeepLastTurns *int  `json:"keep_last_turns"`
}

func parseHeadroomSettings(raw json.RawMessage) HeadroomSettings {
	if len(raw) == 0 || string(raw) == "null" {
		return HeadroomSettings{}
	}
	// Backward compatible with `headroom: true` (pre-normalization path).
	if string(raw) == "true" {
		return HeadroomSettings{Enabled: true}
	}
	var f headroomFlag
	if err := json.Unmarshal(raw, &f); err != nil {
		return HeadroomSettings{}
	}
	out := HeadroomSettings{}
	if f.Enabled != nil {
		out.Enabled = *f.Enabled
	}
	if f.KeepLastTurns != nil && *f.KeepLastTurns > 0 {
		out.KeepLastTurns = *f.KeepLastTurns
	}
	return out
}

// cavemanFlag is the on-disk shape of model_info.caveman after the litellm
// proxy normalizes it: {"enabled": bool, "stones": {...}}.
type cavemanFlag struct {
	Enabled *bool          `json:"enabled"`
	Stones  *cavemanStones `json:"stones"`
	Proxy   *bool          `json:"proxy"`
}

type cavemanStones struct {
	SayLess    *bool `json:"say_less"`
	ReadLess   *bool `json:"read_less"`
	Auto       *bool `json:"auto"`
	Blocks     *bool `json:"blocks"`
	WasteFixes *bool `json:"waste_fixes"`
	Browse     *bool `json:"browse"`
}

// defaultCavemanStones is the stone set a bare `caveman: true` means. Keep it
// byte-identical to the proxy's default in litellm_proxy.py — the two layers
// parse the same config from opposite ends, and if they disagree then which
// stones are on depends on which side normalized the value last. litellm.md
// documents this set; all three must say the same thing.
//
// On: the stones that are pure wins and need nothing external — say_less
// (output-side voice), read_less (content compression, and the stone that
// grants content ownership), browse (web/Playwright snapshots, gain-guarded so
// it can only shrink a payload).
// Off: auto (routes by "ask type", requires a Caveman account, and
// undeterministically swaps the model mid-conversation), blocks (pre-release,
// re-executes shell code), waste_fixes (holds the KV cache open past request
// end). An operator who wants them turns them on by name.
func defaultCavemanStones() CavemanStones {
	return CavemanStones{SayLess: true, ReadLess: true, Browse: true}
}

func parseCavemanSettings(raw json.RawMessage) CavemanSettings {
	if len(raw) == 0 || string(raw) == "null" {
		return CavemanSettings{}
	}
	if string(raw) == "true" {
		// Bare `caveman: true` -> enabled with the default stone set.
		return CavemanSettings{Enabled: true, Stones: defaultCavemanStones()}
	}
	var f cavemanFlag
	if err := json.Unmarshal(raw, &f); err != nil {
		return CavemanSettings{}
	}
	out := CavemanSettings{}
	if f.Enabled != nil {
		out.Enabled = *f.Enabled
	}
	if f.Stones == nil && out.Enabled {
		// Backwards compatible: a bare `caveman: true` (or any enabled dict
		// without a stones block) means "enabled with DEFAULT settings".
		// Without this the enabled flag alone yields an all-false stone set,
		// so optimizationOwners never hands caveman content ownership and the
		// proxy is bypassed even though the operator enabled it (the live bug
		// PR #127 fixed).
		out.Stones = defaultCavemanStones()
		return out
	}
	if f.Proxy != nil {
		out.Proxy = *f.Proxy
	}
	if f.Stones != nil {
		st := CavemanStones{}
		if f.Stones.SayLess != nil {
			st.SayLess = *f.Stones.SayLess
		}
		if f.Stones.ReadLess != nil {
			st.ReadLess = *f.Stones.ReadLess
		}
		if f.Stones.Auto != nil {
			st.Auto = *f.Stones.Auto
		}
		if f.Stones.Blocks != nil {
			st.Blocks = *f.Stones.Blocks
		}
		if f.Stones.WasteFixes != nil {
			st.WasteFixes = *f.Stones.WasteFixes
		}
		if f.Stones.Browse != nil {
			st.Browse = *f.Stones.Browse
		}
		out.Stones = st
	}
	return out
}

// registryEqual compares two model lists for the fields callers act on, so a
// periodic refresh can tell "nothing changed" from a real edit. Caveman and
// Headroom settings are compared by value: they are the per-stone toggles
// operators flip in litellm_config.yaml, and a refresh that ignored them would
// make the toggles invisible.
func registryEqual(a, b []ModelEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Name != y.Name || x.Mode != y.Mode ||
			x.SupportsVision != y.SupportsVision ||
			x.SupportsFunctionCalling != y.SupportsFunctionCalling ||
			x.MaxContextTokens != y.MaxContextTokens ||
			x.Headroom != y.Headroom ||
			x.Caveman != y.Caveman {
			return false
		}
	}
	return true
}

// StartRegistryRefresh polls the litellm proxy so per-model settings — the
// caveman/headroom stone toggles in particular — take effect without a server
// restart.
//
// The registry is a cache of /v1/models, and the proxy builds its Router from
// litellm_config.yaml. Before this, both sides were frozen at startup: editing
// a stone in the YAML changed nothing for the life of the process, so a
// per-stone toggle was not a working feature, it was a restart ritual. The
// proxy hot-reloads its own config on mtime change; this is the matching half.
//
// Safe by construction: Refresh swaps the maps under a mutex and keeps the
// previous cache on any error, so a proxy restart mid-poll or a broken YAML
// leaves the last good registry serving requests.
func StartRegistryRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := Models.Refresh(ctx); err != nil {
					utils.Debug("[Models] refresh skipped: %v", err)
				}
			}
		}
	}()
}
