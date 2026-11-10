package q

import "time"

// event carries two instants, compared in a test file that does not import
// time: the only file of its package that can hold a reference.
type event struct {
	from, at time.Time
}
