package seataccess_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// opsOf is the ops a plan takes on one lane, in order.
func opsOf(plan map[seataccess.Lane][]seataccess.Step, lane seataccess.Lane) []seataccess.Op {
	var out []seataccess.Op
	for _, step := range plan[lane] {
		out = append(out, step.Op)
	}
	return out
}

// A CREDENTIAL DELETED AT THE VENDOR IS RE-MINTED. The ledger records a sealed
// credential, and the vendor proves the account holds none, so the sealed value
// authenticates as nothing: a seat that is on is given a fresh one, rather than
// being reported in sync over a credential every call refuses.
func TestACredentialGoneAtTheVendorIsReMinted(t *testing.T) {
	t.Parallel()
	vendor := seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY")
	lane := seataccess.LaneDatadog
	record := &seataccess.Record{
		Kind: integration.KindDatadog, Handle: "sre",
		Account:    &seataccess.Account{ID: "u-1", State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "DATADOG_APP_KEY_SRE"},
	}
	on := seataccess.Desired{Enabled: true, Level: "a"}
	held := seataccess.Observed{Exists: true, Enabled: true, Level: "a", AccountID: "u-1"}
	gone := held
	gone.CredentialGone = true

	for _, tc := range []struct {
		name string
		want seataccess.Desired
		have seataccess.Observed
		ask  seataccess.Ask
		ops  []seataccess.Op
	}{
		{name: "held: nothing to do", want: on, have: held},
		{name: "gone: re-minted", want: on, have: gone, ops: []seataccess.Op{seataccess.OpMint}},
		{
			name: "gone with a rotation asked: one mint, no rotation of a dead key",
			want: on, have: gone, ask: seataccess.Ask{Rotate: true},
			ops: []seataccess.Op{seataccess.OpMint},
		},
		{
			name: "gone and the seat off: no mint, and the dead sealed value is cleared",
			want: seataccess.Desired{Level: "a"}, have: gone,
			ops: []seataccess.Op{seataccess.OpDisable, seataccess.OpRevoke},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := seataccess.Plan(vendor, seataccess.SeatInput{
				Seat:   seataccess.SeatRef{Handle: "sre"},
				Want:   map[seataccess.Lane]seataccess.Desired{lane: tc.want},
				Have:   map[seataccess.Lane]seataccess.Observed{lane: tc.have},
				Record: record, Ask: tc.ask,
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if got := opsOf(plan, lane); !slices.Equal(got, tc.ops) {
				t.Errorf("ops = %q, want %q", got, tc.ops)
			}
		})
	}
}

// THE RETIRE AFTER A MINT READS THE MINT'S CREDENTIAL ID, the latest first,
// and nothing where no earlier step minted one.
func TestPriorCredentialID(t *testing.T) {
	t.Parallel()
	in := seataccess.ApplyInput{Prior: []seataccess.StepResult{
		{AccountID: "u-1"},
		{Minted: []seataccess.Minted{{Field: "APP_KEY", CredentialID: "k-1"}}},
		{Minted: []seataccess.Minted{{Field: "APP_KEY", CredentialID: "k-2"}, {Field: "EMAIL"}}},
	}}
	if got := in.PriorCredentialID("APP_KEY"); got != "k-2" {
		t.Errorf("APP_KEY = %q, want the latest mint", got)
	}
	if got := in.PriorCredentialID("EMAIL"); got != "" {
		t.Errorf("EMAIL = %q, want none: that mint carried no id", got)
	}
	if got := (seataccess.ApplyInput{}).PriorCredentialID("APP_KEY"); got != "" {
		t.Errorf("an op's first step = %q, want none", got)
	}
}

// WHO TURNED AN ACCOUNT OFF is the ledger's answer where it has a record, and
// the vendor's marker only where it has none: a stale marker never makes
// somebody's own disable read as this engine's.
func TestDisabledByOf(t *testing.T) {
	t.Parallel()
	recorded := func(state seataccess.AccountState) seataccess.Record {
		return seataccess.Record{Account: &seataccess.Account{ID: "u-1", State: state}}
	}
	for _, tc := range []struct {
		name     string
		marked   bool
		rec      seataccess.Record
		recorded bool
		want     seataccess.DisabledBy
	}{
		{"ledger says off", false, recorded(seataccess.AccountDisabled), true, seataccess.DisabledByCrewlet},
		{"ledger says active, stale marker", true, recorded(seataccess.AccountActive), true, seataccess.DisabledByHand},
		{"no record, marker", true, seataccess.Record{}, false, seataccess.DisabledByCrewlet},
		{"no record, no marker", false, seataccess.Record{}, false, seataccess.DisabledByHand},
		{"record with no account, marker", true, seataccess.Record{Handle: "sre"}, true, seataccess.DisabledByCrewlet},
	} {
		if got := seataccess.DisabledByOf(tc.marked, tc.rec, tc.recorded); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
