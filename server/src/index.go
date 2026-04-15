package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/search"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/user"
	"github.com/azukaar/plurality/src/utils"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	// Initialize Firebase Auth
	utils.InitFirebase()
	db.InitDB()
	db.InitSQLite()
	defer db.CloseAllUserDBs()
	storage.Init()

	// Initialize LiteLLM proxy for AI provider routing
	if err := ai.InitLiteLLM(); err != nil {
		log.Printf("[main] WARNING: LiteLLM proxy not available: %v", err)
		log.Printf("[main] Set LITELLM_URL env var or run litellm/setup.sh to enable AI features")
	}
	defer ai.ShutdownLiteLLM()

	// Pass LiteLLM URL to db package for async embedding generation
	db.LiteLLMBaseURL = ai.LiteLLMBaseURL

	utils.Log("[main] Starting server on :8090")

	r := mux.NewRouter()

	// OpenAI-compatible API (stateless, for generic clients)
	r.HandleFunc("/v1/chat/completions", utils.AuthMiddleware(ai.HandleOpenAIChatCompletion)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/models", utils.AuthMiddleware(ai.HandleOpenAIListModels)).Methods("GET", "OPTIONS")
	r.HandleFunc("/v1/tools", utils.AuthMiddleware(ai.HandleListServerTools)).Methods("GET", "OPTIONS")

	// Plurality chat (stateful, with conversation tracking and server-side tool loop)
	r.HandleFunc("/chat", utils.AuthMiddleware(ai.HandleChat)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/stream/{id}", utils.AuthMiddleware(ai.HandleStreamReconnect)).Methods("GET", "OPTIONS")
	r.HandleFunc("/chat/cancel/{id}", utils.AuthMiddleware(ai.HandleCancel)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/approve/{id}", utils.AuthMiddleware(ai.HandleApprove)).Methods("POST", "OPTIONS")
	r.HandleFunc("/status/stream", utils.AuthMiddleware(ai.HandleStatusStream)).Methods("GET", "OPTIONS")

	// Conversations
	r.HandleFunc("/conversations", utils.AuthMiddleware(ai.API_ListConversation)).Methods("GET", "OPTIONS")
	r.HandleFunc("/conversation/{id}", utils.AuthMiddleware(ai.API_HandleConversation)).Methods("GET", "PUT", "DELETE", "OPTIONS")
	r.HandleFunc("/set-conversation-folder/{id}", utils.AuthMiddleware(ai.API_UpdateConversationFolder)).Methods("POST", "OPTIONS")
	r.HandleFunc("/rename-conversation/{id}", utils.AuthMiddleware(ai.API_UpdateConversationTitle)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-title/{id}", utils.AuthMiddleware(ai.API_HandleTitleGeneration)).Methods("GET", "OPTIONS")
	r.HandleFunc("/balance", utils.AuthMiddleware(ai.API_GetUserBalance)).Methods("GET", "OPTIONS")
	r.HandleFunc("/transcribe", utils.AuthMiddleware(ai.HandleTranscribe)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-audio", utils.AuthMiddleware(ai.HandleGenerateAudio)).Methods("POST", "OPTIONS")
	r.HandleFunc("/delete-user", utils.AuthMiddleware(user.API_DeleteUser)).Methods("DELETE", "OPTIONS")

	// Search
	r.HandleFunc("/search", utils.AuthMiddleware(handleSearch)).Methods("GET", "OPTIONS")

	r.HandleFunc("/miniapps", utils.AuthMiddleware(miniapps.API_ListMiniApps)).Methods("GET")
	r.HandleFunc("/miniapps/pinned", utils.AuthMiddleware(miniapps.API_GetUserPinnedMiniApps)).Methods("GET")
	// r.HandleFunc("/miniapps/{id}", utils.AuthMiddleware(miniapps.API_HandleMiniApp)).Methods("GET", "DELETE")
	// r.HandleFunc("/miniapps", utils.AuthMiddleware(miniapps.API_CreateMiniApp)).Methods("POST")
	r.HandleFunc("/miniapps/{id}", utils.AuthMiddleware(miniapps.API_UpdateMiniApp)).Methods("PUT")
	r.HandleFunc("/miniapps/{id}/pin", utils.AuthMiddleware(miniapps.API_PinMiniApp)).Methods("POST")
	r.HandleFunc("/miniapps/{id}/unpin", utils.AuthMiddleware(miniapps.API_UnpinMiniApp)).Methods("POST")
	// r.HandleFunc("/miniapps/{id}/use", utils.AuthMiddleware(miniapps.API_UseMiniApp)).Methods("POST")

	r.HandleFunc("/download/{file}",
		func(w http.ResponseWriter, r *http.Request) {
			// if the file is windows-latest.ext, or linux-latest.ext, or macos-latest.ext, then return the file as a download
			vars := mux.Vars(r)
			file := vars["file"]
			if file == "windows-latest.exe" || file == "linux-latest.zip" || file == "macos-latest.dmg" {
				// Set the content type to application/octet-stream
				w.Header().Set("Content-Type", "application/octet-stream")
				// Set the content disposition to attachment
				w.Header().Set("Content-Disposition", "attachment; filename="+file)
				// Serve the file
				http.ServeFile(w, r, filepath.Join("web", file))
			} else {
				http.NotFound(w, r)
			}
		}).Methods("GET")

	// Attachment file serving (authenticated)
	r.HandleFunc("/attachments/{userId}/{month}/{filename}", utils.AuthMiddleware(storage.ServeAttachment)).Methods("GET", "OPTIONS")

	r.HandleFunc("/stripe-webhook", HandleStripeWebhook).Methods("POST")

	r.HandleFunc("/check", utils.AuthMiddleware(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		},
	)).Methods("GET", "OPTIONS")

	// /static folder as SPA
	exec, _ := os.Executable()
	pwd := filepath.Dir(exec)
	p := filepath.Join(pwd, "web")
	r.PathPrefix("/").Handler(utils.SPAHandler(p))

	// CORS middleware wrapper for the entire router
	corsMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set CORS headers
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

			// Handle preflight OPTIONS requests
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			// Proceed to the next handler
			next.ServeHTTP(w, r)
		})
	}

	// Apply the CORS middleware to all routes
	http.Handle("/", corsMiddleware(r))

	// Start server
	log.Printf("Server starting on port 8090...")
	log.Fatal(http.ListenAndServe(":8090", nil))
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		utils.SendHTTPError(w, "Missing query parameter 'q'", http.StatusBadRequest)
		return
	}

	userID, ok := r.Context().Value("userID").(string)
	if !ok {
		utils.SendHTTPError(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	userDB, err := db.GetUserDB(userID)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	results, err := search.Search(r.Context(), userDB, ai.LiteLLMBaseURL, query, limit)
	if err != nil {
		utils.SendHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}
