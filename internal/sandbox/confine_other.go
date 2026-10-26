//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"os"
	"runtime"
)

// ProbeConfinement reports that this platform confines nothing.
func ProbeConfinement() (*HostConfinement, error) {
	return nil, &ConfinementUnavailableError{Reason: runtime.GOOS + " has no confinement this engine applies"}
}

// ConfineMain is the Landlock helper, which this platform does not have.
func ConfineMain(args []string) int {
	fmt.Fprintln(os.Stderr, "crewlet "+ConfineCommand+": Landlock is a Linux mechanism")
	return 2
}
