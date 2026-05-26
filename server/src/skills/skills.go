package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

const (
	skillFileName = "SKILL.md"
	maxBytes      = 50 * 1024
)

// SkillInfo holds metadata about a discovered skill.
type SkillInfo struct {
	Name        string
	Description string
	Path        string // absolute path to the skill's directory
}

// globalUserID is the sentinel key for the shared layer (data/skills +
// ~/.plurality/skills). A user's effective view merges this layer with their
// own users-data/{userID}/skills, with the global layer winning on name collision.
const globalUserID = ""

var (
	mu         sync.RWMutex
	skills     = map[string][]SkillInfo{}       // userID -> discovered skills
	skillRoots = map[string]map[string]string{} // userID -> skillName -> root dir it loaded from
)

// dataDir returns the configured data dir (env DATA_DIR, default ./data
// next to the binary). Matches mcp.dataDir.
func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

// dataSkillsDir is the server's bundled skills directory (data/skills).
// Auto-created on Init.
func dataSkillsDir() string {
	return filepath.Join(dataDir(), "skills")
}

// userSkillsDir is the per-user override directory (~/.plurality/skills).
// Returns "" if the home directory cannot be resolved. Not auto-created.
func userSkillsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".plurality", "skills")
}

// globalSkillsRoots returns the ordered list of directories that make up the
// shared/global skills layer. The first occurrence of a skill name wins, so
// data/skills takes precedence over ~/.plurality/skills on collision.
func globalSkillsRoots() []string {
	roots := []string{dataSkillsDir()}
	if u := userSkillsDir(); u != "" {
		roots = append(roots, u)
	}
	return roots
}

// perUserSkillsDir is a specific user's skills directory
// (users-data/{userID}/skills).
func perUserSkillsDir(userID string) string {
	return utils.UserFilePath(userID, "skills")
}

// Init scans the global skill roots and every user's
// users-data/{userID}/skills for */SKILL.md. Safe to re-run.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	skills = map[string][]SkillInfo{}
	skillRoots = map[string]map[string]string{}

	// Auto-create the bundled data dir so server installs have a place to drop
	// skills. Other roots are left alone — only scanned if they already exist.
	if err := os.MkdirAll(dataSkillsDir(), 0755); err != nil {
		utils.Error("[Skills] Failed to create skills dir", err)
	}

	// 1. Shared/global layer.
	loadRoots(globalUserID, globalSkillsRoots())

	// 2. Each user's personal layer on top.
	for _, uid := range utils.ListUserIDsWith("skills") {
		loadRoots(uid, []string{perUserSkillsDir(uid)})
	}
}

// loadRoots scans the given roots into a single user's slot. The first
// occurrence of a name within the user's own roots wins; for non-global users,
// a name already present in the global layer is skipped (global wins). Caller
// must hold mu.
func loadRoots(userID string, roots []string) {
	skills[userID] = nil
	skillRoots[userID] = map[string]string{}

	logOwner := "global"
	if userID != globalUserID {
		logOwner = "user " + userID
	}

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				utils.Error("[Skills] Failed to read skills dir "+root, err)
			}
			continue
		}

		loaded := 0
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			skillPath := filepath.Join(root, name)

			// Skill is valid only if SKILL.md exists.
			if _, err := os.Stat(filepath.Join(skillPath, skillFileName)); err != nil {
				continue
			}

			if existing, dup := skillRoots[userID][name]; dup {
				utils.Log("[Skills] (%s) Skipping duplicate skill %q in %s (already loaded from %s)", logOwner, name, root, existing)
				continue
			}
			// A user may not shadow a global skill name.
			if userID != globalUserID {
				if _, clash := skillRoots[globalUserID][name]; clash {
					utils.Log("[Skills] (%s) Skipping %q: shadows a global skill", logOwner, name)
					continue
				}
			}

			skills[userID] = append(skills[userID], SkillInfo{
				Name:        name,
				Description: readMetaDescription(skillPath),
				Path:        skillPath,
			})
			skillRoots[userID][name] = root
			loaded++
		}

		utils.Log("[Skills] (%s) %d skills loaded from %s", logOwner, loaded, root)
	}
}

// readMetaDescription returns the description from meta.json if the file
// exists and the field is present; otherwise returns an empty string.
func readMetaDescription(skillPath string) string {
	data, err := os.ReadFile(filepath.Join(skillPath, "meta.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Description)
}

// mergedSkills returns a user's effective skill list (global layer plus their
// own; global wins on name collision). Caller must hold mu.
func mergedSkills(userID string) []SkillInfo {
	seen := map[string]bool{}
	out := make([]SkillInfo, 0, len(skills[globalUserID])+len(skills[userID]))
	for _, s := range skills[globalUserID] { // global first (wins)
		if !seen[s.Name] {
			out = append(out, s)
			seen[s.Name] = true
		}
	}
	for _, s := range skills[userID] {
		if !seen[s.Name] {
			out = append(out, s)
			seen[s.Name] = true
		}
	}
	return out
}

// List returns the skills visible to a user (global + their own).
func List(userID string) []SkillInfo {
	mu.RLock()
	defer mu.RUnlock()
	return mergedSkills(userID)
}

// Names returns just the skill names visible to a user.
func Names(userID string) []string {
	mu.RLock()
	defer mu.RUnlock()
	merged := mergedSkills(userID)
	out := make([]string, 0, len(merged))
	for _, s := range merged {
		out = append(out, s.Name)
	}
	return out
}

// HasAny reports whether any skills are visible to a user.
func HasAny(userID string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(skills[globalUserID]) > 0 || len(skills[userID]) > 0
}

// ReadFile reads a file from a skill's directory with path-traversal
// protection matching client/lib/api/skills_service.dart:105-124.
// fileName defaults to SKILL.md when empty. Output is capped at 50 KB.
func ReadFile(userID, skillName, fileName string) (string, error) {
	if fileName == "" {
		fileName = skillFileName
	}

	if strings.Contains(skillName, "..") || strings.Contains(fileName, "..") {
		return "", fmt.Errorf("invalid path: \"..\" is not allowed")
	}
	if strings.ContainsAny(skillName, `/\`) || strings.ContainsAny(fileName, `/\`) {
		return "", fmt.Errorf("invalid path: slashes are not allowed in skill_name or file_name")
	}

	// Resolve the root in the user's view: global layer wins, then their own.
	mu.RLock()
	root, ok := skillRoots[globalUserID][skillName]
	if !ok {
		root, ok = skillRoots[userID][skillName]
	}
	mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("skill not found: %s", skillName)
	}
	full := filepath.Join(root, skillName, fileName)

	resolved, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	rootResolved, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving skills root: %w", err)
	}
	if !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) && resolved != rootResolved {
		return "", fmt.Errorf("invalid path: resolved path is outside skills directory")
	}

	data, err := os.ReadFile(resolved)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("file not found: %s/%s", skillName, fileName)
	}
	if err != nil {
		return "", fmt.Errorf("reading file: %w", err)
	}
	if len(data) > maxBytes {
		return string(data[:maxBytes]) + "\n\n[Content truncated — file exceeds 50KB limit]", nil
	}
	return string(data), nil
}
