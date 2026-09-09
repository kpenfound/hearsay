package connector

import (
	"fmt"
	"slices"
	"strings"
)

// safeBytes are the bytes an event id carries unescaped. They are the
// characters source ids and native ids are actually made of — GitHub's
// `owner/repo#12`, Slack's `C123:1725812345.0001`, a Drive file id and the
// `@` a revision hangs off — so an id stays readable in a log line and in a
// bundle's provenance pointer.
const safeBytes = "-._~:@/#+=,"

// ValidSourceID reports whether s is a well-formed source id: 1 to
// [MaxSourceIDLen] bytes of lowercase letters, digits, `-` and `_`, starting
// with a letter or a digit. The charset is deliberately narrow: source ids
// appear in event ids, which are split on their colons, and they are typed into
// config by hand.
//
// It is exported so that configuration can reject a bad source id where it is
// written rather than when a connector is built from it.
func ValidSourceID(s string) bool {
	if s == "" || len(s) > MaxSourceIDLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// isLowerWord reports whether s is a lowercase word an extension kind is built
// from: `figma`, `sprint`, `file_comment`.
func isLowerWord(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case (c >= '0' && c <= '9') || c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isSpaceOrControl(r rune) bool {
	return r <= ' ' || r == 0x7f
}

func sortKinds(kinds []Kind) {
	slices.SortFunc(kinds, func(a, b Kind) int { return strings.Compare(string(a), string(b)) })
}

// encodeSegment percent-encodes everything outside the safe set, including the
// percent sign itself, so the encoding round-trips.
func encodeSegment(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if isSafeByte(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// decodeSegment reverses [encodeSegment].
func decodeSegment(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated %% escape at byte %d", i)
		}
		hi, err := unhex(s[i+1])
		if err != nil {
			return "", fmt.Errorf("bad %% escape at byte %d: %w", i, err)
		}
		lo, err := unhex(s[i+2])
		if err != nil {
			return "", fmt.Errorf("bad %% escape at byte %d: %w", i, err)
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func isSafeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte(safeBytes, c) >= 0
	}
}

func unhex(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	default:
		return 0, fmt.Errorf("%q is not a hex digit", c)
	}
}
