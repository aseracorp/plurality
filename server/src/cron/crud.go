package cron

import (
	"errors"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"

	"github.com/azukaar/plurality/src/jobs"
)

// validateSchedule checks the cron expression parses with gocron's 5-field
// parser. Returns an error suitable for HTTP 400.
func validateSchedule(schedule string) error {
	if strings.TrimSpace(schedule) == "" {
		return errors.New("schedule is required")
	}
	// gocron.CronJob is a JobDefinition; constructing it doesn't parse the
	// expression on its own, but adding it to a temporary scheduler does.
	tmp, err := gocron.NewScheduler()
	if err != nil {
		return err
	}
	defer tmp.Shutdown()
	_, err = tmp.NewJob(gocron.CronJob(schedule, false), gocron.NewTask(func() {}))
	if err != nil {
		return err
	}
	return nil
}

// Create persists a new CronJob and registers it with the scheduler.
func Create(userID, schedule, prompt, presetID, conversationID string) (CronJob, error) {
	if strings.TrimSpace(prompt) == "" {
		return CronJob{}, errors.New("prompt is required")
	}
	if err := validateSchedule(schedule); err != nil {
		return CronJob{}, err
	}
	if presetID == "" {
		presetID = jobs.DefaultPresetID
	}

	job := CronJob{
		Base: jobs.Base{
			ID:             uuid.NewString(),
			Prompt:         prompt,
			PresetID:       presetID,
			Enabled:        true,
			CreatedAt:      time.Now().UTC(),
			ConversationID: conversationID,
		},
		Schedule: schedule,
	}

	list, err := LoadAll(userID)
	if err != nil {
		return CronJob{}, err
	}
	list = append(list, job)
	if err := SaveAll(userID, list); err != nil {
		return CronJob{}, err
	}
	if err := RegisterJob(userID, job); err != nil {
		return CronJob{}, err
	}
	return job, nil
}

// Update applies a sparse patch to a CronJob, re-saves, and re-registers it
// with the scheduler.
func Update(userID, id string, patch CronUpdate) (CronJob, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return CronJob{}, err
	}
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return CronJob{}, errors.New("cron not found")
	}

	updated := list[idx]
	if patch.Schedule != nil {
		if err := validateSchedule(*patch.Schedule); err != nil {
			return CronJob{}, err
		}
		updated.Schedule = *patch.Schedule
	}
	if patch.Prompt != nil {
		if strings.TrimSpace(*patch.Prompt) == "" {
			return CronJob{}, errors.New("prompt cannot be empty")
		}
		updated.Prompt = *patch.Prompt
	}
	if patch.PresetID != nil {
		updated.PresetID = *patch.PresetID
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}
	if patch.ConversationID != nil {
		updated.ConversationID = *patch.ConversationID
	}

	list[idx] = updated
	if err := SaveAll(userID, list); err != nil {
		return CronJob{}, err
	}
	if err := RegisterJob(userID, updated); err != nil {
		return CronJob{}, err
	}
	return updated, nil
}

// Delete removes a CronJob from disk and unregisters its scheduler task.
func Delete(userID, id string) error {
	list, err := LoadAll(userID)
	if err != nil {
		return err
	}
	out := list[:0]
	removed := false
	for _, j := range list {
		if j.ID == id {
			removed = true
			continue
		}
		out = append(out, j)
	}
	if !removed {
		return errors.New("cron not found")
	}
	if err := SaveAll(userID, out); err != nil {
		return err
	}
	UnregisterJob(id)
	return nil
}

// Toggle enables or disables a CronJob in place.
func Toggle(userID, id string, enabled bool) (CronJob, error) {
	return Update(userID, id, CronUpdate{Enabled: &enabled})
}

// RunNow triggers a cron immediately, regardless of its Enabled state.
// Returns an error if the cron does not exist.
func RunNow(userID, id string) error {
	list, err := LoadAll(userID)
	if err != nil {
		return err
	}
	for _, j := range list {
		if j.ID == id {
			go Run(userID, j)
			return nil
		}
	}
	return errors.New("cron not found")
}

// FindByID returns a CronJob by ID, or an error if not found.
func FindByID(userID, id string) (CronJob, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return CronJob{}, err
	}
	for _, j := range list {
		if j.ID == id {
			return j, nil
		}
	}
	return CronJob{}, errors.New("cron not found")
}

// setConversationID rewrites just the ConversationID field on a cron job.
// Called from the trigger path when a configured conversation no longer
// exists so the persisted record points at the freshly-created replacement.
// Returns the updated CronJob so the caller can re-register it with the
// scheduler (gocron caches the task arguments, so disk updates alone are
// invisible to subsequent fires).
func setConversationID(userID, id, convID string) (CronJob, error) {
	list, err := LoadAll(userID)
	if err != nil {
		return CronJob{}, err
	}
	for i := range list {
		if list[i].ID == id {
			list[i].ConversationID = convID
			if err := SaveAll(userID, list); err != nil {
				return CronJob{}, err
			}
			return list[i], nil
		}
	}
	return CronJob{}, errors.New("cron not found")
}
