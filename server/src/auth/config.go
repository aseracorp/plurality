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

type Config struct {
	JWTSecret string       `json:"jwt_secret"`
	OpenID    OpenIDConfig `json:"openid"`
}

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
		if loaded.JWTSecret == "" {
			loaded.JWTSecret = randomHex(32)
			cfg = loaded
			if err := writeConfigLocked(); err != nil {
				return err
			}
		} else {
			cfg = loaded
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
