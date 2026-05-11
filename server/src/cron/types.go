package cron

import (
	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/utils"
)

// CronJob is one scheduled prompt for a user. Persisted as a JSON array in
// users-data/{user}/cron.json. The shared fields (ID/Prompt/PresetID/Enabled/
// CreatedAt) come from jobs.Base — JSON stays flat thanks to Go's embedding.
type CronJob struct {
	jobs.Base
	Schedule      string              `json:"schedule"`
	ModelSelected utils.ModelSelected `json:"model_selected"`
}

// CronUpdate is a sparse patch. nil-valued pointers mean "leave alone".
type CronUpdate struct {
	Schedule       *string `json:"schedule,omitempty"`
	Prompt         *string `json:"prompt,omitempty"`
	PresetID       *string `json:"preset_id,omitempty"`
	Enabled        *bool   `json:"enabled,omitempty"`
	ConversationID *string `json:"conversation_id,omitempty"`
}
