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
			// Re-register so the next fire sees the new ConversationID.
			// gocron's task closure captured `job` by value at RegisterJob
			// time, so a plain disk write alone wouldn't be enough — the
			// in-memory snapshot would keep falling back forever.
			go func() {
				updated, err := setConversationID(userID, job.ID, convID)
				if err != nil {
					utils.Error("[Cron] setConversationID", err)
					return
				}
				if err := RegisterJob(userID, updated); err != nil {
					utils.Error("[Cron] re-register after ConversationID update", err)
				}
			}()
		},
	})
}
