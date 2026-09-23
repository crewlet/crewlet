package period_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// A HOST'S OWN CLOCK IS NOT A CALENDAR, under any spelling that reaches it.
//
// `Local` is the process's clock and `localtime` is the tz database's symlink
// to /etc/localtime; time.LoadLocation accepts both and resolves each to
// whatever the reading host is set to, so two nodes would cut two different
// days from one name. The mixed-case spellings are here because a
// case-insensitive filesystem resolves them to the same symlink.
func TestAHostsOwnClockIsNotACalendar(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Local", "local", "LOCAL", "localtime", "LocalTime"} {
		loc, err := period.LoadZone(name)
		if !errors.Is(err, period.ErrHostClock) {
			t.Errorf("LoadZone(%q) = %v, %v; want %v", name, loc, err, period.ErrHostClock)
			continue
		}
		if !strings.Contains(err.Error(), "IANA") {
			t.Errorf("the refusal of %q does not say what to write instead: %v", name, err)
		}
	}
}

// THE EMPTY NAME IS UTC, and so is UTC: the zero value of a zone name is the
// default clock rather than an error, so a company that writes none has one.
func TestTheEmptyNameIsUTC(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "UTC"} {
		loc, err := period.LoadZone(name)
		if err != nil {
			t.Fatalf("LoadZone(%q): %v", name, err)
		}
		if loc != time.UTC {
			t.Errorf("LoadZone(%q) = %v, want UTC", name, loc)
		}
	}
}

// AN IANA NAME IS THE ZONE IT NAMES, and keeps the name, which is what a
// surface reporting the company's clock prints.
func TestAnIANANameIsTheZoneItNames(t *testing.T) {
	t.Parallel()
	loc, err := period.LoadZone("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadZone: %v", err)
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("the zone is called %q", loc.String())
	}
	// A summer instant, so the offset proves the rules loaded rather than
	// a fixed zone that happens to carry the name.
	if _, offset := time.Date(2026, time.July, 1, 12, 0, 0, 0, loc).Zone(); offset != -7*3600 {
		t.Errorf("July in Los Angeles is UTC%+d, want UTC-7", offset/3600)
	}
}

// A NAME THAT IS NOT A ZONE IS REFUSED NAMING IT, and not as a host clock:
// the two refusals send an operator to different fixes.
func TestANameThatIsNotAZoneIsRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Mars/Olympus", " Europe/Berlin", "europe/berlin/"} {
		loc, err := period.LoadZone(name)
		if err == nil {
			t.Errorf("LoadZone(%q) = %v, want a refusal", name, loc)
			continue
		}
		if errors.Is(err, period.ErrHostClock) {
			t.Errorf("LoadZone(%q) was refused as a host clock: %v", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}
}
