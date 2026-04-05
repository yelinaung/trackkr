package server

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/yelinaung/trackkr/internal/db"
)

func TestGenerateAPIKey(t *testing.T) {
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	if len(key) != 64 {
		t.Errorf("key length = %d, want 64", len(key))
	}

	if _, err := hex.DecodeString(key); err != nil {
		t.Errorf("key is not valid hex: %v", err)
	}
}

func TestGenerateAPIKeyUniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for i := range 100 {
		key, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey[%d]: %v", i, err)
		}
		if keys[key] {
			t.Fatalf("duplicate key generated at iteration %d", i)
		}
		keys[key] = true
	}
}

func TestDeviceFromContext(t *testing.T) {
	device := &db.DeviceRow{
		ID:     42,
		UserID: 1,
		Name:   "test-laptop",
	}

	ctx := context.WithValue(context.Background(), deviceContextKey, device)
	got := DeviceFromContext(ctx)
	if got == nil {
		t.Fatal("DeviceFromContext returned nil")
	}
	if got.ID != 42 {
		t.Errorf("device ID = %d, want 42", got.ID)
	}
}

func TestDeviceFromContextMissing(t *testing.T) {
	got := DeviceFromContext(context.Background())
	if got != nil {
		t.Errorf("DeviceFromContext = %v, want nil", got)
	}
}
