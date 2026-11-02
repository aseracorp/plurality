package ai

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// HeadroomBaseURL is the base URL of the Headroom optimization proxy.
// Defaults to the in-container proxy on 127.0.0.1:8787; HEADROOM_URL
// overrides it with an externally managed instance (analogous to
// LITELLM_URL for LiteLLM).
var HeadroomBaseURL = "http://127.0.0.1:8787"

var headroomProcess *exec.Cmd
var headroomMu sync.Mutex
var headroomReady atomic.Bool
var headroomShuttingDown bool

// InitHeadroom starts the in-container Headroom optimization proxy in front
// of the LOCAL LiteLLM proxy, or adopts an existing healthy one on the port.
//
// Headroom sits between the Go server and LiteLLM for chat completions ONLY
// on models that opt in per model_info.headroom: true in litellm_config.yaml
// (see ChatBaseURL). It runs with HEADROOM_MODE=cache, which freezes prior
// turns byte-identically so llama.cpp/strata/avalon KV prompt-cache reuse
// keeps working (the stable prefix is exactly what cache mode preserves).
//
// This is never fatal: if Headroom cannot start (or an external HEADROOM_URL
// is unhealthy), opted-in models transparently fall back to the direct
// LiteLLM path, which still forwards cache_prompt/n_keep — so prompt caching
// survives even a complete Headroom outage.
func InitHeadroom() error {
	if url := os.Getenv("HEADROOM_URL"); url != "" {
		HeadroomBaseURL = url
		utils.Log("[Headroom] Using external proxy at %s", HeadroomBaseURL)
		if err := waitForHeadroomHealth(30 * time.Second); err != nil {
			utils.Error("[Headroom] external proxy failed health check; opted-in models will fall back to direct LiteLLM", err)
			return err
		}
		headroomReady.Store(true)
		return nil
	}

	// The in-container Headroom front-ends the EMBEDDED local LiteLLM proxy.
	// When LiteLLM itself is external (LITELLM_URL set), there is no local
	// upstream to front and we do not spawn anything — set HEADROOM_URL to
	// opt into an external Headroom in that topology.
	if url := os.Getenv("LITELLM_URL"); url != "" {
		utils.Log("[Headroom] External LiteLLM (%s) detected — in-container Headroom proxy disabled (set HEADROOM_URL to enable)", url)
		return nil
	}

	utils.Log("[Headroom] Starting local proxy on port 8787 (upstream %s)...", LiteLLMBaseURL)

	if probeHeadroomHealth(2*time.Second) == nil {
		utils.Log("[Headroom] Adopting existing healthy proxy at %s", HeadroomBaseURL)
		headroomReady.Store(true)
		return nil
	}

	if err := startHeadroomProcess(); err != nil {
		return fmt.Errorf("failed to start Headroom: %w", err)
	}

	if err := waitForHeadroomHealth(45 * time.Second); err != nil {
		ShutdownHeadroom()
		return fmt.Errorf("Headroom failed health check: %w", err)
	}

	headroomReady.Store(true)
	utils.Log("[Headroom] Proxy is ready at %s (mode=cache, upstream=%s)", HeadroomBaseURL, LiteLLMBaseURL)

	// Watch for crashes and auto-restart, mirroring the LiteLLM watcher.
	go watchHeadroomProcess()

	return nil
}

// ShutdownHeadroom stops the local Headroom process if one was started.
func ShutdownHeadroom() {
	headroomMu.Lock()
	defer headroomMu.Unlock()

	headroomReady.Store(false)
	headroomShuttingDown = true

	if headroomProcess != nil && headroomProcess.Process != nil {
		utils.Log("[Headroom] Shutting down proxy...")
		// Own process group (Setpgid in startHeadroomProcess) so SIGTERM
		// reaches the whole tree and no uvicorn worker is orphaned on
		// :8787 across a fast exit.
		syscall.Kill(-headroomProcess.Process.Pid, syscall.SIGTERM)
		headroomProcess.Wait()
		headroomProcess = nil
	}
}

// HeadroomReady returns whether the Headroom proxy is initialized and healthy.
func HeadroomReady() bool {
	return headroomReady.Load()
}

func startHeadroomProcess() error {
	headroomMu.Lock()
	defer headroomMu.Unlock()

	// Refuse to double-start (mirrors the LiteLLM guard): exactly one proxy
	// must hold :8787.
	if headroomProcess != nil && headroomProcess.Process != nil {
		utils.Log("[Headroom] Process %d still running — not starting a duplicate", headroomProcess.Process.Pid)
		return nil
	}

	bin := findHeadroomBin()
	if bin == "" {
		return fmt.Errorf("headroom binary not found")
	}

	cmd := exec.Command(bin, "proxy", "--host", "127.0.0.1", "--port", "8787")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		// Upstream is the embedded LiteLLM proxy on this host.
		"OPENAI_TARGET_API_URL="+LiteLLMBaseURL+"/v1",
		// Cache mode freezes prior turns byte-identically: the llama.cpp
		// KV-cache prefix (system prompt + unchanged history) is forwarded
		// untouched, so cache_prompt/n_keep hits keep working. This is what
		// the earlier sidecar attempt broke — token mode rewrites history.
		"HEADROOM_MODE=cache",
		// Do not phone home or dump usage telemetry from this deployment.
		"HEADROOM_TELEMETRY=off",
		"HEADROOM_BEACON=off",
		// Persist downloaded Kompress/ModernBERT artifacts under the data
		// volume so a container recreate does not re-download them.
		"HEADROOM_BINARIES_CACHE="+headroomDataDir(),
	)
	// Own process group so ShutdownHeadroom can SIGTERM the whole group and
	// never orphan a proxy that keeps :8787 bound after a fast exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Headroom process: %w", err)
	}

	headroomProcess = cmd
	return nil
}

func watchHeadroomProcess() {
	backoff := time.Second

	for {
		headroomMu.Lock()
		if headroomShuttingDown {
			headroomMu.Unlock()
			return
		}
		cmd := headroomProcess
		headroomMu.Unlock()

		if cmd == nil {
			return
		}

		err := cmd.Wait()
		headroomMu.Lock()
		if headroomProcess == cmd {
			headroomProcess = nil
		}
		wasShuttingDown := headroomShuttingDown
		headroomMu.Unlock()

		if err == nil {
			utils.Log("[Headroom] Proxy exited cleanly")
			headroomReady.Store(false)
			return
		}

		headroomReady.Store(false)
		if wasShuttingDown {
			utils.Log("[Headroom] Proxy terminated during shutdown — not restarting")
			return
		}

		utils.Error("[Headroom] Proxy crashed, restarting in %s...", err, backoff)
		time.Sleep(backoff)

		if err := startHeadroomProcess(); err != nil {
			utils.Error("[Headroom] Failed to restart proxy", err)
		}

		if waitForHeadroomHealth(15*time.Second) == nil {
			headroomReady.Store(true)
			utils.Log("[Headroom] Proxy restarted successfully")
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			utils.Error("[Headroom] Proxy did not become healthy; next restart attempt in %s", err, backoff)
		}
	}
}

// probeHeadroomHealth performs a single health request against the Headroom
// proxy (GET /health, same contract as LiteLLM's).
func probeHeadroomHealth(timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(HeadroomBaseURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}

func waitForHeadroomHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if probeHeadroomHealth(2*time.Second) == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("Headroom proxy did not become healthy within %s", timeout)
}

// findHeadroomBin resolves the headroom console script. The Docker image
// installs it into its own venv at /app/headroom/headroom_venv (deliberately
// separate from the LiteLLM venv so their fastapi/uvicorn/openai pins can
// never conflict). Local installs may put it on PATH.
func findHeadroomBin() string {
	exeDir := filepath.Dir(os.Args[0])
	candidates := []string{
		"headroom/headroom_venv/bin/headroom",
		filepath.Join(exeDir, "headroom", "headroom_venv", "bin", "headroom"),
		filepath.Join(exeDir, "headroom", "bin", "headroom"),
		"headroom/bin/headroom",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}

	// Fall back to PATH (local/dev installs or headless box tooling).
	if p, err := exec.LookPath("headroom"); err == nil {
		return p
	}
	return ""
}

// headroomDataDir returns the directory under which Headroom should persist
// its downloaded model artifacts, preferring the configured DATA_DIR (the
// container's persistent volume) so a recreate does not re-download them.
func headroomDataDir() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return filepath.Join(d, "headroom")
	}
	return filepath.Join(filepath.Dir(os.Args[0]), "data", "headroom")
}
