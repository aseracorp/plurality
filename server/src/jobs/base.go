// Package jobs holds the mechanics shared by triggered background prompts
// (cron, webhook, anything that fires a stored prompt against a preset).
package jobs

import "time"

// Base is the set of fields that every triggered-prompt definition needs.
// Concrete types (cron.CronJob, webhook.Webhook) embed it so their JSON
// stays flat and the shared store/runner code can talk in terms of these
// fields.
type Base struct {
	ID        string    `json:"id"`
	Prompt    string    `json:"prompt"`
	PresetID  string    `json:"preset_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}
