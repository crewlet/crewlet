package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
)

// THE COMPANY'S CLOCK IS THE CURRENT EPOCH'S, AND AN APPLY MOVES IT (ADR-0018).
//
// [engine.Engine.Zone] is what the surfaces that outlive an epoch read (the
// scheduler's loop, built once, and the operator's surface), so a founder
// correcting `timezone` changes what they do without a restart.
func TestTheCompanysClockFollowsAnApply(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if got := e.Zone(); got != time.UTC {
		t.Fatalf("a company that names no clock runs on %v, want UTC", got)
	}

	berlin := parsedCompany(t, strings.Replace(companyDoc,
		"name: Acme", "name: Acme\ntimezone: Europe/Berlin", 1))
	if _, _, err := e.Apply(t.Context(), berlin, time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := e.Zone().String(); got != "Europe/Berlin" {
		t.Fatalf("after the apply the company runs on %q, want Europe/Berlin", got)
	}
}
