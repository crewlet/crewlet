package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// loginHeldBy is a person row under a login, bound to a seat decided at 900 — or
// to none, for an empty seat.
func loginHeldBy(login, seat string, stage iam.Stage) iamdomain.Sighting {
	return iamdomain.Sighting{
		ID: "018f3a9c-0000-7000-8000-00000000" + login[:4], Kind: iam.KindPerson,
		Stage: stage, Login: login, Seat: seat, SeatAt: 900,
	}
}

// A LOGIN NAMES THE RECORD ITS HOLDER ACTS UNDER — their seat as the chart
// knows it now, or the login for somebody bound to none — through the SAME
// two tables a request of theirs resolves through.
//
// The surfaces read somebody else's login literally: an administrator's
// `jane.doe`, for a person bound to a seat, read an empty inbox and wrote pins
// and inbox marks under the login, where no screen of hers reads them. A
// record written under one name and read under another is a record nobody has.
func TestALoginNamesTheRecordItsHolderActsUnder(t *testing.T) {
	t.Parallel()
	chart := companyChart()
	// A RENAME IS FOLLOWED: the binding names the handle the seat had,
	// and the chart answers the one it has now.
	chart.seats["platform-old"] = session.Seat{Handle: "platform-lead",
		Kind: session.SeatKindHuman}
	dir := fakeBindings{rows: map[string]iamdomain.Sighting{
		"jane.doe":  loginHeldBy("jane.doe", "platform-lead", iam.StageActive),
		"ren.amed":  loginHeldBy("ren.amed", "platform-old", iam.StageActive),
		"bo.smith":  loginHeldBy("bo.smith", "", iam.StageActive),
		"away.gone": loginHeldBy("away.gone", "platform-lead", iam.StageSuspended),
		"half.done": {ID: "018f3a9c-0000-7000-8000-0000000000aa",
			Login: "half.done", Reserved: true},
		"token:ops": boundMachine(),
		"token:off": func() iamdomain.Sighting {
			row := boundMachine()
			row.Login, row.Stage = "token:off", iam.StageSuspended
			return row
		}(),
	}}
	for _, c := range []struct {
		name, login, want string
	}{
		{"a bound person's login is their seat's record", "jane.doe", "platform-lead"},
		{"a renamed seat is followed", "ren.amed", "platform-lead"},
		{"an unbound person's login is their own record", "bo.smith", "bo.smith"},
		// SUSPENSION DOES NOT MOVE A RECORD: unsticking the queue of
		// somebody who is away is what writing another's record is for.
		{"a suspended person's record is still their seat's", "away.gone", "platform-lead"},
		{"a bound Tier A token acts as its seat", "token:ops", "platform-lead"},
		// A TIER A TOKEN IS HELD BY ITS ENTRY AND NOT BY A ROW, so a
		// token this node declares with no row is its own record rather
		// than nobody's — and an inactive binding is dropped exactly as
		// the request path drops it.
		{"a Tier A token nobody enrolled is its own record", "token:new", "token:new"},
		{"a suspended token's binding binds nothing", "token:off", "token:off"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := holderRecordOf(t.Context(), dir, chart,
				declares("ops", "new", "off"), c.login)
			if err != nil {
				t.Fatalf("holderRecordOf(%q) = %v", c.login, err)
			}
			if got != c.want {
				t.Errorf("holderRecordOf(%q) = %q, want %q", c.login, got, c.want)
			}
		})
	}
}

// declares is a Tier A token list holding exactly ids.
func declares(ids ...string) func(string) bool {
	return func(id string) bool {
		for _, held := range ids {
			if held == id {
				return true
			}
		}
		return false
	}
}

// NOBODY, AND A HOLDER WHOSE SEAT IS GONE, ARE TWO ANSWERS — and neither is
// what a node that cannot say gives.
//
// A login nobody holds, the reservation an unfinished enrolment leaves, and a
// token login this node's Tier A declares no entry for, name no record. A
// holder bound to a seat the chart has removed, or to an agent's seat, names
// none this node can say either: their record is not under the login, and
// writing it there is the second record nobody reads.
// Every other failure is UNKNOWN — a 503 — because "nobody" read off a
// directory or a chart this node could not read is a guess about whose record
// to write.
func TestNobodyAndAGoneSeatAreAnswersAndABlindNodeIsNot(t *testing.T) {
	t.Parallel()
	rows := map[string]iamdomain.Sighting{
		"half.done": {ID: "018f3a9c-0000-7000-8000-0000000000aa",
			Login: "half.done", Reserved: true},
		"left.lead": loginHeldBy("left.lead", "old-lead", iam.StageActive),
		"bot.bound": loginHeldBy("bot.bound", "triage-bot", iam.StageActive),
		"new.hire":  loginHeldBy("new.hire", "not-applied-yet", iam.StageActive),
		"jane.doe":  loginHeldBy("jane.doe", "platform-lead", iam.StageActive),
	}
	behind := companyChart()
	behind.position = 100 // below every binding's 900
	for _, c := range []struct {
		name  string
		dir   fakeBindings
		chart seatTable
		login string
		want  error // nil: the unknown arm
	}{
		{"a login nobody holds", fakeBindings{rows: rows}, companyChart(),
			"ghost.person", iam.ErrNoHolder},
		{"a reservation nobody fills yet", fakeBindings{rows: rows}, companyChart(),
			"half.done", iam.ErrNoHolder},
		{"a seat the chart removed", fakeBindings{rows: rows}, companyChart(),
			"left.lead", iam.ErrHolderUnseated},
		{"a seat that is an agent's", fakeBindings{rows: rows}, companyChart(),
			"bot.bound", iam.ErrHolderUnseated},
		{"a hire this node's chart has not applied", fakeBindings{rows: rows}, behind,
			"new.hire", nil},
		{"a directory that could not be read",
			fakeBindings{err: errors.New("store blip")}, companyChart(), "jane.doe", nil},
		{"a directory past the stall grace, for a holder",
			fakeBindings{rows: rows, lag: statelog.StallGrace + time.Second},
			companyChart(), "jane.doe", nil},
		{"a directory past the stall grace, for nobody",
			fakeBindings{rows: rows, lag: statelog.StallGrace + time.Second},
			companyChart(), "ghost.person", nil},
		{"a record this node cannot decode", fakeBindings{rows: rows, deferred: true},
			companyChart(), "ghost.person", nil},
		{"a chart this node cannot read", fakeBindings{rows: rows},
			seatTable{posErr: errors.New("no chart view")}, "jane.doe", nil},
		// A TOKEN LOGIN IS HELD BY ITS TIER A ENTRY. A typo is nobody's —
		// and so is a login whose entry is gone although the directory
		// still binds it, since no credential can act as it here.
		{"a token login no entry declares", fakeBindings{rows: rows},
			companyChart(), "token:opps", iam.ErrNoHolder},
		{"a bound token login no entry declares any more",
			fakeBindings{rows: map[string]iamdomain.Sighting{
				"token:ops": boundMachine()}},
			companyChart(), "token:ops", iam.ErrNoHolder},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := holderRecordOf(t.Context(), c.dir, c.chart,
				declares("new"), c.login)
			if got != "" || err == nil {
				t.Fatalf("holderRecordOf(%q) = %q, %v — want no record", c.login, got, err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("holderRecordOf(%q) = %v, want %v", c.login, err, c.want)
			}
			if c.want == nil && (errors.Is(err, iam.ErrNoHolder) ||
				errors.Is(err, iam.ErrHolderUnseated)) {

				t.Fatalf("holderRecordOf(%q) = %v on a node that cannot say — "+
					"want the unknown arm", c.login, err)
			}
		})
	}
}

// A NODE WITH NO IDENTITY DOMAIN CANNOT SAY WHOSE RECORD A LOGIN IS, and says
// so rather than reading an empty copy of the estate as "nobody".
func TestANodeWithNoIdentityDomainCannotSayWhoseRecord(t *testing.T) {
	t.Parallel()
	var e *Engine
	if _, err := e.HolderRecord(t.Context(), "jane.doe"); !errors.Is(err, errNoIdentityDomain) {
		t.Errorf("HolderRecord on a nil engine = %v, want %v", err, errNoIdentityDomain)
	}
}

// A TOKEN LOGIN IS HELD BY THE ENTRY THIS NODE BOOTED WITH, read from its own
// Tier A — the wiring half of [holderRecordOf]'s rule, on a real node.
//
// The rule is tested over an injected list above; what that cannot see is a
// node that never hands its configuration over, which answers ErrNoHolder for
// every token — so an administrator could not reach a real token's own record
// either, and nothing failed. The typo beside it is the defect itself: an
// entry nobody declares named its own record, so `set_priorities` wrote a
// queue under a credential that does not exist.
func TestATokenLoginIsHeldByTheEntryThisNodeBootedWith(t *testing.T) {
	t.Parallel()
	e := bootDirectoryNodeWith(t, nil, func(b *config.Bootstrap) {
		b.API.Auth.Tokens = []config.APIToken{{
			ID: "ops", Token: "a-token-value-nobody-reads-here",
			Grants: []iam.Grant{iam.GrantStateRead},
		}}
	})
	if got, err := e.HolderRecord(t.Context(), "token:ops"); err != nil || got != "token:ops" {
		t.Errorf("HolderRecord(token:ops) = %q, %v — want the token's own "+
			"record, since this node declares it and nothing binds it", got, err)
	}
	if got, err := e.HolderRecord(t.Context(), "token:opps"); !errors.Is(err, iam.ErrNoHolder) {
		t.Errorf("HolderRecord(token:opps) = %q, %v — want %v, since no entry "+
			"on this node has that id", got, err, iam.ErrNoHolder)
	}
}
