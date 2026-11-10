package p

import "time"

// stamp is time.Time itself, under another name.
type stamp = time.Time

// stamped embeds a time.Time, whose methods it promotes.
type stamped struct {
	time.Time
}

// instant is a defined type over time.Time, which has none of its methods.
type instant time.Time
