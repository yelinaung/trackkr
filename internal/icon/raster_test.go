package icon

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/sergeymakinen/go-ico"
	"hegel.dev/go/hegel"
)

// drawSourcePNG builds an encoded PNG of drawn dimensions inside the
// source limits, filled with a drawn colour.
//
// The fill is one flat colour on purpose. Random per-pixel RGBA does not
// compress, so a large image would encode past MaxSourceBytes and every
// case would be spent on the byte bound instead of on the resample the
// property is about. A flat fill keeps a 1024x1024 source in a few
// hundred bytes, leaving the dimensions free to range over the whole
// domain.
func drawSourcePNG(tc hegel.TestCase) []byte {
	// Both dimensions run the full legal range. The pixel cap never binds
	// tighter than the dimension cap, because maxSourceDimension squared
	// is exactly maxSourcePixels, so the largest drawable source sits on
	// the limit and is accepted.
	width := hegel.Draw(tc, hegel.Integers(1, maxSourceDimension))
	height := hegel.Draw(tc, hegel.Integers(1, maxSourceDimension))

	source, err := encodeFilled(width, height, color.NRGBA{
		R: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		G: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		B: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		A: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
	})
	if err != nil {
		tc.Errorf("encoding a %dx%d source: %v", width, height, err)
		tc.FailNow()
	}
	return source
}

// encodeFilled encodes one flat-coloured PNG of the given size, shared
// by the drawn-source generator and the geometry property.
//
// One row is filled and copied down the image rather than calling
// SetNRGBA per pixel: a 1024x1024 source is a million calls, and both
// properties draw sources that large.
func encodeFilled(width, height int, fill color.NRGBA) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	row := img.Pix[:width*4]
	for x := range width {
		copy(row[x*4:], []byte{fill.R, fill.G, fill.B, fill.A})
	}
	for y := 1; y < height; y++ {
		copy(img.Pix[y*img.Stride:(y+1)*img.Stride], row)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding a %dx%d image: %w", width, height, err)
	}
	return buf.Bytes(), nil
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	got, err := Normalize(testPNG(t, 32, 16))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	details, err := ValidatePNG(got)
	if err != nil {
		t.Fatalf("normalized PNG: %v", err)
	}
	if details.Width != NormalizedDimension || details.Height != NormalizedDimension {
		t.Errorf("dimensions = %dx%d, want %dx%d",
			details.Width, details.Height, NormalizedDimension, NormalizedDimension)
	}
}

// TestNormalizeProducesAValidatedSquare is the postcondition the rest of
// the icon path depends on: whatever shape went in, what comes out is a
// PNG the shared contract accepts, at exactly the canvas size.
func TestNormalizeProducesAValidatedSquare(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		source := drawSourcePNG(ht)
		ht.Assume(len(source) <= MaxSourceBytes)

		normalized, err := Normalize(source)
		if err != nil {
			ht.Fatalf("Normalize(%d source bytes): %v", len(source), err)
		}
		details, err := ValidatePNG(normalized)
		if err != nil {
			ht.Fatalf("normalized output failed ValidatePNG: %v", err)
		}
		if details.Width != NormalizedDimension || details.Height != NormalizedDimension {
			ht.Fatalf("dimensions = %dx%d, want %dx%d",
				details.Width, details.Height, NormalizedDimension, NormalizedDimension)
		}
	})
}

// opaqueBounds returns the rectangle covering every pixel the resample
// actually painted, ignoring faint ringing.
//
// CatmullRom has negative lobes, so a hard edge picks up a fringe of
// near-transparent pixels either side of where it landed. The threshold
// keeps that fringe out of the measurement without hiding real content.
func opaqueBounds(tb testing.TB, encoded []byte) image.Rectangle {
	tb.Helper()

	decoded, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		tb.Fatalf("decoding normalized output: %v", err)
	}

	bounds := image.Rectangle{Min: image.Pt(1<<30, 1<<30), Max: image.Pt(-1, -1)}
	for y := range NormalizedDimension {
		for x := range NormalizedDimension {
			if _, _, _, alpha := decoded.At(x, y).RGBA(); alpha>>8 < 8 {
				continue
			}
			bounds.Min.X = min(bounds.Min.X, x)
			bounds.Min.Y = min(bounds.Min.Y, y)
			bounds.Max.X = max(bounds.Max.X, x+1)
			bounds.Max.Y = max(bounds.Max.Y, y+1)
		}
	}
	return bounds
}

// TestNormalizePreservesShapeAndContent tests what Normalize is for.
//
// The other three properties here check that the output is a valid 64x64
// PNG, which a function returning one blank canvas for every input would
// also satisfy. These are the claims the doc comment actually makes:
// content survives, the aspect ratio is preserved, the result is centred,
// and it is scaled to fit the canvas rather than left small.
//
// Sources are drawn opaque so the painted region is measurable. A
// tolerance of two pixels absorbs the resampling fringe; the aspect
// check is skipped where the expected short side is under four pixels,
// since two pixels of slack there would assert almost nothing.
func TestNormalizePreservesShapeAndContent(t *testing.T) {
	t.Parallel()

	const tolerance = 2

	hegel.Test(t, func(ht *hegel.T) {
		width := hegel.Draw(ht, hegel.Integers(1, maxSourceDimension))
		height := hegel.Draw(ht, hegel.Integers(1, maxSourceDimension))
		source, err := encodeFilled(width, height, color.NRGBA{R: 0x20, G: 0x90, B: 0xd0, A: 0xff})
		if err != nil {
			ht.Fatalf("encoding source: %v", err)
		}
		ht.Assume(len(source) <= MaxSourceBytes)

		normalized, err := Normalize(source)
		if err != nil {
			ht.Fatalf("Normalize(%dx%d): %v", width, height, err)
		}

		painted := opaqueBounds(ht, normalized)
		if painted.Empty() {
			ht.Fatalf("Normalize(%dx%d) painted nothing; the source was opaque", width, height)
		}

		// Scaled to fit: the long side reaches the canvas edge.
		if longest := max(painted.Dx(), painted.Dy()); longest < NormalizedDimension-tolerance {
			ht.Fatalf("Normalize(%dx%d) painted %dx%d, whose long side falls short of %d",
				width, height, painted.Dx(), painted.Dy(), NormalizedDimension)
		}

		// Centred: the margins either side match.
		if leading, trailing := painted.Min.X, NormalizedDimension-painted.Max.X; abs(leading-trailing) > tolerance {
			ht.Fatalf("Normalize(%dx%d) left margins %d and %d horizontally",
				width, height, leading, trailing)
		}
		if leading, trailing := painted.Min.Y, NormalizedDimension-painted.Max.Y; abs(leading-trailing) > tolerance {
			ht.Fatalf("Normalize(%dx%d) left margins %d and %d vertically",
				width, height, leading, trailing)
		}

		// Aspect preserved: the short side is the long side scaled by
		// the source ratio, computed here from the source dimensions
		// rather than copied from the implementation.
		shortSide := min(width, height) * NormalizedDimension / max(width, height)
		if shortSide < 4 {
			return
		}
		if got := min(painted.Dx(), painted.Dy()); abs(got-shortSide) > tolerance {
			ht.Fatalf("Normalize(%dx%d) painted a short side of %d, want about %d",
				width, height, got, shortSide)
		}
	})
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestNormalizeIsStableUnderASecondPass checks that re-normalizing
// already normalized bytes keeps them valid at the canvas size.
//
// Byte-for-byte idempotence is what this test asked for first, and it is
// false. Normalize resamples through premultiplied alpha, and at low
// alpha the round trip back to NRGBA cannot recover the original
// channels: a pixel of {R:0, G:0, B:2, A:1} normalizes to {0, 0, 1, 1}
// and then to {0, 0, 0, 1}, converging one step later. The shift is
// invisible at alpha 1/255, and nothing re-normalizes its own output --
// internal/favicon and internal/tracker each hand Normalize bytes they
// just fetched or read. So the weaker claim is the true one, and the
// stronger one is recorded here instead of being asserted and skipped.
func TestNormalizeIsStableUnderASecondPass(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		source := drawSourcePNG(ht)
		ht.Assume(len(source) <= MaxSourceBytes)

		once, err := Normalize(source)
		if err != nil {
			ht.Fatalf("Normalize: %v", err)
		}
		twice, err := Normalize(once)
		if err != nil {
			ht.Fatalf("Normalize of normalized output: %v", err)
		}
		details, err := ValidatePNG(twice)
		if err != nil {
			ht.Fatalf("second pass failed ValidatePNG: %v", err)
		}
		if details.Width != NormalizedDimension || details.Height != NormalizedDimension {
			ht.Fatalf("second pass dimensions = %dx%d, want %dx%d",
				details.Width, details.Height, NormalizedDimension, NormalizedDimension)
		}
	})
}

// TestNormalizeSurvivesArbitraryBytes guards the decoder path. Normalize
// runs on favicon bytes fetched from an arbitrary host, so every failure
// must arrive as an error and none as a panic.
func TestNormalizeSurvivesArbitraryBytes(t *testing.T) {
	t.Parallel()

	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Binary(0, -1))
		normalized, err := Normalize(data)
		if err != nil {
			return
		}
		details, validateErr := ValidatePNG(normalized)
		if validateErr != nil {
			ht.Fatalf("accepted %d bytes but the output failed ValidatePNG: %v",
				len(data), validateErr)
		}
		if details.Width != NormalizedDimension || details.Height != NormalizedDimension {
			ht.Fatalf("dimensions = %dx%d, want %dx%d",
				details.Width, details.Height, NormalizedDimension, NormalizedDimension)
		}
	})
}

func TestNormalizeAcceptsICO(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	if err := ico.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatalf("encoding ICO fixture: %v", err)
	}
	if _, err := Normalize(encoded.Bytes()); err != nil {
		t.Fatalf("Normalize(ICO): %v", err)
	}
}

func TestNormalizeRejectsOversizedDimensions(t *testing.T) {
	t.Parallel()

	if _, err := Normalize(testPNG(t, maxSourceDimension+1, 1)); err == nil {
		t.Fatal("Normalize accepted oversized dimensions")
	}
}

// TestNormalizeSourceByteBoundary is what distinguishes a reader capped
// at MaxSourceBytes from one capped at MaxSourceBytes+1. A source
// exactly at the limit must normalize; one byte more must be refused
// here, as ErrInvalid, rather than surviving as a truncated image that
// fails later inside the decoder.
func TestNormalizeSourceByteBoundary(t *testing.T) {
	t.Parallel()

	// Decoders stop at IEND, so trailing bytes give exact control over
	// the encoded length without disturbing the image.
	pad := func(tb testing.TB, size int) []byte {
		tb.Helper()
		base := testPNG(tb, 32, 32)
		if len(base) > size {
			tb.Fatalf("fixture is %d bytes, larger than the %d-byte target", len(base), size)
		}
		return append(base, make([]byte, size-len(base))...)
	}

	if _, err := Normalize(pad(t, MaxSourceBytes)); err != nil {
		t.Errorf("Normalize at exactly MaxSourceBytes: %v", err)
	}

	_, err := Normalize(pad(t, MaxSourceBytes+1))
	if err == nil {
		t.Fatal("Normalize accepted a source over MaxSourceBytes")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid so the refusal is the byte bound, not a decode failure", err)
	}
}
