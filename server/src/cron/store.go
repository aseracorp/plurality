package cron

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const cronFile = "cron.json"

var fileMu sync.Mutex // serialises writes to any user's cron.json

func userDataRoot() string {
	if p := os.Getenv("USER_DATA_STORAGE"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "users-data")
}

func cronPath(userID string) string {
	return filepath.Join(userDataRoot(), userID, cronFile)
}

// LoadAll returns the user's CRON list. Missing file -> empty slice.
func LoadAll(userID string) ([]CronJob, error) {
	data, err := os.ReadFile(cronPath(userID))
	if errors.Is(err, os.ErrNotExist) {
		return []CronJob{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []CronJob{}, nil
	}
	var jobs []CronJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// SaveAll writes the user's CRON list atomically (temp file + rename).
func SaveAll(userID string, jobs []CronJob) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	path := cronPath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// listUserIDsOnDisk returns every userID that has a cron.json on disk. Used at
// startup so we can re-register their jobs without enumerating auth users.
func listUserIDsOnDisk() []string {
	root := userDataRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), cronFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}
