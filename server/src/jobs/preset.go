package jobs

import (
	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/utils"
)

// DefaultPresetID is used when a job has no PresetID set.
const DefaultPresetID = "default-background-agent"

// ResolvePreset returns the MiniApp for a background job (nil-safe) and the
// ModelSelected that should drive its LLM call.
//
//   - Empty presetID -> DefaultPresetID.
//   - Preset's ModelSelected.Text wins over everything else.
//   - Otherwise fall back to the "smart" shortcut from data/config.json.
//
// We deliberately do NOT carry a job's stored ModelSelected — background
// jobs run autonomously and shouldn't be pinned to whatever model the user
// happened to have selected when they created the job.
func ResolvePreset(userID, presetID string) (*utils.MiniApp, utils.ModelSelected) {
	if presetID == "" {
		presetID = DefaultPresetID
	}
	preset, err := miniapps.Get(userID, presetID)
	if err != nil {
		utils.Error("[jobs] preset lookup failed", err)
		preset = nil
	}
	if preset != nil && preset.ModelSelected.Text != nil {
		return preset, preset.ModelSelected
	}
	return preset, ai.ShortcutModelSelected("smart")
}
