package tracker

import (
	"testing"
	"time"

	"github.com/yelinaung/trackkr/internal/identity"
)

const (
	testExtensionURL    = "https://example.com/"
	testExtensionOrigin = "moz-extension://abc123"
)

func testExtensionStart() time.Time {
	return time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
}

// The route names the producer. A caller-supplied application name would let a
// Chrome build claim Firefox coverage and subtract it from desktop Firefox.
func TestToRecordTakesIdentityFromTheRoute(t *testing.T) {
	t.Parallel()

	start := testExtensionStart()
	in := extensionRecord{
		URL:       testExtensionURL,
		Title:     "docs",
		StartedAt: start,
		EndedAt:   start.Add(time.Minute),
	}

	tests := []struct {
		name     string
		producer identity.Producer
		appName  string
	}{
		{"firefox route", identity.ProducerFirefox, extensionAppName},
		{"chrome route", identity.ProducerChrome, chromeAppName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := toRecord(&in, tt.producer, tt.appName)
			if !ok {
				t.Fatal("toRecord rejected a valid record")
			}
			if got.Producer != tt.producer {
				t.Errorf("producer = %q, want %q", got.Producer, tt.producer)
			}
			if got.AppName != tt.appName {
				t.Errorf("app name = %q, want %q", got.AppName, tt.appName)
			}
		})
	}
}

// An extension mints the ID when the segment starts and replays it from its own
// durable queue, so a canonical value is preserved rather than replaced.
func TestToRecordPreservesACanonicalRecordID(t *testing.T) {
	t.Parallel()

	supplied, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	start := testExtensionStart()

	got, ok := toRecord(&extensionRecord{
		RecordID:  supplied,
		URL:       testExtensionURL,
		StartedAt: start,
		EndedAt:   start.Add(time.Minute),
	}, identity.ProducerChrome, chromeAppName)
	if !ok {
		t.Fatal("toRecord rejected a valid record")
	}
	if got.RecordID != supplied {
		t.Errorf("record id = %q, want the supplied %q", got.RecordID, supplied)
	}
}

// A non-canonical ID is dropped rather than normalized: accepting several
// spellings of one ID would let the same segment insert twice.
func TestToRecordDropsANonCanonicalRecordID(t *testing.T) {
	t.Parallel()

	start := testExtensionStart()
	for _, supplied := range []string{
		"3F2504E0-4F89-41D3-9A0C-0305E82C3301",
		"3f2504e04f8941d39a0c0305e82c3301",
		"not-a-uuid",
		"",
	} {
		got, ok := toRecord(&extensionRecord{
			RecordID:  supplied,
			URL:       testExtensionURL,
			StartedAt: start,
			EndedAt:   start.Add(time.Minute),
		}, identity.ProducerChrome, chromeAppName)
		if !ok {
			t.Fatalf("toRecord rejected a valid record for id %q", supplied)
		}
		if got.RecordID != "" {
			t.Errorf("record id = %q for supplied %q, want it dropped", got.RecordID, supplied)
		}
	}
}

// The previous check only tested the scheme prefix, so anything beginning
// "moz-extension://" passed -- credentials, a port, a path, a query.
func TestOriginAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, true},
		{"empty value", []string{""}, true},
		{"firefox extension", []string{testExtensionOrigin}, true},
		{"chrome extension", []string{"chrome-extension://abcdefghijklmnop"}, true},

		{"null", []string{"null"}, false},
		{"no host", []string{"moz-extension://"}, false},
		{"with port", []string{"moz-extension://abc123:8080"}, false},
		{"with credentials", []string{"moz-extension://user:pass@abc123"}, false},
		{"with path", []string{"moz-extension://abc123/popup.html"}, false},
		{"with query", []string{"moz-extension://abc123?x=1"}, false},
		{"with fragment", []string{"moz-extension://abc123#f"}, false},
		{"trailing slash is a path", []string{"moz-extension://abc123/"}, false},
		{"web origin", []string{"https://example.com"}, false},
		{"scheme confusable", []string{"moz-extension-evil://abc123"}, false},
		{"prefix confusable", []string{"moz-extensionx://abc123"}, false},
		{"opaque form", []string{"moz-extension:abc123"}, false},
		{"duplicate headers", []string{testExtensionOrigin, testExtensionOrigin}, false},
		{"comma joined", []string{"moz-extension://abc123,moz-extension://def456"}, false},
		{"malformed", []string{"moz-extension://a b c"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := originAllowed(tt.values); got != tt.want {
				t.Errorf("originAllowed(%q) = %v, want %v", tt.values, got, tt.want)
			}
		})
	}
}
