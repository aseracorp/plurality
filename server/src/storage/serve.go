package storage

import (
	"net/http"
	"path/filepath"

	"github.com/gorilla/mux"

	"github.com/azukaar/plurality/src/utils"
)

// ServeAttachment handles GET /attachments/{userId}/{month}/{filename}.
// It verifies the authenticated user matches the path's userId.
func ServeAttachment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	requestedUserID := vars["userId"]
	month := vars["month"]
	filename := vars["filename"]

	// Auth check: requesting user must own these files
	authedUserID, ok := r.Context().Value("userID").(string)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}
	if authedUserID != requestedUserID {
		utils.SendHTTPError(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Sanitize path components
	if err := validatePathComponent(requestedUserID); err != nil {
		utils.SendHTTPError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	if err := validatePathComponent(month); err != nil {
		utils.SendHTTPError(w, "Invalid path", http.StatusBadRequest)
		return
	}
	if err := validatePathComponent(filename); err != nil {
		utils.SendHTTPError(w, "Invalid path", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(storagePath, requestedUserID, "attachments", month, filename)
	http.ServeFile(w, r, filePath)
}
