package tracker

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	testFinder = "Finder"
	testSafari = "Safari"
)

func TestMapFrontmost(t *testing.T) {
	t.Parallel()

	app := appInfo{Name: testFinder, PID: 42}
	tests := []struct {
		name      string
		status    int
		wantNil   bool
		wantError bool
	}{
		{"ok", statusOK, false, false},
		{"no app", statusNoApp, true, false},
		{"failed", statusFailed, true, true},
		{"unknown", 99, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := mapFrontmost(tt.status, app)
			if (err != nil) != tt.wantError {
				t.Fatalf("mapFrontmost error = %v, wantError %v", err, tt.wantError)
			}
			if (got == nil) != tt.wantNil {
				t.Fatalf("mapFrontmost app = %#v, wantNil %v", got, tt.wantNil)
			}
			if got != nil && *got != app {
				t.Errorf("mapFrontmost app = %#v, want %#v", *got, app)
			}
		})
	}
}

func TestDetectorCoreNoActiveWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		app  *appInfo
	}{
		{"nil application", nil},
		{"empty name", &appInfo{PID: 1}},
		{"whitespace name", &appInfo{Name: "  ", PID: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &detectorCore{frontmost: func() (*appInfo, error) { return tt.app, nil }}
			_, err := d.ActiveWindow(context.Background())
			if !errors.Is(err, ErrNoActiveWindow) {
				t.Errorf("ActiveWindow error = %v, want ErrNoActiveWindow", err)
			}
		})
	}
}

func TestDetectorCoreKeepsApplicationWithoutTitle(t *testing.T) {
	t.Parallel()

	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { return true },
		prompt:    func() {},
		now:       time.Now,
		log:       func(bool) {},
	}
	d := &detectorCore{
		policy: p,
		frontmost: func() (*appInfo, error) {
			return &appInfo{Name: testSafari, PID: 7}, nil
		},
		titleFor: func(int) string { return "" },
	}

	info, err := d.ActiveWindow(context.Background())
	if err != nil {
		t.Fatalf("ActiveWindow: %v", err)
	}
	if info.AppName != testSafari || info.Title != "" {
		t.Errorf("ActiveWindow = %#v, want Safari with empty title", info)
	}
}

func TestDetectorCoreSkipsTitleWhenUntrusted(t *testing.T) {
	t.Parallel()

	titleCalls := 0
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { return false },
		prompt:    func() {},
		now:       time.Now,
		log:       func(bool) {},
	}
	d := &detectorCore{
		policy: p,
		frontmost: func() (*appInfo, error) {
			return &appInfo{Name: testSafari, PID: 7}, nil
		},
		titleFor: func(int) string { titleCalls++; return "private" },
	}

	info, err := d.ActiveWindow(context.Background())
	if err != nil {
		t.Fatalf("ActiveWindow: %v", err)
	}
	if info.Title != "" {
		t.Errorf("Title = %q, want empty", info.Title)
	}
	if titleCalls != 0 {
		t.Errorf("title calls = %d, want 0", titleCalls)
	}
}

func TestDetectorCorePropagatesFrontmostError(t *testing.T) {
	t.Parallel()

	want := errors.New("window server unavailable")
	d := &detectorCore{frontmost: func() (*appInfo, error) { return nil, want }}
	_, err := d.ActiveWindow(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("ActiveWindow error = %v, want %v", err, want)
	}
}

func TestDetectorCoreChecksContextBeforeNativeCalls(t *testing.T) {
	t.Parallel()

	t.Run("frontmost", func(t *testing.T) {
		t.Parallel()
		calls := 0
		d := &detectorCore{frontmost: func() (*appInfo, error) {
			calls++
			return &appInfo{Name: testFinder}, nil
		}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := d.ActiveWindow(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ActiveWindow error = %v, want context.Canceled", err)
		}
		if calls != 0 {
			t.Errorf("frontmost calls = %d, want 0", calls)
		}
	})

	t.Run("title", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		titleCalls := 0
		p := &titlePolicy{
			enabled: true,
			isTrusted: func() bool {
				cancel()
				return true
			},
			prompt: func() {},
			now:    time.Now,
			log:    func(bool) {},
		}
		d := &detectorCore{
			policy: p,
			frontmost: func() (*appInfo, error) {
				return &appInfo{Name: testFinder, PID: 3}, nil
			},
			titleFor: func(int) string { titleCalls++; return "Desktop" },
		}
		_, err := d.ActiveWindow(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ActiveWindow error = %v, want context.Canceled", err)
		}
		if titleCalls != 0 {
			t.Errorf("title calls = %d, want 0", titleCalls)
		}
	})

	t.Run("trust", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		trustCalls := 0
		p := &titlePolicy{
			enabled:   true,
			isTrusted: func() bool { trustCalls++; return true },
			prompt:    func() {},
			now:       time.Now,
			log:       func(bool) {},
		}
		d := &detectorCore{
			policy: p,
			frontmost: func() (*appInfo, error) {
				cancel()
				return &appInfo{Name: testFinder, PID: 3}, nil
			},
			titleFor: func(int) string { return "Desktop" },
		}
		_, err := d.ActiveWindow(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ActiveWindow error = %v, want context.Canceled", err)
		}
		if trustCalls != 0 {
			t.Errorf("trust calls = %d, want 0", trustCalls)
		}
	})
}
