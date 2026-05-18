// Package memory persists a per-user "important memory" snippet — a short,
// user-editable string that is always injected into the system prompt before
// every chat completion. The assistant can overwrite it via the
// update_important_memory tool; the user can edit it from the account page.
package memory

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// DefaultMemory is returned when a user has not yet written a memory.
const DefaultMemory = "In order to be a useful assistant, you should suggest your owner to tell you more about them: Name, email, phone number for example."

const filename = "important_memory.txt"

var fileMu sync.Mutex

func userDataRoot() string {
	if p := os.Getenv("USER_DATA_STORAGE"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "users-data")
}

func memoryPath(userID string) string {
	return filepath.Join(userDataRoot(), userID, filename)
}

// Get returns the user's important memory, or DefaultMemory when no file
// exists yet (or when reading fails).
func Get(userID string) string {
	if userID == "" {
		return DefaultMemory
	}
	data, err := os.ReadFile(memoryPath(userID))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultMemory
	}
	if err != nil {
		return DefaultMemory
	}
	if len(data) == 0 {
		return DefaultMemory
	}
	return string(data)
}

// Set overwrites the user's memory snippet atomically (temp file + rename).
func Set(userID, content string) error {
	if userID == "" {
		return errors.New("empty userID")
	}
	fileMu.Lock()
	defer fileMu.Unlock()

	path := memoryPath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
