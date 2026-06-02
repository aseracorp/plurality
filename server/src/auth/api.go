package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/utils"
)

// Init wires up the auth subsystem. Call once during server bootstrap.
func Init() {
	if err := LoadConfig(); err != nil {
		utils.Fatal("[Auth] load config failed", err)
	}
	if err := LoadUsers(); err != nil {
		utils.Fatal("[Auth] load users failed", err)
	}
	SeedAdminFromEnv()
	utils.Log("[Auth] %d local user(s) loaded; OpenID enabled=%v", len(ListUsernames()), OpenIDEnabled())
}

// HandleLogin authenticates against data/user.json and returns a JWT.
func HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	canonical, err := VerifyPassword(req.Username, req.Password)
	if err != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	tok, err := IssueToken(canonical)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tok, "username": canonical})
}

// HandleMe returns the currently authenticated username (auth middleware required).
func HandleMe(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value("userID").(string)
	method := "local"
	if !UserExists(username) {
		method = "openid"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"username":    username,
		"auth_method": method,
	})
}

// HandleLogout is a no-op for stateless JWTs. Kept so the client has a
// canonical endpoint to call on logout (and to leave room for future
// revocation lists).
func HandleLogout(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// HandleAuthMethods reports which login methods are available (no auth required).
// When OpenID is enabled it also returns the issuer and client_id so the
// Flutter client can run its own openid_client discovery + auth flow.
func HandleAuthMethods(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"local":  len(ListUsernames()) > 0,
		"openid": OpenIDEnabled(),
	}
	if OpenIDEnabled() {
		c := GetConfig().OpenID
		resp["openid_issuer"] = c.Issuer
		resp["openid_client_id"] = c.ClientID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleChangePassword updates a local user's password (auth middleware required).
// OpenID-only users (not present in user.json) get a 400.
func HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value("userID").(string)
	if !UserExists(username) {
		http.Error(w, "password change not available for OpenID accounts", http.StatusBadRequest)
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := ChangePassword(username, req.OldPassword, req.NewPassword); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleSetShortcut updates one entry in cfg.Shortcuts and persists
// data/config.json. PUT /shortcuts/{name}. Body is a utils.ModelSelected;
// only Text/Vision/ImageGen are read. Existing Label/Pricing/Color are
// preserved.
func HandleSetShortcut(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	var ms utils.ModelSelected
	if err := json.NewDecoder(r.Body).Decode(&ms); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	existing := Shortcut{Name: name}
	for _, s := range GetConfig().Shortcuts {
		if s.Name == name {
			existing = s
			break
		}
	}

	existing.Models = ShortcutModels{
		Text:      fromUtilsModel(ms.Text),
		Vision:    fromUtilsModel(ms.Vision),
		ImageGen:  fromUtilsModel(ms.ImageGen),
		ImageEdit: fromUtilsModel(ms.ImageEdit),
	}

	if err := SetShortcut(existing); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func fromUtilsModel(m *utils.Model) *ShortcutModel {
	if m == nil || m.Name == "" {
		return nil
	}
	return &ShortcutModel{Name: m.Name, Tools: m.Tools}
}

// DeleteUserData removes a user's data directory (used by the account-delete handler).
func DeleteUserData(ctx context.Context, username string) error {
	if username == "" {
		return errors.New("empty username")
	}
	root := os.Getenv("USER_DATA_STORAGE")
	if root == "" {
		exec, _ := os.Executable()
		root = filepath.Join(filepath.Dir(exec), "users-data")
	}
	dir := filepath.Join(root, username)
	if dir == root || dir == "" {
		return errors.New("refusing to remove root data dir")
	}
	return os.RemoveAll(dir)
}
