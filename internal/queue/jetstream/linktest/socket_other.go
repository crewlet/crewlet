//go:build !linux && !darwin

package linktest

import (
	"errors"
	"syscall"
)

// errUnpinnable is a platform whose sockets the relay cannot pin or read: the
// package compiles everywhere, so a build for another platform reaches the
// store's own message naming the platforms it supports
// (internal/store/platform.go), and a case that opens a link here fails at it.
var errUnpinnable = errors.New("linktest: this platform's socket buffers cannot be pinned or read")

func pin(_, _ string, _ syscall.RawConn) error { return errUnpinnable }

func granted(syscall.RawConn) (Buffers, error) { return Buffers{}, errUnpinnable }

func unreadOn(uintptr) (int, error) { return 0, errUnpinnable }
