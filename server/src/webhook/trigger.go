package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/azukaar/plurality/src/jobs"
	"github.com/azukaar/plurality/src/utils"
)

// errInvalid is the single error returned for every authentication-related
// failure (unknown ID, disabled, bad token). Same response prevents an
// attacker from distinguishing "this ID exists but the token is wrong"
// from "this ID doesn't exist".
var errInvalid = errors.New("invalid")

// Trigger validates the presented token, kicks off the LLM run, and
// returns. Runs the actual work in a goroutine — callers should respond
// to the HTTP request immediately (202 Accepted).
func Trigger(webhookID, providedToken string, payload TriggerPayload) error {
	userID, ok := lookupOwner(webhookID)
	if !ok {
		return errInvalid
	}
	w, err := FindByID(userID, webhookID)
	if err != nil {
		return errInvalid
	}
	if !w.Enabled {
		return errInvalid
	}
	if !CheckToken(providedToken, w.TokenHash) {
		return errInvalid
	}

	go func() {
		if err := recordTriggered(userID, w.ID); err != nil {
			utils.Error("[Webhook] recordTriggered", err)
		}
	}()

	go jobs.RunPrompt(context.Background(), userID, jobs.RunOptions{
		TitlePrefix:  "Webhook",
		Prompt:       w.Prompt,
		PresetID:     w.PresetID,
		ExtraContext: formatPayload(payload),
		TriggerType:  "webhook",
		TriggerID:    w.ID,
	})
	return nil
}

// formatPayload renders the trigger request as text the LLM can read.
// Keeps things readable rather than dumping a JSON blob.
func formatPayload(p TriggerPayload) string {
	var b strings.Builder
	b.WriteString("Webhook trigger:\n")
	b.WriteString("Method: " + p.Method + "\n")

	if len(p.Query) > 0 {
		b.WriteString("Query: " + url.Values(p.Query).Encode() + "\n")
	}

	if len(p.Headers) > 0 {
		keys := make([]string, 0, len(p.Headers))
		for k := range p.Headers {
			if exposeHeader(k) {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			b.WriteString("Headers:\n")
			for _, k := range keys {
				b.WriteString(fmt.Sprintf("  %s: %s\n", k, strings.Join(p.Headers[k], ", ")))
			}
		}
	}

	if p.Body != "" {
		b.WriteString("Body:\n")
		b.WriteString(p.Body)
		if !strings.HasSuffix(p.Body, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// exposeHeader filters out hop-by-hop and noisy infrastructure headers
// before surfacing the request to the LLM. The auth token header is
// already stripped before formatPayload is called.
func exposeHeader(name string) bool {
	switch strings.ToLower(name) {
	case "user-agent", "content-type", "content-length", "accept",
		"x-forwarded-for", "x-real-ip":
		return true
	}
	// Common provider conventions: X-GitHub-*, X-Hub-*, X-Stripe-*, etc.
	return strings.HasPrefix(strings.ToLower(name), "x-")
}
