package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

const fakePAT = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"

func assertRedacted(t *testing.T, in string) {
	t.Helper()
	got := RedactSecrets(in)
	if strings.Contains(got, fakePAT) {
		t.Errorf("secret survived redaction\n in: %s\nout: %s", in, got)
	}
	if !strings.Contains(got, RedactedPlaceholder) {
		t.Errorf("expected placeholder\n in: %s\nout: %s", in, got)
	}
}

func TestRedactsEnvAssignment(t *testing.T) {
	assertRedacted(t, "GITHUB_PERSONAL_ACCESS_TOKEN="+fakePAT)
	assertRedacted(t, `["GITHUB_PERSONAL_ACCESS_TOKEN=`+fakePAT+`", "stdio"]`)
	assertRedacted(t, "export ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnopqrstuvwxyz")
}

func TestRedactsGitURLUserinfo(t *testing.T) {
	// Observed in production: the github-git MCP server logs the git command.
	in := `git clone "https://madejackson:` + fakePAT + `@github.com/aseracorp/cosmos-owrt.git" "cosmos-owrt"`
	got := RedactSecrets(in)
	if strings.Contains(got, fakePAT) {
		t.Errorf("PAT leaked in git URL form:\n%s", got)
	}
	// The host/path must survive so the log line stays useful.
	for _, want := range []string{"github.com/aseracorp/cosmos-owrt.git", "git clone", "madejackson"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q preserved in:\n%s", want, got)
		}
	}
}

func TestRedactsJSONKeyValue(t *testing.T) {
	assertRedacted(t, `{"GITHUB_PERSONAL_ACCESS_TOKEN":"`+fakePAT+`"}`)
	assertRedacted(t, `{"api_key": "`+fakePAT+`"}`)
	assertRedacted(t, `{"PRIVATE_KEY": "MIIEvQIBADANBgqh"}`)
}

func TestRedactsBearerHeader(t *testing.T) {
	assertRedacted(t, "Authorization: Bearer "+fakePAT)
}

func TestPreservesNonSecretConfig(t *testing.T) {
	// The manage_mcp tool exists so the model can EDIT this config. Blinding
	// ordinary fields would break that workflow outright.
	in := `{
  "mcpServers": {
    "searxng": {
      "command": "python3",
      "args": ["/app/mcp/searxng/server.py", "--port", "8101"],
      "env": {"SEARXNG_URL": "http://searxng:8080"},
      "description": "Web search"
    },
    "openrouter": {
      "model": "openrouter/anthropic/claude-opus-4.5",
      "api_base_url": "https://openrouter.ai/api/v1",
      "temperature": 0.7,
      "max_tokens": 8192,
      "monkey": "yes", "compass": "keepme"
    }
  }
}`
	got := RedactSecrets(in)
	if got != in {
		t.Errorf("non-secret config was modified\n--- got ---\n%s\n--- want ---\n%s", got, in)
	}
}

func TestPreservesEnvReferences(t *testing.T) {
	// ${VAR} indirection is the recommended form: it is not a secret, and
	// redacting it would hide the very thing the user is trying to inspect.
	in := `"args": ["--token", "${GITHUB_PERSONAL_ACCESS_TOKEN}"]`
	if got := RedactSecrets(in); got != in {
		t.Errorf("env reference was redacted:\n%s", got)
	}
}

func TestPreservesBenignEnvNames(t *testing.T) {
	in := "PATH=/usr/local/bin:/usr/bin HOME=/app LANG=C.UTF-8 DATA_DIR=/app/data"
	if got := RedactSecrets(in); got != in {
		t.Errorf("benign env names redacted:\n%s", got)
	}
}

func TestLooksLikeSecretKey(t *testing.T) {
	secrets := []string{
		"GITHUB_PERSONAL_ACCESS_TOKEN", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY",
		"ANTHROPIC_API_KEY", "PRIVATE_KEY", "DB_PASSWORD", "AUTHORIZATION",
		"GITHUB_TOKEN", "SESSION_COOKIE", "GH_TOKEN", "pat",
	}
	for _, k := range secrets {
		if !looksLikeSecretKey(k) {
			t.Errorf("expected %q to be classified as secret", k)
		}
	}
	benign := []string{
		"PATH", "HOME", "LANG", "DATA_DIR", "USER_DATA_STORAGE", "model",
		"api_base_url", "temperature", "max_tokens", "SEARXNG_URL", "command",
	}
	for _, k := range benign {
		if looksLikeSecretKey(k) {
			t.Errorf("expected %q to be classified as non-secret", k)
		}
	}
}

func TestExpandEnvArgs(t *testing.T) {
	t.Setenv("PL_TEST_SECRET", "s3cr3t-value")
	got := expandEnvArgs([]string{"--token", "${PL_TEST_SECRET}", "stdio", "${PL_TEST_MISSING}"})
	want := []string{"--token", "s3cr3t-value", "stdio", ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
	// Literal args must pass through untouched.
	if lit := expandEnvArgs([]string{"stdio", "--read-only"}); lit[0] != "stdio" || lit[1] != "--read-only" {
		t.Errorf("literal args modified: %v", lit)
	}
}

func TestChildEnvPrecedence(t *testing.T) {
	t.Setenv("PL_TEST_VAR", "from-parent")
	p := &ProcessManager{env: []string{"PL_TEST_VAR=from-config"}}
	env := p.childEnv()
	var last string
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, "PL_TEST_VAR=") {
			last = e
			count++
		}
	}
	if count < 2 {
		t.Fatalf("expected configured entry appended after inherited, got %d", count)
	}
	// exec.Cmd takes the LAST occurrence, so config must win.
	if last != "PL_TEST_VAR=from-config" {
		t.Errorf("config env must win: got %q", last)
	}
	// No env configured -> inherit unchanged (nil = do not set cmd.Env).
	if (&ProcessManager{}).childEnv() != nil {
		t.Errorf("empty env must return nil to preserve inheritance")
	}
}

func TestRedactSecretsEdgeCases(t *testing.T) {
	if got := RedactSecrets(""); got != "" {
		t.Errorf("empty string must round-trip, got %q", got)
	}
	// Long non-secret text must not be mangled by the bearer pattern.
	in := "the token based approach is widely used in modern compilers"
	if got := RedactSecrets(in); got != in {
		t.Errorf("prose mangled:\n%s", got)
	}
}

// parse is a test helper for JSON snippets.
func parse(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// The manage_mcp workflow is get -> edit -> set. 'get' redacts, so a config
// written back verbatim contains placeholders exactly where the user's real
// credentials live. RestoreRedactedSecrets must put them back, or every
// server using those credentials is bricked.
func TestRestoreRedactedSecretsRoundTrip(t *testing.T) {
	const pat = "ghp_REALSECRETVALUE0123456789abcdefghij"
	onDisk := parse(t, `{
      "mcpServers": {
        "github": {
          "command": "env",
          "args": ["GITHUB_PERSONAL_ACCESS_TOKEN=`+pat+`", "stdio"],
          "env": {"GITHUB_TOKEN": "`+pat+`"},
          "description": "GitHub"
        }
      }
    }`)

	// Exactly what 'get' hands the model, edited, then handed back to 'set'.
	model := parse(t, `{
      "mcpServers": {
        "github": {
          "command": "env",
          "args": ["GITHUB_PERSONAL_ACCESS_TOKEN=`+RedactedPlaceholder+`", "stdio"],
          "env": {"GITHUB_TOKEN": "`+RedactedPlaceholder+`"},
          "description": "GitHub (read-only)"
        }
      }
    }`)

	n := RestoreRedactedSecrets(model, onDisk)
	if n != 2 {
		t.Errorf("expected 2 restored secrets, got %d", n)
	}

	got, _ := json.Marshal(model)
	s := string(got)
	if !strings.Contains(s, pat) {
		t.Errorf("real PAT not restored; config would be bricked:\n%s", s)
	}
	if strings.Contains(s, RedactedPlaceholder) {
		t.Errorf("placeholder persisted to disk:\n%s", s)
	}
	// The model's legitimate edit must survive.
	if !strings.Contains(s, "GitHub (read-only)") {
		t.Errorf("user edit lost:\n%s", s)
	}
}

func TestRestoreRedactedSecretsLeavesUnknownAlone(t *testing.T) {
	// A brand-new server has no previous value to restore.
	next := parse(t, `{"mcpServers":{"new":{"env":{"TOKEN":"`+RedactedPlaceholder+`"}}}}`)
	prev := parse(t, `{"mcpServers":{"other":{"env":{"TOKEN":"abc"}}}}`)

	n := RestoreRedactedSecrets(next, prev)
	if n != 0 {
		t.Errorf("nothing should be restored, got %d", n)
	}
	servers := next["mcpServers"].(map[string]interface{})
	env := servers["new"].(map[string]interface{})["env"].(map[string]interface{})
	if env["TOKEN"] != RedactedPlaceholder {
		t.Errorf("unknown placeholder should be left for the user to fill, got %v", env["TOKEN"])
	}
}

func TestRestoreRedactedSecretsIgnoresEmptyPrev(t *testing.T) {
	// An empty previous value must not be treated as a secret to restore.
	next := parse(t, `{"mcpServers":{"a":{"env":{"TOKEN":"`+RedactedPlaceholder+`"}}}}`)
	prev := parse(t, `{"mcpServers":{"a":{"env":{"TOKEN":""}}}}`)
	if n := RestoreRedactedSecrets(next, prev); n != 0 {
		t.Errorf("empty prev must not be restored, got %d", n)
	}
}

func TestRestoreRedactedSecretsHandlesShapeMismatch(t *testing.T) {
	// Model reshaped the config (object -> array, missing keys); must not panic.
	next := parse(t, `{"mcpServers":{"a":{"args":["x", "`+RedactedPlaceholder+`"]}}}`)
	prev := parse(t, `{"mcpServers":{"a":{"args":"not-an-array"}}}`)
	if n := RestoreRedactedSecrets(next, prev); n != 0 {
		t.Errorf("shape mismatch should restore nothing, got %d", n)
	}
}
