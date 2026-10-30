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
