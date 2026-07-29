// Package favicon securely fetches and normalizes public website favicons.
package favicon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"

	_ "github.com/sergeymakinen/go-ico"
	"github.com/yelinaung/trackkr/internal/icon"
	"golang.org/x/image/draw"
	"golang.org/x/net/html"
	"golang.org/x/net/idna"
)

const (
	maxSourceBytes       = 256 << 10
	maxHTMLBytes         = 128 << 10
	maxSourceDimension   = 1024
	maxSourcePixels      = 1024 * 1024
	maxDiscoveredIcons   = 4
	normalizedDimension  = 64
	conventionalIconPath = "/favicon.ico"
)

var (
	// ErrNoFavicon means the site definitively has no icon usable under the
	// fetcher's size, format, and transport policy.
	ErrNoFavicon  = errors.New("no usable favicon")
	errDefinitive = errors.New("definitive favicon failure")
)

// Fetcher retrieves favicons through an SSRF-restricted HTTP client.
type Fetcher struct {
	client *http.Client
}

// NewFetcher returns a fetcher with bounded requests and DNS-pinned dials.
func NewFetcher() *Fetcher {
	return &Fetcher{client: newSafeHTTPClient()}
}

// Fetch retrieves a site's conventional favicon, then falls back to icon
// links from its public home page. The result is always a bounded 64px PNG.
func (f *Fetcher) Fetch(ctx context.Context, site string) ([]byte, error) {
	canonical, err := CanonicalSite(site)
	if err != nil || canonical != site {
		return nil, errors.New("invalid site key")
	}

	origin := &url.URL{Scheme: "https", Host: canonical}
	direct := origin.ResolveReference(&url.URL{Path: conventionalIconPath})
	directData, directErr := f.fetchImage(ctx, direct)
	if directErr == nil {
		return directData, nil
	}

	candidates, discoverErr := f.discoverIcons(ctx, origin)
	if discoverErr != nil {
		return nil, classifyFaviconFailure(directErr, discoverErr)
	}

	failures := []error{directErr}
	for _, candidate := range candidates {
		data, fetchErr := f.fetchImage(ctx, candidate)
		if fetchErr == nil {
			return data, nil
		}
		failures = append(failures, fetchErr)
	}
	return nil, classifyFaviconFailure(failures...)
}

func classifyFaviconFailure(failures ...error) error {
	for _, err := range failures {
		if err != nil && !errors.Is(err, errDefinitive) {
			return errors.Join(failures...)
		}
	}
	return ErrNoFavicon
}

func definitiveFailure(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errDefinitive, fmt.Sprintf(format, args...))
}

func statusFailure(resource string, status int) error {
	if status == http.StatusNotFound || status == http.StatusGone {
		return definitiveFailure("%s returned status %d", resource, status)
	}
	return fmt.Errorf("%s returned status %d", resource, status)
}

// CanonicalSite validates and normalizes a DNS hostname used as a cache key.
// IP literals and single-label names are excluded before any network access.
func CanonicalSite(site string) (string, error) {
	if site == "" || site != strings.TrimSpace(site) || len(site) > 253 {
		return "", errors.New("invalid site length or whitespace")
	}
	if strings.HasSuffix(site, ".") || !strings.Contains(site, ".") {
		return "", errors.New("site must be a rooted public hostname")
	}
	ascii, err := idna.Lookup.ToASCII(site)
	if err != nil {
		return "", fmt.Errorf("normalizing site: %w", err)
	}
	ascii = strings.ToLower(ascii)
	if _, parseErr := netip.ParseAddr(ascii); parseErr == nil {
		return "", errors.New("IP literals are not site keys")
	}
	if strings.ContainsAny(ascii, "/:@[]") {
		return "", errors.New("site contains URL delimiters")
	}
	for label := range strings.SplitSeq(ascii, ".") {
		if !validDNSLabel(label) {
			return "", errors.New("site contains an invalid DNS label")
		}
	}
	return ascii, nil
}

func validDNSLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, char := range []byte(label) {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func (f *Fetcher) discoverIcons(ctx context.Context, origin *url.URL) ([]*url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "trackkr-favicon/1")

	response, err := f.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusFailure("home page", response.StatusCode)
	}
	if response.ContentLength > maxHTMLBytes {
		return nil, definitiveFailure("home page is too large")
	}

	limited := io.LimitReader(response.Body, maxHTMLBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxHTMLBytes {
		return nil, definitiveFailure("home page is too large")
	}
	links := iconLinks(data, response.Request.URL)
	slices.SortStableFunc(links, func(a, b *url.URL) int {
		return faviconPriority(a) - faviconPriority(b)
	})
	return links, nil
}

func iconLinks(document []byte, base *url.URL) []*url.URL {
	tokenizer := html.NewTokenizer(bytes.NewReader(document))
	var result []*url.URL
	seen := make(map[string]struct{})
	for len(result) < maxDiscoveredIcons {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return result
		case html.TextToken, html.EndTagToken, html.CommentToken, html.DoctypeToken:
			continue
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data != "link" {
				continue
			}
			var rel, href string
			for _, attribute := range token.Attr {
				switch attribute.Key {
				case "rel":
					rel = strings.ToLower(attribute.Val)
				case "href":
					href = attribute.Val
				}
			}
			if !slices.Contains(strings.Fields(rel), "icon") || href == "" {
				continue
			}
			reference, err := url.Parse(href)
			if err != nil {
				continue
			}
			candidate := base.ResolveReference(reference)
			if err := validateRemoteURL(candidate); err != nil {
				continue
			}
			key := candidate.String()
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, candidate)
		}
	}
	return result
}

func (f *Fetcher) fetchImage(ctx context.Context, target *url.URL) ([]byte, error) {
	if err := validateRemoteURL(target); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "image/png,image/x-icon,image/jpeg,image/gif;q=0.8,*/*;q=0.1")
	request.Header.Set("User-Agent", "trackkr-favicon/1")

	response, err := f.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, statusFailure("favicon", response.StatusCode)
	}
	if response.ContentLength > maxSourceBytes {
		return nil, definitiveFailure("favicon is too large")
	}
	if mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type")); parseErr == nil && mediaType == "image/svg+xml" {
		return nil, definitiveFailure("SVG favicons are not accepted")
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSourceBytes {
		return nil, definitiveFailure("favicon is too large")
	}
	normalized, err := normalizeImage(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDefinitive, err)
	}
	return normalized, nil
}

func validateRemoteURL(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.User != nil || target.Port() != "" {
		return errors.New("favicon URL must be an HTTPS origin without credentials or a port")
	}
	canonical, err := CanonicalSite(target.Hostname())
	if err != nil {
		return err
	}
	target.Host = canonical
	return nil
}

func normalizeImage(data []byte) ([]byte, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding favicon configuration: %w", err)
	}
	if config.Width < 1 || config.Height < 1 ||
		config.Width > maxSourceDimension || config.Height > maxSourceDimension ||
		config.Width*config.Height > maxSourcePixels {
		return nil, errors.New("favicon dimensions exceed the source limit")
	}

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding favicon: %w", err)
	}
	bounds := source.Bounds()
	if bounds.Dx() != config.Width || bounds.Dy() != config.Height {
		return nil, errors.New("favicon dimensions changed during decode")
	}

	scale := math.Min(
		float64(normalizedDimension)/float64(bounds.Dx()),
		float64(normalizedDimension)/float64(bounds.Dy()),
	)
	width := max(1, int(math.Round(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Round(float64(bounds.Dy())*scale)))
	x := (normalizedDimension - width) / 2
	y := (normalizedDimension - height) / 2
	destination := image.NewNRGBA(image.Rect(0, 0, normalizedDimension, normalizedDimension))
	draw.CatmullRom.Scale(
		destination,
		image.Rect(x, y, x+width, y+height),
		source,
		bounds,
		draw.Over,
		nil,
	)

	var output bytes.Buffer
	if err := png.Encode(&output, destination); err != nil {
		return nil, fmt.Errorf("encoding normalized favicon: %w", err)
	}
	if _, err := icon.ValidatePNG(output.Bytes()); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func faviconPriority(target *url.URL) int {
	switch strings.ToLower(path.Ext(target.Path)) {
	case ".png", ".ico":
		return 0
	case ".jpg", ".jpeg", ".gif":
		return 1
	default:
		return 2
	}
}
