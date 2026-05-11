// Package webhook is the HTTP-triggered sibling of cron: a stored prompt
// fires when an external request hits /webhook/{id}, authenticated by a
// per-webhook secret token. Most of the heavy lifting (storage, preset
// resolution, conversation kickoff) lives in src/jobs — only the trigger
// mechanics and token handling are webhook-specific.
package webhook

import (
	"time"

	"github.com/azukaar/plurality/src/jobs"
)

// Webhook is one HTTP-triggered prompt for a user. Persisted as a JSON
// array in users-data/{user}/webhooks.json. Shared identity fields come
// from jobs.Base via embedding.
type Webhook struct {
	jobs.Base
	// TokenHash is sha256(plaintext token) hex-encoded. The plaintext is
	// shown to the caller exactly once at create/rotate time and never
	// persisted.
	TokenHash       string     `json:"token_hash"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
}

// WebhookUpdate is a sparse patch — webhooks have no schedule, so Prompt /
// PresetID / Enabled are the only mutable user-facing fields.
type WebhookUpdate struct {
	Prompt   *string `json:"prompt,omitempty"`
	PresetID *string `json:"preset_id,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// PublicWebhook is what API responses and the LLM tool see. It strips
// TokenHash so secrets can't leak through list/get endpoints.
type PublicWebhook struct {
	ID              string     `json:"id"`
	Prompt          string     `json:"prompt"`
	PresetID        string     `json:"preset_id"`
	Enabled         bool       `json:"enabled"`
	CreatedAt       time.Time  `json:"created_at"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
}

// Public converts a Webhook to the safe-to-expose shape.
func (w Webhook) Public() PublicWebhook {
	return PublicWebhook{
		ID:              w.ID,
		Prompt:          w.Prompt,
		PresetID:        w.PresetID,
		Enabled:         w.Enabled,
		CreatedAt:       w.CreatedAt,
		LastTriggeredAt: w.LastTriggeredAt,
	}
}

// WebhookCreateResponse is returned ONLY by Create and RotateToken — the
// only two times the plaintext token is ever surfaced. The caller is
// expected to record `Token` immediately; the server can't recover it.
type WebhookCreateResponse struct {
	PublicWebhook
	URL   string `json:"url"`
	Token string `json:"token"`
}

// TriggerPayload captures the parts of an incoming HTTP request that get
// surfaced to the LLM. The trigger handler MUST strip the auth token from
// Query and Headers before constructing this — see api.go.
type TriggerPayload struct {
	Method  string
	Query   map[string][]string
	Headers map[string][]string
	Body    string
}

const webhooksFile = "webhooks.json"
