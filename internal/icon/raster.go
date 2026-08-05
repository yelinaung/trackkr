package icon

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // Registers the GIF decoder for Normalize.
	_ "image/jpeg" // Registers the JPEG decoder for Normalize.
	"image/png"
	"math"

	_ "github.com/sergeymakinen/go-ico" // Registers the ICO decoder for Normalize.
	"golang.org/x/image/draw"
)

const (
	// MaxSourceBytes bounds the encoded input Normalize will decode.
	//
	// A caller reading from a socket or a file must cap its reader at
	// MaxSourceBytes+1 and compare the length afterwards. Capping at
	// MaxSourceBytes exactly cannot distinguish a source at the limit
	// from one silently truncated to it, and the truncated one then
	// fails inside the decoder as corrupt artwork rather than here as
	// an oversized file.
	MaxSourceBytes = 256 << 10

	maxSourceDimension = 1024
	maxSourcePixels    = 1024 * 1024

	// NormalizedDimension is the square canvas Normalize resamples onto.
	//
	// It is exported because a caller choosing which source to hand over
	// needs the target: the freedesktop search in internal/tracker picks
	// the smallest installed icon at or above this size, so that the
	// resample downscales rather than upscales.
	NormalizedDimension = 64
)

// Normalize decodes an encoded image and re-encodes it as a PNG drawn
// onto a transparent square canvas, preserving aspect ratio and
// centring the result.
//
// The byte bound is enforced before DecodeConfig because the dimension
// limits can only be applied once the bytes are already in memory. A
// 900x900 PNG can be tens of megabytes, so without the bound a caller
// would read and decode a file of any size.
func Normalize(data []byte) ([]byte, error) {
	if len(data) > MaxSourceBytes {
		return nil, invalid("source is %d bytes; maximum is %d", len(data), MaxSourceBytes)
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding image configuration: %w", err)
	}
	if config.Width < 1 || config.Height < 1 ||
		config.Width > maxSourceDimension || config.Height > maxSourceDimension ||
		config.Width*config.Height > maxSourcePixels {
		return nil, errors.New("image dimensions exceed the source limit")
	}

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding image: %w", err)
	}
	bounds := source.Bounds()
	if bounds.Dx() != config.Width || bounds.Dy() != config.Height {
		return nil, errors.New("image dimensions changed during decode")
	}

	scale := math.Min(
		float64(NormalizedDimension)/float64(bounds.Dx()),
		float64(NormalizedDimension)/float64(bounds.Dy()),
	)
	width := max(1, int(math.Round(float64(bounds.Dx())*scale)))
	height := max(1, int(math.Round(float64(bounds.Dy())*scale)))
	x := (NormalizedDimension - width) / 2
	y := (NormalizedDimension - height) / 2
	destination := image.NewNRGBA(image.Rect(0, 0, NormalizedDimension, NormalizedDimension))
	draw.CatmullRom.Scale(
		destination,
		image.Rect(x, y, x+width, y+height),
		source,
		bounds,
		draw.Over,
		nil,
	)

	var output bytes.Buffer
	if err := png.Encode(&output, destination); err != nil {
		return nil, fmt.Errorf("encoding normalized image: %w", err)
	}
	if _, err := ValidatePNG(output.Bytes()); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
