package assistant_test

import "errors"

// errorAs reports whether err holds a T.
func errorAs[T error](err error) (T, bool) {
	var target T
	ok := errors.As(err, &target)
	return target, ok
}
