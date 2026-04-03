package shared

import (
	"net/http"
	"strings"
)

// InternalAuthMiddleware validates Bearer token on /api/internal/* routes.
// Other routes pass through without auth.
func InternalAuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		// WebSocket: token in query param
		if r.URL.Query().Get("token") == token {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidateToken checks Bearer token from HTTP request (header or query param).
func ValidateToken(r *http.Request, token string) bool {
	if r.URL.Query().Get("token") == token {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+token
}
