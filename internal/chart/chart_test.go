package chart_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A KEY IS FOLDED INTO SOMETHING A SUBJECT AND A PATH CAN BOTH CARRY.
//
// A unit's key is minted from its display name when the document declares no
// `id:`, so `Product Team` is a key a company can be running on today. Here a
// key is a BROKER SUBJECT TOKEN and a SCOPE PATH SEGMENT, and neither may
// contain a space — so the fold has to close that itself. Refusing the space
// would refuse a company that is valid; escaping it would mean a second
// alphabet that has to agree with this one for ever.
func TestAKeyIsFoldedIntoSomethingASubjectCanCarry(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ in, want string }{
		"already an address":       {"engineering", "engineering"},
		"a capitalised key":        {"Engineering", "engineering"},
		"a key minted from a name": {"Product Team", "product-team"},
		"runs of whitespace":       {"  Infrastructure   Tribe\t", "infrastructure-tribe"},
		"a handle is left alone":   {"sarah-chen", "sarah-chen"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := chart.NormalizeKey(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// AND THE FOLD IS IDEMPOTENT, which is what lets the applier
			// recompute a subject from a payload and compare it: a second
			// fold that moved the value would make every record look like
			// one that took one address and claimed another.
			if again := chart.NormalizeKey(got); again != got {
				t.Errorf("NormalizeKey(%q) = %q — the fold moves its own output", got, again)
			}
			// AND WHAT IT PRODUCES IS PUBLISHABLE AND FILEABLE. These are
			// the two rules the fold exists to satisfy, asserted on its
			// own output rather than assumed.
			if err := chart.UnitSubject(tc.in).Validate(); err != nil {
				t.Errorf("a subject built from %q is refused: %v", tc.in, err)
			}
			if strings.Contains(got, statelog.ScopeSeparator) {
				t.Errorf("%q carries the scope path separator", got)
			}
		})
	}
}

// AND A SUBJECT REFUSES A KEY THAT WOULD BE FILED WHERE NO PROBE LOOKS.
//
// The broker would take a key carrying the scope separator happily — `/` is a
// legal character in a NATS subject token. What it breaks is the deferral: the
// record is filed under a path with an extra level in it, and every probe for
// that object searches the path the key was meant to produce.
func TestASubjectRefusesAKeyThatBreaksTheGrammar(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		subject chart.Subject
		says    string
	}{
		"no kind at all": {
			chart.Subject{ID: "engineering"}, "no kind"},
		"a kind carrying the subject separator": {
			chart.Subject{Kind: "unit.content", ID: "engineering"}, "separator"},
		"a unit with no id": {
			chart.Subject{Kind: chart.KindUnit}, "needs an id"},
		"a key carrying a wildcard": {
			chart.Subject{Kind: chart.KindUnit, ID: "eng>"}, "wildcard"},
		"a key carrying the scope separator": {
			chart.Subject{Kind: chart.KindUnit, ID: "eng/platform"}, "scope path separator"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.subject.Validate()
			if err == nil {
				t.Fatalf("%v was accepted", tc.subject)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.says)
			}
		})
	}

	// The two kinds that legitimately have no id, so the rule above is not
	// simply "every subject needs one".
	for _, s := range []chart.Subject{chart.TreeSubject(), chart.BarrierSubject()} {
		if err := s.Validate(); err != nil {
			t.Errorf("%v is refused and it is a kind with exactly one object: %v", s, err)
		}
	}
}

// A VALUE PAST ITS CAP IS REFUSED NAMING THE FIELD, never silently cut.
//
// Every cap here is checked where a record is WRITTEN and never where one is
// applied, which is the asymmetry every value rule in this tree follows: the
// applier deliberately salvages, because refusing a record there would stop
// that object's every later change on every node.
func TestAnObjectPastItsCapsIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", chart.MaxProse+1)

	for name, tc := range map[string]struct {
		object interface{ Validate() error }
		says   string
	}{
		"a unit with no key": {
			chart.Unit{}, "key"},
		"a unit key that is not folded": {
			chart.Unit{Key: "Engineering"}, "folded form"},
		"a unit key past its cap": {
			chart.Unit{Key: strings.Repeat("u", chart.MaxKey+1)}, "key"},
		"a purpose past its cap": {
			chart.Unit{Key: "engineering", Purpose: long}, "purpose"},
		"too many retired keys": {
			chart.Unit{Key: "engineering", FormerKeys: keys(chart.MaxFormerKeys + 1)}, "former_keys"},
		"a seat with an unknown kind": {
			chart.Seat{Handle: "sarah-chen", Kind: "contractor"}, "kind"},
		"a backstory past its cap": {
			chart.Seat{Handle: "sarah-chen", Kind: chart.SeatAgent, Backstory: long}, "backstory"},
		"an address past its cap": {
			chart.Seat{Handle: "sarah-chen", Kind: chart.SeatAgent,
				Email: strings.Repeat("a", chart.MaxEmail+1)}, "email"},
		"too many responsibilities": {
			chart.Seat{Handle: "sarah-chen", Kind: chart.SeatAgent,
				Responsibilities: keys(chart.MaxList + 1)}, "responsibilities"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.object.Validate()
			if err == nil {
				t.Fatalf("accepted")
			}
			if !errors.Is(err, chart.ErrInvalid) {
				t.Errorf("refusal %v is not comparable to chart.ErrInvalid — a "+
					"caller deciding what to do about it has only the string", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to name %q — an error a person "+
					"reads names the field they have to change", err, tc.says)
			}
		})
	}

	// The control: the same objects, inside their caps.
	if err := (chart.Unit{Key: "engineering", Purpose: "ship the platform"}).Validate(); err != nil {
		t.Errorf("a valid unit is refused: %v", err)
	}
	if err := (chart.Seat{Handle: "sarah-chen", Kind: chart.SeatAgent,
		Email: "sarah.chen@example.com"}).Validate(); err != nil {
		t.Errorf("a valid seat is refused: %v", err)
	}
}

// keys builds n distinct short strings.
func keys(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, string(rune('a'+i%26))+strings.Repeat("k", 1+i/26))
	}
	return out
}
