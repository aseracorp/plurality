package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const stateCookie = "plurality_oidc_state"

type oidcRuntime struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

var (
	oidcInst  *oidcRuntime
	oidcInitE error
)

func setupOIDC(ctx context.Context) (*oidcRuntime, error) {
	if !OpenIDEnabled() {
		return nil, errors.New("OpenID is not configured")
	}
	c := GetConfig().OpenID
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: c.ClientID})
	oauthCfg := &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURL:  c.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return &oidcRuntime{provider: provider, verifier: verifier, oauth: oauthCfg}, nil
}

func getOIDC(ctx context.Context) (*oidcRuntime, error) {
	if oidcInst != nil || oidcInitE != nil {
		return oidcInst, oidcInitE
	}
	oidcInst, oidcInitE = setupOIDC(ctx)
	return oidcInst, oidcInitE
}

// HandleOIDCStart redirects the browser to the provider's auth endpoint.
func HandleOIDCStart(w http.ResponseWriter, r *http.Request) {
	rt, err := getOIDC(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(15 * time.Minute),
	})
	http.Redirect(w, r, rt.oauth.AuthCodeURL(state), http.StatusFound)
}

// HandleOIDCCallback exchanges the auth code, validates the ID token, checks
// the allowlist, and issues a local JWT. The JWT is rendered in a tiny HTML
// page that posts the token back via window.opener.postMessage so the Flutter
// client can pick it up.
func HandleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	rt, err := getOIDC(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	stateCk, err := r.Cookie(stateCookie)
	if err != nil || stateCk.Value == "" || stateCk.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	tok, err := rt.oauth.Exchange(r.Context(), code)
	if err != nil {
		http.Error(w, "code exchange failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusBadRequest)
		return
	}
	username, status, err := exchangeIDTokenForUsername(r.Context(), rt, rawID)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	jwtTok, err := IssueToken(username)
	if err != nil {
		http.Error(w, "token issue failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(callbackHTML(jwtTok, username)))
}

// HandleOIDCExchange takes an ID token obtained client-side (e.g. by the
// Flutter client's openid_client flow) and returns a Plurality JWT. The same
// verifier and allowlist rules used by the server-side callback apply.
func HandleOIDCExchange(w http.ResponseWriter, r *http.Request) {
	rt, err := getOIDC(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IDToken == "" {
		http.Error(w, "id_token required", http.StatusBadRequest)
		return
	}
	username, status, err := exchangeIDTokenForUsername(r.Context(), rt, req.IDToken)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	jwtTok, err := IssueToken(username)
	if err != nil {
		http.Error(w, "token issue failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": jwtTok, "username": username})
}

// exchangeIDTokenForUsername verifies the ID token, runs the allowlist check,
// and returns the canonical username. On failure it returns an HTTP status
// suitable for surfacing to the client.
func exchangeIDTokenForUsername(ctx context.Context, rt *oidcRuntime, rawID string) (string, int, error) {
	idTok, err := rt.verifier.Verify(ctx, rawID)
	if err != nil {
		return "", http.StatusUnauthorized, errors.New("id_token verify failed: " + err.Error())
	}
	var claims struct {
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return "", http.StatusBadRequest, errors.New("claims parse failed")
	}
	if claims.Email == "" {
		return "", http.StatusBadRequest, errors.New("provider did not return email")
	}
	if !AllowlistMatch(claims.Email) {
		utils.Log("[Auth] OIDC: email %s rejected by allowlist", claims.Email)
		return "", http.StatusForbidden, errors.New("Access denied: email not in allowlist")
	}
	username := claims.PreferredUsername
	if username == "" {
		username = claims.Email
	}
	return sanitizeUsername(username), http.StatusOK, nil
}

// sanitizeUsername strips characters unsafe for use as a directory name.
func sanitizeUsername(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		s = "openid_user"
	}
	return s
}

func callbackHTML(token, username string) string {
	payload, _ := json.Marshal(map[string]string{"token": token, "username": username})
	// Defensive: prevent the closing-script trick from breaking out of the script tag.
	safe := strings.ReplaceAll(string(payload), "</", "<\\/")
	return `<!doctype html><html><body><script>
const data = ` + safe + `;
try {
  if (window.opener) {
    window.opener.postMessage({plurality_oidc: data}, "*");
    document.body.innerText = "Login complete. You can close this window.";
    setTimeout(() => window.close(), 1000);
  } else {
    document.body.innerText = "Login complete: " + data.username;
  }
} catch(e) { document.body.innerText = String(e); }
</script></body></html>`
}
