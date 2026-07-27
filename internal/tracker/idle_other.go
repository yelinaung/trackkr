//go:build !linux

package tracker

import "github.com/rs/zerolog"

// NewIdleDetectorOrNop returns a NopIdleDetector on platforms without
// an idle detection implementation.
func NewIdleDetectorOrNop(logger *zerolog.Logger) IdleDetector {
	logger.Info().Msg("idle detection not implemented on this platform")
	return NopIdleDetector{}
}
