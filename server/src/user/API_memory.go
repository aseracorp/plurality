package user

import (
	"encoding/json"
	"net/http"

	"github.com/azukaar/plurality/src/memory"
	"github.com/azukaar/plurality/src/utils"
)

// API_GetMemory returns the authenticated user's important_memory snippet.
// Falls back to memory.DefaultMemory when nothing has been written yet.
func API_GetMemory(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok || userID == "" {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"memory":  memory.Get(userID),
		"default": memory.DefaultMemory,
	})
}

// API_UpdateMemory overwrites the authenticated user's important_memory
// snippet with the request body's "memory" field.
func API_UpdateMemory(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok || userID == "" {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	var body struct {
		Memory string `json:"memory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := memory.Set(userID, body.Memory); err != nil {
		utils.Error("[API_UpdateMemory] write failed", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
