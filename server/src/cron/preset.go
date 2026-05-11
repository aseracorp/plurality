package cron

import (
	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/utils"
)

// DefaultPresetID is used when a CronJob has no PresetID set.
const DefaultPresetID = "default-background-agent"

// resolveJobPreset returns the MiniApp for a cron job (nil-safe) and the
// ModelSelected that should drive its LLM call.
//
//   - Empty PresetID -> DefaultPresetID.
//   - Preset's ModelSelected.Text wins over everything else.
//   - Otherwise fall back to the "smart" shortcut from data/config.json
//     (mirrors how title generation uses the "fast" shortcut).
//
// We deliberately do NOT carry job.ModelSelected — crons run autonomously
// and shouldn't be pinned to whatever model the user happened to have
// selected when they created the cron.
func resolveJobPreset(userID string, job CronJob) (*utils.MiniApp, utils.ModelSelected) {
	presetID := job.PresetID
	if presetID == "" {
		presetID = DefaultPresetID
	}
	preset, err := miniapps.Get(userID, presetID)
	if err != nil {
		utils.Error("[Cron] preset lookup failed", err)
		preset = nil
	}
	if preset != nil && preset.ModelSelected.Text != nil {
		return preset, preset.ModelSelected
	}
	return preset, ai.ShortcutModelSelected("smart")
}
