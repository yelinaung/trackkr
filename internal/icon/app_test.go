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
