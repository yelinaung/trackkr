package server

import (
	"testing"

	"github.com/yelinaung/trackkr/internal/db"
)

const (
	chromeExactKey    = "google chrome"
	chromeFallbackKey = "google-chrome"
)

// The ordered alias list, not the database result order, decides which stored
// row represents a browser. A user with a Mac and a Linux box has two rows for
// Chrome, and the answer must not change between renders.
func TestFirstPresentIconPrefersTheOrderedAlias(t *testing.T) {
	t.Parallel()

	macRow := db.AppIconRow{ID: 1, AppKey: chromeExactKey}
	linuxRow := db.AppIconRow{ID: 2, AppKey: chromeFallbackKey}
	keys := db.AppIconKeys("Google Chrome")

	tests := []struct {
		name   string
		rows   map[string]db.AppIconRow
		wantID int64
		wantOK bool
	}{
		{
			name:   "exact only",
			rows:   map[string]db.AppIconRow{chromeExactKey: macRow},
			wantID: 1,
			wantOK: true,
		},
		{
			name:   "fallback only",
			rows:   map[string]db.AppIconRow{chromeFallbackKey: linuxRow},
			wantID: 2,
			wantOK: true,
		},
		{
			// Both present: the preferred alias wins whichever order the
			// query returned them in, since the map is order-free.
			name:   "both aliases",
			rows:   map[string]db.AppIconRow{chromeExactKey: macRow, chromeFallbackKey: linuxRow},
			wantID: 1,
			wantOK: true,
		},
		{
			name:   "neither alias",
			rows:   map[string]db.AppIconRow{testFirefoxLower: {ID: 9, AppKey: testFirefoxLower}},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row, ok := firstPresentIcon(tt.rows, keys)
			if ok != tt.wantOK {
				t.Fatalf("found = %v, want %v", ok, tt.wantOK)
			}
			if ok && row.ID != tt.wantID {
				t.Errorf("row id = %d, want %d", row.ID, tt.wantID)
			}
		})
	}
}

func TestAppIconKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		appName string
		want    []string
	}{
		{"Google Chrome", []string{chromeExactKey, chromeFallbackKey}},
		{chromeFallbackKey, []string{chromeExactKey, chromeFallbackKey}},
		{chromeExactKey, []string{chromeExactKey, chromeFallbackKey}},
		{"Firefox", []string{testFirefoxLower}},
		{testFirefoxLower, []string{testFirefoxLower}},
		{"Ghostty", []string{"ghostty"}},
		{"   ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.appName, func(t *testing.T) {
			t.Parallel()
			got := db.AppIconKeys(tt.appName)
			if len(got) != len(tt.want) {
				t.Fatalf("keys = %q, want %q", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("key %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
