//go:build !unix

package sandbox

import "errors"

// Placeholder is a mount-point placeholder (Linux backend only).
type Placeholder struct {
	Path string
	Dir  bool
}

func createPlaceholder(path string, dir bool) (Placeholder, error) {
	return Placeholder{}, errors.New("sandbox: placeholders are not supported on this platform")
}

// RemovePlaceholders is a no-op where placeholders are never created.
func RemovePlaceholders([]Placeholder) {}

func placeholderSkippable(error) bool { return false }
