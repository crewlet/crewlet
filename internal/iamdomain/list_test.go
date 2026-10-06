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
// reached. Where the person's epoch moved, the move that ended it is the one
// the epoch's row keeps, so it is listed with THAT record's reason and instant
// — a suspension, a reset link, an administrator ending them all and a
// password change otherwise all read "ended with every session they held",
// with no end.
//
// Mutation: drop Superseded from SessionRecord.Live, or from
// CredentialRow.Revoked, and the session or the token minted before the
// counter moved is listed live; drop the reason and it is listed with none;
// list the generic reason for an epoch move and the first case goes red.
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
		}, "signed out everywhere"},
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
			endedAt := map[string]time.Time{}
			for _, s := range sessions {
				listed[s.Lineage] = s.Live(wall)
				why[s.Lineage] = s.EndedWhy
				endedAt[s.Lineage] = s.EndedAt
			}
			if why[sessionBefore] != tc.why || why[sessionAfter] != "" {
				t.Errorf("the sessions are listed ended because %q and %q, "+
					"want %q and none", why[sessionBefore], why[sessionAfter], tc.why)
			}
			// AN EPOCH MOVE SAYS WHEN; the company's generation is listed
			// with no end, since an earlier invalidation may have been the
			// one that ended it.
			if epoch := tc.why == "signed out everywhere"; endedAt[sessionBefore].IsZero() == epoch ||
				!endedAt[sessionAfter].IsZero() {
				t.Errorf("the sessions are listed ended at %v and %v", endedAt[sessionBefore],
					endedAt[sessionAfter])
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

// ONLY THE LAST MOVE IS KEPT, so only a session it ended is listed with it.
//
// A session opened before an EARLIER move of the epoch was ended by that one,
// whose reason the epoch's row no longer holds: it is listed with the generic
// reason and no end rather than with a later move's, which would name a cause
// that came after it was already over. The CONTROL is the session the last
// move ended. Mutation: list the last move for every superseded session and the
// first one names it.
func TestOnlyTheEpochsLastMoveIsListedAsWhatEndedASession(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	expires := time.Now().UTC().Add(24 * time.Hour)
	revoke := func(op, why string) {
		t.Helper()
		if err := rig.draining(func() error {
			_, err := rig.writer.Revoke(rig.t.Context(), owner, op, why)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		rig.drain()
	}
	first := rig.openSession(owner, expires)
	revoke("op-first", "changed their own password")
	second := rig.openSession(owner, expires)
	revoke("op-second", "signed out everywhere")
	sessions, err := rig.reader(t).Sessions(t.Context(), owner)
	if err != nil {
		t.Fatalf("list the sessions: %v", err)
	}
	for _, s := range sessions {
		switch s.Lineage {
		case first:
			if s.EndedWhy != "ended with every session they held" || !s.EndedAt.IsZero() {
				t.Errorf("the session an earlier move ended is listed %q at %v",
					s.EndedWhy, s.EndedAt)
			}
		case second:
			if s.EndedWhy != "signed out everywhere" || s.EndedAt.IsZero() {
				t.Errorf("the session the last move ended is listed %q at %v",
					s.EndedWhy, s.EndedAt)
			}
		}
	}
}
