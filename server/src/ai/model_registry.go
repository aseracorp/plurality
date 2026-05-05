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

// ModelEntry mirrors the metadata the litellm proxy exposes per configured model.
// The proxy reads `model_info` blocks from litellm_config.yaml and surfaces them
// on /v1/models. Anything not in the config simply remains a zero value.
type ModelEntry struct {
	Name                    string
	Mode                    string // chat | image_generation | audio_speech | audio_transcription | embedding
	SupportsVision          bool
	SupportsFunctionCalling bool
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
	ID                      string `json:"id"`
	Mode                    string `json:"mode"`
	SupportsVision          bool   `json:"supports_vision"`
	SupportsFunctionCalling bool   `json:"supports_function_calling"`
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
		}
		byName[m.ID] = entry
		list = append(list, entry)
	}

	r.mu.Lock()
	r.byName = byName
	r.list = list
	r.mu.Unlock()

	utils.Log("[Models] Loaded %d models from litellm proxy", len(list))
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

// IsAudioModel reports whether the model is an audio (TTS or STT) model.
func (r *ModelRegistry) IsAudioModel(name string) bool {
	entry, ok := r.Get(name)
	if !ok {
		return false
	}
	return strings.HasPrefix(entry.Mode, "audio_")
}
