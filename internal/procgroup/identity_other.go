//go:build !linux && !darwin

package procgroup

import "errors"

// inspect has no kernel record to read on a platform outside the release
// matrix. It says so rather than answering "no such process", because absence
// is what licenses a caller to act on a pid, and an unread record licenses
// nothing.
func inspect(int) (Process, bool, error) {
	return Process{}, false, errors.ErrUnsupported
}

// groupLiving has no kernel record to read either, for the same reason: a
// group reported as having no living member is one a caller may discard.
func groupLiving(int) (bool, error) {
	return false, errors.ErrUnsupported
}
