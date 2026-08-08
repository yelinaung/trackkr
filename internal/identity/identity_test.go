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
// The claim holds only inside the shape the one caller uses, and the two
// restrictions below are the finding rather than housekeeping. Derive
// hashes `producer + "\x00" + strings.Join(parts, "\x00")` and commits
// neither the count nor the lengths, so anything that produces the same
// joined string produces the same ID.
//
// Fixed arity. Derive() and Derive("") both join to "", so they collide.
// Unreachable today: reporter.ensureIdentity always passes exactly five
// parts.
//
// No NUL in a part. At any arity, content can slide across a separator:
// ("a", "b\x00c", "d") and ("a\x00b", "c", "d") join identically, so an
// application name and a window title that differ only in where a NUL
// falls derive one identity, and the second record is discarded as a
// replay of the first. Titles come from the window manager, which is why
// this is a restriction and not an axiom.
//
// Both disappear if Derive commits the part count and lengths to the
// digest. That changes every derived ID, so it is a decision about
// legacy replay stability, not a free fix.
func TestDeriveDistinguishesDifferentContent(t *testing.T) {
	t.Parallel()

	const partCount = 5 // reporter.ensureIdentity passes exactly this many
	partGen := hegel.Text().ExcludeCharacters("\x00")

	drawParts := func(ht *hegel.T) []string {
		parts := make([]string, partCount)
		for i := range parts {
			parts[i] = hegel.Draw(ht, partGen)
		}
		return parts
	}

	hegel.Test(t, func(ht *hegel.T) {
		leftProducer := hegel.Draw(ht, hegel.SampledFrom(testProducers))
		leftParts := drawParts(ht)
		rightProducer := hegel.Draw(ht, hegel.SampledFrom(testProducers))
		rightParts := drawParts(ht)

		ht.Assume(leftProducer != rightProducer || !slicesEqual(leftParts, rightParts))

		if Derive(leftProducer, leftParts...) == Derive(rightProducer, rightParts...) {
			ht.Fatalf("Derive(%q, %q) collides with Derive(%q, %q)",
				leftProducer, leftParts, rightProducer, rightParts)
		}
	})
}

// TestDeriveCollidesAcrossNULSeparators records the collision the
// property above has to exclude, so the restriction is executable rather
// than a claim in a comment. Delete it if Derive ever commits its part
// lengths to the digest -- it will start failing, which is the point.
func TestDeriveCollidesAcrossNULSeparators(t *testing.T) {
	t.Parallel()

	shifted := Derive(ProducerDesktop, "a", "b\x00c", "d")
	unshifted := Derive(ProducerDesktop, "a\x00b", "c", "d")
	if shifted != unshifted {
		t.Errorf("Derive no longer collides across a NUL: %q and %q."+
			" If the digest now commits part lengths, drop this test and"+
			" widen TestDeriveDistinguishesDifferentContent.", shifted, unshifted)
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

// TestValidImpliesCanonicalSpelling holds Valid to the doc comment: it
// accepts one spelling of an ID, so an uppercase or braced variant of an
// accepted ID must be refused. Accepting several spellings of one ID
// would let the same segment insert twice.
func TestValidImpliesCanonicalSpelling(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		if !Valid(id) {
			return
		}

		if len(id) != 36 {
			ht.Fatalf("Valid accepted %q of length %d, want 36", id, len(id))
		}
		if lowered := strings.ToLower(id); lowered != id {
			ht.Fatalf("Valid accepted %q, which is not lowercase", id)
		}
		if upper := strings.ToUpper(id); upper != id && Valid(upper) {
			ht.Fatalf("Valid accepted both %q and its uppercase spelling", id)
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
