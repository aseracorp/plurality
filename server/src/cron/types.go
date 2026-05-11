package cron

import (
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// CronJob is one scheduled prompt for a user. Persisted in
// users-data/{user}/cron.json as part of a JSON array.
type CronJob struct {
	ID            string              `json:"id"`
	Schedule      string              `json:"schedule"`
	Prompt        string              `json:"prompt"`
	PresetID      string              `json:"preset_id"`
	Enabled       bool                `json:"enabled"`
	ModelSelected utils.ModelSelected `json:"model_selected"`
	CreatedAt     time.Time           `json:"created_at"`
}

// CronUpdate is a sparse patch. nil-valued pointers mean "leave alone".
type CronUpdate struct {
	Schedule *string `json:"schedule,omitempty"`
	Prompt   *string `json:"prompt,omitempty"`
	PresetID *string `json:"preset_id,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}
