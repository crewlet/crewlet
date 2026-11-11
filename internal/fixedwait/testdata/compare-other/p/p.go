package p

import "time"

// version is a type of the fixture's own with a Compare, a Before and an
// After of its own.
type version struct{ major int }

func (v version) Compare(o version) int { return v.major - o.major }

func (v version) Before(o version) bool { return v.major < o.major }

func (v version) After(o version) bool { return v.major > o.major }

// instant is a defined type over time.Time: it has none of time.Time's
// methods, and the Before, After and Compare it declares are its own.
type instant time.Time

func (i instant) Before(o instant) bool { return false }

func (i instant) After(o instant) bool { return false }

func (i instant) Compare(o instant) int { return 0 }

// page is a cursor whose fields are named After and Before.
type page struct{ After, Before string }
