// Package identity defines the stable identity of one activity record: who
// produced it and which logical segment it is.
//
// Both the daemon and the server depend on these rules. The daemon mints an
// ID when a segment finalizes and preserves it through every retry; the server
// stores it as the replay guard. Keeping the rules in one package is what stops
// the two sides drifting into disagreeing about what counts as a valid ID.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// Producer names the component that observed the activity. It is assigned by
// the daemon from the route the record arrived on, never taken from a
// caller-supplied application name -- an extension that could name its own
// producer could also claim another browser's coverage and subtract it.
type Producer string

const (
	ProducerDesktop Producer = "desktop"
	ProducerFirefox Producer = "firefox"
	ProducerChrome  Producer = "chrome"
)

// ErrInvalid wraps every validation failure so HTTP callers can answer 400
// without matching on strings.
var ErrInvalid = errors.New("invalid record identity")

// ValidProducer reports whether p is one of the three known producers.
func ValidProducer(p Producer) bool {
	switch p {
	case ProducerDesktop, ProducerFirefox, ProducerChrome:
		return true
	default:
		return false
	}
}

// New mints a random version-4 UUID for a newly finalized segment.
func New() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating record id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return format(raw), nil
}

// Derive returns a stable version-8 UUID for a record that predates record
// IDs, so the same legacy replay always produces the same identity and
// conflicts as a replay rather than inserting a duplicate.
//
// Version 8 is the RFC 9562 slot for custom, implementation-defined layouts,
// which is exactly what a content digest is.
//
// Each field is written length-prefixed rather than joined on a separator.
// Joining on "\x00" made the digest ambiguous, because a part may contain
// that byte: an application name and a window title that differ only in
// where a NUL falls produced identical input, so two distinct records
// derived one identity and the second was discarded as a replay of the
// first. Nothing sanitizes titles on the way here -- they arrive from the
// window manager -- so the encoding has to carry the boundaries itself.
func Derive(producer Producer, parts ...string) string {
	digest := sha256.New()
	var header [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(header[:], uint64(len(value)))
		_, _ = digest.Write(header[:])
		_, _ = digest.Write([]byte(value))
	}

	// The producer is part of the digest input: the same segment observed by
	// the desktop tracker and by a browser is two records, not one.
	write(string(producer))
	// The count too, so Derive(p) and Derive(p, "") stay distinct.
	binary.BigEndian.PutUint64(header[:], uint64(len(parts)))
	_, _ = digest.Write(header[:])
	for _, part := range parts {
		write(part)
	}

	var raw [16]byte
	copy(raw[:], digest.Sum(nil)[:16])
	raw[6] = (raw[6] & 0x0f) | 0x80 // version 8
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return format(raw)
}

// Valid reports whether id is the canonical lowercase 36-character UUID text
// form. Uppercase, braced, and unhyphenated variants are rejected rather than
// normalized: accepting several spellings of one ID would let the same segment
// insert twice.
func Valid(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func format(raw [16]byte) string {
	buf := make([]byte, 0, 36)
	encoded := make([]byte, 32)
	hex.Encode(encoded, raw[:])
	for _, group := range [][2]int{{0, 8}, {8, 12}, {12, 16}, {16, 20}, {20, 32}} {
		if len(buf) > 0 {
			buf = append(buf, '-')
		}
		buf = append(buf, encoded[group[0]:group[1]]...)
	}
	return string(buf)
}
