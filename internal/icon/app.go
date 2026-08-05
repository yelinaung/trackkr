// Package icon defines application icon identity, image validation, and
// raster normalization shared by the daemon and the server.
//
// The package performs no HTTP, database, filesystem, or platform-native
// work. Decoding and scaling bytes handed to it is pure computation and
// stays; anything that has to name a URL, a row, or a path belongs in the
// caller. The freedesktop icon search in internal/tracker takes its roots
// as arguments for this reason.
package icon

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"image/png"
	"strings"
)

const (
	MaxKeyBytes  = 255
	MaxPNGBytes  = 64 << 10
	MaxDimension = 128
)

var (
	// ErrInvalid marks an image that does not satisfy the shared
	// contract. It says "icon" rather than "application icon" because
	// Normalize serves site favicons too, and a favicon failure reading
	// "invalid application icon" sends the next reader looking in the
	// wrong package.
	ErrInvalid = errors.New("invalid icon image")
	pngHeader  = []byte("\x89PNG\r\n\x1a\n")
)

// App is a normalized application key and its PNG artwork.
type App struct {
	Key string `json:"key"`
	PNG []byte `json:"png"`
}

// Details are derived from validated PNG bytes.
type Details struct {
	Digest [sha256.Size]byte
	Width  int
	Height int
}

// AppKey normalizes an application display name for icon lookup.
func AppKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// Validate checks an application icon and returns derived metadata.
func Validate(app App) (Details, error) {
	if app.Key == "" {
		return Details{}, invalid("key is empty")
	}
	if len(app.Key) > MaxKeyBytes {
		return Details{}, invalid("key is %d bytes; maximum is %d", len(app.Key), MaxKeyBytes)
	}
	if normalized := AppKey(app.Key); normalized != app.Key {
		return Details{}, invalid("key %q is not canonical; expected %q", app.Key, normalized)
	}
	return ValidatePNG(app.PNG)
}

// ValidatePNG checks normalized PNG artwork without imposing an application
// identity. Site favicons use the same bounded image contract.
func ValidatePNG(data []byte) (Details, error) {
	if len(data) > MaxPNGBytes {
		return Details{}, invalid("PNG is %d bytes; maximum is %d", len(data), MaxPNGBytes)
	}
	if !bytes.HasPrefix(data, pngHeader) {
		return Details{}, invalid("PNG signature is missing")
	}

	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Details{}, invalid("decoding PNG configuration: %v", err)
	}
	if config.Width < 1 || config.Width > MaxDimension {
		return Details{}, invalid("PNG width is %d; expected 1 through %d", config.Width, MaxDimension)
	}
	if config.Height < 1 || config.Height > MaxDimension {
		return Details{}, invalid("PNG height is %d; expected 1 through %d", config.Height, MaxDimension)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return Details{}, invalid("decoding complete PNG: %v", err)
	}

	return Details{
		Digest: sha256.Sum256(data),
		Width:  config.Width,
		Height: config.Height,
	}, nil
}

// Clone returns an icon whose PNG bytes do not alias the source.
func Clone(app App) App {
	return App{Key: app.Key, PNG: bytes.Clone(app.PNG)}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
