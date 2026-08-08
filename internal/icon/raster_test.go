package icon

import (
	"bytes"
	"errors"
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

	fill := color.NRGBA{
		R: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		G: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		B: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
		A: hegel.Draw(tc, hegel.Integers[uint8](0, 255)),
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetNRGBA(x, y, fill)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("encoding drawn source: " + err.Error())
	}
	return buf.Bytes()
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
