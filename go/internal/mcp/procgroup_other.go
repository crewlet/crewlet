//go:build !unix

package mcp

import (
	"errors"
	"os/exec"
)

// setProcessGroup is a no-op where process groups are not a thing.
//
// The consequence is stated rather than hidden: on such a platform a package
// runner's grandchild can outlive the server and this package cannot reach it.
// Pretending otherwise — by killing the leader and reporting success — would
// make a leak look like a clean shutdown.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup reports that the platform has no group to kill. The caller
// logs it; there is nothing else honest to do.
func killProcessGroup(int) error { return errors.ErrUnsupported }
