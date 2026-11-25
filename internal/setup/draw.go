package setup

import (
	"errors"
	"fmt"
)

// Draw is what a live public view is drawn from ([PublicView.Live]): Field is
// the requirement whose view it is, and Value answers any field of the same
// list as [Check.Value] does, what is typed, else what is saved (a credential
// resolved on this node), else the default.
type Draw struct {
	Field   string
	Company string
	Value   func(field string) string
}

// Drawer draws a requirement's live view from what the form holds, writing
// nothing. An error wrapping [ErrUnusableInput] is a sentence saying what to
// type; [ErrNotDrawable] is a field it draws no view for.
type Drawer func(d Draw) (*PublicView, error)

// ErrNotDrawable marks a drawer asked about a field it draws no live view for.
var ErrNotDrawable = errors.New("setup: this drawer draws no view for that field")

// NotDrawable is the error a drawer answers for a field it draws nothing for.
func NotDrawable(app, field string) error {
	return fmt.Errorf("%w: the %s drawer was asked about %q", ErrNotDrawable, app, field)
}
