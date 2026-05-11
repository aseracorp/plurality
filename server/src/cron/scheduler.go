package cron

import (
	"fmt"
	"sync"

	"github.com/go-co-op/gocron/v2"

	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/utils"
)

var (
	sched     gocron.Scheduler
	scheduled sync.Map // map[jobID]gocron.Job
)

// Init builds the scheduler, walks every user's cron.json on disk, and
// registers their enabled jobs. Idempotent in the sense that
// RegisterJob/UnregisterJob will keep working after this.
func Init() {
	s, err := gocron.NewScheduler()
	if err != nil {
		utils.Error("[Cron] failed to create scheduler", err)
		return
	}
	sched = s

	for _, userID := range jobs.ListUserIDsOnDisk(cronFile) {
		userJobs, err := LoadAll(userID)
		if err != nil {
			utils.Error("[Cron] loading user "+userID, err)
			continue
		}
		for _, job := range userJobs {
			if err := RegisterJob(userID, job); err != nil {
				utils.Error("[Cron] register at startup "+job.ID, err)
			}
		}
	}

	sched.Start()
	utils.Log("[Cron] scheduler started")
}

// Shutdown stops the scheduler. Call from main's defer chain.
func Shutdown() {
	if sched == nil {
		return
	}
	if err := sched.Shutdown(); err != nil {
		utils.Error("[Cron] shutdown", err)
	}
}

// RegisterJob (re-)registers a job with gocron. Always unregisters any
// existing task with the same ID first so callers can simply call this on
// every mutation. Skips registration when !job.Enabled or scheduler not init.
func RegisterJob(userID string, job CronJob) error {
	UnregisterJob(job.ID)

	if sched == nil {
		return fmt.Errorf("scheduler not initialised")
	}
	if !job.Enabled {
		return nil
	}

	g, err := sched.NewJob(
		gocron.CronJob(job.Schedule, false),
		gocron.NewTask(Run, userID, job),
	)
	if err != nil {
		return err
	}
	scheduled.Store(job.ID, g)
	return nil
}

// UnregisterJob removes the gocron task for a CronJob ID, if any. No-op when
// missing.
func UnregisterJob(jobID string) {
	v, ok := scheduled.LoadAndDelete(jobID)
	if !ok {
		return
	}
	if sched == nil {
		return
	}
	g := v.(gocron.Job)
	if err := sched.RemoveJob(g.ID()); err != nil {
		utils.Error("[Cron] remove job "+jobID, err)
	}
}
