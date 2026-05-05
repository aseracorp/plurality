package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

var (
	usersMu  sync.RWMutex
	usersDB  []User
	usersErr error
)

// usersPath returns data/user.json (matches skills.dataDir convention).
func usersPath() string {
	return filepath.Join(dataDir(), "user.json")
}

// LoadUsers reads data/user.json into memory. Creates an empty file if missing.
func LoadUsers() error {
	usersMu.Lock()
	defer usersMu.Unlock()

	path := usersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		usersErr = err
		return err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		usersDB = []User{}
		usersErr = nil
		return saveLocked()
	}
	if err != nil {
		usersErr = err
		return err
	}

	var loaded []User
	if len(data) == 0 {
		loaded = []User{}
	} else if err := json.Unmarshal(data, &loaded); err != nil {
		usersErr = err
		return err
	}

	usersDB = loaded
	usersErr = nil
	return nil
}

func saveLocked() error {
	data, err := json.MarshalIndent(usersDB, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(usersPath(), data, 0600)
}

// AddUser hashes the password and persists a new user. Returns error if name exists.
func AddUser(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username cannot be empty")
	}
	if strings.ContainsAny(username, `/\`) || strings.Contains(username, "..") {
		return errors.New("username contains forbidden characters")
	}
	if len(password) < 4 {
		return errors.New("password must be at least 4 characters")
	}

	usersMu.Lock()
	defer usersMu.Unlock()

	for _, u := range usersDB {
		if strings.EqualFold(u.Username, username) {
			return errors.New("user already exists")
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	usersDB = append(usersDB, User{Username: username, PasswordHash: string(hash)})
	return saveLocked()
}

// RemoveUser deletes a user by username. No-op if not found.
func RemoveUser(username string) error {
	usersMu.Lock()
	defer usersMu.Unlock()

	out := usersDB[:0]
	removed := false
	for _, u := range usersDB {
		if strings.EqualFold(u.Username, username) {
			removed = true
			continue
		}
		out = append(out, u)
	}
	if !removed {
		return errors.New("user not found")
	}
	usersDB = out
	return saveLocked()
}

// VerifyPassword returns the canonical username (with original casing) on success.
func VerifyPassword(username, password string) (string, error) {
	usersMu.RLock()
	defer usersMu.RUnlock()

	for _, u := range usersDB {
		if strings.EqualFold(u.Username, username) {
			if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
				return "", errors.New("invalid credentials")
			}
			return u.Username, nil
		}
	}
	return "", errors.New("invalid credentials")
}

// ChangePassword replaces a user's password hash.
func ChangePassword(username, oldPassword, newPassword string) error {
	if _, err := VerifyPassword(username, oldPassword); err != nil {
		return err
	}
	if len(newPassword) < 4 {
		return errors.New("password must be at least 4 characters")
	}

	usersMu.Lock()
	defer usersMu.Unlock()

	for i, u := range usersDB {
		if strings.EqualFold(u.Username, username) {
			hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			usersDB[i].PasswordHash = string(hash)
			return saveLocked()
		}
	}
	return errors.New("user not found")
}

// UserExists reports whether a username (case-insensitive) is registered locally.
func UserExists(username string) bool {
	usersMu.RLock()
	defer usersMu.RUnlock()
	for _, u := range usersDB {
		if strings.EqualFold(u.Username, username) {
			return true
		}
	}
	return false
}

// ListUsernames returns all registered usernames.
func ListUsernames() []string {
	usersMu.RLock()
	defer usersMu.RUnlock()
	out := make([]string, len(usersDB))
	for i, u := range usersDB {
		out[i] = u.Username
	}
	return out
}

// HasAny reports whether at least one local user exists.
func HasAny() bool {
	usersMu.RLock()
	defer usersMu.RUnlock()
	return len(usersDB) > 0
}

// SeedAdminFromEnv creates a single user from INIT_ADMIN_USER / INIT_ADMIN_PASSWORD
// when no users exist. Idempotent.
func SeedAdminFromEnv() {
	if HasAny() {
		return
	}
	user := os.Getenv("INIT_ADMIN_USER")
	pw := os.Getenv("INIT_ADMIN_PASSWORD")
	if user == "" || pw == "" {
		return
	}
	if err := AddUser(user, pw); err != nil {
		utils.Error("[Auth] INIT_ADMIN seed failed", err)
		return
	}
	utils.Log("[Auth] Seeded initial admin user from env: %s", user)
}
