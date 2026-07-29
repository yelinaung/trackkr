package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	siteIconWorkerCount      = 4
	siteIconQueueLimit       = 64
	siteIconUserPendingLimit = 16
	siteIconRateLimit        = 60
	siteIconRateWindow       = time.Hour
	siteIconDatabaseBudget   = 3 * time.Second
)

type siteIconRefreshQueue interface {
	Enqueue(userID int64, site string) bool
	EnqueueRepair(userID int64, site string) bool
}

type siteIconRefreshJob struct {
	userID int64
	site   string
	repair bool
}

type siteIconRefresherConfig struct {
	workers          int
	queueLimit       int
	userPendingLimit int
	rateLimit        int
	rateWindow       time.Duration
	now              func() time.Time
}

func defaultSiteIconRefresherConfig() siteIconRefresherConfig {
	return siteIconRefresherConfig{
		workers:          siteIconWorkerCount,
		queueLimit:       siteIconQueueLimit,
		userPendingLimit: siteIconUserPendingLimit,
		rateLimit:        siteIconRateLimit,
		rateWindow:       siteIconRateWindow,
		now:              time.Now,
	}
}

type siteIconRefresher struct {
	store   siteIconStore
	fetcher siteFaviconFetcher
	logger  *zerolog.Logger
	config  siteIconRefresherConfig
	limiter *slidingWindowLimiter

	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan siteIconRefreshJob
	wait   sync.WaitGroup

	mu            sync.Mutex
	pending       map[siteIconRefreshJob]struct{}
	pendingByUser map[int64]int
}

func newSiteIconRefresher(
	store siteIconStore,
	fetcher siteFaviconFetcher,
	logger *zerolog.Logger,
	config siteIconRefresherConfig,
) *siteIconRefresher {
	ctx, cancel := context.WithCancel(context.Background())
	r := &siteIconRefresher{
		store:         store,
		fetcher:       fetcher,
		logger:        logger,
		config:        config,
		limiter:       newSlidingWindowLimiter(config.rateLimit, config.rateWindow),
		ctx:           ctx,
		cancel:        cancel,
		jobs:          make(chan siteIconRefreshJob, config.queueLimit),
		pending:       make(map[siteIconRefreshJob]struct{}),
		pendingByUser: make(map[int64]int),
	}
	for range config.workers {
		r.wait.Go(r.run)
	}
	return r
}

func (r *siteIconRefresher) Enqueue(userID int64, site string) bool {
	return r.enqueue(siteIconRefreshJob{userID: userID, site: site})
}

func (r *siteIconRefresher) EnqueueRepair(userID int64, site string) bool {
	return r.enqueue(siteIconRefreshJob{userID: userID, site: site, repair: true})
}

func (r *siteIconRefresher) enqueue(job siteIconRefreshJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.pending[job]; exists {
		return true
	}
	if len(r.pending) >= r.config.queueLimit {
		return false
	}
	if r.pendingByUser[job.userID] >= r.config.userPendingLimit {
		return false
	}
	select {
	case <-r.ctx.Done():
		return false
	case r.jobs <- job:
		r.pending[job] = struct{}{}
		r.pendingByUser[job.userID]++
		return true
	default:
		return false
	}
}

func (r *siteIconRefresher) Close() {
	r.cancel()
	r.wait.Wait()
}

func (r *siteIconRefresher) run() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case job := <-r.jobs:
			reservedAt := r.config.now()
			allowed, retryAfter := r.limiter.reserve(job.userID, reservedAt)
			if !allowed {
				r.deferJob(job, retryAfter)
				continue
			}
			if !r.refresh(r.ctx, job) {
				r.limiter.refund(job.userID, reservedAt)
			}
			r.finish(job)
		}
	}
}

func (r *siteIconRefresher) deferJob(job siteIconRefreshJob, retryAfter time.Duration) {
	r.logger.Debug().
		Int64("user_id", job.userID).
		Dur("retry_after", retryAfter).
		Msg("site favicon refresh rate limited")
	r.wait.Go(func() {
		timer := time.NewTimer(retryAfter)
		defer timer.Stop()
		select {
		case <-r.ctx.Done():
			r.finish(job)
		case <-timer.C:
			select {
			case <-r.ctx.Done():
				r.finish(job)
			case r.jobs <- job:
			}
		}
	})
}

func (r *siteIconRefresher) finish(job siteIconRefreshJob) {
	r.mu.Lock()
	delete(r.pending, job)
	r.pendingByUser[job.userID]--
	if r.pendingByUser[job.userID] == 0 {
		delete(r.pendingByUser, job.userID)
	}
	r.mu.Unlock()
}

func (r *siteIconRefresher) refresh(ctx context.Context, job siteIconRefreshJob) bool {
	now := r.config.now()
	claimUntil := now.Add(siteIconClaimLease)
	databaseCtx, cancelDatabase := context.WithTimeout(ctx, siteIconDatabaseBudget)
	_, claimed, err := r.store.ClaimSiteIconRefresh(
		databaseCtx, job.userID, job.site, now, claimUntil, job.repair,
	)
	cancelDatabase()
	if err != nil {
		r.logger.Error().Err(err).Msg("claiming site favicon refresh")
		return false
	}
	if !claimed {
		return false
	}

	fetchCtx, cancelFetch := context.WithTimeout(ctx, siteIconFetchBudget)
	pngBytes, fetchErr := r.fetcher.Fetch(fetchCtx, job.site)
	cancelFetch()

	var details *icon.Details
	if fetchErr == nil {
		validated, validateErr := icon.ValidatePNG(pngBytes)
		if validateErr == nil {
			details = &validated
		} else {
			fetchErr = fmt.Errorf("validating fetched favicon: %w", validateErr)
		}
	}
	if fetchErr != nil {
		// A fetcher may return partial bytes with an error. Never let those
		// bytes reach the database without matching validated metadata.
		pngBytes = nil
		r.logger.Debug().Err(fetchErr).Msg("site favicon unavailable")
	}

	attemptedAt := r.config.now()
	expiresAt := attemptedAt.AddDate(1, 0, 0)
	databaseCtx, cancelDatabase = context.WithTimeout(ctx, siteIconDatabaseBudget)
	_, err = r.store.CompleteSiteIconRefresh(
		databaseCtx,
		job.userID,
		job.site,
		pngBytes,
		details,
		attemptedAt,
		expiresAt,
		claimUntil,
	)
	cancelDatabase()
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.logger.Error().Err(err).Msg("completing site favicon refresh")
	}
	return true
}
