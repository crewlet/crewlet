//go:build !linux && !darwin

package filelock

import (
	"errors"
	"os"
)

// tryLock refuses: this platform has no flock here.
//
// No release target is on such a platform — internal/store refuses to compile
// off linux and darwin (see its platform.go), and that refusal is the one
// place the supported set is named. This file exists so a build for another
// platform COMPILES THIS PACKAGE and reaches that message, rather than
// stopping one package earlier on an undefined function here, which says
// nothing about why. There was a Windows twin once (LockFileEx, mandatory
// rather than advisory); it went with the Windows release target.
func tryLock(*os.File) (bool, error) { return false, errors.ErrUnsupported }

// unlock has nothing to drop, since tryLock never took anything.
func unlock(*os.File) error { return errors.ErrUnsupported }
