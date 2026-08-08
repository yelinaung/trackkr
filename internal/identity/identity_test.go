package identity

import (
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

var testProducers = []Producer{ProducerDesktop, ProducerFirefox, ProducerChrome}

// TestDeriveAlwaysProducesACanonicalVersion8 covers the half of Derive's
// contract that holds for every input: whatever it is handed, the result
// is an ID the server will accept as a replay guard.
func TestDeriveAlwaysProducesACanonicalVersion8(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		producer := hegel.Draw(ht, hegel.SampledFrom(testProducers))
		parts := hegel.Draw(ht, hegel.Lists(hegel.Text()))

		id := Derive(producer, parts...)
		if !Valid(id) {
			ht.Fatalf("Derive(%q, %q) = %q, which is not canonical", producer, parts, id)
		}
		if id[14] != '8' {
			ht.Errorf("Derive returned version %c, want 8: %s", id[14], id)
		}
		if v := id[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			ht.Errorf("Derive returned variant %c, want 8-b: %s", v, id)
		}
		if again := Derive(producer, parts...); again != id {
			ht.Fatalf("Derive is not stable: %q then %q", id, again)
		}
	})
}

// TestDeriveDistinguishesDifferentContent is the injectivity claim: two
// different inputs must not land on one identity, or a record conflicts
// as a replay of something it is not and is dropped.
//
// Arity and content are both unrestricted, which they could not be while
// Derive joined its parts on "\x00". That encoding lost the boundaries,
// so this property held only at fixed arity with no NUL in any part.
func TestDeriveDistinguishesDifferentContent(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		leftProducer := hegel.Draw(ht, hegel.SampledFrom(testProducers))
		leftParts := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		rightProducer := hegel.Draw(ht, hegel.SampledFrom(testProducers))
		rightParts := hegel.Draw(ht, hegel.Lists(hegel.Text()))

		ht.Assume(leftProducer != rightProducer || !slicesEqual(leftParts, rightParts))

		if Derive(leftProducer, leftParts...) == Derive(rightProducer, rightParts...) {
			ht.Fatalf("Derive(%q, %q) collides with Derive(%q, %q)",
				leftProducer, leftParts, rightProducer, rightParts)
		}
	})
}

// TestDeriveSeparatesShiftedNULs pins the two collisions that closed,
// because a generator will not rediscover either: both need a second
// input whose encoding coincides with the first, which random search
// does not stumble onto at production arity.
func TestDeriveSeparatesShiftedNULs(t *testing.T) {
	t.Parallel()

	// An application name and a title differing only in where a NUL
	// falls. Titles arrive unsanitized from the window manager.
	if Derive(ProducerDesktop, "a", "b\x00c", "d") == Derive(ProducerDesktop, "a\x00b", "c", "d") {
		t.Error("Derive collides when a NUL shifts across a part boundary")
	}
	// No parts against one empty part.
	if Derive(ProducerDesktop) == Derive(ProducerDesktop, "") {
		t.Error("Derive collides between no parts and one empty part")
	}
	// A part boundary against the same bytes inside one part.
	if Derive(ProducerDesktop, "ab", "c") == Derive(ProducerDesktop, "abc") {
		t.Error("Derive collides between two parts and their concatenation")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// drawCanonicalID builds a canonical UUID text form directly, rather
// than hoping a text generator produces one. Random Unicode never will,
// which would leave every claim below vacuously true.
func drawCanonicalID(tc hegel.TestCase) string {
	digits := hegel.Draw(tc, hegel.Text().Alphabet("0123456789abcdef").MinSize(32).MaxSize(32))
	return digits[0:8] + "-" + digits[8:12] + "-" + digits[12:16] + "-" +
		digits[16:20] + "-" + digits[20:32]
}

// TestValidAcceptsCanonicalIDs is the half that keeps the rejection
// property honest: every canonical spelling must be accepted, so a Valid
// that refused everything would fail here.
func TestValidAcceptsCanonicalIDs(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		id := drawCanonicalID(ht)
		if !Valid(id) {
			ht.Fatalf("Valid rejected the canonical id %q", id)
		}
	})
}

// TestValidRejectsNonCanonicalSpellings holds Valid to its doc comment.
// Accepting several spellings of one ID would let the same segment
// insert twice, so each perturbation of an accepted ID must be refused.
func TestValidRejectsNonCanonicalSpellings(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		id := drawCanonicalID(ht)

		variants := map[string]string{
			"uppercase":    strings.ToUpper(id),
			"braced":       "{" + id + "}",
			"urn prefixed": "urn:uuid:" + id,
			"unhyphenated": strings.ReplaceAll(id, "-", ""),
			"truncated":    id[:35],
			"padded":       id + "0",
			"trailing sp":  id + " ",
			"leading sp":   " " + id,
		}
		// A hyphen moved one place along, and a non-hex digit dropped
		// into a position that must hold one.
		variants["hyphen misplaced"] = id[:8] + id[9:] + "-"
		variants["non-hex digit"] = "g" + id[1:]

		for name, variant := range variants {
			if variant == id {
				continue // an all-digit id uppercases to itself
			}
			if Valid(variant) {
				ht.Fatalf("Valid accepted the %s spelling %q of %q", name, variant, id)
			}
		}
	})
}

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
