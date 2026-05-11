package webhook

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

// PerWebhookPerMinute caps how many times a single client IP can hit any
// one webhook ID in 60s. Counts ALL requests, including bad-token ones,
// so attackers probing the URL are throttled just like legitimate clients.
//
// PerClientPerMinute is the global ceiling for a single IP across every
// webhook. Crossing this trips a permanent (until process restart)
// in-memory block — subsequent requests short-circuit immediately.
//
// Both are var (not const) so Init can override them from data/config.json.
var (
	PerWebhookPerMinute = 10
	PerClientPerMinute  = 200
)

// counter is a fixed-window request counter. windowStart is the moment the
// current 60s window opened; count is the number of requests inside it. The
// window resets the next time bump() is called > 60s after windowStart.
type counter struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
}

var (
	// counters holds *counter values keyed by either "{ip}" (global per-IP
	// window) or "{ip}|{webhookID}" (per-webhook window).
	counters sync.Map

	// blocked holds IPs that have crossed PerClientPerMinute. No TTL — once
	// blocked, an IP stays blocked until the process restarts. By design.
	blocked sync.Map
)

// bump increments the named counter and returns the post-increment count.
// Resets to 1 when the existing window is already over a minute old.
func bump(key string) int {
	v, _ := counters.LoadOrStore(key, &counter{})
	c := v.(*counter)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.windowStart) > time.Minute {
		c.windowStart = now
		c.count = 1
		return 1
	}
	c.count++
	return c.count
}

// retryAfterFor returns how long until the current window for key resets.
// 1s minimum (clients shouldn't retry instantly even if we're at the edge).
func retryAfterFor(key string) time.Duration {
	v, ok := counters.Load(key)
	if !ok {
		return time.Second
	}
	c := v.(*counter)
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := time.Minute - time.Since(c.windowStart)
	if remaining < time.Second {
		return time.Second
	}
	return remaining
}

// CheckRate is called by the trigger handler before any auth or body work.
// Returns:
//   - allowed=true: continue with the request.
//   - allowed=false, permanentBlock=true: this IP has been blocked. No
//     Retry-After is meaningful; just respond 429.
//   - allowed=false, permanentBlock=false: per-webhook window is exhausted.
//     retryAfter tells the caller how long until the window resets.
func CheckRate(ip, webhookID string) (allowed bool, retryAfter time.Duration, permanentBlock bool) {
	if _, ok := blocked.Load(ip); ok {
		return false, 0, true
	}

	if n := bump(ip); n > PerClientPerMinute {
		blocked.Store(ip, struct{}{})
		utils.Log("[Webhook] blocking client %s after %d requests in <60s", ip, n)
		return false, 0, true
	}

	perWebhookKey := ip + "|" + webhookID
	if n := bump(perWebhookKey); n > PerWebhookPerMinute {
		return false, retryAfterFor(perWebhookKey), false
	}

	return true, 0, false
}

// clientIP best-effort extracts the originating client IP, honouring common
// reverse-proxy headers. The trigger endpoint is internet-facing so this is
// the only honest way to identify a client when running behind nginx /
// cloudflare / etc.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// X-Forwarded-For is a comma-separated chain — the first entry is
		// the originating client.
		if i := strings.Index(fwd, ","); i > 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return strings.TrimSpace(real)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// startCleanupLoop sweeps counters whose window is so old they can't
// affect any future CheckRate result. Called from Init().
// blocked is intentionally NOT swept — permanent by design.
func startCleanupLoop() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-2 * time.Minute)
			counters.Range(func(k, v any) bool {
				c := v.(*counter)
				c.mu.Lock()
				stale := c.windowStart.Before(cutoff)
				c.mu.Unlock()
				if stale {
					counters.Delete(k)
				}
				return true
			})
		}
	}()
}
