//go:build !linux && !darwin

package sockcount

// socketUnacked answers that no socket's count can be read here: see the
// package doc's "Where it reads".
func socketUnacked(uintptr) (int, error) { return 0, ErrUncountable }

// socketAcked answers the same.
func socketAcked(uintptr, uint64) (uint64, error) { return 0, ErrUncountable }
