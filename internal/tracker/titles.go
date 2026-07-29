package tracker

import "sync"

// titlePolicy decides whether a title lookup should be attempted.
//
// It asks once, at the first poll, and never asks again. macOS answers
// AXIsProcessTrusted() from the identity the process was launched with
// and does not revise it: a permission granted while the daemon runs
// stays invisible until the daemon restarts, and one revoked while it
// runs still reports as granted. A periodic recheck would re-ask a
// question whose answer cannot change.
//
// Revocation still takes effect, just not here. The AX calls themselves
// begin failing within a poll or two, trackkr_focused_window_title
// returns NULL, and the record keeps its application name with an empty
// title -- the same degradation as an application that exposes no title
// at all.
type titlePolicy struct {
	enabled       bool
	promptEnabled bool
	isTrusted     func() bool
	prompt        func()
	log           func(bool)

	mu       sync.Mutex
	resolved bool
	checking bool
	trusted  bool
}

func newTitlePolicy(
	cfg *Config,
	isTrusted func() bool,
	prompt func(),
	log func(bool),
) *titlePolicy {
	return &titlePolicy{
		enabled:       cfg.MacOSReadTitles,
		promptEnabled: cfg.MacOSPromptForAccessibility,
		isTrusted:     isTrusted,
		prompt:        prompt,
		log:           log,
	}
}

func (p *titlePolicy) canRead() bool {
	if p == nil || !p.enabled {
		return false
	}

	p.mu.Lock()
	// checking covers re-entry as well as concurrency: the trust check
	// is an injected callback, and one that reaches back into the
	// policy must not recurse.
	if p.resolved || p.checking {
		trusted := p.trusted
		p.mu.Unlock()
		return trusted
	}
	p.checking = true
	p.mu.Unlock()

	trusted := p.isTrusted()

	p.mu.Lock()
	p.trusted = trusted
	p.resolved = true
	p.checking = false
	shouldPrompt := !trusted && p.promptEnabled
	p.mu.Unlock()

	// Outside the lock: both callbacks are injected, and the prompt
	// opens a system dialog. One check means one prompt and one log
	// line without a flag to track either.
	if shouldPrompt {
		p.prompt()
	}
	p.log(trusted)

	return trusted
}
