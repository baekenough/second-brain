package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireAPIKey returns middleware that enforces Bearer token authentication
// on all routes it wraps. When apiKey is empty the middleware is a no-op,
// preserving backward compatibility in development environments.
//
// Timing-safe comparison via subtle.ConstantTimeCompare prevents timing attacks.
func requireAPIKey(apiKey string) func(http.Handler) http.Handler {
	expected := []byte(apiKey)
	enabled := len(expected) > 0
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !enabled {
				next.ServeHTTP(w, r)
				return
			}
			const prefix = "Bearer "
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, prefix) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			token := []byte(strings.TrimPrefix(authz, prefix))
			if subtle.ConstantTimeCompare(token, expected) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
