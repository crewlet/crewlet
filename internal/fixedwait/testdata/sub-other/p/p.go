package p

import "time"

// set is a type of the fixture's own with a Sub of its own.
type set map[string]bool

func (s set) Sub(o set) set { return s }

// instant is a defined type over time.Time: it has none of time.Time's
// methods, and the Sub it declares is its own.
type instant time.Time

func (i instant) Sub(o instant) time.Duration { return 0 }
