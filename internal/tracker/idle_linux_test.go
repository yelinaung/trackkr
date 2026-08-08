//go:build linux

package tracker

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"hegel.dev/go/hegel"
)

// TestParseIdleMsSurvivesArbitraryText covers the half of the contract
// that holds for any input: xprintidle's output is not trusted, so
// garbage must come back as an error.
func TestParseIdleMsSurvivesArbitraryText(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		output := hegel.Draw(ht, hegel.Text())
		idle, err := parseIdleMs(output)

		_, parseErr := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
		if (err != nil) != (parseErr != nil) {
			ht.Fatalf("parseIdleMs(%q) error = %v, but ParseInt error = %v", output, err, parseErr)
		}
		if err != nil && idle != 0 {
			ht.Fatalf("parseIdleMs(%q) failed but returned %v", output, idle)
		}
	})
}

// TestParseIdleMsConvertsExactly checks the conversion against an oracle
// that is not the implementation.
//
// Asserting idle == time.Duration(ms) * time.Millisecond would restate
// the function's own expression, overflow included, and so would agree
// with it at exactly the inputs worth asking about. math/big does the
// multiply without wrapping, which makes the failure visible: past
// math.MaxInt64 / 1e6 milliseconds, roughly 292 years, the production
// expression wraps and the exact product no longer fits in an int64.
//
// The draw is bounded to what the conversion can represent, and the
// bound is the finding. xprintidle reports milliseconds since the last
// input event, so no real session reaches it, but parseIdleMs accepts
// any int64 the text spells and wraps silently past the limit rather
// than refusing it.
func TestParseIdleMsConvertsExactly(t *testing.T) {
	t.Parallel()

	const maxRepresentableMs = math.MaxInt64 / int64(time.Millisecond)

	hegel.Test(t, func(ht *hegel.T) {
		ms := hegel.Draw(ht, hegel.Integers(-maxRepresentableMs, maxRepresentableMs))

		idle, err := parseIdleMs(strconv.FormatInt(ms, 10))
		if err != nil {
			ht.Fatalf("parseIdleMs(%d): %v", ms, err)
		}

		want := new(big.Int).Mul(big.NewInt(ms), big.NewInt(int64(time.Millisecond)))
		if !want.IsInt64() {
			ht.Fatalf("%d ms does not fit in an int64 of nanoseconds", ms)
		}
		if int64(idle) != want.Int64() {
			ht.Fatalf("parseIdleMs(%d) = %d ns, want %d ns", ms, int64(idle), want.Int64())
		}
	})
}

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
	// Not parallel, and the session variables are cleared: left to the
	// ambient environment this picks a detector based on the shell the
	// suite happens to run from, and on a Wayland box it would start a
	// real swayidle.
	clearSessionEnv(t)
	logger := zerolog.Nop()

	// Either XIdleDetector or NopIdleDetector, depending on whether
	// xprintidle is installed.
	d := NewIdleDetectorOrNop(DefaultConfig(), &logger)
	if d == nil {
		t.Fatal("NewIdleDetectorOrNop returned nil")
	}
	if _, ok := d.(*SwayIdleDetector); ok {
		t.Fatal("picked the Wayland detector for a session with nothing set")
	}

	// Either way, IdleTime should not error.
	_, err := d.IdleTime(context.Background())
	if err != nil {
		t.Logf("IdleTime returned error (expected if no X display): %v", err)
	}
}

// TestIdleDetectorNeverFallsBackToX is the rule that costs data when
// it breaks: xprintidle runs happily under XWayland and returns a
// plausible number that climbs while the user types in a native
// Wayland application.
func TestIdleDetectorNeverFallsBackToX(t *testing.T) {
	clearSessionEnv(t)
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	// An empty PATH guarantees swayidle cannot be found, which is the
	// case where a fallback would be tempting.
	t.Setenv("PATH", t.TempDir())

	logger := zerolog.Nop()
	d := NewIdleDetectorOrNop(DefaultConfig(), &logger)

	if _, ok := d.(NopIdleDetector); !ok {
		t.Fatalf("detector = %T, want NopIdleDetector", d)
	}
}

// TestWindowDetectorRefusesUnsupportedWayland covers the other half of
// the same rule: xdotool under a Wayland compositor answers about
// whichever X client XWayland last saw focused.
func TestWindowDetectorRefusesUnsupportedWayland(t *testing.T) {
	clearSessionEnv(t)
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")

	logger := zerolog.Nop()
	d, err := NewWindowDetector(DefaultConfig(), &logger)

	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("error = %v, want ErrUnsupportedPlatform", err)
	}
	if d != nil {
		t.Errorf("detector = %T, want nil", d)
	}
}
