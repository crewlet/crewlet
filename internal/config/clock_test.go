package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// THE ONE CLOCK IS WHAT THE COMPANY WROTE, or UTC when it wrote none
// (ADR-0018).
func TestTheCompanysClockIsTheZoneItNames(t *testing.T) {
	t.Parallel()
	c, err := ParseCompany([]byte("name: Acme\ntimezone: America/Los_Angeles\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Location().String(); got != "America/Los_Angeles" {
		t.Errorf("the company's clock is %q, want America/Los_Angeles", got)
	}
	// The rules loaded, not only the name: July in Los Angeles is UTC-7.
	if _, offset := time.Date(2026, time.July, 1, 12, 0, 0, 0, c.Location()).Zone(); offset != -7*3600 {
		t.Errorf("July on the company's clock is UTC%+d, want UTC-7", offset/3600)
	}

	for name, company := range map[string]*Company{
		"a company that names no clock": {Name: "Acme"},
		"no company at all":             nil,
		// Assembled in code past validation: the default clock, the one
		// reading every node agrees on, rather than a nil location for
		// a reader to crash on.
		"a clock that does not load": {Name: "Acme", Timezone: "Mars/Olympus"},
		"a host's own clock":         {Name: "Acme", Timezone: "Local"},
	} {
		if got := company.Location(); got != time.UTC {
			t.Errorf("%s: the clock is %v, want UTC", name, got)
		}
	}
}

// A HOST'S OWN CLOCK IS REFUSED AT THE FIELD, through the one reader every zone
// name goes through — so the company's clock and a schedule's own zone are
// admitted by the same rule.
func TestAHostsOwnClockIsRefusedAsTheCompanysClock(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"Local", "localtime"} {
		_, err := ParseCompany([]byte("name: Acme\ntimezone: " + zone + "\n"))
		if !errors.Is(err, ErrUnknownValue) {
			t.Errorf("timezone: %s parsed as %v, want %v", zone, err, ErrUnknownValue)
			continue
		}
		if !strings.Contains(err.Error(), period.ErrHostClock.Error()) {
			t.Errorf("timezone: %s was refused without saying it is a host's "+
				"own clock: %v", zone, err)
		}
		var fault *Fault
		if !errors.As(err, &fault) || fault.Path.String() != "timezone" {
			t.Errorf("timezone: %s was refused at %v, want the field `timezone`", zone, err)
		}
	}
}
