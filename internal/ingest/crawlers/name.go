package crawlers

import (
	"errors"
	"regexp"
)

// ErrInvalidName is returned for a name outside 1–64 characters of
// [A-Za-z0-9._-] starting with a letter or digit.
var ErrInvalidName = errors.New("invalid crawler name: use 1-64 characters from A-Z a-z 0-9 . _ - starting with a letter or digit")

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateName reports ErrInvalidName for an unacceptable name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}
