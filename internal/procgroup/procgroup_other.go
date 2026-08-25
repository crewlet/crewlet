//go:build !unix

package procgroup

import (
	"errors"
	"os/exec"
)

// The signal identifiers are placeholders on a platform with no process
// groups; nothing reads them, because signal() refuses before it would.
const (
	sigTerm = 0
	sigKill = 0
)

// set is a no-op where process groups are not a thing.
//
// The consequence is stated rather than hidden: on such a platform a
// launcher's grandchild can outlive its parent and this package cannot reach
// it. Pretending otherwise — by signalling the leader and reporting success —
// would make a leak look like a clean shutdown.
func set(*exec.Cmd) {}

// signal reports that the platform has no group to signal. The caller logs
// it; there is nothing else honest to do.
func signal(int, int) error { return errors.ErrUnsupported }
