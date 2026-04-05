//go:build linux

package tracker

import "testing"

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
			want:  "unknown",
		},
		{
			name:  "empty after equals",
			input: `WM_CLASS(STRING) = `,
			want:  "unknown",
		},
		{
			name:  "empty string value",
			input: `WM_CLASS(STRING) = "", ""`,
			want:  "unknown",
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
