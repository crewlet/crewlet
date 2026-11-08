package accessdomain

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The sentinels: each is a fact a caller acts on differently.
var (
	// ErrRecorded reports a record whose subject already holds one: the turn
	// was told, the certificate or the command recorded, by this write's
	// earlier attempt or another node's. Nothing more is needed.
	ErrRecorded = errors.New("accessdomain: already recorded")

	// ErrNotDue reports an expiry of an hour that holds nothing past the
	// retention any more.
	ErrNotDue = errors.New("accessdomain: nothing in that hour is past its retention")

	// ErrUnreadable reports a sealed command this node's keyring does not
	// open: its key left the ring before the record aged out.
	ErrUnreadable = errors.New("accessdomain: a sealed value does not open under this node's keyring")

	// ErrConflict reports a write that lost its compare-and-set every round.
	ErrConflict = errors.New("accessdomain: the record's subject kept changing under this write")

	// ErrInvalid reports a write or a query whose input the domain refuses,
	// naming the field and what to change.
	ErrInvalid = errors.New("accessdomain: invalid")
)

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkText refuses a text field outside its bounds.
func checkText(field, value string, maxBytes int) error {
	switch {
	case !utf8.ValidString(value):
		return invalid("%s is not valid UTF-8", field)
	case len(value) > maxBytes:
		return invalid("%s is %d bytes and the maximum is %d", field, len(value), maxBytes)
	}
	return nil
}
