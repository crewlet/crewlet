package p

import "time"

// stamp is time.Time itself, under another name.
type stamp = time.Time

// event carries two instants, read in a test file that does not import time.
type event struct {
	from, at time.Time
}

// stamped embeds a time.Time, whose Sub it promotes.
type stamped struct {
	time.Time
}

// instant is a defined type over time.Time, which has none of its methods.
type instant time.Time
