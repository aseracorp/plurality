package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// fileMu serialises writes across every users-data/{user}/{file}.json file.
// One mutex is enough — writes are short and contention is low.
var fileMu sync.Mutex

func userDataRoot() string {
	if p := os.Getenv("USER_DATA_STORAGE"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "users-data")
}

func filePath(userID, filename string) string {
	return filepath.Join(userDataRoot(), userID, filename)
}

// LoadAll reads a JSON array from users-data/{userID}/{filename}. A missing
// file is not an error — it returns an empty slice.
func LoadAll[T any](userID, filename string) ([]T, error) {
	data, err := os.ReadFile(filePath(userID, filename))
	if errors.Is(err, os.ErrNotExist) {
		return []T{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []T{}, nil
	}
	var items []T
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// SaveAll writes the JSON array atomically (temp file + rename).
func SaveAll[T any](userID, filename string, items []T) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	path := filePath(userID, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ListUserIDsOnDisk returns every userID that has a users-data/{user}/{filename}
// on disk. Used at startup to rebuild in-memory indexes.
func ListUserIDsOnDisk(filename string) []string {
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
		if _, err := os.Stat(filepath.Join(root, e.Name(), filename)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}
