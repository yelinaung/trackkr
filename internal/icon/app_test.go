package icon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"unicode"

	"hegel.dev/go/hegel"
)

const testFirefoxKey = "firefox"

func TestAppKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", " \t\n", ""},
		{"case", "FireFox", testFirefoxKey},
		{"repeated whitespace", " Visual\t Studio  Code ", "visual studio code"},
		{"unicode", " CAFÉ 浏览器 ", "café 浏览器"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AppKey(tt.raw); got != tt.want {
				t.Errorf("AppKey(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestAppKeyIsIdempotent pins a contract, not a nicety: Validate rejects
// any key that is not its own AppKey, so a second application of AppKey
// that changed anything would mint keys the validator refuses.
func TestAppKeyIsIdempotent(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		once := AppKey(name)
		if twice := AppKey(once); twice != once {
			ht.Fatalf("AppKey(%q) = %q, AppKey again = %q", name, once, twice)
		}
	})
}

// TestAppKeyOutputShape checks what the key is normalized to: single
// spaces between fields, nothing at the ends, and every rune that has a
// lowercase form already in it.
//
// The case clause tests unicode.ToLower(r) == r, not !unicode.IsUpper(r).
// IsUpper is the wrong question: U+1D400 MATHEMATICAL BOLD CAPITAL A is
// upper case and has no lowercase mapping at all, so AppKey leaves it
// alone and is right to. ToLower being the identity on every rune is the
// claim that would actually break if the fold stopped happening, and it
// is stricter in the other direction too -- U+2160 ROMAN NUMERAL ONE is
// not IsUpper yet does have a lowercase form.
func TestAppKeyOutputShape(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		key := AppKey(name)

		if strings.TrimSpace(key) != key {
			ht.Fatalf("AppKey(%q) = %q, which has leading or trailing whitespace", name, key)
		}
		if strings.Contains(key, "  ") {
			ht.Fatalf("AppKey(%q) = %q, which has repeated spaces", name, key)
		}
		for _, r := range key {
			if unicode.IsSpace(r) && r != ' ' {
				ht.Fatalf("AppKey(%q) = %q, which kept the non-space whitespace %U", name, key, r)
			}
			if unicode.ToLower(r) != r {
				ht.Fatalf("AppKey(%q) = %q, which left %U unfolded", name, key, r)
			}
		}
	})
}

// TestValidateAcceptsAppKeyOutput closes the loop between the two: a key
// AppKey produced, within the byte bound, is one Validate accepts.
//
// strings.ToLower can lengthen a string -- U+0130 lowercases to two
// runes -- so the bound is a real condition here, not a formality, and
// db.AppIconKeys re-checks it for exactly this reason.
func TestValidateAcceptsAppKeyOutput(t *testing.T) {
	t.Parallel()

	valid := testPNG(t, 64, 64)
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		key := AppKey(name)
		ht.Assume(key != "")
		ht.Assume(len(key) <= MaxKeyBytes)

		details, err := Validate(App{Key: key, PNG: valid})
		if err != nil {
			ht.Fatalf("Validate(AppKey(%q) = %q): %v", name, key, err)
		}
		if details.Width != 64 || details.Height != 64 {
			ht.Fatalf("dimensions = %dx%d, want 64x64", details.Width, details.Height)
		}
	})
}

// TestValidatePNGSurvivesArbitraryBytes is the closest thing here to a
// fuzz target: ValidatePNG sits behind an HTTP upload, so a panic on
// hostile bytes is the failure that matters.
//
// Success is not an error case. Arbitrary bytes can encode a small valid
// PNG, and the generator will eventually produce one, so the property is
// the disjunction -- Details and no error, or an error wrapping
// ErrInvalid -- never a panic and never a bare error.
func TestValidatePNGSurvivesArbitraryBytes(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Binary(0, -1))
		details, err := ValidatePNG(data)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				ht.Fatalf("ValidatePNG(%d bytes) error = %v, want ErrInvalid", len(data), err)
			}
			return
		}
		if details.Width < 1 || details.Width > MaxDimension ||
			details.Height < 1 || details.Height > MaxDimension {
			ht.Fatalf("accepted %dx%d, outside 1 through %d",
				details.Width, details.Height, MaxDimension)
		}
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()

	valid := testPNG(t, 64, 64)
	truncated := bytes.Clone(valid[:33])
	badCRC := bytes.Clone(valid)
	badCRC[len(badCRC)-5] ^= 0xff

	tests := []struct {
		name string
		app  App
	}{
		{"empty key", App{PNG: valid}},
		{"noncanonical key", App{Key: " Firefox ", PNG: valid}},
		{"oversized key", App{Key: strings.Repeat("a", MaxKeyBytes+1), PNG: valid}},
		{"empty PNG", App{Key: testFirefoxKey}},
		{"wrong signature", App{Key: testFirefoxKey, PNG: []byte("not a png")}},
		{"oversized PNG", App{Key: testFirefoxKey, PNG: append(bytes.Clone(valid), make([]byte, MaxPNGBytes)...)}},
		{"truncated after header", App{Key: testFirefoxKey, PNG: truncated}},
		{"invalid CRC", App{Key: testFirefoxKey, PNG: badCRC}},
		{"zero width", App{Key: testFirefoxKey, PNG: dimensionPNG(0, 1)}},
		{"zero height", App{Key: testFirefoxKey, PNG: dimensionPNG(1, 0)}},
		{"oversized width", App{Key: testFirefoxKey, PNG: testPNG(t, MaxDimension+1, 1)}},
		{"oversized height", App{Key: testFirefoxKey, PNG: testPNG(t, 1, MaxDimension+1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate(tt.app)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidatePNGDoesNotRequireAppKey(t *testing.T) {
	t.Parallel()

	pngBytes := testPNG(t, 32, 32)
	details, err := ValidatePNG(pngBytes)
	if err != nil {
		t.Fatalf("ValidatePNG: %v", err)
	}
	if details.Width != 32 || details.Height != 32 {
		t.Errorf("dimensions = %dx%d, want 32x32", details.Width, details.Height)
	}
}

func TestValidateReturnsDetails(t *testing.T) {
	t.Parallel()

	app := App{Key: strings.Repeat("a", MaxKeyBytes), PNG: testPNG(t, 48, 32)}
	first, err := Validate(app)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	second, err := Validate(app)
	if err != nil {
		t.Fatalf("Validate again: %v", err)
	}
	if first.Width != 48 || first.Height != 32 {
		t.Errorf("dimensions = %dx%d, want 48x32", first.Width, first.Height)
	}
	if first.Digest != second.Digest {
		t.Error("digest is not stable")
	}
}

func TestCloneOwnsPNGBytes(t *testing.T) {
	t.Parallel()

	source := App{Key: "finder", PNG: testPNG(t, 1, 1)}
	cloned := Clone(source)
	source.PNG[0] = 0
	if cloned.PNG[0] != 0x89 {
		t.Errorf("clone first byte = %#x, want PNG signature byte", cloned.PNG[0])
	}
}

func testPNG(tb testing.TB, width, height int) []byte {
	tb.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, color.NRGBA{R: 0x36, G: 0x78, B: 0xa8, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func dimensionPNG(width, height uint32) []byte {
	var result bytes.Buffer
	result.Write(pngHeader)
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8
	ihdr[9] = 6
	writePNGChunk(&result, "IHDR", ihdr)
	writePNGChunk(&result, "IEND", nil)
	return result.Bytes()
}

func writePNGChunk(dst *bytes.Buffer, kind string, data []byte) {
	if uint64(len(data)) > uint64(^uint32(0)) {
		panic("PNG test chunk is too large")
	}
	length := uint32(len(data)) //nolint:gosec // The length is bounded above.
	_ = binary.Write(dst, binary.BigEndian, length)
	dst.WriteString(kind)
	dst.Write(data)
	checksum := crc32.NewIEEE()
	_, _ = checksum.Write([]byte(kind))
	_, _ = checksum.Write(data)
	_ = binary.Write(dst, binary.BigEndian, checksum.Sum32())
}
