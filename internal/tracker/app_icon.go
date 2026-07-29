package tracker

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	appIconCacheLimit       = 128
	appIconWorkerQueueLimit = appIconCacheLimit
	appIconPositiveLifetime = 5 * time.Minute
	appIconNegativeLifetime = 30 * time.Second
	appIconRefreshInterval  = 30 * 24 * time.Hour
	appIconStateLimit       = 128
)

type appIconCacheKey struct {
	PID int
	Key string
}

type appIconCacheEntry struct {
	Icon      *icon.App
	ExpiresAt time.Time
}

type appIconLoadRequest struct {
	ctx context.Context
	app appInfo
	key appIconCacheKey
}

type appIconCache struct {
	mu       sync.Mutex
	entries  map[appIconCacheKey]appIconCacheEntry
	inFlight map[appIconCacheKey]struct{}
	requests chan appIconLoadRequest
	stop     chan struct{}
	stopOnce sync.Once
	closed   bool
	now      func() time.Time
	load     func(context.Context, appInfo) *icon.App
	logger   *zerolog.Logger
}

func newAppIconCache(
	now func() time.Time,
	load func(context.Context, appInfo) *icon.App,
	logger *zerolog.Logger,
) *appIconCache {
	cache := &appIconCache{
		entries:  make(map[appIconCacheKey]appIconCacheEntry),
		inFlight: make(map[appIconCacheKey]struct{}),
		requests: make(chan appIconLoadRequest, appIconWorkerQueueLimit),
		stop:     make(chan struct{}),
		now:      now,
		load:     load,
		logger:   logger,
	}
	go cache.run()
	return cache
}

func (c *appIconCache) iconForApp(ctx context.Context, app appInfo) *icon.App {
	key := appIconCacheKey{PID: app.PID, Key: icon.AppKey(app.Name)}
	if err := ctx.Err(); err != nil || key.Key == "" {
		return nil
	}
	now := c.now()

	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && now.Before(entry.ExpiresAt) {
		cached := cloneOptionalIcon(entry.Icon)
		c.mu.Unlock()
		return cached
	}
	if ok {
		delete(c.entries, key)
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if _, exists := c.inFlight[key]; exists {
		c.mu.Unlock()
		return nil
	}
	c.inFlight[key] = struct{}{}
	c.mu.Unlock()

	request := appIconLoadRequest{ctx: ctx, app: app, key: key}
	select {
	case <-c.stop:
		c.cancelLoad(key)
	case c.requests <- request:
	default:
		c.cancelLoad(key)
	}
	return nil
}

func (c *appIconCache) run() {
	for {
		select {
		case <-c.stop:
			return
		case request := <-c.requests:
			select {
			case <-c.stop:
				c.cancelLoad(request.key)
				return
			default:
			}
			if request.ctx.Err() != nil {
				c.cancelLoad(request.key)
				continue
			}
			c.loadAndCache(request)
		}
	}
}

func (c *appIconCache) loadAndCache(request appIconLoadRequest) {
	loaded := c.load(request.ctx, request.app)
	if loaded != nil {
		if loaded.Key != request.key.Key {
			loaded = nil
		} else if _, err := icon.Validate(*loaded); err != nil {
			loaded = nil
		}
	}
	if request.ctx.Err() != nil {
		c.cancelLoad(request.key)
		return
	}

	lifetime := appIconPositiveLifetime
	if loaded == nil {
		lifetime = appIconNegativeLifetime
	}
	owned := cloneOptionalIcon(loaded)
	stored := c.finishLoad(request.key, owned, c.now().Add(lifetime))

	if stored && loaded == nil && c.logger != nil {
		c.logger.Debug().
			Str("app", request.app.Name).
			Int("pid", request.app.PID).
			Msg("application icon unavailable")
	}
}

func (c *appIconCache) finishLoad(
	key appIconCacheKey,
	loaded *icon.App,
	expiresAt time.Time,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inFlight, key)
	if c.closed {
		return false
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= appIconCacheLimit {
		c.evictOneLocked()
	}
	c.entries[key] = appIconCacheEntry{Icon: loaded, ExpiresAt: expiresAt}
	return true
}

func (c *appIconCache) cancelLoad(key appIconCacheKey) {
	c.mu.Lock()
	delete(c.inFlight, key)
	c.mu.Unlock()
}

// Close prevents new icon lookups without waiting for a possibly blocked native
// conversion.
func (c *appIconCache) Close() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.stop)
	})
}

func (c *appIconCache) evictOneLocked() {
	var oldestKey appIconCacheKey
	var oldestTime time.Time
	first := true
	for key, entry := range c.entries {
		if first || entry.ExpiresAt.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.ExpiresAt
			first = false
		}
	}
	if !first {
		delete(c.entries, oldestKey)
	}
}

func cloneOptionalIcon(appIcon *icon.App) *icon.App {
	if appIcon == nil {
		return nil
	}
	cloned := icon.Clone(*appIcon)
	return &cloned
}

func appIconForObservedProcess(expectedName, observedName string, png []byte) *icon.App {
	expectedKey := icon.AppKey(expectedName)
	if expectedKey == "" || icon.AppKey(observedName) != expectedKey {
		return nil
	}
	appIcon := icon.App{Key: expectedKey, PNG: append([]byte(nil), png...)}
	if _, err := icon.Validate(appIcon); err != nil {
		return nil
	}
	return &appIcon
}

type queuedAppIcon struct {
	Digest       [sha256.Size]byte
	LastQueuedAt time.Time
}

func (t *Tracker) maybeEnqueueAppIcon(info WindowInfo, now time.Time) {
	if info.AppIcon == nil || t.reporter == nil {
		return
	}

	digest := sha256.Sum256(info.AppIcon.PNG)
	previous, exists := t.appIcons[info.AppIcon.Key]
	if exists && previous.Digest == digest && now.Before(previous.LastQueuedAt.Add(appIconRefreshInterval)) {
		return
	}
	if _, err := icon.Validate(*info.AppIcon); err != nil {
		return
	}
	if !t.reporter.EnqueueAppIcon(*info.AppIcon) {
		return
	}
	if !exists && len(t.appIcons) >= appIconStateLimit {
		t.evictAppIconState()
	}
	t.appIcons[info.AppIcon.Key] = queuedAppIcon{
		Digest:       digest,
		LastQueuedAt: now,
	}
}

func (t *Tracker) evictAppIconState() {
	oldestKey := ""
	var oldest time.Time
	for key, state := range t.appIcons {
		if oldestKey == "" || state.LastQueuedAt.Before(oldest) ||
			(state.LastQueuedAt.Equal(oldest) && key < oldestKey) {
			oldestKey = key
			oldest = state.LastQueuedAt
		}
	}
	delete(t.appIcons, oldestKey)
}
