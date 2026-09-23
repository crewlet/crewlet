package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeBindings is the identity reader as a token's binding reads it.
type fakeBindings struct {
	rows     map[string]iamdomain.Sighting
	err      error
	lag      time.Duration
	deferred bool
}

func (f fakeBindings) PersonByLogin(_ context.Context, login string) (
	iamdomain.Sighting, error) {

	if f.err != nil {
		return iamdomain.Sighting{}, f.err
	}
	return f.rows[login], nil
}

func (f fakeBindings) Staleness(string) (time.Duration, bool) {
	return f.lag, f.deferred
}

// boundMachine is an active machine under a Tier A token's login, bound to a
// seat at chart position 900.
func boundMachine() iamdomain.Sighting {
	return iamdomain.Sighting{
		ID: "018f3a9c-0000-7000-8000-0000000000c1", Kind: iam.KindMachine,
		Stage: iam.StageActive, Login: "token:ops",
		Seat: "platform-lead", SeatAt: 900,
		// A ROW'S GRANTS ARE NOT THE TOKEN'S, and a case below holds
		// that they never travel.
		Grants: []iam.Grant{iam.GrantPeopleManage},
	}
}

// ONLY AN ACTIVE MACHINE BINDS A TIER A TOKEN.
//
// `token:<id>` is a machine's name. A PERSON row under it — enrolled before the
// login grammar was held to its kind, or by a build without that rule — is the
// finding this guards: honouring it made the deployment's own credential act
// as whoever had chosen that name. And a suspended or retired machine binds
// nothing, because a binding is a way of acting and only an active principal
// may act.
func TestOnlyAnActiveMachineBindsATierAToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		row   func(iamdomain.Sighting) iamdomain.Sighting
		bound bool
	}{
		{"an active machine bound to a seat", func(s iamdomain.Sighting) iamdomain.Sighting { return s }, true},
		{"a person holding the token's login", func(s iamdomain.Sighting) iamdomain.Sighting {
			s.Kind = iam.KindPerson
			return s
		}, false},
		{"a kind this build cannot name", func(s iamdomain.Sighting) iamdomain.Sighting {
			s.Kind = iam.Kind("delegate")
			return s
		}, false},
		{"a suspended machine", func(s iamdomain.Sighting) iamdomain.Sighting {
			s.Stage = iam.StageSuspended
			return s
		}, false},
		// AS THE READER ANSWERS ONE: the claimed columns, no kind, no
		// stage, and marked as what it is — which it used to answer as
		// an error, so this token got 503 on every guarded route.
		{"a reservation nobody finished enrolling", func(s iamdomain.Sighting) iamdomain.Sighting {
			return iamdomain.Sighting{ID: s.ID, Login: s.Login, Seat: s.Seat,
				SeatAt: s.SeatAt, Reserved: true}
		}, false},
		{"an active machine bound to nothing", func(s iamdomain.Sighting) iamdomain.Sighting {
			s.Seat, s.SeatAt = "", 0
			return s
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := fakeBindings{rows: map[string]iamdomain.Sighting{
				"token:ops": tc.row(boundMachine()),
			}}
			row, err := boundRowOf(t.Context(), dir, "token:ops")
			if err != nil {
				t.Fatalf("an answerable row was an error: %v", err)
			}
			if row.Found != tc.bound {
				t.Fatalf("bound = %v, want %v (row %+v)", row.Found, tc.bound, row)
			}
			if !tc.bound {
				if row.Seat != "" {
					t.Errorf("an unbound answer carries seat %q", row.Seat)
				}
				return
			}
			if row.Seat != "platform-lead" || row.SeatAt != 900 {
				t.Errorf("row %+v, want platform-lead at 900", row)
			}
			if len(row.Grants) != 0 || row.Colleague != "" {
				t.Errorf("the directory row's authority travelled with the "+
					"binding (%v, %q): a Tier A token's grants are Tier A's",
					row.Grants, row.Colleague)
			}
		})
	}
}

// A BINDING THIS NODE CANNOT VOUCH FOR IS UNKNOWN — and only a BINDING.
//
// Unknown is 503 and never "not bound": answered as the bare credential, one
// actor would author under a seat on one node and under the token's own name
// on the next. But it is only the WIDENING direction a stale node refuses: a
// bound row past the stall grace may be a binding the fleet has withdrawn, so
// honouring it is acting as a seat the token no longer holds — while an
// unbound answer from the same node is the narrower surface, and the one an
// operator's break-glass token (normally bound to nothing) must still get on a
// node whose identity applier is what they came to fix.
func TestABindingThisNodeCannotVouchForIsUnknown(t *testing.T) {
	t.Parallel()
	past := statelog.StallGrace + time.Second
	for _, tc := range []struct {
		name    string
		dir     fakeBindings
		unknown bool
	}{
		{"a directory that cannot be read",
			fakeBindings{err: errors.New("the replicated estate is not open")}, true},
		{"a bound row past the stall grace",
			fakeBindings{lag: past}, true},
		{"a bound row whose bucket holds a deferred record",
			fakeBindings{deferred: true}, true},
		{"a bound row inside the grace",
			fakeBindings{lag: statelog.StallGrace}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.dir.rows = map[string]iamdomain.Sighting{"token:ops": boundMachine()}
			row, err := boundRowOf(t.Context(), tc.dir, "token:ops")
			if tc.unknown {
				if err == nil {
					t.Fatalf("answered %+v with no error, so a node that cannot "+
						"say is read as one that said", row)
				}
				if row.Found || row.Seat != "" {
					t.Errorf("an unknown answer carries a binding: %+v", row)
				}
				return
			}
			if err != nil || !row.Found {
				t.Errorf("answered (%+v, %v), want the binding", row, err)
			}
		})
	}

	// AND THE NARROWING DIRECTION IS SERVED from the same stale node: no
	// row, a person's row, or a suspended machine all answer UNBOUND rather
	// than unknown, so a break-glass token keeps working.
	stale := fakeBindings{lag: past, deferred: true,
		rows: map[string]iamdomain.Sighting{"token:person": func() iamdomain.Sighting {
			s := boundMachine()
			s.Kind = iam.KindPerson
			return s
		}()}}
	for _, login := range []string{"token:nobody", "token:person"} {
		row, err := boundRowOf(t.Context(), stale, login)
		if err != nil || row.Found {
			t.Errorf("%s on a stale node answered (%+v, %v), want unbound", login,
				row, err)
		}
	}
}

// A NODE WITH NO IDENTITY DOMAIN CANNOT SAY, and says so.
//
// Its tables are legitimately empty because the domain is not running, not
// because nobody is bound — so the answer is an error, which the guard turns
// into 503, and never the zero row.
func TestANodeWithNoIdentityDomainCannotSay(t *testing.T) {
	t.Parallel()
	var e *Engine
	if _, err := e.BoundSeat(t.Context(), "token:ops"); !errors.Is(err, errNoIdentityDomain) {
		t.Errorf("a nil engine answered %v, want %v", err, errNoIdentityDomain)
	}
	if _, err := (&Engine{}).BoundSeat(t.Context(), "token:ops"); !errors.Is(err,
		errNoIdentityDomain) {
		t.Errorf("an engine with no identity reader answered %v, want %v", err,
			errNoIdentityDomain)
	}
}
