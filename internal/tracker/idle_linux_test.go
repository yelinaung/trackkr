//go:build linux

package tracker

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestNopIdleDetector(t *testing.T) {
	t.Parallel()
	d := NopIdleDetector{}
	idle, err := d.IdleTime(context.Background())
	if err != nil {
		t.Fatalf("IdleTime: %v", err)
	}
	if idle != 0 {
		t.Errorf("idle = %v, want 0", idle)
	}
}

func TestParseIdleMs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"zero", "0\n", 0, false},
		{"one second", "1000\n", time.Second, false},
		{"five minutes", "300000\n", 5 * time.Minute, false},
		{"no newline", "500", 500 * time.Millisecond, false},
		{"empty", "", 0, true},
		{"non-numeric", "abc\n", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseIdleMs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseIdleMs(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseIdleMs(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewIdleDetectorOrNop(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()

	// Should return either XIdleDetector or NopIdleDetector
	// depending on whether xprintidle is installed.
	d := NewIdleDetectorOrNop(DefaultConfig(), &logger)
	if d == nil {
		t.Fatal("NewIdleDetectorOrNop returned nil")
	}

	// Either way, IdleTime should not error.
	_, err := d.IdleTime(context.Background())
	if err != nil {
		t.Logf("IdleTime returned error (expected if no X display): %v", err)
	}
}
