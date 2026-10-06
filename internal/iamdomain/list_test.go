package iamdomain_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A COUNTER THAT ENDS A SESSION OR A TOKEN ENDS IT IN THE LISTING TOO.
//
// A password change, "sign out everywhere" and an administrator ending
// somebody's sessions move the person's revocation epoch, and a restore moves
// the company's session generation: neither writes `ended_at` on a session or
// `revoked_at` on a token. A listing that read the rows alone showed every one
// of them still live — the Account page went on offering to sign out sessions
// the engine answered were already over, and counted tokens it refused as in
// use. The named sign-out reads the same predicate, so the case holds the two
// against each other. The controls: what was opened after the counter moved
// is live in both.
//
// AND IT SAYS WHY, since no record named the session and the row holds no
// reason: an ended session with none was listed beside the deadline it never
// reached.
//
// Mutation: drop Superseded from SessionRecord.Live, or from
// CredentialRow.Revoked, and the session or the token minted before the
// counter moved is listed live; drop the reason and it is listed with none.
func TestACounterThatEndsASessionOrATokenEndsItInTheListingToo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		move func(rig *writeRig, owner string) error
		why  string
	}{
		{"the person's revocation epoch", func(rig *writeRig, owner string) error {
			_, err := rig.writer.Revoke(rig.t.Context(), owner, "op-revoke",
				"signed out everywhere")
			return err
		}, "ended with every session they held"},
		{"the company's session generation", func(rig *writeRig, _ string) error {
			_, err := rig.writer.InvalidateAll(rig.t.Context(), "op-invalidate",
				"restored from a backup")
			return err
		}, "ended with every session in the company"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			owner := tokenOwner(t, rig, "jane.doe")
			wall := time.Now().UTC()
			expires := wall.Add(24 * time.Hour)

			sessionBefore := rig.openSession(owner, expires)
			_, tokenBefore, err := mintFor(t, rig, asOwner(rig, owner),
				iamdomain.TokenMint{PersonID: owner})
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.draining(func() error { return tc.move(rig, owner) }); err != nil {
				t.Fatal(err)
			}
			rig.drain()
			sessionAfter := rig.openSession(owner, expires)
			_, tokenAfter, err := mintFor(t, rig, asOwner(rig, owner),
				iamdomain.TokenMint{PersonID: owner})
			if err != nil {
				t.Fatal(err)
			}

			reader := rig.reader(t)
			sessions, err := reader.Sessions(t.Context(), owner)
			if err != nil {
				t.Fatalf("list the sessions: %v", err)
			}
			listed := map[string]bool{}
			why := map[string]string{}
			for _, s := range sessions {
				listed[s.Lineage] = s.Live(wall)
				why[s.Lineage] = s.EndedWhy
			}
			if why[sessionBefore] != tc.why || why[sessionAfter] != "" {
				t.Errorf("the sessions are listed ended because %q and %q, "+
					"want %q and none", why[sessionBefore], why[sessionAfter], tc.why)
			}
			end, err := rig.end(t.Context())
			if err != nil {
				t.Fatalf("read the log's end: %v", err)
			}
			for lineage, want := range map[string]bool{
				sessionBefore: false, sessionAfter: true,
			} {
				live, ok := listed[lineage]
				if !ok || live != want {
					t.Errorf("the session %s is listed live=%v (listed: %v), "+
						"want %v", lineage, live, ok, want)
				}
				_, standing, err := reader.SessionStanding(t.Context(), lineage,
					wall, end)
				if err != nil || standing != want {
					t.Errorf("a named sign-out of the session %s reads it "+
						"live=%v (%v), want %v — the listing and the sign-out "+
						"must agree", lineage, standing, err, want)
				}
			}

			creds, err := reader.Credentials(t.Context(), owner)
			if err != nil {
				t.Fatalf("list the credentials: %v", err)
			}
			revoked := map[string]bool{}
			for _, c := range creds {
				revoked[c.ID] = c.Revoked(brokerAt)
			}
			if !revoked[tokenBefore.ID] {
				t.Errorf("a token minted before the counter moved is listed " +
					"in use, while the guard refuses it")
			}
			if got, ok := revoked[tokenAfter.ID]; !ok || got {
				t.Errorf("a token minted after the counter moved is listed "+
					"revoked=%v (listed: %v), want in use", got, ok)
			}
		})
	}
}
