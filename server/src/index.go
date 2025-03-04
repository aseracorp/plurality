package main

import (
	"log"
	"net/http"

	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

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
	r.HandleFunc("/generate-title/{id}", utils.AuthMiddleware(ai.API_HandleTitleGeneration)).Methods("GET", "OPTIONS")

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