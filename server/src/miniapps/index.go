package miniapps

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/utils"
)

func userFromCtx(r *http.Request) (string, bool) {
	v, ok := r.Context().Value("userID").(string)
	return v, ok
}

// API_ListMiniApps handles GET /miniapps — merged builtin + user view.
func API_ListMiniApps(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	apps := ListForUser(username)
	writeJSON(w, http.StatusOK, apps)
}

// API_GetUserPinnedMiniApps handles GET /miniapps/pinned.
func API_GetUserPinnedMiniApps(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, GetPinned(username))
}

// API_HandleMiniApp handles GET and DELETE for a specific mini app.
func API_HandleMiniApp(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]

	switch r.Method {
	case http.MethodGet:
		app, err := Get(username, id)
		if err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, app)
	case http.MethodDelete:
		if err := Delete(username, id); err != nil {
			utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		utils.SendHTTPError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// API_CreateMiniApp handles POST /miniapps.
func API_CreateMiniApp(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var app utils.MiniApp
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if app.Name == "" {
		utils.SendHTTPError(w, "name is required", http.StatusBadRequest)
		return
	}
	created, err := Create(username, app)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// API_UpdateMiniApp handles PUT /miniapps/{id}.
func API_UpdateMiniApp(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	var app utils.MiniApp
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}
	updated, err := Update(username, id, app)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// API_PinMiniApp handles POST /miniapps/{id}/pin.
func API_PinMiniApp(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	if err := Pin(username, id); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// API_UnpinMiniApp handles POST /miniapps/{id}/unpin.
func API_UnpinMiniApp(w http.ResponseWriter, r *http.Request) {
	username, ok := userFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	if err := Unpin(username, id); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		w.Write([]byte("null"))
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		utils.Error("[MiniApps] marshal failed", err)
		w.Write([]byte("null"))
		return
	}
	if string(data) == "null" {
		w.Write([]byte("[]"))
		return
	}
	w.Write(data)
}
