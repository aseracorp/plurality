package webhook

import (
	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/utils"
)

// Init rebuilds the in-memory owners index from disk, applies rate-limit
// overrides from data/config.json, and starts the counter cleanup loop.
// Call once at server startup, AFTER auth.Init (which loads the config).
func Init() {
	loadOwners()

	cfg := auth.GetConfig().Webhook
	if cfg.PerClientPerMinute > 0 {
		PerClientPerMinute = cfg.PerClientPerMinute
	}
	if cfg.PerWebhookPerMinute > 0 {
		PerWebhookPerMinute = cfg.PerWebhookPerMinute
	}

	startCleanupLoop()
	utils.Log("[Webhook] ready (rate limits: %d/min per IP+webhook, %d/min per IP global)",
		PerWebhookPerMinute, PerClientPerMinute)
}

// Shutdown is a no-op today — webhooks have no background workers we need
// to drain on exit. Kept for symmetry with cron.Shutdown so main's defer
// chain reads consistently.
func Shutdown() {}
