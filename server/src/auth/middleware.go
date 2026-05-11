package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/azukaar/plurality/src/utils"
)

// AuthMiddleware verifies a JWT in the Authorization header and injects
// userID=username into the request context.
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header is required", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == authHeader {
			http.Error(w, "Authorization header must be in the format 'Bearer {token}'", http.StatusUnauthorized)
			return
		}

		username, err := VerifyToken(token)
		if err != nil {
			utils.Debug("[Auth] token verify failed: %v", err)
			http.Error(w, "Invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), "userID", username)
		next(w, r.WithContext(ctx))
	}
}
