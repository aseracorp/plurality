package user

import (
	"encoding/json"
	"net/http"

	"github.com/azukaar/plurality/src/utils"
)

// API_DeleteUser handles the HTTP request to delete a user account
func API_DeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		utils.Error("[API_DeleteUser] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	utils.Log("[API_DeleteUser] Processing user deletion request")

	// Delete the user
	err := DeleteUser(r.Context())
	if err != nil {
		utils.Error("[API_DeleteUser] Error deleting user", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]string{"message": "User successfully deleted"}
	json.NewEncoder(w).Encode(response)
}
