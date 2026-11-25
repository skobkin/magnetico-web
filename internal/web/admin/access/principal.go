// Package access holds the one type that proves an administrator check
// happened: Principal. Data-access functions of the administrator section take
// a Principal, so a handler that forgot the role check does not compile, and
// the zero Principal (the only value another package can forge) is refused at
// run time by every function that receives it.
package access

import "errors"

// ErrForbidden is returned by data-access functions given an invalid Principal.
var ErrForbidden = errors.New("admin: forbidden")

// Principal is an authenticated administrator. Only the guard of the
// administrator section constructs one (Grant); a repository test checks that
// no other non-test file calls it.
type Principal struct {
	userID   int64
	username string
	granted  bool
}

// Grant returns the Principal of an administrator the guard has verified: an
// enabled account with the admin role, resolved from a session cookie.
func Grant(userID int64, username string) Principal {
	return Principal{userID: userID, username: username, granted: true}
}

// Valid reports whether the Principal was granted by the guard.
func (p Principal) Valid() bool { return p.granted && p.userID > 0 }

// UserID is the administrator's account id (0 for an invalid Principal).
func (p Principal) UserID() int64 { return p.userID }

// Username is the administrator's name.
func (p Principal) Username() string { return p.username }

// Check returns ErrForbidden for an invalid Principal.
func (p Principal) Check() error {
	if !p.Valid() {
		return ErrForbidden
	}
	return nil
}
