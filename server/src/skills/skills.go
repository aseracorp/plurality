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

var (
	mu         sync.RWMutex
	skills     []SkillInfo
	skillRoots map[string]string // skillName -> root directory the skill was loaded from
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

// skillsRoots returns the ordered list of directories to scan for skills.
// The first occurrence of a skill name wins, so data/skills takes precedence
// over ~/.plurality/skills on collision.
func skillsRoots() []string {
	roots := []string{dataSkillsDir()}
	if u := userSkillsDir(); u != "" {
		roots = append(roots, u)
	}
	return roots
}

// Init scans the configured skill roots for */SKILL.md. Safe to re-run.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	skills = nil
	skillRoots = map[string]string{}

	roots := skillsRoots()

	// Auto-create the bundled data dir so server installs have a place to drop
	// skills. The user dir is left alone — we only scan it if it already exists.
	if len(roots) > 0 {
		if err := os.MkdirAll(roots[0], 0755); err != nil {
			utils.Error("[Skills] Failed to create skills dir", err)
		}
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

			if existing, dup := skillRoots[name]; dup {
				utils.Log("[Skills] Skipping duplicate skill %q in %s (already loaded from %s)", name, root, existing)
				continue
			}

			skills = append(skills, SkillInfo{
				Name:        name,
				Description: readMetaDescription(skillPath),
				Path:        skillPath,
			})
			skillRoots[name] = root
			loaded++
		}

		utils.Log("[Skills] %d skills loaded from %s", loaded, root)
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

// List returns a copy of the discovered skills.
func List() []SkillInfo {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]SkillInfo, len(skills))
	copy(out, skills)
	return out
}

// Names returns just the skill names.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.Name)
	}
	return out
}

// HasAny reports whether any skills are configured.
func HasAny() bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(skills) > 0
}

// ReadFile reads a file from a skill's directory with path-traversal
// protection matching client/lib/api/skills_service.dart:105-124.
// fileName defaults to SKILL.md when empty. Output is capped at 50 KB.
func ReadFile(skillName, fileName string) (string, error) {
	if fileName == "" {
		fileName = skillFileName
	}

	if strings.Contains(skillName, "..") || strings.Contains(fileName, "..") {
		return "", fmt.Errorf("invalid path: \"..\" is not allowed")
	}
	if strings.ContainsAny(skillName, `/\`) || strings.ContainsAny(fileName, `/\`) {
		return "", fmt.Errorf("invalid path: slashes are not allowed in skill_name or file_name")
	}

	mu.RLock()
	root, ok := skillRoots[skillName]
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
