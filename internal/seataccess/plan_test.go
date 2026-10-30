package seataccess_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

const dd = seataccess.LaneDatadog

var sre = seataccess.SeatRef{Handle: "sre", Name: "SRE"}

func on(level string, scope ...string) seataccess.Desired {
	return seataccess.Desired{Enabled: true, Level: level, Scope: scope}
}

func held(level string, scope ...string) seataccess.Observed {
	return seataccess.Observed{Exists: true, Enabled: true, Level: level, Scope: scope,
		Credential: &seataccess.Credential{Variable: "DATADOG_APP_KEY_SRE"}}
}

func ops(steps []seataccess.Step) []seataccess.Op {
	var out []seataccess.Op
	for _, s := range steps {
		out = append(out, s.Op)
	}
	return out
}

func applied(d seataccess.Desired) *seataccess.Record {
	return &seataccess.Record{
		Kind: integration.KindDatadog, Handle: "sre",
		Account:    &seataccess.Account{ID: "acct-1", State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "DATADOG_APP_KEY_SRE"},
		Lanes:      map[seataccess.Lane]seataccess.LaneRecord{dd: {Applied: &d}},
	}
}

// THE SHARED RULES, one row each, on a single-lane vendor.
func TestPlanDecidesTheOpsEveryVendorShares(t *testing.T) {
	t.Parallel()
	off := seataccess.Desired{}
	disabledByHand := held("a")
	disabledByHand.Enabled, disabledByHand.DisabledBy = false, seataccess.DisabledByHand
	disabledByUs := held("a")
	disabledByUs.Enabled, disabledByUs.DisabledBy = false, seataccess.DisabledByCrewlet
	noCred := held("a")
	noCred.Credential = nil

	cases := []struct {
		name   string
		want   seataccess.Desired
		have   seataccess.Observed
		record *seataccess.Record
		ask    seataccess.Ask
		ops    []seataccess.Op
		drift  bool
	}{
		{name: "on with no account creates it", want: on("a"), ops: []seataccess.Op{seataccess.OpCreate}},
		{name: "on and converged needs nothing", want: on("a", "x"), have: held("a", "x"),
			record: applied(on("a", "x"))},
		{name: "off disables and then revokes, keeping the account (D1)", want: off,
			have: held("a"), record: applied(on("a")),
			ops: []seataccess.Op{seataccess.OpDisable, seataccess.OpRevoke}},
		{name: "off with no account needs nothing", want: off},
		{name: "a level edit moves the account, and is not drift", want: on("b"),
			have: held("a"), record: applied(on("a")), ops: []seataccess.Op{seataccess.OpLevel}},
		{name: "a level changed at the vendor is restored as drift (D2)", want: on("a"),
			have: held("b"), record: applied(on("a")), ops: []seataccess.Op{seataccess.OpLevel}, drift: true},
		{name: "a scope edit narrows the account", want: on("a", "x"), have: held("a", "x", "y"),
			record: applied(on("a", "x", "y")), ops: []seataccess.Op{seataccess.OpScope}},
		{name: "an account disabled by hand stays off unasked (D3)", want: on("a"),
			have: disabledByHand, record: applied(on("a"))},
		{name: "an account disabled by hand is turned back on when asked", want: on("a"),
			have: disabledByHand, record: applied(on("a")), ask: seataccess.Ask{Reenable: true},
			ops: []seataccess.Op{seataccess.OpEnable}},
		{name: "an account this engine disabled comes back with its seat", want: on("a"),
			have: disabledByUs, record: applied(off), ops: []seataccess.Op{seataccess.OpEnable}},
		{name: "an account with nothing sealed is minted for", want: on("a"), have: noCred,
			ops: []seataccess.Op{seataccess.OpMint}},
		{name: "a rotate asked of an account that is on rotates", want: on("a"), have: held("a"),
			record: applied(on("a")), ask: seataccess.Ask{Rotate: true}, ops: []seataccess.Op{seataccess.OpRotate}},
		{name: "a remove asked of a seat that is off removes", want: off,
			have: held("a"), record: applied(on("a")), ask: seataccess.Ask{Remove: true},
			ops: []seataccess.Op{seataccess.OpRemove}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY")
			plan, err := seataccess.Plan(v, seataccess.SeatInput{
				Seat: sre, Want: map[seataccess.Lane]seataccess.Desired{dd: tc.want},
				Have: map[seataccess.Lane]seataccess.Observed{dd: tc.have}, Record: tc.record, Ask: tc.ask,
			})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if got := ops(plan[dd]); !slices.Equal(got, tc.ops) {
				t.Fatalf("ops = %v, want %v", got, tc.ops)
			}
			for _, step := range plan[dd] {
				if step.Drift != tc.drift {
					t.Errorf("step %s drift = %v, want %v", step.Op, step.Drift, tc.drift)
				}
			}
		})
	}
}

// REMOVE IS REFUSED WHILE THE SEAT IS ON (D1), on any lane of the account.
func TestPlanRefusesARemoveWhileAnyLaneIsOn(t *testing.T) {
	t.Parallel()
	v := seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")
	_, err := seataccess.Plan(v, seataccess.SeatInput{
		Seat: sre,
		Want: map[seataccess.Lane]seataccess.Desired{
			seataccess.LaneJira: {}, seataccess.LaneConfluence: on("a"),
		},
		Have: map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneJira: held("a"), seataccess.LaneConfluence: held("a"),
		},
		Ask: seataccess.Ask{Remove: true},
	})
	if !errors.Is(err, seataccess.ErrStillOn) {
		t.Fatalf("Plan = %v, want ErrStillOn", err)
	}
}

// ONE ACCOUNT, TWO LANES: the second lane is granted on the account the first
// creates, and the credential is revoked only once both are off.
func TestPlanTreatsAtlassianAsOneAccountOverTwoLanes(t *testing.T) {
	t.Parallel()
	v := seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")
	plan, err := seataccess.Plan(v, seataccess.SeatInput{
		Seat: sre,
		Want: map[seataccess.Lane]seataccess.Desired{
			seataccess.LaneJira: on("a"), seataccess.LaneConfluence: on("b"),
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := ops(plan[seataccess.LaneJira]); !slices.Equal(got, []seataccess.Op{seataccess.OpCreate}) {
		t.Errorf("jira ops = %v, want one create", got)
	}
	if got := ops(plan[seataccess.LaneConfluence]); !slices.Equal(got, []seataccess.Op{seataccess.OpEnable}) {
		t.Errorf("confluence ops = %v, want the product granted on the new account", got)
	}

	oneOff, err := seataccess.Plan(v, seataccess.SeatInput{
		Seat: sre,
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneJira: {}, seataccess.LaneConfluence: on("a")},
		Have: map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneJira: held("a"), seataccess.LaneConfluence: held("a"),
		},
		Record: &seataccess.Record{Credential: &seataccess.Credential{Variable: "ATLASSIAN_TOKEN_SRE"}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := ops(oneOff[seataccess.LaneJira]); !slices.Equal(got, []seataccess.Op{seataccess.OpDisable}) {
		t.Errorf("one product off: jira ops = %v, want a disable and no revoke", got)
	}
}
