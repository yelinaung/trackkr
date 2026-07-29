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
	siteIconClaimLease  = 30 * time.Second
	siteIconShortCache  = 60 * time.Second
	siteIconSVGType     = "image/svg+xml"
)

type siteIconStore interface {
	SiteIcon(context.Context, int64, string) (*db.SiteIconRow, error)
	ClaimSiteIconRefresh(context.Context, int64, string, time.Time, time.Time, bool) (*db.SiteIconRow, bool, error)
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
		fresh := err == nil && row.ExpiresAt.After(now)
		if fresh && (siteIconPNGValid(row) || siteIconNegative(row)) {
			h.serveSiteIcon(w, r, row, now)
			return
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			h.fail(w, err, "loading site favicon")
			return
		}

		if h.siteRefresh != nil {
			if fresh {
				h.siteRefresh.EnqueueRepair(user.ID, site)
			} else {
				h.siteRefresh.Enqueue(user.ID, site)
			}
		}
		h.serveSiteIconFor(w, r, row, siteIconShortCache)
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
	if siteIconPNGValid(row) {
		h.setSiteIconCacheHeaders(w, ttl)
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
	if row != nil && (len(row.PNG) > 0 || len(row.SHA256) > 0) {
		ttl = siteIconShortCache
	}
	h.setSiteIconCacheHeaders(w, ttl)
	h.serveSiteIconFallback(w, r)
}

func siteIconPNGValid(row *db.SiteIconRow) bool {
	if row == nil || len(row.PNG) == 0 || len(row.SHA256) != 32 {
		return false
	}
	details, err := icon.ValidatePNG(row.PNG)
	return err == nil && bytes.Equal(details.Digest[:], row.SHA256)
}

func siteIconNegative(row *db.SiteIconRow) bool {
	return row != nil && len(row.PNG) == 0 && len(row.SHA256) == 0
}

func (h *webHandlers) setSiteIconCacheHeaders(w http.ResponseWriter, ttl time.Duration) {
	seconds := max(int64(ttl/time.Second), int64(1))
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(seconds, 10))
	w.Header().Add("Vary", "Cookie")
}

func (h *webHandlers) serveSiteIconFallback(w http.ResponseWriter, r *http.Request) {
	site := chi.URLParam(r, "site")
	fill, foreground := appPalette(site)
	monogram := html.EscapeString(appMonogram(site))
	w.Header().Set("Content-Type", siteIconSVGType)
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
