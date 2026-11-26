//go:build linux && (amd64 || arm64)

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// renameNoReplace moves from to to by renameat2(2) with RENAME_NOREPLACE, so
// an entry already at to, of any kind, fails the move (EEXIST) and is never
// replaced, whatever came there after a check of it. std's syscall has no
// wrapper for it, so it is the raw call with the arch's number
// ([sysRenameat2]), the note helper's builds being linux amd64 and arm64.
func renameNoReplace(from, to string) error {
	f, err := syscall.BytePtrFromString(from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	t, err := syscall.BytePtrFromString(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	cwd := atFDCWD
	_, _, errno := syscall.Syscall6(sysRenameat2, uintptr(cwd), uintptr(unsafe.Pointer(f)), uintptr(cwd),
		uintptr(unsafe.Pointer(t)), renameNoReplaceFlag, 0)
	if errno != 0 {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errno}
	}
	return nil
}

// atFDCWD and renameNoReplaceFlag are AT_FDCWD and RENAME_NOREPLACE
// (linux/fcntl.h, linux/fs.h).
const (
	atFDCWD             = -100
	renameNoReplaceFlag = 1
)
