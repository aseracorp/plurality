package ai

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// LiteLLMBaseURL is the base URL for the LiteLLM proxy.
// Set via LITELLM_URL env var (Docker) or defaults to http://127.0.0.1:4000.
var LiteLLMBaseURL = "http://127.0.0.1:4000"

var litellmProcess *exec.Cmd
var litellmMu sync.Mutex
var litellmReady atomic.Bool

// InitLiteLLM initializes the LiteLLM proxy connection.
// If LITELLM_URL is set (Docker/external), it just uses that URL.
// Otherwise, it starts a local Python process.
func InitLiteLLM() error {
	if url := os.Getenv("LITELLM_URL"); url != "" {
		LiteLLMBaseURL = url
		utils.Log("[LiteLLM] Using external proxy at %s", LiteLLMBaseURL)
		if err := waitForHealth(30 * time.Second); err != nil {
			return err
		}
		litellmReady.Store(true)
		return nil
	}

	utils.Log("[LiteLLM] Starting local proxy on port 4000...")
	if err := startLocalProcess(); err != nil {
		return fmt.Errorf("failed to start LiteLLM: %w", err)
	}

	if err := waitForHealth(30 * time.Second); err != nil {
		ShutdownLiteLLM()
		return fmt.Errorf("LiteLLM failed health check: %w", err)
	}

	litellmReady.Store(true)
	utils.Log("[LiteLLM] Proxy is ready at %s", LiteLLMBaseURL)

	// Watch for crashes and auto-restart
	go watchProcess()

	return nil
}

// ShutdownLiteLLM stops the local LiteLLM process if one was started.
func ShutdownLiteLLM() {
	litellmMu.Lock()
	defer litellmMu.Unlock()

	litellmReady.Store(false)

	if litellmProcess != nil && litellmProcess.Process != nil {
		utils.Log("[LiteLLM] Shutting down proxy...")
		litellmProcess.Process.Kill()
		litellmProcess.Wait()
		litellmProcess = nil
	}
}

// LiteLLMReady returns whether the proxy has been initialized and is running.
func LiteLLMReady() bool {
	return litellmReady.Load()
}

func startLocalProcess() error {
	litellmMu.Lock()
	defer litellmMu.Unlock()

	configPath := findConfigPath()
	if configPath == "" {
		return fmt.Errorf("litellm_config.yaml not found")
	}

	proxyScript := findProxyScript()
	if proxyScript == "" {
		return fmt.Errorf("litellm_proxy.py not found")
	}

	pythonBin := findPythonBin()
	cmd := exec.Command(pythonBin, proxyScript, "--config", configPath, "--port", "4000", "--host", "127.0.0.1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start LiteLLM process: %w", err)
	}

	litellmProcess = cmd
	return nil
}

func waitForHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		resp, err := client.Get(LiteLLMBaseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("LiteLLM proxy did not become healthy within %s", timeout)
}

func watchProcess() {
	backoff := time.Second

	for {
		litellmMu.Lock()
		cmd := litellmProcess
		litellmMu.Unlock()

		if cmd == nil {
			return
		}

		err := cmd.Wait()
		if err == nil {
			utils.Log("[LiteLLM] Proxy exited cleanly")
			litellmReady.Store(false)
			return
		}

		litellmReady.Store(false)
		utils.Error("[LiteLLM] Proxy crashed, restarting in %s...", err, backoff)
		time.Sleep(backoff)

		if err := startLocalProcess(); err != nil {
			utils.Error("[LiteLLM] Failed to restart proxy", err)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		if waitForHealth(15*time.Second) == nil {
			litellmReady.Store(true)
			utils.Log("[LiteLLM] Proxy restarted successfully")
		}
		backoff = time.Second
	}
}

func findConfigPath() string {
	exeDir := filepath.Dir(os.Args[0])
	candidates := []string{
		"litellm_config.yaml",
		"litellm/litellm_config.yaml",
		"../litellm_config.yaml",
		filepath.Join(exeDir, "litellm_config.yaml"),
		filepath.Join(exeDir, "litellm", "litellm_config.yaml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func findProxyScript() string {
	exeDir := filepath.Dir(os.Args[0])
	candidates := []string{
		"litellm_proxy.py",
		"litellm/litellm_proxy.py",
		"../litellm_proxy.py",
		filepath.Join(exeDir, "litellm_proxy.py"),
		filepath.Join(exeDir, "litellm", "litellm_proxy.py"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func findPythonBin() string {
	// Check for venv first
	exeDir := filepath.Dir(os.Args[0])
	venvPaths := []string{
		"litellm_venv/bin/python",
		"litellm_venv/Scripts/python.exe",
		"litellm/litellm_venv/bin/python",
		"litellm/litellm_venv/Scripts/python.exe",
		"../litellm_venv/bin/python",
		"../litellm_venv/Scripts/python.exe",
		filepath.Join(exeDir, "litellm", "litellm_venv", "bin", "python"),
		filepath.Join(exeDir, "litellm", "litellm_venv", "Scripts", "python.exe"),
	}
	for _, p := range venvPaths {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}

	// Fall back to system python
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	return "python"
}

// liteLLMModelName strips the provider prefix from internal model names
// so they match the LiteLLM config model_name entries.
// e.g. "ChatGPT/gpt-5" -> "gpt-5", "Claude/claude-sonnet-4-6" -> "claude-sonnet-4-6"
func liteLLMModelName(name string) string {
	prefixes := []string{"ChatGPT/", "Claude/", "Gemini/"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return name
}

