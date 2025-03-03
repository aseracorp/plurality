package main

import (
	"log"
	"net/http"
	"github.com/joho/godotenv"

	"github.com/gorilla/mux"
	"github.com/azukaar/plurality/src/ai"
	"github.com/azukaar/plurality/src/db"
	"github.com/azukaar/plurality/src/utils"
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
	r.HandleFunc("/chat", utils.AuthMiddleware(ai.HandleChat))
	r.HandleFunc("/generate-image", utils.AuthMiddleware(ai.HandleImageGeneration))
	r.HandleFunc("/conversations", utils.AuthMiddleware(ai.API_ListConversation))
	r.HandleFunc("/conversation/{id}", utils.AuthMiddleware(ai.API_HandleConversation))
	
	// You can also have public routes without the middleware
	// http.HandleFunc("/public", PublicHandler)

	http.Handle("/", r)
	http.ListenAndServe(":8090", nil)
}