package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/ai_tools"
	"github.com/azukaar/plurality/src/auth"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/mcp"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/search"
	"github.com/azukaar/plurality/src/skills"
	"github.com/azukaar/plurality/src/storage"
	"github.com/azukaar/plurality/src/user"
	"github.com/azukaar/plurality/src/utils"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	// CLI subcommands (adduser / removeuser / listusers) short-circuit before
	// the server starts.
	if handled, code := auth.HandleCLI(os.Args[1:]); handled {
		os.Exit(code)
	}

	auth.Init()
	db.InitSQLite()
	defer db.CloseAllUserDBs()
	storage.Init()

	// Load global server-side skills from data/skills/
	skills.Init()
	if skills.HasAny() {
		ai_tools.RegisterRetrieveServerSkill()
	}

	// Load mini app presets from data/presets/
	miniapps.LoadBuiltins()

	// Start MCP servers configured in data/mcp.json
	mcp.Init()
	defer mcp.Shutdown()

	// Initialize LiteLLM proxy for AI provider routing
	if err := ai.InitLiteLLM(); err != nil {
		log.Printf("[main] WARNING: LiteLLM proxy not available: %v", err)
		log.Printf("[main] Set LITELLM_URL env var or run litellm/setup.sh to enable AI features")
	} else {
		// Pull the model registry from litellm. The proxy is the single source of
		// truth for which models exist and what they support.
		if err := ai.InitModels(context.Background()); err != nil {
			log.Printf("[main] WARNING: failed to load models from litellm: %v", err)
		}
	}
	defer ai.ShutdownLiteLLM()

	// Pass LiteLLM URL to packages that talk to the proxy directly.
	db.LiteLLMBaseURL = ai.LiteLLMBaseURL
	ai_tools.LiteLLMBaseURL = ai.LiteLLMBaseURL

	utils.Log("[main] Starting server on :8090")

	r := mux.NewRouter()

	// --- Auth (no middleware on login/methods; everything else requires JWT) ---
	r.HandleFunc("/auth/methods", auth.HandleAuthMethods).Methods("GET", "OPTIONS")
	r.HandleFunc("/auth/login", auth.HandleLogin).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/openid/start", auth.HandleOIDCStart).Methods("GET")
	r.HandleFunc("/auth/openid/callback", auth.HandleOIDCCallback).Methods("GET")
	r.HandleFunc("/auth/openid/exchange", auth.HandleOIDCExchange).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/me", auth.AuthMiddleware(auth.HandleMe)).Methods("GET", "OPTIONS")
	r.HandleFunc("/auth/logout", auth.AuthMiddleware(auth.HandleLogout)).Methods("POST", "OPTIONS")
	r.HandleFunc("/auth/change-password", auth.AuthMiddleware(auth.HandleChangePassword)).Methods("POST", "OPTIONS")

	// OpenAI-compatible API (stateless, for generic clients)
	r.HandleFunc("/v1/chat/completions", auth.AuthMiddleware(ai.HandleOpenAIChatCompletion)).Methods("POST", "OPTIONS")
	r.HandleFunc("/v1/models", auth.AuthMiddleware(ai.HandleOpenAIListModels)).Methods("GET", "OPTIONS")
	r.HandleFunc("/v1/tools", auth.AuthMiddleware(ai.HandleListServerTools)).Methods("GET", "OPTIONS")

	// Plurality rich models endpoint: OpenAI-list-compatible with embedded presets + function metadata.
	r.HandleFunc("/models", auth.AuthMiddleware(ai.HandleListModels)).Methods("GET", "OPTIONS")

	// Plurality chat (stateful, with conversation tracking and server-side tool loop)
	r.HandleFunc("/chat", auth.AuthMiddleware(ai.HandleChat)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/stream/{id}", auth.AuthMiddleware(ai.HandleStreamReconnect)).Methods("GET", "OPTIONS")
	r.HandleFunc("/chat/cancel/{id}", auth.AuthMiddleware(ai.HandleCancel)).Methods("POST", "OPTIONS")
	r.HandleFunc("/chat/approve/{id}", auth.AuthMiddleware(ai.HandleApprove)).Methods("POST", "OPTIONS")
	r.HandleFunc("/status/stream", auth.AuthMiddleware(ai.HandleStatusStream)).Methods("GET", "OPTIONS")

	// Conversations
	r.HandleFunc("/conversations", auth.AuthMiddleware(ai.API_ListConversation)).Methods("GET", "OPTIONS")
	r.HandleFunc("/conversation/{id}", auth.AuthMiddleware(ai.API_HandleConversation)).Methods("GET", "PUT", "DELETE", "OPTIONS")
	r.HandleFunc("/set-conversation-folder/{id}", auth.AuthMiddleware(ai.API_UpdateConversationFolder)).Methods("POST", "OPTIONS")
	r.HandleFunc("/rename-conversation/{id}", auth.AuthMiddleware(ai.API_UpdateConversationTitle)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-title/{id}", auth.AuthMiddleware(ai.API_HandleTitleGeneration)).Methods("GET", "OPTIONS")
	r.HandleFunc("/transcribe", auth.AuthMiddleware(ai.HandleTranscribe)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-audio", auth.AuthMiddleware(ai.HandleGenerateAudio)).Methods("POST", "OPTIONS")
	r.HandleFunc("/delete-user", auth.AuthMiddleware(user.API_DeleteUser)).Methods("DELETE", "OPTIONS")

	// Search
	r.HandleFunc("/search", auth.AuthMiddleware(handleSearch)).Methods("GET", "OPTIONS")

	// Mini apps (file-backed)
	r.HandleFunc("/miniapps", auth.AuthMiddleware(miniapps.API_ListMiniApps)).Methods("GET", "OPTIONS")
	r.HandleFunc("/miniapps", auth.AuthMiddleware(miniapps.API_CreateMiniApp)).Methods("POST", "OPTIONS")
	r.HandleFunc("/miniapps/pinned", auth.AuthMiddleware(miniapps.API_GetUserPinnedMiniApps)).Methods("GET", "OPTIONS")
	r.HandleFunc("/miniapps/{id}", auth.AuthMiddleware(miniapps.API_HandleMiniApp)).Methods("GET", "DELETE", "OPTIONS")
	r.HandleFunc("/miniapps/{id}", auth.AuthMiddleware(miniapps.API_UpdateMiniApp)).Methods("PUT")
	r.HandleFunc("/miniapps/{id}/pin", auth.AuthMiddleware(miniapps.API_PinMiniApp)).Methods("POST", "OPTIONS")
	r.HandleFunc("/miniapps/{id}/unpin", auth.AuthMiddleware(miniapps.API_UnpinMiniApp)).Methods("POST", "OPTIONS")

	r.HandleFunc("/download/{file}",
		func(w http.ResponseWriter, r *http.Request) {
			vars := mux.Vars(r)
			file := vars["file"]
			if file == "windows-latest.exe" || file == "linux-latest.zip" || file == "macos-latest.dmg" {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment; filename="+file)
				http.ServeFile(w, r, filepath.Join("web", file))
			} else {
				http.NotFound(w, r)
			}
		}).Methods("GET")

	// Attachment file serving (authenticated)
	r.HandleFunc("/attachments/{userId}/{month}/{filename}", auth.AuthMiddleware(storage.ServeAttachment)).Methods("GET", "OPTIONS")
	r.HandleFunc("/upload", auth.AuthMiddleware(storage.HandleUpload)).Methods("POST", "OPTIONS")

	r.HandleFunc("/check", auth.AuthMiddleware(
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
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	http.Handle("/", corsMiddleware(r))

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
