package cron

import (
	"context"

	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/utils"
)

// Run fires a single CRON: delegates to jobs.RunPrompt with the right
// trigger tags. gocron calls this on schedule (see scheduler.go).
func Run(userID string, job CronJob) {
	jobs.RunPrompt(context.Background(), userID, jobs.RunOptions{
		TitlePrefix:    "Cron",
		Prompt:         job.Prompt,
		PresetID:       job.PresetID,
		TriggerType:    "cron",
		TriggerID:      job.ID,
		ConversationID: job.ConversationID,
		OnConversationResolved: func(convID string) {
			if convID == job.ConversationID {
				return
			}
			go func() {
				if err := setConversationID(userID, job.ID, convID); err != nil {
					utils.Error("[Cron] setConversationID", err)
				}
			}()
		},
	})
}
