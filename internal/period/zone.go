package period

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrHostClock is a zone name that means whichever host reads it.
var ErrHostClock = errors.New("a host's own clock is not a calendar every node shares")

// LoadZone reads a clock's name into the location a calendar is cut on.
//
// An IANA name (`Europe/Berlin`), or `UTC`, and the empty name is UTC: the
// zero value of a name is the default clock, so a company that writes none
// still has one.
//
// # One rule for every reader of a zone name
//
// The company's `timezone`, a schedule's own `timezone`, the validator that
// admits each and the scheduler that fires on each all read a name through
// this function, because a name one of them accepts and another refuses is a
// schedule that validates and never fires, or fires on a clock nothing
// admitted.
//
// # A host's own clock is refused
//
// [time.LoadLocation] accepts two names that are not a zone at all but a
// POINTER to one: `Local`, the process's own clock from its TZ variable or
// /etc/localtime, and `localtime`, which the tz database on Debian and Ubuntu
// ships as a symlink to /etc/localtime. Each resolves to whatever zone the
// host that reads it is set to. That is the one thing a calendar may not be:
// two nodes in two regions would read one name as two zones and cut "today"
// at two midnights, which is exactly the disagreement a label exists to rule
// out (see the package doc), and a fleet-singleton duty that moved from one
// host to the other would fire a 09:00 schedule at a different instant with
// nothing in the configuration having changed. So both are refused by name,
// case-insensitively because a case-insensitive filesystem resolves
// `LocalTime` to the same symlink, and the error says what to write instead.
func LoadZone(name string) (*time.Location, error) {
	if strings.EqualFold(name, "Local") || strings.EqualFold(name, "localtime") {
		return nil, fmt.Errorf("%w: %q is whatever zone the host reading it "+
			"is set to, so two nodes would cut two different days from it — "+
			"write the IANA name of the zone you mean (Europe/Berlin, "+
			"America/New_York) or UTC", ErrHostClock, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not an IANA time zone (Europe/Berlin, "+
			"America/New_York, UTC): %w", name, err)
	}
	return loc, nil
}
