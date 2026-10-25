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
		Grants:     []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		Epoch:      2,
		Generation: 1, FleetGeneration: 1,
		Owner: credential.TokenOwner{
			Found: true, ID: "0192f00d-0000-7000-8000-00000000000a",
			Kind: iam.KindPerson, Stage: iam.StageActive, Login: "jane.doe",
			Grants: []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
			Epoch:  2,
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

// A TOKEN CARRIES WHAT ITS MINT AND ITS OWNER SHARE, READ THROUGH EXPAND.
//
// Re-evaluated per request, so demoting a person demotes every token they
// made — the mint's own list is a ceiling on the token, never a grant of its
// own — and both sides are expanded, so admin is just another permission
// here: a token minted [admin] carries everything while its owner holds admin
// and the one app its owner keeps once they do not, and a token minted an app
// its administrator owner never listed carries it. A name this build does not
// know grants nothing. Mutation: answer the mint's list whole and the
// withdrawn admin is still carried; drop the owner's Expand and the app an
// administrator holds through admin is not.
func TestATokenCarriesWhatItsMintAndItsOwnerShare(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 5)
	for _, c := range []struct {
		name   string
		minted []iam.Grant
		owner  []iam.Grant
		want   []iam.Grant
	}{
		{"minted admin, owner an administrator", []iam.Grant{iam.GrantAdmin},
			[]iam.Grant{iam.GrantAdmin}, iam.AllGrants},
		{"minted admin, owner keeps worklet", []iam.Grant{iam.GrantAdmin},
			[]iam.Grant{iam.GrantWorklet}, []iam.Grant{iam.GrantWorklet}},
		{"minted an app, owner holds it through admin",
			[]iam.Grant{iam.GrantDoclet}, []iam.Grant{iam.GrantAdmin},
			[]iam.Grant{iam.GrantDoclet}},
		{"minted an unknown name", []iam.Grant{"swarm"},
			[]iam.Grant{iam.GrantAdmin, "swarm"}, []iam.Grant{}},
		{"owner lost the app", []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
			[]iam.Grant{iam.GrantDoclet}, []iam.Grant{iam.GrantDoclet}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row := liveRow(token, verifier)
			row.Grants, row.Owner.Grants = c.minted, c.owner
			if got := row.EffectiveGrants(); !slices.Equal(got, c.want) {
				t.Errorf("minted %v, owner %v: carries %v, want %v", c.minted,
					c.owner, got, c.want)
			}
		})
	}
}

// A TOKEN HELD BY NAME IS DECIDED BY THE SAME TABLE, BUT THE TWO FACTS ONLY A
// BEARER HAS.
//
// A view ticket minted through a machine token names the token and never
// carries its secret, so the artifact origin decides it by its rows: every
// fact that ends a good token ends it there too, and an absent row is a node
// that cannot tell (it has no mint position to cover) rather than a refusal.
// Mutation: answer an absent row refused, and a frame whose token a lagging
// node has not applied yet is ended; skip the ending facts, and a revoked
// token keeps opening frames.
func TestATokenHeldByNameIsDecidedByItsRows(t *testing.T) {
	t.Parallel()
	token, verifier := mintToken(t, 1<<40|100)
	for _, tc := range []struct {
		name   string
		mutate func(*credential.TokenRow)
		want   credential.TokenAnswer
	}{
		{"a token nothing is wrong with", nil, credential.TokenValid},
		{"a node past the stall grace", func(r *credential.TokenRow) { r.Lag = 2 * stall },
			credential.TokenUnknown},
		{"a node holding an undecodable record about the owner",
			func(r *credential.TokenRow) { r.Deferred = true }, credential.TokenUnknown},
		{"no row at all", func(r *credential.TokenRow) {
			*r = credential.TokenRow{Applied: r.Applied}
		}, credential.TokenUnknown},
		{"an id naming a password", func(r *credential.TokenRow) { r.IsToken = false },
			credential.TokenRefused},
		{"a revoked token", func(r *credential.TokenRow) { r.RevokedAt = checkAt },
			credential.TokenRefused},
		{"an expired token", func(r *credential.TokenRow) { r.ExpiresAt = checkAt },
			credential.TokenRefused},
		{"an owner who is gone", func(r *credential.TokenRow) { r.Owner.Found = false },
			credential.TokenRefused},
		{"a suspended owner", func(r *credential.TokenRow) { r.Owner.Stage = iam.StageSuspended },
			credential.TokenRefused},
		{"an owner signed out everywhere since the mint",
			func(r *credential.TokenRow) { r.Owner.Epoch = r.Epoch + 1 }, credential.TokenRefused},
		{"every credential invalidated since the mint",
			func(r *credential.TokenRow) { r.FleetGeneration = r.Generation + 1 },
			credential.TokenRefused},
		// THE SECRET IS NOT ASKED: a row whose verifier names some other
		// secret entirely is still the token the ticket was minted through.
		{"a verifier no presented secret would match",
			func(r *credential.TokenRow) { r.Verifier = otherSecret(r.Verifier) },
			credential.TokenValid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := liveRow(token, verifier)
			if tc.mutate != nil {
				tc.mutate(&row)
			}
			if got := credential.CheckHeld(row, checkAt, stall); got.Answer != tc.want {
				t.Errorf("%s answered %q (%s), want %q", tc.name, got.Answer, got.Detail, tc.want)
			}
		})
	}
}
