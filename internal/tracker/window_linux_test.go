//go:build linux

package tracker

import (
	"testing"

	"hegel.dev/go/hegel"
)

// TestParseWMClassAlwaysNamesSomething pins the fallback. The result
// becomes a record's application name, and an empty one produces a
// record the server has to reject, so every input must yield a name.
func TestParseWMClassAlwaysNamesSomething(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		output := hegel.Draw(ht, hegel.Text())
		if got := parseWMClass(output); got == "" {
			ht.Fatalf("parseWMClass(%q) returned an empty name", output)
		}
	})
}

func TestParseWMClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "firefox",
			input: `WM_CLASS(STRING) = "Navigator", "firefox"`,
			want:  "firefox",
		},
		{
			name:  "gnome-terminal",
			input: `WM_CLASS(STRING) = "gnome-terminal-server", "Gnome-terminal"`,
			want:  "Gnome-terminal",
		},
		{
			name:  "single value",
			input: `WM_CLASS(STRING) = "code"`,
			want:  "code",
		},
		{
			name:  "no equals sign",
			input: `WM_CLASS not found.`,
			want:  unknownApp,
		},
		{
			name:  "empty after equals",
			input: `WM_CLASS(STRING) = `,
			want:  unknownApp,
		},
		{
			name:  "empty string value",
			input: `WM_CLASS(STRING) = "", ""`,
			want:  unknownApp,
		},
		{
			name:  "spaces in value",
			input: `WM_CLASS(STRING) = "main", "Visual Studio Code"`,
			want:  "Visual Studio Code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseWMClass(tt.input)
			if got != tt.want {
				t.Errorf("parseWMClass(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
