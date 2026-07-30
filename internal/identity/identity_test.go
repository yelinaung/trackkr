package identity

import (
	"strings"
	"testing"
)

func TestNewProducesCanonicalVersion4(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 200)
	for range 200 {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !Valid(id) {
			t.Fatalf("New returned %q, which is not canonical", id)
		}
		if id[14] != '4' {
			t.Errorf("New returned version %c, want 4: %s", id[14], id)
		}
		if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("New returned variant %c, want 8-b: %s", v, id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("New repeated %s", id)
		}
		seen[id] = struct{}{}
	}
}

// A legacy replay must land on the same identity every time, or the record
// inserts again instead of conflicting.
func TestDeriveIsStableAndContentAddressed(t *testing.T) {
	t.Parallel()

	first := Derive(ProducerFirefox, "https://example.com/", "2026-07-30T09:00:00Z")
	again := Derive(ProducerFirefox, "https://example.com/", "2026-07-30T09:00:00Z")
	if first != again {
		t.Errorf("Derive is not stable: %s then %s", first, again)
	}
	if !Valid(first) {
		t.Errorf("Derive returned %q, which is not canonical", first)
	}
	if first[14] != '8' {
		t.Errorf("Derive returned version %c, want 8: %s", first[14], first)
	}

	// The producer participates: one segment seen by the tracker and by a
	// browser is two records.
	if other := Derive(ProducerDesktop, "https://example.com/", "2026-07-30T09:00:00Z"); other == first {
		t.Error("Derive ignored the producer")
	}
	if other := Derive(ProducerFirefox, "https://example.com/", "2026-07-30T09:00:01Z"); other == first {
		t.Error("Derive ignored the content")
	}
}

// Joining parts without a separator would let different content collide.
func TestDeriveSeparatesParts(t *testing.T) {
	t.Parallel()

	if Derive(ProducerChrome, "ab", "c") == Derive(ProducerChrome, "a", "bc") {
		t.Error("Derive concatenated its parts without a separator")
	}
}

func TestValid(t *testing.T) {
	t.Parallel()

	canonical := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"canonical", canonical, true},
		{"empty", "", false},
		{"uppercase", strings.ToUpper(canonical), false},
		{"braced", "{" + canonical + "}", false},
		{"unhyphenated", strings.ReplaceAll(canonical, "-", ""), false},
		{"too short", canonical[:35], false},
		{"too long", canonical + "0", false},
		{"non-hex digit", "3f2504e0-4f89-41d3-9a0c-0305e82c330g", false},
		{"hyphen misplaced", "3f2504e04-f89-41d3-9a0c-0305e82c3301", false},
		{"urn prefixed", "urn:uuid:" + canonical, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Valid(tt.id); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestValidProducer(t *testing.T) {
	t.Parallel()

	for _, p := range []Producer{ProducerDesktop, ProducerFirefox, ProducerChrome} {
		if !ValidProducer(p) {
			t.Errorf("ValidProducer(%q) = false, want true", p)
		}
	}
	for _, p := range []Producer{"", "Desktop", "FIREFOX", "safari", "chrome ", "desktop\n"} {
		if ValidProducer(p) {
			t.Errorf("ValidProducer(%q) = true, want false", p)
		}
	}
}
