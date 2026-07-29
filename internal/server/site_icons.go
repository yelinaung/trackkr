package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/favicon"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	siteIconFetchBudget = 12 * time.Second
	siteIconClaimLease  = 15 * time.Second
	siteIconShortCache  = 60 * time.Second
)

type siteIconStore interface {
	SiteIcon(context.Context, int64, string) (*db.SiteIconRow, error)
	ClaimSiteIconRefresh(context.Context, int64, string, time.Time, time.Time) (*db.SiteIconRow, bool, error)
	CompleteSiteIconRefresh(
		context.Context,
		int64,
		string,
		[]byte,
		*icon.Details,
		time.Time,
		time.Time,
		time.Time,
	) (*db.SiteIconRow, error)
}

type siteFaviconFetcher interface {
	Fetch(context.Context, string) ([]byte, error)
}

func (h *webHandlers) handleSiteIcon() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			http.NotFound(w, r)
			return
		}

		site := chi.URLParam(r, "site")
		canonical, err := favicon.CanonicalSite(site)
		if err != nil || canonical != site {
			http.NotFound(w, r)
			return
		}
		if !h.codec.validSiteIconSignature(user.ID, site, r.URL.Query().Get("sig")) {
			http.NotFound(w, r)
			return
		}

		now := time.Now()
		row, err := h.siteIcons.SiteIcon(r.Context(), user.ID, site)
		if err == nil && row.ExpiresAt.After(now) {
			h.serveSiteIcon(w, r, row, now)
			return
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			h.fail(w, err, "loading site favicon")
			return
		}

		claimUntil := now.Add(siteIconClaimLease)
		row, claimed, err := h.siteIcons.ClaimSiteIconRefresh(
			r.Context(), user.ID, site, now, claimUntil,
		)
		if err != nil {
			h.fail(w, err, "claiming site favicon refresh")
			return
		}
		if !claimed {
			h.serveSiteIconFor(w, r, row, siteIconShortCache)
			return
		}

		fetchCtx, cancel := context.WithTimeout(r.Context(), siteIconFetchBudget)
		pngBytes, fetchErr := h.favicons.Fetch(fetchCtx, site)
		cancel()

		var details *icon.Details
		if fetchErr == nil {
			validated, validateErr := icon.ValidatePNG(pngBytes)
			if validateErr != nil {
				pngBytes = nil
			} else {
				details = &validated
			}
		}

		attemptedAt := time.Now()
		expiresAt := attemptedAt.AddDate(1, 0, 0)
		row, err = h.siteIcons.CompleteSiteIconRefresh(
			r.Context(),
			user.ID,
			site,
			pngBytes,
			details,
			attemptedAt,
			expiresAt,
			claimUntil,
		)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				h.fail(w, err, "completing site favicon refresh")
				return
			}
			row, err = h.siteIcons.SiteIcon(r.Context(), user.ID, site)
			if err != nil {
				h.fail(w, err, "reloading site favicon")
				return
			}
		}

		// Fetch failures are expected for sites without a conventional icon.
		// The negative row suppresses another outbound request for one year.
		h.serveSiteIcon(w, r, row, attemptedAt)
	}
}

func (h *webHandlers) serveSiteIcon(w http.ResponseWriter, r *http.Request, row *db.SiteIconRow, now time.Time) {
	ttl := row.ExpiresAt.Sub(now)
	if ttl <= 0 {
		ttl = siteIconShortCache
	}
	h.serveSiteIconFor(w, r, row, ttl)
}

func (h *webHandlers) serveSiteIconFor(w http.ResponseWriter, r *http.Request, row *db.SiteIconRow, ttl time.Duration) {
	seconds := max(int64(ttl/time.Second), int64(1))
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(seconds, 10))
	w.Header().Add("Vary", "Cookie")

	if row != nil && len(row.PNG) > 0 && len(row.SHA256) == 32 {
		details, err := icon.ValidatePNG(row.PNG)
		if err != nil || !bytes.Equal(details.Digest[:], row.SHA256) {
			h.serveSiteIconFallback(w, r)
			return
		}
		etag := `"` + hex.EncodeToString(row.SHA256) + `"`
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", etag)
		if matchesETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		//nolint:gosec // G705: ValidatePNG fully decodes a bounded PNG before this write.
		_, _ = w.Write(row.PNG)
		return
	}
	h.serveSiteIconFallback(w, r)
}

func (h *webHandlers) serveSiteIconFallback(w http.ResponseWriter, r *http.Request) {
	site := chi.URLParam(r, "site")
	fill, foreground := appPalette(site)
	monogram := html.EscapeString(appMonogram(site))
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = fmt.Fprintf(
		w,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">`+
			`<rect width="64" height="64" rx="16" fill="%s"/>`+
			`<text x="32" y="41" text-anchor="middle" fill="%s" `+
			`font-family="monospace" font-size="26" font-weight="700">%s</text></svg>`,
		fill,
		foreground,
		monogram,
	)
}
