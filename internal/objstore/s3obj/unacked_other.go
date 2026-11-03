//go:build !linux && !darwin

package s3obj

// socketUnacked answers that no socket's count can be read here: the package
// compiles everywhere, so a build for another platform reaches the store's
// own message naming the platforms it supports (internal/store/platform.go),
// and a watch that cannot read a socket ends nothing ([watchedClient]).
func socketUnacked(uintptr) (int, error) { return 0, errUncountable }
