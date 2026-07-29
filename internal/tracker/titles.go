package tracker

import (
	"sync"
	"time"
)

type trustState int

const (
	trustUnknown trustState = iota
	trustGranted
	trustDenied

	trustRecheckInterval = 2 * time.Minute
)

// titlePolicy caches Accessibility trust and decides whether a title
// lookup should be attempted.
type titlePolicy struct {
	enabled       bool
	promptEnabled bool
	isTrusted     func() bool
	prompt        func()
	now           func() time.Time
	log           func(bool)

	mu        sync.Mutex
	state     trustState
	checking  bool
	prompted  bool
	nextCheck time.Time
}

func newTitlePolicy(
	cfg *Config,
	isTrusted func() bool,
	prompt func(),
	now func() time.Time,
	log func(bool),
) *titlePolicy {
	return &titlePolicy{
		enabled:       cfg.MacOSReadTitles,
		promptEnabled: cfg.MacOSPromptForAccessibility,
		isTrusted:     isTrusted,
		prompt:        prompt,
		now:           now,
		log:           log,
	}
}

func (p *titlePolicy) canRead() bool {
	if p == nil || !p.enabled {
		return false
	}

	now := p.now()
	p.mu.Lock()
	if p.state != trustUnknown && now.Before(p.nextCheck) {
		trusted := p.state == trustGranted
		p.mu.Unlock()
		return trusted
	}
	if p.checking {
		trusted := p.state == trustGranted
		p.mu.Unlock()
		return trusted
	}
	p.checking = true
	p.mu.Unlock()

	trusted := p.isTrusted()
	state := trustDenied
	if trusted {
		state = trustGranted
	}

	p.mu.Lock()
	shouldLog := p.state != state
	p.state = state
	p.checking = false
	p.nextCheck = now.Add(trustRecheckInterval)
	shouldPrompt := !trusted && p.promptEnabled && !p.prompted
	if shouldPrompt {
		p.prompted = true
	}
	p.mu.Unlock()

	if shouldPrompt {
		p.prompt()
	}
	if shouldLog {
		p.log(trusted)
	}

	return trusted
}
