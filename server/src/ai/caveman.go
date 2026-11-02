package ai

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// CavemanBaseURL is the base URL of the in-container Caveman compression
// proxy. CAVEMAN_URL overrides it with an externally managed instance
// (analogous to LITELLM_URL / HEADROOM_URL).
var CavemanBaseURL = "http://127.0.0.1:8789"

var cavemanProcess *exec.Cmd
var cavemanMu sync.Mutex
var cavemanReady atomic.Bool
var cavemanShuttingDown bool

// InitCaveman starts the in-container Caveman proxy, or adopts a healthy
// instance already bound to the port.
//
// Caveman makes chat models "say less and read less" (Apache-2.0). It is a
// base-URL-swap reverse proxy: the Go server targets its OpenAI-compatible
// mounts at /compat/<name>/v1/chat/completions and Caveman forwards to that
// upstream. Per-model enablement comes from model_info.caveman in
// litellm_config.yaml; the routing decision lives in ChatCompletionURL.
//
// Headroom and Caveman compose on the SAME model: Caveman compresses the
// turn, then Headroom (cache mode) freezes the compressed prefix
// byte-identically, so llama.cpp/strata/avalon KV prompt-cache reuse still
// hits on top of a smaller prompt (see ChatCompletionURL).
//
// This is never fatal: if Caveman cannot start (or an external CAVEMAN_URL
// is unhealthy), the model transparently falls back to the direct LiteLLM
// path — routing, and therefore prompt caching, survives any Caveman outage.
func InitCaveman() error {
	if url := os.Getenv("CAVEMAN_URL"); url != "" {
		CavemanBaseURL = url
		utils.Log("[Caveman] Using external proxy at %s", CavemanBaseURL)
		if err := waitForCavemanHealth(30 * time.Second); err != nil {
			utils.Error("[Caveman] external proxy failed health check; caveman models will fall back to direct LiteLLM", err)
			return err
		}
		cavemanReady.Store(true)
		return nil
	}

	if url := os.Getenv("LITELLM_URL"); url != "" {
		utils.Log("[Caveman] External LiteLLM (%s) detected — in-container Caveman proxy disabled (set CAVEMAN_URL to enable)", url)
		return nil
	}

	utils.Log("[Caveman] Starting local proxy on port 8789 (upstream %s)...", LiteLLMBaseURL)

	if probeCavemanHealth(2*time.Second) == nil {
		utils.Log("[Caveman] Adopting existing healthy proxy at %s", CavemanBaseURL)
		cavemanReady.Store(true)
		return nil
	}

	if err := startCavemanProcess(); err != nil {
		return fmt.Errorf("failed to start Caveman: %w", err)
	}

	if err := waitForCavemanHealth(30 * time.Second); err != nil {
		ShutdownCaveman()
		return fmt.Errorf("Caveman failed health check: %w", err)
	}

	cavemanReady.Store(true)
	utils.Log("[Caveman] Proxy is ready at %s (mode=compress, upstream=%s)", CavemanBaseURL, LiteLLMBaseURL)

	go watchCavemanProcess()

	return nil
}

// ShutdownCaveman stops the local Caveman process if one was started.
func ShutdownCaveman() {
	cavemanMu.Lock()
	defer cavemanMu.Unlock()

	cavemanReady.Store(false)
	cavemanShuttingDown = true

	if cavemanProcess != nil && cavemanProcess.Process != nil {
		utils.Log("[Caveman] Shutting down proxy...")
		syscall.Kill(-cavemanProcess.Process.Pid, syscall.SIGTERM)
		cavemanProcess.Wait()
		cavemanProcess = nil
	}
}

// CavemanReady reports whether the Caveman proxy is initialized and healthy.
func CavemanReady() bool {
	return cavemanReady.Load()
}

// ChatCompletionURL returns the base URL (with any proxy mount prefix) that
// chat-completion requests for the given model should be sent to, honoring
// the per-model model_info.headroom / model_info.caveman flags from
// litellm_config.yaml.
//
// Ordering when both are enabled routes plurality -> Caveman (compress) ->
// Headroom (cache) -> LiteLLM. Caveman's compat mounts are wired in its
// caveman.yaml: "headroom" upstreams Headroom (chained, so its frozen
// compressed prefix keeps the KV cache hot), "litellm" upstreams LiteLLM
// directly. A proxy that is unhealthy is skipped, degrading stepwise to
// direct LiteLLM.
func ChatCompletionURL(model utils.Model) string {
	return ChatCompletionURLForName(model.Name)
}

// ChatCompletionURLForName is ChatCompletionURL for call sites that only
// carry a model name string.
//
// Deduplication ("no double optimization"): a model may enable BOTH Headroom
// and Caveman, but only ONE proxy owns input-content compression per model.
// Caveman's read_less stone is the structured-content compressor (JSON, logs,
// CSV, YAML, test output) and wins ownership when it is on; otherwise Headroom
// owns content. The losing proxy is NOT chained for that model, so a payload
// category is never optimized twice. (Headroom's cache mode freezes prior
// turns; Caveman's PrefixStabilizer keeps the KV prefix stable in its place.)
func ChatCompletionURLForName(name string) string {
	contentOwner, e := optimizationOwners(name)
	switch contentOwner {
	case ownerCaveman:
		if e.Caveman.Enabled && cavemanReady.Load() {
			return CavemanBaseURL + "/compat/litellm"
		}
	case ownerHeadroom:
		if e.Headroom.Enabled && headroomReady.Load() {
			return HeadroomBaseURL
		}
	default:
		// no content owner -> direct LiteLLM, byte-for-byte untouched
	}
	return LiteLLMBaseURL
}

// contentOwner names which proxy is the single content-compression owner for a
// model. Zero value (ownerNone) means neither is enabled for content.
type contentOwner int

const (
	ownerNone contentOwner = iota
	ownerCaveman
	ownerHeadroom
)

// optimizationOwners resolves the deduplicated per-model optimization plan.
// Caveman wins content ownership whenever it is enabled with the read_less
// stone on (caveman's structured-content compressor measurably beats headroom
// on JSON/log/CSV/YAML/test output per its own benchmarks); Headroom owns
// content for models that enable headroom without caveman read_less.
func optimizationOwners(name string) (contentOwner, ModelEntry) {
	entry, _ := Models.Get(name)

	cavemanOwns := entry.Caveman.Enabled && entry.Caveman.Stones.ReadLess
	if cavemanOwns {
		return ownerCaveman, entry
	}
	if entry.Headroom.Enabled {
		return ownerHeadroom, entry
	}
	return ownerNone, entry
}

// ApplyChatOptimizationHeaders sets any per-request optimization headers for
// the given model on an outbound chat-completion request. Called right before
// the POST to the base URL returned by ChatCompletionURLForName.
//
//   - Headroom: `x-headroom-keep-last-turns` (trims history before optimizing)
//     when the model sets headroom.keep_last_turns; 0 = keep everything.
func ApplyChatOptimizationHeaders(name string, req *http.Request) {
	owner, e := optimizationOwners(name)
	if owner == ownerHeadroom && e.Headroom.KeepLastTurns > 0 {
		req.Header.Set("x-headroom-keep-last-turns", strconv.Itoa(e.Headroom.KeepLastTurns))
	}
	// Caveman's per-stone toggles are honored via its own caveman.yaml /
	// engine (read_less = compress mode). Nothing to add per request today.
}

func startCavemanProcess() error {
	cavemanMu.Lock()
	defer cavemanMu.Unlock()

	if cavemanProcess != nil && cavemanProcess.Process != nil {
		utils.Log("[Caveman] Process %d still running — not starting a duplicate", cavemanProcess.Process.Pid)
		return nil
	}

	bin := findCavemanBin()
	if bin == "" {
		return fmt.Errorf("caveman-proxy binary not found")
	}

	home := cavemanDataDir()
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("cannot create Caveman home %s: %w", home, err)
	}

	cfgPath := filepath.Join(home, "caveman.yaml")
	if err := writeCavemanConfig(cfgPath); err != nil {
		return err
	}

	cmd := exec.Command(bin, "serve")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CAVEMAN_HOME="+home,
		"CAVEMAN_CONFIG="+cfgPath,
		// The in-container proxy binds loopback only; no inbound token needed.
		"CAVEMAN_LISTEN=127.0.0.1:8789",
		"CAVEMAN_MODE=compress",
		// Cache warming is a spend-optimizer for provider-native caching; the
		// Plurality stack already sends explicit cache_prompt/n_keep hints, so
		// keep Caveman from making extra max_tokens=0 upstream calls.
		"CAVEMAN_CACHE_WARM=off",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Caveman process: %w", err)
	}

	cavemanProcess = cmd
	return nil
}

// writeCavemanConfig writes caveman.yaml with the two named OpenAI-compatible
// upstreams the Go router targets (see ChatCompletionURLForName): "litellm"
// and "headroom" (the local proxies on this host).
func writeCavemanConfig(path string) error {
	cfg := fmt.Sprintf(`# Generated by Plurality at boot — do not edit.
compat:
  litellm:
    base_url: %s
  headroom:
    base_url: %s
mode: compress
listen: 127.0.0.1:8789
`, LiteLLMBaseURL, HeadroomBaseURL)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		return fmt.Errorf("failed to write Caveman config: %w", err)
	}
	return nil
}

func watchCavemanProcess() {
	backoff := time.Second

	for {
		cavemanMu.Lock()
		if cavemanShuttingDown {
			cavemanMu.Unlock()
			return
		}
		cmd := cavemanProcess
		cavemanMu.Unlock()

		if cmd == nil {
			return
		}

		err := cmd.Wait()
		cavemanMu.Lock()
		if cavemanProcess == cmd {
			cavemanProcess = nil
		}
		wasShuttingDown := cavemanShuttingDown
		cavemanMu.Unlock()

		if err == nil {
			utils.Log("[Caveman] Proxy exited cleanly")
			cavemanReady.Store(false)
			return
		}

		cavemanReady.Store(false)
		if wasShuttingDown {
			utils.Log("[Caveman] Proxy terminated during shutdown — not restarting")
			return
		}

		utils.Error("[Caveman] Proxy crashed, restarting in %s...", err, backoff)
		time.Sleep(backoff)

		if err := startCavemanProcess(); err != nil {
			utils.Error("[Caveman] Failed to restart proxy", err)
		}

		if waitForCavemanHealth(15*time.Second) == nil {
			cavemanReady.Store(true)
			utils.Log("[Caveman] Proxy restarted successfully")
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			utils.Error("[Caveman] Proxy did not become healthy; next restart attempt in %s", err, backoff)
		}
	}
}

func probeCavemanHealth(timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(CavemanBaseURL + "/health/ready")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}
	return nil
}

func waitForCavemanHealth(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probeCavemanHealth(2*time.Second) == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Caveman proxy did not become healthy within %s", timeout)
}

// findCavemanBin resolves the caveman-proxy binary. The Docker image installs
// it at /app/caveman/caveman-proxy from the pinned Apache-2.0 release asset;
// local/dev installs may put it on PATH.
func findCavemanBin() string {
	exeDir := filepath.Dir(os.Args[0])
	candidates := []string{
		"caveman/caveman-proxy",
		filepath.Join(exeDir, "caveman", "caveman-proxy"),
		filepath.Join(exeDir, "caveman-proxy"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	if p, err := exec.LookPath("caveman-proxy"); err == nil {
		return p
	}
	return ""
}

// cavemanDataDir returns where Caveman persists its SQLite spend store and
// generated caveman.yaml, preferring DATA_DIR (the container volume) so a
// recreate does not reset its learned stats.
func cavemanDataDir() string {
	if d := os.Getenv("DATA_DIR"); d != "" {
		return filepath.Join(d, "caveman")
	}
	return filepath.Join(filepath.Dir(os.Args[0]), "data", "caveman")
}

// ---- per-model compression factor (rolling) ----
//
// The preemptive eco guard in SendChatCompletion estimates tokens from the
// pre-proxy marshalled request. For models routed through Headroom/Caveman
// the provider actually sees fewer tokens, so the guard would fire too early
// and force compaction it does not need. We keep a small rolling average of
// each model's observed compression ratio (pre-proxy estimate / provider
// prompt_tokens) and discount the guard estimate with it.

var (
	compressFactorMu    sync.Mutex
	compressFactorByMod = map[string]float64{} // model name -> rolling ratio
)

// RecordCompression feeds one observed turn's ratio (estUncompressed /
// providerPromptTokens) into the rolling per-model factor. Ignored unless
// the ratio is meaningfully >1 (a proxy is actually compressing) and bounded
// to a sane range (1.0..50.0) so a single bad sample can't skew the guard.
func RecordCompression(modelName string, estUncompressed, providerPromptTokens int) {
	if modelName == "" || estUncompressed <= 0 || providerPromptTokens <= 0 {
		return
	}
	ratio := float64(estUncompressed) / float64(providerPromptTokens)
	if ratio < 1.01 || ratio > 50 {
		return
	}
	compressFactorMu.Lock()
	defer compressFactorMu.Unlock()
	if prev, ok := compressFactorByMod[modelName]; ok && prev > 0 {
		// EMA with alpha=0.3: recent turns matter more, but a model that
		// stops routing through a proxy recovers back toward 1.0 gradually.
		compressFactorByMod[modelName] = 0.7*prev + 0.3*ratio
	} else {
		compressFactorByMod[modelName] = ratio
	}
}

// CompressionFactor returns the rolling compression factor for a model
// (>1 = a Headroom/Caveman proxy is actively reducing prompt tokens).
// Returns 1.0 when nothing has been observed. The factor is only used to
// relax the eco guard's pre-compression estimate, never to loosen other
// window math, so a 1.0 fallback is the conservative default.
func CompressionFactor(modelName string) float64 {
	compressFactorMu.Lock()
	defer compressFactorMu.Unlock()
	if f, ok := compressFactorByMod[modelName]; ok && f > 1.0 {
		return f
	}
	return 1.0
}
