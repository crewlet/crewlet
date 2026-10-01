package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// THE TWO CLOCKS THE COMPANY'S ONE CLOCK REPLACED ARE REFUSED BY NAME, and
// each refusal says where the value went (ADR-0018).
//
// Both shipped in the example companies, so an operator has them written down;
// "check the spelling" would send somebody looking for a typo in a key they
// were told to write. And the scheduler's refusal says the one thing that is
// not a pure rename: a company that set the two to DIFFERENT zones fired its
// zone-less schedules on one and cut its tracker's days on the other, and one
// clock at the top moves the schedules.
//
// Strict decoding is what refuses them — no field carries either name any
// more, so a key the loader silently dropped would be a company running on UTC
// while its file said Berlin.
func TestTheRetiredClocksAreRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc  string
		path string
		says []string
	}{
		"the tracker's clock": {
			"name: Acme\ntracker:\n  native:\n    timezone: Europe/Berlin\n",
			"tracker.native.timezone",
			[]string{"top-level `timezone`", "Move the value there"},
		},
		"the scheduler's default zone": {
			"name: Acme\nscheduling:\n  default_timezone: UTC\n",
			"scheduling.default_timezone",
			[]string{"top-level `timezone`", "its own `timezone:`"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCompany([]byte(tc.doc))
			if err == nil {
				t.Fatal("a retired clock was accepted")
			}
			if !errors.Is(err, ErrUnknownField) {
				t.Errorf("want %v, got %v", ErrUnknownField, err)
			}
			if strings.Contains(err.Error(), "check the spelling") {
				t.Errorf("a key the example companies shipped was reported as "+
					"a misspelling: %v", err)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("the refusal does not name %s: %v", tc.path, err)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}

// THE ONE CLOCK IS WHAT THE COMPANY WROTE, or UTC when it wrote none.
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
