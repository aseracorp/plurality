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
//   - The base shortcut is picked from the preset's Complexity field
//     (low -> fast, medium -> medium, high -> smart; empty/unknown -> medium).
//     The preset's ModelSelected is layered on top — per-field model names
//     override when set, and tools are merged additively (preset entries
//     override matching keys, the rest are kept; "false" subtracts).
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
	complexity := ""
	if preset != nil {
		complexity = preset.Complexity
	}
	base := ai.ShortcutModelSelected(ShortcutForComplexity(complexity))
	if preset == nil {
		return preset, base
	}
	return preset, mergePresetOnto(preset.ModelSelected, base)
}

// ShortcutForComplexity maps a preset's Complexity field to a config shortcut
// name. Empty/unknown values fall back to "medium".
func ShortcutForComplexity(c string) string {
	switch c {
	case "low":
		return "fast"
	case "high":
		return "smart"
	default:
		return "medium"
	}
}

// mergePresetOnto layers a preset's ModelSelected on top of a base
// ModelSelected. For each per-field model: an empty preset name falls back to
// base's name. Tools are merged additively — preset entries override matching
// keys and other base entries are preserved — except that a preset value of
// "false" subtracts the key from the resulting set.
func mergePresetOnto(preset, base utils.ModelSelected) utils.ModelSelected {
	out := base
	out.Text = mergeModel(preset.Text, base.Text)
	out.Vision = mergeModel(preset.Vision, base.Vision)
	out.ImageGen = mergeModel(preset.ImageGen, base.ImageGen)
	out.ImageEdit = mergeModel(preset.ImageEdit, base.ImageEdit)
	out.AudioTranscribe = mergeModel(preset.AudioTranscribe, base.AudioTranscribe)
	out.VoiceGen = mergeModel(preset.VoiceGen, base.VoiceGen)
	out.AudioGen = mergeModel(preset.AudioGen, base.AudioGen)
	out.VideoGen = mergeModel(preset.VideoGen, base.VideoGen)
	out.VideoVision = mergeModel(preset.VideoVision, base.VideoVision)
	out.Code = mergeModel(preset.Code, base.Code)
	if preset.ClientFolderPath != "" {
		out.ClientFolderPath = preset.ClientFolderPath
	}
	return out
}

func mergeModel(preset, base *utils.Model) *utils.Model {
	if preset == nil {
		return base
	}
	name := preset.Name
	if name == "" && base != nil {
		name = base.Name
	}
	tools := map[string]string{}
	if base != nil {
		for k, v := range base.Tools {
			tools[k] = v
		}
	}
	for k, v := range preset.Tools {
		if v == "false" {
			delete(tools, k)
			continue
		}
		tools[k] = v
	}
	if len(tools) == 0 {
		tools = nil
	}
	params := preset.Params
	if params == nil && base != nil {
		params = base.Params
	}
	return &utils.Model{Name: name, Params: params, Tools: tools}
}
