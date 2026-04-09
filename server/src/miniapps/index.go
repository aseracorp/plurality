package miniapps

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
)

// API_ListMiniApps handles GET requests to retrieve all available mini-apps
func API_ListMiniApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.Error("[API_ListMiniApps] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	miniApps, err := db.GetAllMiniApps(r.Context())
	if err != nil {
		utils.Error("[API_ListMiniApps] Error retrieving mini-apps", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(miniApps)
	if err != nil {
		utils.Error("[API_ListMiniApps] Error marshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Handle empty array case
	if string(response) == "null" {
		response = []byte("[]")
	}

	utils.Log("[API_ListMiniApps] Returning %d mini-apps", len(miniApps))
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

// API_GetUserPinnedMiniApps handles GET requests to retrieve user's pinned mini-apps
func API_GetUserPinnedMiniApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.Error("[API_GetUserPinnedMiniApps] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pinnedApps, err := db.GetUserPinnedMiniApps(r.Context())
	if err != nil {
		utils.Error("[API_GetUserPinnedMiniApps] Error retrieving pinned mini-apps", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(pinnedApps)
	if err != nil {
		utils.Error("[API_GetUserPinnedMiniApps] Error marshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Handle empty array case
	if string(response) == "null" {
		response = []byte("[]")
	}

	utils.Log("[API_GetUserPinnedMiniApps] Returning %d pinned mini-apps", len(pinnedApps))
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

// API_HandleMiniApp handles GET, DELETE operations for a specific mini-app
func API_HandleMiniApp(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	if r.Method == http.MethodGet {
		// Get specific mini-app details
		miniApp, err := db.GetMiniAppByID(r.Context(), id)
		if err != nil {
			utils.Error("[API_HandleMiniApp] Error retrieving mini-app", err)
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		response, err := json.Marshal(miniApp)
		if err != nil {
			utils.Error("[API_HandleMiniApp] Error marshaling response", err)
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		utils.Log("[API_HandleMiniApp] Returning mini-app %s", id)
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
		return
	} else if r.Method == http.MethodDelete {
		// Delete mini-app (only available to author or admin)
		err := db.DeleteMiniApp(r.Context(), id)
		if err != nil {
			utils.Error("[API_HandleMiniApp] Error deleting mini-app", err)
			utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		utils.Log("[API_HandleMiniApp] Mini-app deleted: %s", id)
		w.WriteHeader(http.StatusNoContent)
		return
	} else {
		utils.Error("[API_HandleMiniApp] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
}

// API_CreateMiniApp handles POST requests to create a new mini-app
func API_CreateMiniApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.Error("[API_CreateMiniApp] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var miniApp utils.MiniApp
	if err := json.NewDecoder(r.Body).Decode(&miniApp); err != nil {
		utils.Error("[API_CreateMiniApp] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if miniApp.Name == "" {
		utils.Error("[API_CreateMiniApp] Name is required", nil)
		utils.SendHTTPError(w, "Name is required", http.StatusBadRequest)
		return
	}

	// Set new ID
	miniApp.ID = primitive.NewObjectID()

	// Set author from context
	userID, ok := r.Context().Value("userID").(string)
	if ok {
		miniApp.Author = userID
	} else {
		utils.Error("[API_CreateMiniApp] User ID not found in context", nil)
		utils.SendHTTPError(w, "User ID not found", http.StatusUnauthorized)
		return
	}

	// Create mini-app in database
	createdApp, err := db.CreateMiniApp(r.Context(), miniApp)
	if err != nil {
		utils.Error("[API_CreateMiniApp] Error creating mini-app", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(createdApp)
	if err != nil {
		utils.Error("[API_CreateMiniApp] Error marshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_CreateMiniApp] Mini-app created with ID: %s", createdApp.ID.Hex())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(response)
}

// API_UpdateMiniApp handles PUT requests to update an existing mini-app
func API_UpdateMiniApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		utils.Error("[API_UpdateMiniApp] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	var updates utils.MiniApp
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		utils.Error("[API_UpdateMiniApp] Invalid request body", err)
		utils.SendHTTPError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Update mini-app in database (only available to author or admin)
	updatedApp, err := db.UpdateMiniApp(r.Context(), id, updates)
	if err != nil {
		utils.Error("[API_UpdateMiniApp] Error updating mini-app", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response, err := json.Marshal(updatedApp)
	if err != nil {
		utils.Error("[API_UpdateMiniApp] Error marshaling response", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_UpdateMiniApp] Mini-app updated: %s", id)
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

// API_PinMiniApp handles POST requests to pin a mini-app for the current user
func API_PinMiniApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.Error("[API_PinMiniApp] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	// Pin mini-app for user
	err := db.PinMiniApp(r.Context(), id)
	if err != nil {
		utils.Error("[API_PinMiniApp] Error pinning mini-app", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_PinMiniApp] Mini-app pinned: %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// API_UnpinMiniApp handles POST requests to unpin a mini-app for the current user
func API_UnpinMiniApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.Error("[API_UnpinMiniApp] Method not allowed", nil)
		utils.SendHTTPError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	// Unpin mini-app for user
	err := db.UnpinMiniApp(r.Context(), id)
	if err != nil {
		utils.Error("[API_UnpinMiniApp] Error unpinning mini-app", err)
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	utils.Log("[API_UnpinMiniApp] Mini-app unpinned: %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// Helper function to process mini-app prompt with form inputs
func processMiniAppPrompt(ctx context.Context, miniApp utils.MiniApp, formInputs map[string]interface{}) (string, error) {
	// Start with the base prompt
	basePrompt, ok := miniApp.Prompt["base"]
	if !ok {
		return "", nil
	}

	// Replace placeholders in the prompt with form input values
	processedPrompt := basePrompt
	for key, value := range formInputs {
		placeholder := "{{" + key + "}}"

		// Convert value to string based on type
		var stringValue string
		switch v := value.(type) {
		case string:
			stringValue = v
		case float64:
			stringValue = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			stringValue = strconv.FormatBool(v)
		default:
			// For complex types, convert to JSON
			jsonBytes, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			stringValue = string(jsonBytes)
		}

		processedPrompt = strings.Replace(processedPrompt, placeholder, stringValue, -1)
	}

	return processedPrompt, nil
}
