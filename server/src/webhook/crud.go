package webhook

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/azukaar/plurality/src/jobs"
)

// LoadAll returns the user's webhooks. Missing file -> empty slice.
func LoadAll(userID string) ([]Webhook, error) {
	return jobs.LoadAll[Webhook](userID, webhooksFile)
}

// SaveAll writes the user's webhooks atomically.
func SaveAll(userID string, items []Webhook) error {
	return jobs.SaveAll(userID, webhooksFile, items)
}

// Create persists a new Webhook with a freshly minted token and registers
// it in the owners index. The returned struct carries the plaintext token —
// the caller is responsible for handing it back to the user; we never get
// another chance to surface it.
func Create(userID, prompt, presetID string) (Webhook, string, error) {
	if strings.TrimSpace(prompt) == "" {
		return Webhook{}, "", errors.New("prompt is required")
	}
	if presetID == "" {
		presetID = jobs.DefaultPresetID
	}

	token := GenerateToken()
	w := Webhook{
		Base: jobs.Base{
			ID:        uuid.NewString(),
			Prompt:    prompt,
			PresetID:  presetID,
			Enabled:   true,
			CreatedAt: time.Now().UTC(),
		},
		TokenHash: HashToken(token),
	}

	list, err := LoadAll(userID)
	if err != nil {
		return Webhook{}, "", err
	}
	list = append(list, w)
	if err := SaveAll(userID, list); err != nil {
		return Webhook{}, "", err
	}
	registerOwner(w.ID, userID)
	return w, token, nil
}

// Update applies a sparse patch to a webhook and re-saves.
func Update(userID, id string, patch WebhookUpdate) (Webhook, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return Webhook{}, err
	}
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Webhook{}, errors.New("webhook not found")
	}

	updated := list[idx]
	if patch.Prompt != nil {
		if strings.TrimSpace(*patch.Prompt) == "" {
			return Webhook{}, errors.New("prompt cannot be empty")
		}
		updated.Prompt = *patch.Prompt
	}
	if patch.PresetID != nil {
		updated.PresetID = *patch.PresetID
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}

	list[idx] = updated
	if err := SaveAll(userID, list); err != nil {
		return Webhook{}, err
	}
	return updated, nil
}

// Delete removes a webhook and its owners-index entry.
func Delete(userID, id string) error {
	list, err := LoadAll(userID)
	if err != nil {
		return err
	}
	out := list[:0]
	removed := false
	for _, w := range list {
		if w.ID == id {
			removed = true
			continue
		}
		out = append(out, w)
	}
	if !removed {
		return errors.New("webhook not found")
	}
	if err := SaveAll(userID, out); err != nil {
		return err
	}
	unregisterOwner(id)
	return nil
}

// Toggle enables or disables a webhook in place.
func Toggle(userID, id string, enabled bool) (Webhook, error) {
	return Update(userID, id, WebhookUpdate{Enabled: &enabled})
}

// RotateToken generates and stores a fresh token for an existing webhook,
// returning the plaintext exactly once.
func RotateToken(userID, id string) (Webhook, string, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return Webhook{}, "", err
	}
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Webhook{}, "", errors.New("webhook not found")
	}

	token := GenerateToken()
	list[idx].TokenHash = HashToken(token)
	if err := SaveAll(userID, list); err != nil {
		return Webhook{}, "", err
	}
	return list[idx], token, nil
}

// FindByID returns a Webhook by ID, or an error if not found.
func FindByID(userID, id string) (Webhook, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return Webhook{}, err
	}
	for _, w := range list {
		if w.ID == id {
			return w, nil
		}
	}
	return Webhook{}, errors.New("webhook not found")
}

// recordTriggered stamps LastTriggeredAt = now. Called asynchronously from
// the trigger handler so a slow disk doesn't delay the LLM kickoff.
func recordTriggered(userID, id string) error {
	list, err := LoadAll(userID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range list {
		if list[i].ID == id {
			list[i].LastTriggeredAt = &now
			return SaveAll(userID, list)
		}
	}
	return errors.New("webhook not found")
}
