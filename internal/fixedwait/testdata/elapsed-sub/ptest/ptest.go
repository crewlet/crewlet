package ptest

import "time"

// Took is a support package's own reading of the clock, in a file that is
// not a test file: the gate reaches it only as part of the package.
func Took(start time.Time) time.Duration {
	return time.Now().Sub(start)
}
