# Phase 8: Server-Fetched Site Favicons

## Decision

Trackkr fetches website favicons from the server when an authenticated
dashboard first displays a canonical public site. Successful and failed
attempts are cached for one year in PostgreSQL.

This deliberately changes the earlier privacy decision in Phase 6. The
Firefox extension still receives no tracked-site host permissions and sends
no favicon bytes. Instead, the Trackkr server makes a bounded outbound HTTPS
request using the hostname already present in the user's activity data. This
reveals the Trackkr server's address to the requested site and any public icon
origin selected by that site's HTML. Operators who do not accept that trade-off
should not deploy this phase.

The cache remains user-scoped. That avoids exposing whether another account
has already visited a site, at the cost of one request per user and site per
year rather than one global request per site.

```text
site total -> signed dashboard image URL -> user-scoped cache lookup
                                               |
                                      expired or absent
                                               |
                                               v
                            SSRF-restricted HTTPS favicon fetch
                                               |
                                               v
                                normalized 64x64 PNG or failure
                                               |
                                               v
                                       one-year cache row
```

## Scope

- Canonical DNS hostnames from the existing per-site totals.
- Lazy fetches initiated by authenticated dashboard image requests.
- Direct `/favicon.ico` lookup followed by HTML icon-link discovery.
- PNG, JPEG, GIF, and ICO source decoding into a 64x64 PNG.
- Positive and negative cache entries with a one-year expiry.
- A colour-matched monogram when no favicon is available.

## Non-Goals

- Fetching favicon bytes in the Firefox extension.
- Adding tracked-site host permissions to the extension.
- HTTP-only sites, explicit ports, IP literals, or single-label hostnames.
- SVG decoding or sanitization.
- Backfilling icons for sites that are never displayed on the dashboard.
- Reusing one account's cached icon for another account.

## Identity And Authorization

`favicon.CanonicalSite` is the normative cache-key function. It trims nothing:
leading or trailing whitespace is invalid. It converts Unicode names with the
IDNA lookup profile, lowercases the ASCII result, and then requires a DNS name
with at least two valid labels. Trailing dots, empty labels, URL delimiters,
credentials, ports, bracketed addresses, and all IP literals are rejected.

The dashboard emits an icon URL only when the site total is already in that
canonical form. The URL contains an HMAC over the user ID and site key. The
image handler requires both the current authenticated session and a matching
signature before consulting the cache or making an outbound request. Invalid
or hand-written site URLs return 404.

This signature is an authorization boundary, not a general-purpose favicon
proxy token. A user can obtain it only for a site rendered from their own
activity totals, and a token copied between users does not validate.

## Fetch Boundary

All outbound URLs must use HTTPS without credentials or an explicit port. The
fetcher disables environment proxies, permits at most three redirects, and
validates every redirect target. Its resolver rejects the entire DNS answer if
any returned address is loopback, private, link-local, carrier-grade NAT,
documentation, benchmark, multicast, or otherwise non-public. The accepted
address is then passed directly to the dialer so a second DNS lookup cannot
redirect the connection.

Requests have an eight-second client timeout and a four-second response-header
timeout. A background worker supplies a twelve-second outer budget. The
fetcher first tries `https://<site>/favicon.ico`; if that is unusable, it reads
at most 128 KiB of the HTTPS home page and tries up to four unique safe
`<link rel="icon">` targets.

Source images are limited to 256 KiB, 1024 pixels on either axis, and one
million pixels total. `image.DecodeConfig` enforces dimensions before the full
decode. SVG is rejected. Accepted raster images are fitted into a transparent
64x64 canvas, encoded as PNG, and checked again with the shared bounded PNG
validator before storage.

## Cache Contract

Migration `004_site_icons` adds a user-scoped row keyed by `(user_id, site)`.
A row stores normalized PNG bytes and their digest when a fetch succeeds. A
failed attempt stores no image but still advances `attempted_at` and
`expires_at` by one calendar year. A failed refresh retains an older PNG, so a
temporary site failure does not replace useful artwork with a fallback.

The image handler never performs outbound I/O. It queues a refresh without
blocking and immediately serves the existing image or a short-lived monogram.
Four workers consume a 64-entry global queue. At most 16 jobs may be pending
for one user, and one user may start at most 60 claimed refreshes per hour.
No-op and failed claims refund their limiter reservation. Duplicate user/site
jobs share one pending slot. A rate-limited job keeps that slot, logs the
deferral at debug level, and retries when the limiter permits it.

An atomic 30-second database lease allows only one worker to refresh an
expired row. Completion updates a row only when the lease token still matches,
preventing an older fetch from overwriting a newer claim. Claims and
completions take a site-icon-specific per-user transaction advisory lock while
pruning, so concurrent refreshes cannot leave more than 2,048 cache rows for
that user or contend with application-icon retention.

Image responses are private, vary on the session cookie, and use a digest ETag.
The server fully validates the stored PNG and verifies its digest before
writing bytes. A fresh row whose PNG or digest is corrupt queues a forced
repair despite its annual expiry. Positive and negative responses inherit the
remaining annual cache lifetime. A corrupt PNG or digest mismatch uses the
short cache lifetime so a repaired row is not hidden for a year. A missing or
invalid icon always degrades to a deterministic SVG monogram without changing
activity totals.

## Verification

Automated coverage includes:

- canonical, Unicode, malformed, local, and IP site keys;
- public-address filtering, mixed public/private DNS answers, and DNS pinning;
- PNG and ICO normalization plus oversized-dimension rejection;
- safe HTML icon discovery and direct-to-HTML fallback order;
- authenticated fetch, positive cache, negative cache, one-year expiry, ETag,
  cookie isolation, invalid signature, and digest mismatch behaviour;
- non-blocking delivery, bounded worker concurrency, and per-user pending caps;
- database refresh lifecycle, stale-image retention, concurrent claims, and
  concurrency-safe 2,048-row retention;
- dashboard image rendering and monogram fallback.

Manual verification:

1. Open a dashboard day containing several public sites and confirm icons
   appear without changing extension permissions.
2. Reload the day and confirm no new outbound favicon requests occur.
3. Display a site without a usable favicon and confirm its monogram remains
   stable across reloads.
4. Display a local, single-label, or IP-literal URL and confirm Trackkr does
   not make an outbound request for it.
5. Inspect one cache row and confirm `expires_at` is one calendar year after
   `attempted_at`.
