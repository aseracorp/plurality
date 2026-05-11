package cron

import "github.com/azukaar/plurality/src/jobs"

const cronFile = "cron.json"

// LoadAll returns the user's CRON list. Missing file -> empty slice.
func LoadAll(userID string) ([]CronJob, error) {
	return jobs.LoadAll[CronJob](userID, cronFile)
}

// SaveAll writes the user's CRON list atomically.
func SaveAll(userID string, items []CronJob) error {
	return jobs.SaveAll(userID, cronFile, items)
}
