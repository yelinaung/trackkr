package tracker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTitlePolicyDisabled(t *testing.T) {
	t.Parallel()

	var checks, prompts atomic.Int64
	cfg := DefaultConfig()
	cfg.MacOSReadTitles = false
	cfg.MacOSPromptForAccessibility = true
	p := newTitlePolicy(
		cfg,
		func() bool {
			checks.Add(1)
			return true
		},
		func() { prompts.Add(1) },
		time.Now,
		func(bool) {},
	)

	if p.canRead() {
		t.Error("canRead = true, want false")
	}
	if checks.Load() != 0 {
		t.Errorf("trust checks = %d, want 0", checks.Load())
	}
	if prompts.Load() != 0 {
		t.Errorf("prompts = %d, want 0", prompts.Load())
	}
}

func TestTitlePolicyCachesTrust(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	checks := 0
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { checks++; return true },
		prompt:    func() {},
		now:       func() time.Time { return now },
		log:       func(bool) {},
	}

	for range 11 {
		if !p.canRead() {
			t.Fatal("canRead = false, want true")
		}
	}
	if checks != 1 {
		t.Errorf("trust checks = %d, want 1", checks)
	}

	now = now.Add(trustRecheckInterval)
	if !p.canRead() {
		t.Fatal("canRead after cache expiry = false, want true")
	}
	if checks != 2 {
		t.Errorf("trust checks after expiry = %d, want 2", checks)
	}
}

func TestTitlePolicyLogsTransitions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	trusted := false
	var logged []bool
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { return trusted },
		prompt:    func() {},
		now:       func() time.Time { return now },
		log:       func(value bool) { logged = append(logged, value) },
	}

	p.canRead()
	p.canRead()
	trusted = true
	now = now.Add(trustRecheckInterval)
	p.canRead()
	now = now.Add(trustRecheckInterval)
	p.canRead()
	trusted = false
	now = now.Add(trustRecheckInterval)
	p.canRead()

	want := []bool{false, true, false}
	if len(logged) != len(want) {
		t.Fatalf("logged = %v, want %v", logged, want)
	}
	for i := range want {
		if logged[i] != want[i] {
			t.Errorf("logged[%d] = %v, want %v", i, logged[i], want[i])
		}
	}
}

func TestTitlePolicyPromptsOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	prompts := 0
	p := &titlePolicy{
		enabled:       true,
		promptEnabled: true,
		isTrusted:     func() bool { return false },
		prompt:        func() { prompts++ },
		now:           func() time.Time { return now },
		log:           func(bool) {},
	}

	if p.canRead() {
		t.Error("canRead = true, want denied state")
	}
	for range 3 {
		now = now.Add(trustRecheckInterval)
		p.canRead()
	}
	if prompts != 1 {
		t.Errorf("prompts = %d, want 1", prompts)
	}
}

func TestTitlePolicyPromptDisabled(t *testing.T) {
	t.Parallel()

	prompts := 0
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { return false },
		prompt:    func() { prompts++ },
		now:       time.Now,
		log:       func(bool) {},
	}
	p.canRead()
	if prompts != 0 {
		t.Errorf("prompts = %d, want 0", prompts)
	}
}

func TestTitlePolicySerializesConcurrentChecks(t *testing.T) {
	t.Parallel()

	var checks atomic.Int64
	p := &titlePolicy{
		enabled: true,
		isTrusted: func() bool {
			checks.Add(1)
			time.Sleep(time.Millisecond)
			return true
		},
		prompt: func() {},
		now:    time.Now,
		log:    func(bool) {},
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			p.canRead()
		})
	}
	wg.Wait()

	if checks.Load() != 1 {
		t.Errorf("trust checks = %d, want 1", checks.Load())
	}
}

func TestTitlePolicyTrustCallbackCanReenter(t *testing.T) {
	t.Parallel()

	var p *titlePolicy
	p = &titlePolicy{
		enabled: true,
		isTrusted: func() bool {
			return p.canRead()
		},
		prompt: func() {},
		now:    time.Now,
		log:    func(bool) {},
	}

	done := make(chan struct{})
	go func() {
		p.canRead()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("trust callback deadlocked while re-entering the policy")
	}
}

func TestTitlePolicyCallbacksRunUnlocked(t *testing.T) {
	t.Parallel()

	now := time.Now()
	p := &titlePolicy{
		enabled:       true,
		promptEnabled: true,
		isTrusted:     func() bool { return false },
		now:           func() time.Time { return now },
	}
	p.prompt = func() { p.canRead() }
	p.log = func(bool) { p.canRead() }

	done := make(chan struct{})
	go func() {
		p.canRead()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callbacks deadlocked while re-entering the policy")
	}
}
