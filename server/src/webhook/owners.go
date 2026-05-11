package webhook

import (
	"sync"

	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/utils"
)

// owners maps webhookID -> userID. The trigger endpoint is unauthenticated
// (the URL token IS the auth), so we need a way to find which user owns a
// given webhook ID without a JWT. Walking users-data/ on every request
// would be wasteful — we build this index at startup and keep it current
// on Create/Delete.
var owners sync.Map // map[string]string

// loadOwners walks every user's webhooks.json on disk and re-builds the
// in-memory ID -> userID index. Called from Init at startup.
func loadOwners() {
	for _, userID := range jobs.ListUserIDsOnDisk(webhooksFile) {
		list, err := jobs.LoadAll[Webhook](userID, webhooksFile)
		if err != nil {
			utils.Error("[Webhook] loading user "+userID, err)
			continue
		}
		for _, w := range list {
			owners.Store(w.ID, userID)
		}
	}
}

// registerOwner records that webhookID is owned by userID.
func registerOwner(webhookID, userID string) {
	owners.Store(webhookID, userID)
}

// unregisterOwner removes the mapping.
func unregisterOwner(webhookID string) {
	owners.Delete(webhookID)
}

// lookupOwner returns the userID that owns this webhook, or "" if unknown.
func lookupOwner(webhookID string) (string, bool) {
	v, ok := owners.Load(webhookID)
	if !ok {
		return "", false
	}
	return v.(string), true
}
