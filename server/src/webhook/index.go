package webhook

import "github.com/azukaar/plurality/src/utils"

// Init rebuilds the in-memory owners index from disk. Call once at server
// startup, before serving requests.
func Init() {
	loadOwners()
	utils.Log("[Webhook] owners index loaded")
}

// Shutdown is a no-op today — webhooks have no background workers. Kept
// for symmetry with cron.Shutdown so main's defer chain reads consistently.
func Shutdown() {}
