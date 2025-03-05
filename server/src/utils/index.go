package utils

import (
	"context"
	"net/http"
	"strings"
	"os"
	"math/rand"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

// Firebase auth client
var firebaseAuth *auth.Client

func InitFirebase() {
	var app *firebase.App
	var err error
	
	// Check if credentials are provided via environment variable
	if credentials := os.Getenv("FIREBASE_CREDENTIALS"); credentials != "" {
			// Use credentials from environment variable
			opt := option.WithCredentialsJSON([]byte(credentials))
			app, err = firebase.NewApp(context.Background(), nil, opt)
	} else if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
			// Use application default credentials path
			app, err = firebase.NewApp(context.Background(), nil)
	} else {
			// Fallback to file if it exists
			opt := option.WithCredentialsFile("firebase.json")
			app, err = firebase.NewApp(context.Background(), nil, opt)
	}
	
	if err != nil {
			Fatal("Error initializing Firebase app", err)
	}
	
	// Initialize Firebase Auth client
	firebaseAuth, err = app.Auth(context.Background())
	if err != nil {
			Fatal("Error initializing Firebase Auth client", err)
	}
}

// AuthMiddleware verifies the Firebase ID token in the Authorization header
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get the Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			Error("Authorization header is required", nil)
			http.Error(w, "Authorization header is required", http.StatusUnauthorized)
			return
		}

		// Check if the header has the Bearer prefix
		idToken := strings.TrimPrefix(authHeader, "Bearer ")
		if idToken == authHeader {
			Error("Authorization header must be in the format 'Bearer {token}'", nil)
			http.Error(w, "Authorization header must be in the format 'Bearer {token}'", http.StatusUnauthorized)
			return
		}

		// Verify the ID token
		token, err := firebaseAuth.VerifyIDToken(context.Background(), idToken)
		if err != nil {
			Error("Invalid token", err)
			http.Error(w, "Invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}

		// Add the verified user ID to the request context
		ctx := context.WithValue(r.Context(), "userID", token.UID)

		// Call the next handler with the updated context
		next(w, r.WithContext(ctx))
	}
}

var AlphaNumRunes = []rune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func GenerateRandomString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = AlphaNumRunes[rand.Intn(len(AlphaNumRunes))]
	}
	return string(b)
}

func SendHTTPError(w http.ResponseWriter,  message string, code int) {
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		http.Error(w, message, code)
	} else {
		userError := GenerateRandomString(8)
		Error("User error", nil, userError, ":", message)
		http.Error(w, "An unexpected error happened (Code: " + userError + ")", http.StatusInternalServerError)
	}
}

func SPAHandler(targetFolder string) http.Handler {
	// pwd,_ := os.Getwd()
	fs := http.FileServer(http.Dir(targetFolder))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Debug("Serving SPA from " + targetFolder + r.URL.Path)
		// if file does not exist or is a directory, serve index.html
		if stat, err := os.Stat(targetFolder + r.URL.Path); os.IsNotExist(err) || stat.IsDir() {
			Debug("Serving SPA index.html")
			http.ServeFile(w, r, targetFolder + "/index.html")
		} else {
			Debug("Serving SPA from " + targetFolder + r.URL.Path)
			fs.ServeHTTP(w, r)
		}
	})
}