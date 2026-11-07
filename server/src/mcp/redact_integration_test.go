package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spawnChildToFile runs `sh -c script` and returns what the child wrote to
// the file named by $OUT. ProcessManager's reader goroutine owns the child's
// stdout pipe, so tests must not read that pipe directly; a file avoids the
// race. `script` is expected to write to "$OUT".
func spawnChildToFile(t *testing.T, name, script string, extraArgs, env []string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "child.out")

	args := []string{"-c", script, "sh"}
	args = append(args, extraArgs...)

	pm := NewProcessManager(name, "sh", args, append([]string{"OUT=" + out}, env...))
	if err := pm.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pm.Stop()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(out)
		if err == nil && len(b) > 0 && !pm.IsRunning() {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(out)
	if len(b) == 0 {
		t.Fatalf("child produced no output")
	}
	return string(b)
}

// End-to-end proof of the env indirection against a real subprocess: the
// secret reaches the child's ENVIRONMENT and never appears in its command
// line. argv is world-readable (/proc/<pid>/cmdline, mode 0444, copied into
// `ps`, core dumps and audit logs); environ is mode 0400.
func TestRealProcessEnvKeepsSecretOutOfArgv(t *testing.T) {
	const secret = "ghp_INTEGRATIONSECRET0123456789abcdefghij"

	// $0 is "sh", $1/$2 are inert args. Record env value and full argv.
	script := `printf 'ENV=[%s]\n' "$GITHUB_PERSONAL_ACCESS_TOKEN" > "$OUT"; ` +
		`printf 'ARGV=%s %s %s %s\n' "$0" "$1" "$2" "$3" >> "$OUT"`

	got := spawnChildToFile(t, "env-test", script,
		[]string{"stdout", "stdio"},
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN=" + secret})

	if !strings.Contains(got, "ENV=["+secret+"]") {
		t.Errorf("child did not receive the secret via env\n--- child output ---\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "ARGV=") && strings.Contains(line, secret) {
			t.Errorf("secret leaked into child argv:\n%s", line)
		}
	}
}

// ${VAR} inside args expands from the server's own environment, so mcp.json
// can name a credential without containing it.
func TestRealProcessExpandsEnvRefInArgs(t *testing.T) {
	t.Setenv("PL_TEST_ARG_TARGET", "expanded-from-server-env")

	script := `printf 'GOT=%s\n' "$1" > "$OUT"`
	got := spawnChildToFile(t, "expand-test", script,
		[]string{"${PL_TEST_ARG_TARGET}"},
		nil)

	if !strings.Contains(got, "GOT=expanded-from-server-env") {
		t.Errorf("${VAR} arg not expanded\n--- child output ---\n%s", got)
	}
}

// A child printing a credential-bearing command on stderr must not leak it
// into the debug log buffer. Reproduces the real-world case found in
// server.log: the github-git MCP printing `git clone https://user:<PAT>@…`.
func TestRealProcessStderrIsRedacted(t *testing.T) {
	const secret = "ghp_STDERRLEAKSECRET0123456789abcdefghij"
	script := `echo "git clone \"https://madejackson:` + secret +
		`@github.com/aseracorp/cosmos.git\" \"cosmos\"" 1>&2`

	pm := NewProcessManager("stderr-test", "sh", []string{"-c", script}, nil)
	if err := pm.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer pm.Stop()

	var logs string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs = strings.Join(pm.GetLogs(), "\n")
		if strings.Contains(logs, "git clone") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(logs, "git clone") {
		t.Fatalf("stderr never reached the log buffer:\n%s", logs)
	}
	if strings.Contains(logs, secret) {
		t.Errorf("PAT leaked into the debug log buffer:\n%s", logs)
	}
	if !strings.Contains(logs, RedactedPlaceholder) {
		t.Errorf("expected placeholder in logs:\n%s", logs)
	}
	// The line must stay useful for debugging.
	if !strings.Contains(logs, "github.com/aseracorp/cosmos.git") {
		t.Errorf("useful log context lost:\n%s", logs)
	}
}

// With no explicit env entries the child inherits the parent environment
// (cmd.Env stays nil), preserving existing behaviour for every MCP server.
func TestRealProcessInheritsParentEnv(t *testing.T) {
	t.Setenv("PL_TEST_INHERIT", "yes-inherited")

	script := `printf 'VAL=%s\n' "$PL_TEST_INHERIT" > "$OUT"`
	got := spawnChildToFile(t, "inherit-test", script, nil, nil)

	if !strings.Contains(got, "VAL=yes-inherited") {
		t.Errorf("child did not inherit parent env\n--- child output ---\n%s", got)
	}
}

// Configured env entries must win over an inherited variable of the same
// name (exec.Cmd honours the last occurrence).
func TestRealProcessEnvOverridesInherited(t *testing.T) {
	t.Setenv("PL_TEST_SHADOW", "from-parent")

	script := `printf 'VAL=%s\n' "$PL_TEST_SHADOW" > "$OUT"`
	got := spawnChildToFile(t, "shadow-test", script, nil,
		[]string{"PL_TEST_SHADOW=from-config"})

	if !strings.Contains(got, "VAL=from-config") {
		t.Errorf("configured env must override inherited\n--- child output ---\n%s", got)
	}
}
