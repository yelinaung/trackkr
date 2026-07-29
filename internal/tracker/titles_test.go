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

// The trust check is a cgo call whose answer is fixed for the life of
// the process, so asking twice costs something and learns nothing.
func TestTitlePolicyChecksTrustOnce(t *testing.T) {
	t.Parallel()

	checks := 0
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { checks++; return true },
		prompt:    func() {},
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
}

// A later change of heart from the trust checker must not reach the
// caller: the real API cannot change its answer, and a policy that
// tracked one would report a permission the AX calls do not have.
func TestTitlePolicyIgnoresLaterTrustChanges(t *testing.T) {
	t.Parallel()

	trusted := false
	var logged []bool
	p := &titlePolicy{
		enabled:   true,
		isTrusted: func() bool { return trusted },
		prompt:    func() {},
		log:       func(value bool) { logged = append(logged, value) },
	}

	if p.canRead() {
		t.Error("canRead = true, want the denied first answer")
	}

	trusted = true
	for range 5 {
		if p.canRead() {
			t.Fatal("canRead = true, want the first answer to stand")
		}
	}

	if len(logged) != 1 || logged[0] {
		t.Errorf("logged = %v, want exactly one denied line", logged)
	}
}

func TestTitlePolicyPromptsOnce(t *testing.T) {
	t.Parallel()

	prompts := 0
	p := &titlePolicy{
		enabled:       true,
		promptEnabled: true,
		isTrusted:     func() bool { return false },
		prompt:        func() { prompts++ },
		log:           func(bool) {},
	}

	if p.canRead() {
		t.Error("canRead = true, want denied")
	}
	for range 3 {
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

	p := &titlePolicy{
		enabled:       true,
		promptEnabled: true,
		isTrusted:     func() bool { return false },
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
