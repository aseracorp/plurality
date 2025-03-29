package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/user"
	"github.com/azukaar/plurality/src/miniapps"
	"github.com/azukaar/plurality/src/utils"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	// Initialize Firebase Auth
	utils.InitFirebase()
	db.InitDB()

	utils.Log("[main] Starting server on :8090")

	r := mux.NewRouter()

	// Register secure routes with the auth middleware
	r.HandleFunc("/chat", utils.AuthMiddleware(ai.HandleChat)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-image", utils.AuthMiddleware(ai.HandleImageGeneration)).Methods("POST", "OPTIONS")
	r.HandleFunc("/conversations", utils.AuthMiddleware(ai.API_ListConversation)).Methods("GET", "OPTIONS")
	r.HandleFunc("/conversation/{id}", utils.AuthMiddleware(ai.API_HandleConversation)).Methods("GET", "PUT", "DELETE", "OPTIONS")
	r.HandleFunc("/set-conversation-folder/{id}", utils.AuthMiddleware(ai.API_UpdateConversationFolder)).Methods("POST", "OPTIONS")
	r.HandleFunc("/rename-conversation/{id}", utils.AuthMiddleware(ai.API_UpdateConversationTitle)).Methods("POST", "OPTIONS")
	r.HandleFunc("/generate-title/{id}", utils.AuthMiddleware(ai.API_HandleTitleGeneration)).Methods("GET", "OPTIONS")
	r.HandleFunc("/balance", utils.AuthMiddleware(ai.API_GetUserBalance)).Methods("GET", "OPTIONS")
	r.HandleFunc("/transcribe", utils.AuthMiddleware(ai.HandleTranscribe)).Methods("POST", "OPTIONS")
	r.HandleFunc("/delete-user", utils.AuthMiddleware(user.API_DeleteUser)).Methods("DELETE", "OPTIONS")
	
	r.HandleFunc("/miniapps", utils.AuthMiddleware(miniapps.API_ListMiniApps)).Methods("GET")
	r.HandleFunc("/miniapps/pinned", utils.AuthMiddleware(miniapps.API_GetUserPinnedMiniApps)).Methods("GET")
	// r.HandleFunc("/miniapps/{id}", utils.AuthMiddleware(miniapps.API_HandleMiniApp)).Methods("GET", "DELETE")
	// r.HandleFunc("/miniapps", utils.AuthMiddleware(miniapps.API_CreateMiniApp)).Methods("POST")
	r.HandleFunc("/miniapps/{id}", utils.AuthMiddleware(miniapps.API_UpdateMiniApp)).Methods("PUT")
	r.HandleFunc("/miniapps/{id}/pin", utils.AuthMiddleware(miniapps.API_PinMiniApp)).Methods("POST")
	r.HandleFunc("/miniapps/{id}/unpin", utils.AuthMiddleware(miniapps.API_UnpinMiniApp)).Methods("POST")
	// r.HandleFunc("/miniapps/{id}/use", utils.AuthMiddleware(miniapps.API_UseMiniApp)).Methods("POST")
		
	r.HandleFunc("/check", utils.AuthMiddleware(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		},
	)).Methods("GET", "OPTIONS")

	// /static folder as SPA
	exec,_ := os.Executable()
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