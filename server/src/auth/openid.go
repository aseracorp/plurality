package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/azukaar/plurality/src/utils"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidcRuntime holds just what we need to verify ID tokens. The OAuth flow
// (authorization code exchange, client secret, redirect URL) lives entirely in
// the client, which uses PKCE — so the server never needs a secret. The
// provider is kept so we can call the userinfo endpoint for providers (e.g.
// Ory) that return only "sub" in the ID token.
type oidcRuntime struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
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
	return &oidcRuntime{provider: provider, verifier: verifier}, nil
}

func getOIDC(ctx context.Context) (*oidcRuntime, error) {
	if oidcInst != nil || oidcInitE != nil {
		return oidcInst, oidcInitE
	}
	oidcInst, oidcInitE = setupOIDC(ctx)
	return oidcInst, oidcInitE
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
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IDToken == "" {
		http.Error(w, "id_token required", http.StatusBadRequest)
		return
	}
	username, status, err := exchangeIDTokenForUsername(r.Context(), rt, req.IDToken, req.AccessToken)
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

// oidcClaims holds the subset of ID-token claims we care about. Providers
// vary in which name fields they populate, so we read all the common ones.
type oidcClaims struct {
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
	Username          string `json:"username"`
	Nickname          string `json:"nickname"`
	Name              string `json:"name"`
	Sub               string `json:"sub"`
}

// names returns the candidate display names, most-preferred first.
func (c oidcClaims) names() []string {
	return []string{c.PreferredUsername, c.Username, c.Nickname, c.Name}
}

// userName picks the canonical local username from the claims: the first
// non-empty name field, falling back to the email. Returns "" if nothing is
// usable.
func (c oidcClaims) userName() string {
	if n := firstNonEmpty(c.names()); n != "" {
		return n
	}
	return strings.TrimSpace(c.Email)
}

// exchangeIDTokenForUsername verifies the ID token, runs the allowlist check,
// and returns the canonical username. On failure it returns an HTTP status
// suitable for surfacing to the client.
//
// Some providers (e.g. Ory/Hydra) return only "sub" in the ID token and expose
// email/profile at the userinfo endpoint. When the verified ID token has no
// email and an access token is supplied, we fetch userinfo to fill the gaps,
// binding the result to the ID token via the "sub" claim.
func exchangeIDTokenForUsername(ctx context.Context, rt *oidcRuntime, rawID, accessToken string) (string, int, error) {
	idTok, err := rt.verifier.Verify(ctx, rawID)
	if err != nil {
		return "", http.StatusUnauthorized, errors.New("id_token verify failed: " + err.Error())
	}
	var claims oidcClaims
	if err := idTok.Claims(&claims); err != nil {
		return "", http.StatusBadRequest, errors.New("claims parse failed")
	}

	// Fall back to userinfo when the ID token didn't carry an email.
	if claims.Email == "" && accessToken != "" && rt.provider != nil {
		ui, err := rt.provider.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken}))
		if err != nil {
			return "", http.StatusBadGateway, errors.New("userinfo lookup failed: " + err.Error())
		}
		// Security: the userinfo response must describe the same subject as the
		// verified ID token, otherwise a mismatched access token could inject a
		// different identity.
		if claims.Sub != "" && ui.Subject != "" && ui.Subject != claims.Sub {
			return "", http.StatusUnauthorized, errors.New("userinfo subject mismatch")
		}
		var uiClaims oidcClaims
		if err := ui.Claims(&uiClaims); err == nil {
			claims.Email = firstNonEmpty([]string{claims.Email, uiClaims.Email})
			claims.PreferredUsername = firstNonEmpty([]string{claims.PreferredUsername, uiClaims.PreferredUsername})
			claims.Username = firstNonEmpty([]string{claims.Username, uiClaims.Username})
			claims.Nickname = firstNonEmpty([]string{claims.Nickname, uiClaims.Nickname})
			claims.Name = firstNonEmpty([]string{claims.Name, uiClaims.Name})
		}
	}

	username := claims.userName()
	if username == "" {
		return "", http.StatusBadRequest, errors.New("provider did not return email or username")
	}
	if !AllowlistMatch(claims.Email, claims.names()...) {
		utils.Log("[Auth] OIDC: user (email=%s, username=%s, nickname=%s) rejected by allowlist", claims.Email, claims.Username, claims.Nickname)
		return "", http.StatusForbidden, errors.New("Access denied: user not in allowlist")
	}
	return sanitizeUsername(username), http.StatusOK, nil
}

// firstNonEmpty returns the first trimmed-non-empty string, or "".
func firstNonEmpty(vals []string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
