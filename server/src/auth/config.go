package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/azukaar/plurality/src/utils"
)

type OpenIDConfig struct {
	Enabled      bool     `json:"enabled"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RedirectURL  string   `json:"redirect_url"`
	Allowlist    []string `json:"allowlist"`
}

// ShortcutModel is one model entry inside a shortcut. Tools maps tool keys
// (e.g. "search_web") to their default state ("true" / "ask" / "false").
type ShortcutModel struct {
	Name  string            `json:"name"`
	Tools map[string]string `json:"tools,omitempty"`
}

// ShortcutModels groups the per-mode model selections for a shortcut.
type ShortcutModels struct {
	Text     *ShortcutModel `json:"text,omitempty"`
	Vision   *ShortcutModel `json:"vision,omitempty"`
	ImageGen *ShortcutModel `json:"imagegen,omitempty"`
}

// Shortcut is a named bundle of model selections + tool defaults shown in the
// picker. The names "fast", "medium", and "smart" are reserved and surfaced in
// the UI; any extra entry is preserved on disk but ignored by the picker.
type Shortcut struct {
	Name    string         `json:"name"`
	Label   string         `json:"label"`
	Pricing string         `json:"pricing"`
	Color   string         `json:"color"`
	Models  ShortcutModels `json:"models"`
}

type Config struct {
	JWTSecret string       `json:"jwt_secret"`
	OpenID    OpenIDConfig `json:"openid"`
	Shortcuts []Shortcut   `json:"shortcuts"`
}

// ReservedShortcutNames are the picker-visible shortcut names, in display order.
var ReservedShortcutNames = []string{"fast", "medium", "smart"}

var (
	cfgMu sync.RWMutex
	cfg   Config
)

// dataDir mirrors skills.dataDir / mcp.dataDir.
func dataDir() string {
	if p := os.Getenv("DATA_DIR"); p != "" {
		return p
	}
	exec, _ := os.Executable()
	return filepath.Join(filepath.Dir(exec), "data")
}

func configPath() string {
	return filepath.Join(dataDir(), "config.json")
}

// LoadConfig reads data/config.json (creating defaults if missing) and
// applies env-var overrides on top. Env wins over file.
func LoadConfig() error {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	if err := os.MkdirAll(dataDir(), 0755); err != nil {
		return err
	}

	path := configPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = Config{
			JWTSecret: randomHex(32),
			OpenID: OpenIDConfig{
				Enabled:   false,
				Allowlist: []string{},
			},
		}
		validateShortcuts(&cfg)
		if err := writeConfigLocked(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var loaded Config
		if err := json.Unmarshal(data, &loaded); err != nil {
			return err
		}
		needsWrite := false
		if loaded.JWTSecret == "" {
			loaded.JWTSecret = randomHex(32)
			needsWrite = true
		}
		if validateShortcuts(&loaded) {
			needsWrite = true
		}
		cfg = loaded
		if needsWrite {
			if err := writeConfigLocked(); err != nil {
				return err
			}
		}
	}

	applyEnvOverrides()
	return nil
}

func writeConfigLocked() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

func applyEnvOverrides() {
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv("OPENID_ISSUER"); v != "" {
		cfg.OpenID.Issuer = v
		cfg.OpenID.Enabled = true
	}
	if v := os.Getenv("OPENID_CLIENT_ID"); v != "" {
		cfg.OpenID.ClientID = v
	}
	if v := os.Getenv("OPENID_CLIENT_SECRET"); v != "" {
		cfg.OpenID.ClientSecret = v
	}
	if v := os.Getenv("OPENID_REDIRECT_URL"); v != "" {
		cfg.OpenID.RedirectURL = v
	}
	if v := os.Getenv("OPENID_ALLOWLIST"); v != "" {
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		cfg.OpenID.Allowlist = out
	}
}

func GetConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func OpenIDEnabled() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.OpenID.Enabled && cfg.OpenID.Issuer != "" && cfg.OpenID.ClientID != ""
}

// AllowlistMatch returns true if the email matches any allowlist entry.
// "*" alone allows any authenticated email; "*@domain" matches that domain;
// a literal entry must match exactly (case-insensitive).
func AllowlistMatch(email string) bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	for _, entry := range cfg.OpenID.Allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if strings.HasPrefix(entry, "*@") {
			if strings.HasSuffix(email, entry[1:]) {
				return true
			}
			continue
		}
		if entry == email {
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		utils.Error("[Auth] randomHex failed", err)
	}
	return hex.EncodeToString(b)
}

// defaultShortcutTools is the on-by-default tool set for the standard chat shortcuts.
var defaultShortcutTools = map[string]string{
	"search_web":                           "true",
	"place_search":                         "true",
	"visit_link":                           "true",
	"generate_image":                       "true",
	"long_task":                            "true",
	"manage_cron":                          "true",
	"list_presets":                         "true",
	"conversations__search_conversations":  "true",
	"conversations__retrieve_conversation": "true",
}

// defaultShortcutVisionTools is a slimmer set used when the vision model is
// distinct from the text model — drops web/place/link search to keep cost down.
var defaultShortcutVisionTools = map[string]string{
	"generate_image":                       "true",
	"long_task":                            "true",
	"manage_cron":                          "true",
	"list_presets":                         "true",
	"conversations__search_conversations":  "true",
	"conversations__retrieve_conversation": "true",
}

// defaultShortcuts returns baked-in definitions for the three reserved names.
func defaultShortcuts() map[string]Shortcut {
	cloneTools := func(src map[string]string) map[string]string {
		out := make(map[string]string, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}

	return map[string]Shortcut{
		"fast": {
			Name: "fast", Label: "Fast and low cost", Pricing: "$", Color: "green",
			Models: ShortcutModels{
				Text:     &ShortcutModel{Name: "qwen3-vl-30b-a3b-instruct", Tools: cloneTools(defaultShortcutTools)},
				Vision:   &ShortcutModel{Name: "qwen3-vl-30b-a3b-instruct", Tools: cloneTools(defaultShortcutTools)},
				ImageGen: &ShortcutModel{Name: "black-forest-labs/FLUX.2-dev"},
			},
		},
		"medium": {
			Name: "medium", Label: "Recommended", Pricing: "$$", Color: "blue",
			Models: ShortcutModels{
				Text:     &ShortcutModel{Name: "qwen3p6-plus", Tools: cloneTools(defaultShortcutTools)},
				Vision:   &ShortcutModel{Name: "qwen3p6-plus", Tools: cloneTools(defaultShortcutTools)},
				ImageGen: &ShortcutModel{Name: "black-forest-labs/FLUX.2-dev"},
			},
		},
		"smart": {
			Name: "smart", Label: "Best quality but slow", Pricing: "$$$", Color: "purple",
			Models: ShortcutModels{
				Text:     &ShortcutModel{Name: "claude-opus-4-7", Tools: cloneTools(defaultShortcutTools)},
				Vision:   &ShortcutModel{Name: "claude-opus-4-7", Tools: cloneTools(defaultShortcutVisionTools)},
				ImageGen: &ShortcutModel{Name: "black-forest-labs/FLUX.2-pro"},
			},
		},
	}
}

// validateShortcuts ensures the three reserved names exist and appear first
// (in fast → medium → smart order). Custom entries trail. Returns true when
// the slice was mutated, signalling the caller to persist the config.
func validateShortcuts(c *Config) bool {
	defaults := defaultShortcuts()
	mutated := false

	// Index the existing entries by lowercased name; preserve insertion order
	// for any custom entries we encounter.
	byName := map[string]int{}
	var customOrder []int
	for i, s := range c.Shortcuts {
		key := strings.ToLower(strings.TrimSpace(s.Name))
		c.Shortcuts[i].Name = key
		byName[key] = i
		isReserved := false
		for _, r := range ReservedShortcutNames {
			if key == r {
				isReserved = true
				break
			}
		}
		if !isReserved {
			customOrder = append(customOrder, i)
		}
	}

	// Build the new ordered list: reserved first (filling in defaults), then customs.
	newList := make([]Shortcut, 0, len(c.Shortcuts)+len(ReservedShortcutNames))
	for _, name := range ReservedShortcutNames {
		if idx, ok := byName[name]; ok {
			newList = append(newList, c.Shortcuts[idx])
		} else {
			utils.Log("[Auth] shortcut %q missing, injecting default", name)
			newList = append(newList, defaults[name])
			mutated = true
		}
	}
	for _, idx := range customOrder {
		newList = append(newList, c.Shortcuts[idx])
	}

	if !mutated {
		// Still need to detect re-ordering or name normalisation as a mutation.
		if len(newList) != len(c.Shortcuts) {
			mutated = true
		} else {
			for i := range newList {
				if newList[i].Name != c.Shortcuts[i].Name {
					mutated = true
					break
				}
			}
		}
	}

	c.Shortcuts = newList
	return mutated
}
