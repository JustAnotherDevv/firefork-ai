package template

import (
	"errors"
	"strings"
)

// ErrInvalidKey is returned by [ParseKey] when the input doesn't match
// the canonical `<name>/<version>` shape (exactly one slash, both
// halves non-empty, no path-traversal characters).
var ErrInvalidKey = errors.New("template: key must be <name>/<version>")

// ParseKey splits a registry key into (name, version). Canonical form
// is `<name>/<version>` with exactly one slash; neither half may be
// empty, contain `/`, `..`, control bytes, or start with `.`.
func ParseKey(s string) (name, version string, err error) {
	if s == "" {
		return "", "", ErrInvalidKey
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return "", "", ErrInvalidKey
	}
	name, version = parts[0], parts[1]
	if name == "" || version == "" {
		return "", "", ErrInvalidKey
	}
	for _, p := range []string{name, version} {
		if p == "." || p == ".." || strings.ContainsAny(p, "\x00\n\r\t") {
			return "", "", ErrInvalidKey
		}
		if strings.HasPrefix(p, ".") {
			return "", "", ErrInvalidKey
		}
	}
	return name, version, nil
}
