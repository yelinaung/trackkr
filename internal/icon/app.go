// Package icon defines application icon identity and image validation.
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
	// ErrInvalid marks an icon that does not satisfy the shared contract.
	ErrInvalid = errors.New("invalid application icon")
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
	if len(app.PNG) > MaxPNGBytes {
		return Details{}, invalid("PNG is %d bytes; maximum is %d", len(app.PNG), MaxPNGBytes)
	}
	if !bytes.HasPrefix(app.PNG, pngHeader) {
		return Details{}, invalid("PNG signature is missing")
	}

	config, err := png.DecodeConfig(bytes.NewReader(app.PNG))
	if err != nil {
		return Details{}, invalid("decoding PNG configuration: %v", err)
	}
	if config.Width < 1 || config.Width > MaxDimension {
		return Details{}, invalid("PNG width is %d; expected 1 through %d", config.Width, MaxDimension)
	}
	if config.Height < 1 || config.Height > MaxDimension {
		return Details{}, invalid("PNG height is %d; expected 1 through %d", config.Height, MaxDimension)
	}
	if _, err := png.Decode(bytes.NewReader(app.PNG)); err != nil {
		return Details{}, invalid("decoding complete PNG: %v", err)
	}

	return Details{
		Digest: sha256.Sum256(app.PNG),
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
