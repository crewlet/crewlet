package backup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A DESTINATION REFUSAL PROMISES NOTHING WAS COPIED. POST /backup leaves an
// ErrBadDestination out of the audit on that promise, so a store refusal that
// arrives once an estate is already in the directory must not wear it: the
// directory then holds a partial copy, and that is a failed backup somebody
// has to know about.
func TestAStoreRefusalAfterACopyIsAFailedBackupNotARefusal(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{
		fmt.Errorf("%w: /b/store-replicated.db", store.ErrBackupExists),
		fmt.Errorf("%w: /b/store-replicated.db", store.ErrBadBackupPath),
	} {
		if err := storeRefusal(refusal, 0); !errors.Is(err, ErrBadDestination) {
			t.Errorf("refused before any copy: %v, want ErrBadDestination", err)
		}
		err := storeRefusal(refusal, 1)
		if errors.Is(err, ErrBadDestination) {
			t.Errorf("refused after an estate was copied: %v, must not be ErrBadDestination", err)
		}
		if !errors.Is(err, refusal) {
			t.Errorf("the store's own refusal is lost: %v", err)
		}
	}
	engine := errors.New("disk I/O error")
	if err := storeRefusal(engine, 0); errors.Is(err, ErrBadDestination) {
		t.Errorf("an engine failure became a destination refusal: %v", err)
	}
}

// THE CALLER'S PATH, NOT THE HOST'S DISK. A path through a file, one that may
// not be written or a read-only mount is fixed by naming another directory; an
// I/O error or a full disk is not, and answering 400 for it would send an
// operator to re-type a path on a host that is failing.
func TestPreparingADirectoryBlamesThePathOnlyForThePathsFaults(t *testing.T) {
	t.Parallel()
	pathErr := func(errno syscall.Errno) error {
		return &os.PathError{Op: "mkdir", Path: "/x", Err: errno}
	}
	for errno, caller := range map[syscall.Errno]bool{
		syscall.ENOTDIR:      true,
		syscall.ENOENT:       true,
		syscall.EEXIST:       true,
		syscall.EACCES:       true,
		syscall.EPERM:        true,
		syscall.EROFS:        true,
		syscall.ENAMETOOLONG: true,
		syscall.ELOOP:        true,
		syscall.EIO:          false,
		syscall.ENOSPC:       false,
		syscall.EDQUOT:       false,
	} {
		err := prepareFailure("create", "/x", "name another", pathErr(errno))
		if got := errors.Is(err, ErrBadDestination); got != caller {
			t.Errorf("%v: destination refusal = %v, want %v (%v)", errno, got, caller, err)
		}
		if !errors.Is(err, errno) {
			t.Errorf("%v: the cause is lost: %v", errno, err)
		}
	}
}
