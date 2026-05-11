package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/utils"
)

const (
	// TokenHeader is the case-insensitive header name callers can use as an
	// alternative to the ?token= query param.
	TokenHeader = "X-WEBHOOK-TOKEN"

	// maxBodyBytes caps the trigger request body. 1 MiB is plenty for typical
	// webhook providers (GitHub events run ~100 KB at most).
	maxBodyBytes = 1 << 20
)

func userIDFromCtx(r *http.Request) (string, bool) {
	userID, ok := r.Context().Value("userID").(string)
	return userID, ok
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// buildURL composes the trigger URL the caller is told to use. We honour
// X-Forwarded-Proto / Host when present (typical behind a reverse proxy);
// otherwise fall back to r.Host and the request scheme.
func buildURL(r *http.Request, id, token string) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host + "/webhook/" + id + "?token=" + token
}

// --- Authenticated CRUD ---

type createReq struct {
	Prompt         string `json:"prompt"`
	PresetID       string `json:"preset_id"`
	ConversationID string `json:"conversation_id,omitempty"`
}

func API_ListWebhooks(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	list, err := LoadAll(userID)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]PublicWebhook, 0, len(list))
	for _, item := range list {
		out = append(out, item.Public())
	}
	writeJSON(w, http.StatusOK, out)
}

func API_CreateWebhook(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	var body createReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}
	hook, token, err := Create(userID, body.Prompt, body.PresetID, body.ConversationID)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, WebhookCreateResponse{
		PublicWebhook: hook.Public(),
		URL:           buildURL(r, hook.ID, token),
		Token:         token,
	})
}

func API_UpdateWebhook(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	var patch WebhookUpdate
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}
	hook, err := Update(userID, id, patch)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, hook.Public())
}

func API_DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	if err := Delete(userID, id); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func API_RotateWebhookToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	hook, token, err := RotateToken(userID, id)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, WebhookCreateResponse{
		PublicWebhook: hook.Public(),
		URL:           buildURL(r, hook.ID, token),
		Token:         token,
	})
}

// --- Public trigger endpoint (UNAUTHENTICATED — token is the auth) ---

// API_TriggerWebhook handles inbound trigger requests. Reads the token
// from EITHER ?token=... or the X-WEBHOOK-TOKEN header (header wins if
// both), strips it from the payload, then dispatches via Trigger().
//
// Returns:
//   - 202 on success (LLM run started asynchronously)
//   - 401 for any auth failure (unknown ID, disabled, bad/missing token)
//   - 413 if the body exceeds maxBodyBytes
//   - 429 if rate-limited (per-IP-per-webhook or per-IP global). Per-webhook
//     overruns include a Retry-After header; an IP that has crossed the
//     global threshold gets 429 permanently until the server restarts.
func API_TriggerWebhook(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	// Rate-limit FIRST — before body read, before disk hit, before token
	// compare. A flood of bad requests from one IP should cost us nothing.
	ip := clientIP(r)
	allowed, retryAfter, _ := CheckRate(ip, id)
	if !allowed {
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	// Token: header takes priority over query so a caller can override a
	// URL token by passing a header. Both are stripped before payload
	// formatting so the secret never reaches the LLM context.
	token := r.URL.Query().Get("token")
	if h := r.Header.Get(TokenHeader); h != "" {
		token = h
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesReader sets a *http.MaxBytesError; any read error
		// here is best surfaced as 413 (most cases are oversize bodies).
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	// Strip token from query/headers before building the payload.
	query := r.URL.Query()
	query.Del("token")
	headers := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		if strings.EqualFold(k, TokenHeader) {
			continue
		}
		headers[k] = v
	}

	payload := TriggerPayload{
		Method:  r.Method,
		Query:   query,
		Headers: headers,
		Body:    string(body),
	}

	if err := Trigger(id, token, payload); err != nil {
		if errors.Is(err, errInvalid) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		utils.Error("[Webhook] trigger", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
