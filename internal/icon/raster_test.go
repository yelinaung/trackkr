package icon

import (
	"bytes"
	"errors"
	"image"
	"testing"

	"github.com/sergeymakinen/go-ico"
)

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
