package favicon

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/yelinaung/trackkr/internal/icon"
)

const testSite = "example.com"

func TestCanonicalSite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "canonical", raw: testSite, want: testSite},
		{name: "uppercase", raw: "EXAMPLE.com", want: testSite},
		{name: "unicode", raw: "bücher.de", want: "xn--bcher-kva.de"},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace", raw: " example.com", wantErr: true},
		{name: "trailing dot", raw: "example.com.", wantErr: true},
		{name: "single label", raw: "localhost", wantErr: true},
		{name: "IPv4", raw: "127.0.0.1", wantErr: true},
		{name: "credentials", raw: "user@example.com", wantErr: true},
		{name: "port", raw: "example.com:443", wantErr: true},
		{name: "empty label", raw: "example..com", wantErr: true},
		{name: "leading hyphen", raw: "-bad.example", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalSite(test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("CanonicalSite(%q) error = %v, wantErr %v", test.raw, err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("CanonicalSite(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestIsPublicAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		want    bool
	}{
		{address: "93.184.216.34", want: true},
		{address: "2606:2800:220:1:248:1893:25c8:1946", want: true},
		{address: "127.0.0.1", want: false},
		{address: "10.0.0.1", want: false},
		{address: "100.64.0.1", want: false},
		{address: "169.254.169.254", want: false},
		{address: "192.0.2.1", want: false},
		{address: "192.88.99.1", want: false},
		{address: "::1", want: false},
		{address: "fc00::1", want: false},
		{address: "2001:db8::1", want: false},
		{address: "64:ff9b::c000:201", want: false},
		{address: "64:ff9b:1::1", want: false},
		{address: "fec0::1", want: false},
		{address: "2001:2::1", want: false},
		{address: "2001:10::1", want: false},
		{address: "2002:c000:201::1", want: false},
		{address: "3fff::1", want: false},
		{address: "5f00::1", want: false},
		{address: "2001:3::1", want: true},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()
			if got := isPublicAddress(netip.MustParseAddr(test.address)); got != test.want {
				t.Errorf("isPublicAddress(%s) = %v, want %v", test.address, got, test.want)
			}
		})
	}
}

func TestSafeHTTPClientRejectsOversizedResponseHeaders(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", maxResponseHeaders+1))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := newSafeHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	transport.DialContext = (&net.Dialer{}).DialContext
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("client accepted response headers above its configured bound")
	}
}

func TestSafeDialerRejectsMixedPublicAndPrivateDNS(t *testing.T) {
	t.Parallel()

	dialer := &recordingDialer{}
	safe := &safeDialer{
		resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		}},
		dialer: dialer,
	}
	if _, err := safe.dialContext(t.Context(), "tcp", "example.com:443"); err == nil {
		t.Fatal("dialContext accepted a private DNS answer")
	}
	if len(dialer.addresses) != 0 {
		t.Errorf("dialer reached %v before rejecting DNS", dialer.addresses)
	}
}

func TestSafeDialerPinsResolvedAddress(t *testing.T) {
	t.Parallel()

	dialer := &recordingDialer{}
	safe := &safeDialer{
		resolver: staticResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
		dialer:   dialer,
	}
	connection, err := safe.dialContext(t.Context(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("dialContext: %v", err)
	}
	_ = connection.Close()
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "93.184.216.34:443" {
		t.Errorf("dialed %v, want resolved public address", dialer.addresses)
	}
}

func TestIconLinksKeepsSafeHTTPSCandidates(t *testing.T) {
	t.Parallel()

	document := []byte(`<html><head>
<link rel="icon" href="http://example.com/insecure.ico">
<link rel="shortcut icon" href="/icon.svg">
<link rel="icon" href="/icon.png">
<link rel="icon" href="/icon.png">
</head></html>`)
	base, err := url.Parse("https://example.com/path")
	if err != nil {
		t.Fatal(err)
	}
	got := iconLinks(document, base)
	if len(got) != 2 {
		t.Fatalf("links = %v, want two unique HTTPS links", got)
	}
	if got[0].String() != "https://example.com/icon.svg" || got[1].String() != "https://example.com/icon.png" {
		t.Errorf("links = %v", got)
	}
}

func TestFetcherFallsBackToHTMLIcon(t *testing.T) {
	t.Parallel()

	pngBytes := testImagePNG(t, 24, 24)
	requests := make([]string, 0, 3)
	fetcher := &Fetcher{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.Path)
		switch request.URL.Path {
		case conventionalIconPath:
			return testResponse(request, http.StatusNotFound, "text/plain", nil), nil
		case conventionalPNGIconPath:
			return testResponse(request, http.StatusNotFound, "text/plain", nil), nil
		case "":
			return testResponse(request, http.StatusOK, "text/html", []byte(`<link rel="icon" href="/assets/icon.png">`)), nil
		case "/assets/icon.png":
			return testResponse(request, http.StatusOK, "image/png", pngBytes), nil
		default:
			return nil, errors.New("unexpected request")
		}
	})}}

	got, err := fetcher.Fetch(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	assertNormalized(t, got)
	if strings.Join(requests, ",") != "/favicon.ico,/favicon.png,,/assets/icon.png" {
		t.Errorf("requests = %v", requests)
	}
}

func TestFetcherFallsBackToConventionalPNG(t *testing.T) {
	t.Parallel()

	pngBytes := testImagePNG(t, 32, 32)
	requests := make([]string, 0, 2)
	fetcher := &Fetcher{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.Path)
		switch request.URL.Path {
		case conventionalIconPath:
			return testResponse(request, http.StatusNotFound, "text/plain", nil), nil
		case conventionalPNGIconPath:
			return testResponse(request, http.StatusOK, "image/png", pngBytes), nil
		case "":
			return testResponse(
				request,
				http.StatusOK,
				"text/html",
				[]byte(`<meta http-equiv="refresh" content="0; url=./2026/index.html">`),
			), nil
		default:
			return nil, errors.New("unexpected request")
		}
	})}}

	got, err := fetcher.Fetch(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	assertNormalized(t, got)
	if strings.Join(requests, ",") != "/favicon.ico,/favicon.png" {
		t.Errorf("requests = %v", requests)
	}
}

func TestFetcherClassifiesDefinitiveAbsence(t *testing.T) {
	t.Parallel()

	fetcher := &Fetcher{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusNotFound, "text/plain", nil), nil
	})}}

	_, err := fetcher.Fetch(t.Context(), testSite)
	if !errors.Is(err, ErrNoFavicon) {
		t.Fatalf("Fetch error = %v, want ErrNoFavicon", err)
	}
}

func TestFetcherPreservesTransientFailure(t *testing.T) {
	t.Parallel()

	fetcher := &Fetcher{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == conventionalIconPath {
			return nil, context.DeadlineExceeded
		}
		return testResponse(request, http.StatusServiceUnavailable, "text/plain", nil), nil
	})}}

	_, err := fetcher.Fetch(t.Context(), testSite)
	if err == nil {
		t.Fatal("Fetch unexpectedly succeeded")
	}
	if errors.Is(err, ErrNoFavicon) {
		t.Fatalf("Fetch classified transient error as definitive: %v", err)
	}
}

type staticResolver struct {
	addresses []netip.Addr
}

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addresses, nil
}

type recordingDialer struct {
	addresses []string
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testResponse(request *http.Request, status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}
}

func testImagePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			canvas.SetNRGBA(x, y, color.NRGBA{R: 0x1f, G: 0x6f, B: 0x5f, A: 0xff})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		t.Fatalf("encoding PNG fixture: %v", err)
	}
	return output.Bytes()
}

// assertNormalized checks that the fetch path actually normalized its
// result rather than passing the source bytes through.
//
// ValidatePNG alone cannot tell: every fixture here is already a valid
// PNG, so returning the untouched 24x24 or 32x32 source would satisfy
// it. Asserting the canvas size is what makes these two tests the
// regression cover for the normalizer living in another package.
func assertNormalized(t *testing.T, data []byte) {
	t.Helper()
	details, err := icon.ValidatePNG(data)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if details.Width != icon.NormalizedDimension || details.Height != icon.NormalizedDimension {
		t.Errorf("result is %dx%d, want %dx%[3]d: the fetch path did not normalize",
			details.Width, details.Height, icon.NormalizedDimension)
	}
}
