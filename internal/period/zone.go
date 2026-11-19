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
// An IANA name (`Europe/Berlin`) or `UTC`; the empty name is UTC. Every reader
// of a zone name goes through here: a name one accepts and another refuses is
// a schedule that validates and never fires.
//
// A host's own clock is refused. [time.LoadLocation] accepts `Local` and
// `localtime` (a symlink to /etc/localtime on Debian and Ubuntu), which mean
// whatever the reading host is set to: two nodes would cut "today" at two
// midnights (see the package doc), and a duty moving hosts would fire a 09:00
// schedule at another instant. Refused by name, case-insensitively, since a
// case-insensitive filesystem resolves `LocalTime` to the same symlink.
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
