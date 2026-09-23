package credential_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

var checkAt = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

const stall = time.Minute

// liveRow is a token row that checks out: the mint applied, the secret
// verifies, the owner active and not signed out since.
func liveRow(token credential.Token, verifier string) credential.TokenRow {
	return credential.TokenRow{
		Applied: token.Position + 10, Found: true, IsToken: true,
		Verifier: verifier, ExpiresAt: checkAt.Add(24 * time.Hour),
		Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
		Colleague: iam.ColleagueWrite, Epoch: 2,
		Generation: 1, FleetGeneration: 1,
		Owner: credential.TokenOwner{
			Found: true, ID: "0192f00d-0000-7000-8000-00000000000a",
			Kind: iam.KindPerson, Stage: iam.StageActive, Login: "jane.doe",
			Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
			Colleague: iam.ColleagueWrite, Epoch: 2,
		},
	}
}

// otherSecret is a secret that differs from s in its first digit, whatever
// that digit is — a fixed replacement would leave one secret in sixteen
// unchanged and the case asserting nothing.
func otherSecret(s string) string {
	if s[0] == '0' {
		return "1" + s[1:]
	}
	return "0" + s[1:]
}

// THE TABLE, ONE CELL PER FACT THAT DECIDES.
//
// Every refusal is a statement this node can make about a credential it has
// established is real, and every UNKNOWN is a node that cannot vouch — never
// a 401, because a pipeline that reads 401 throws its credential away. The
// control is the first row: a token nothing is wrong with is valid, or every
// other row would pass on a check that refused everything.
func TestAPresentedTokenIsDecidedByTheTable(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 1<<40|100)
	for _, tc := range []struct {
		name   string
		mutate func(*credential.TokenRow, *credential.Token)
		want   credential.TokenAnswer
	}{
		{"a token nothing is wrong with", nil, credential.TokenValid},
		{"a node past the stall grace", func(r *credential.TokenRow, _ *credential.Token) {
			r.Lag = 2 * stall
		}, credential.TokenUnknown},
		{"a node holding an undecodable record about the owner", func(r *credential.TokenRow, _ *credential.Token) {
			r.Deferred = true
		}, credential.TokenUnknown},
		{"no row, on a node below the mint", func(r *credential.TokenRow, tk *credential.Token) {
			*r = credential.TokenRow{Applied: tk.Position - 1}
		}, credential.TokenUnknown},
		{"no row, on a node that covers the mint", func(r *credential.TokenRow, tk *credential.Token) {
			*r = credential.TokenRow{Applied: tk.Position}
		}, credential.TokenRefused},
		{"an id naming a password", func(r *credential.TokenRow, _ *credential.Token) {
			r.IsToken = false
		}, credential.TokenRefused},
		{"a secret that does not verify", func(_ *credential.TokenRow, tk *credential.Token) {
			tk.Secret = otherSecret(tk.Secret)
		}, credential.TokenRefused},
		{"a revoked token", func(r *credential.TokenRow, _ *credential.Token) {
			r.RevokedAt = checkAt.Add(-time.Minute)
		}, credential.TokenRefused},
		// THE REVOKING NODE'S CLOCK RUNS AHEAD OF THIS ONE: its stamp names
		// an instant this node has not reached, and the record carrying it
		// has applied here all the same. Revoked is revoked.
		{"a token revoked by a node whose clock runs ahead", func(r *credential.TokenRow, _ *credential.Token) {
			r.RevokedAt = checkAt.Add(time.Hour)
		}, credential.TokenRefused},
		{"an expired token", func(r *credential.TokenRow, _ *credential.Token) {
			r.ExpiresAt = checkAt
		}, credential.TokenRefused},
		{"a token with no expiry", func(r *credential.TokenRow, _ *credential.Token) {
			r.ExpiresAt = time.Time{}
		}, credential.TokenRefused},
		// FOUND DECIDES, not the zero stage an absent owner happens to
		// carry: every other field is left as a live owner's, so a check
		// that leaned on the stage being empty would serve this row.
		{"an owner who is gone", func(r *credential.TokenRow, _ *credential.Token) {
			r.Owner.Found = false
		}, credential.TokenRefused},
		{"a suspended owner", func(r *credential.TokenRow, _ *credential.Token) {
			r.Owner.Stage = iam.StageSuspended
		}, credential.TokenRefused},
		{"an owner signed out everywhere since the mint", func(r *credential.TokenRow, _ *credential.Token) {
			r.Owner.Epoch = r.Epoch + 1
		}, credential.TokenRefused},
		{"every credential invalidated since the mint", func(r *credential.TokenRow, _ *credential.Token) {
			r.FleetGeneration = r.Generation + 1
		}, credential.TokenRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			presented, row := token, liveRow(token, verifier)
			if tc.mutate != nil {
				tc.mutate(&row, &presented)
			}
			got := credential.CheckToken(presented, row, checkAt, stall)
			if got.Answer != tc.want {
				t.Errorf("%s answered %q (%s), want %q", tc.name, got.Answer,
					got.Detail, tc.want)
			}
		})
	}
}

// A TOKEN NEVER CARRIES A GRANT THAT NEEDS A PERSON PRESENT, WHATEVER ITS ROW
// SAYS.
//
// The mint refuses `secrets:read` and `people:manage`, and this is the half the
// request path relies on: a token is fresh by construction in both step-up
// windows, which is safe only while no token reaches the gestures behind the
// sensitive one. A row carrying one — from a peer whose decide did not refuse
// it — whose owner holds it too is the case that matters, because the owner
// check alone would let it through. Mutation: drop the filter and the reveal
// grant is carried.
func TestATokenNeverCarriesAGrantThatNeedsAPersonPresent(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 5)
	row := liveRow(token, verifier)
	row.Grants = append([]iam.Grant{iam.GrantStateRead},
		iam.PersonPresentGrants...)
	row.Owner.Grants = iam.AllGrants
	if got := row.EffectiveGrants(); !slices.Equal(got,
		[]iam.Grant{iam.GrantStateRead}) {
		t.Errorf("a token carries %v, and %v need a person present",
			got, iam.PersonPresentGrants)
	}
}

// A TOKEN CARRIES WHAT ITS OWNER STILL HOLDS, AND REACHES NO FURTHER THAN
// THEY DO.
//
// Re-evaluated per request, so demoting a person demotes every token they
// made — the mint's own list is a ceiling on the token, never a grant of its
// own. Mutation: answer the mint's list whole and the withdrawn grant is
// still carried.
func TestATokenCarriesOnlyWhatItsOwnerStillHolds(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 5)
	row := liveRow(token, verifier)
	row.Owner.Grants = []iam.Grant{iam.GrantStateRead} // work:write withdrawn
	if got := row.EffectiveGrants(); !slices.Equal(got,
		[]iam.Grant{iam.GrantStateRead}) {
		t.Errorf("the token carries %v after its owner lost work:write", got)
	}
	row.Owner.Colleague = iam.ColleagueRead
	if got := row.EffectiveColleague(); got != iam.ColleagueRead {
		t.Errorf("the token reaches the company at %q past its owner's read",
			got)
	}
	row.Owner.Colleague, row.Colleague = iam.ColleagueWrite, iam.ColleagueNone
	if got := row.EffectiveColleague(); got != iam.ColleagueNone {
		t.Errorf("a token minted at none reaches the company at %q", got)
	}
	row.Colleague = ""
	if got := row.EffectiveColleague(); got != iam.ColleagueNone {
		t.Errorf("a token with no reach stated reaches the company at %q, "+
			"want the closed end", got)
	}
}
