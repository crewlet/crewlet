//go:build !(linux && (amd64 || arm64))

package main

import (
	"errors"
	"os"
)

// renameNoReplace refuses: the note helper runs only in a Linux box (its
// builds are linux amd64 and arm64), and is compiled elsewhere only for the
// tree's build; a rename that could replace an entry is never used instead.
func renameNoReplace(from, to string) error {
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.ErrUnsupported}
}
