package cron

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/utils"
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

func API_ListCrons(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, list)
}

type createReq struct {
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	PresetID string `json:"preset_id"`
}

func API_CreateCron(w http.ResponseWriter, r *http.Request) {
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
	job, err := Create(userID, body.Schedule, body.Prompt, body.PresetID)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func API_UpdateCron(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	var patch CronUpdate
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		utils.SendHTTPError(w, "invalid body", http.StatusBadRequest)
		return
	}
	job, err := Update(userID, id, patch)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func API_DeleteCron(w http.ResponseWriter, r *http.Request) {
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

func API_RunCron(w http.ResponseWriter, r *http.Request) {
	userID, ok := userIDFromCtx(r)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	id := mux.Vars(r)["id"]
	if err := RunNow(userID, id); err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
