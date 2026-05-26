package utils

import (
	"os"
	"path/filepath"
)

// UserDataRoot returns the per-user data root (env USER_DATA_STORAGE, default
// ./users-data next to the binary). Mirrors jobs.userDataRoot — kept here in
// the leaf utils package so mcp/skills can reuse it without an import cycle
// (jobs imports ai, which transitively imports mcp).
func UserDataRoot() string {
	if p := os.Getenv("USER_DATA_STORAGE"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "users-data")
}

// UserFilePath returns the absolute path to users-data/{userID}/{name}.
func UserFilePath(userID, name string) string {
	return filepath.Join(UserDataRoot(), userID, name)
}

// ListUserIDsWith returns every userID that has a users-data/{user}/{name}
// (file or directory) on disk. Used at startup to discover per-user config.
func ListUserIDsWith(name string) []string {
	entries, err := os.ReadDir(UserDataRoot())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(UserDataRoot(), e.Name(), name)); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}
