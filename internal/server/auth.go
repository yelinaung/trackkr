package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/yelinaung/trackkr/internal/db"
)

type contextKey string

const deviceContextKey contextKey = "device"

func DeviceFromContext(ctx context.Context) *db.DeviceRow {
	d, _ := ctx.Value(deviceContextKey).(*db.DeviceRow)
	return d
}

// APIKeyAuth is middleware that validates the X-API-Key header against the devices table.
func APIKeyAuth(queries Querier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				http.Error(w, `{"error":"missing X-API-Key header"}`, http.StatusUnauthorized)
				return
			}

			device, err := queries.GetDeviceByAPIKey(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), deviceContextKey, device)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GenerateAPIKey creates a 32-byte (64 hex char) random API key.
func GenerateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
