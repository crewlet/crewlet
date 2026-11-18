package iamdomain_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A COUNTER THAT ENDS A SESSION OR A TOKEN ENDS IT IN THE LISTING TOO.
//
// A revocation epoch move and the company's session generation write no
// `ended_at` or `revoked_at`, so a listing reading rows alone shows sessions
// and tokens live that the engine refuses. The named sign-out reads the same
// predicate, so the case holds the two against each other; what was opened
// after the counter moved is live in both. AND IT SAYS WHY: an epoch move's
// reason and instant, or the generation's own wording with no end.
//
// Mutation: drop Superseded from SessionRecord.Live or CredentialRow.Revoked,
// the reason, or the epoch move's write on the sessions it ends, and this goes
// red.
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

// EACH SESSION KEEPS WHAT ENDED IT, however many moves come after.
//
// The epoch's row keeps only its LAST move, so each move writes its reason and
// instant on the sessions it ends: the first session reads the first move and
// the second the second, at two instants. A session the company's generation
// ended before the epoch moved is listed as the generation's.
//
// Mutation: derive the reason from the epoch's row and the first session names
// the last move or none; drop the generation's exclusion from the epoch's
// write and the third names the epoch's move.
func TestEachSessionKeepsTheMoveThatEndedIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	owner := tokenOwner(t, rig, "jane.doe")
	expires := time.Now().UTC().Add(24 * time.Hour)
	move := func(write func() error) {
		t.Helper()
		if err := rig.draining(write); err != nil {
			t.Fatal(err)
		}
		rig.drain()
	}
	revoke := func(op, why string) func() error {
		return func() error {
			_, err := rig.writer.Revoke(rig.t.Context(), owner, op, why)
			return err
		}
	}
	first := rig.openSession(owner, expires)
	move(revoke("op-first", "changed their own password"))
	second := rig.openSession(owner, expires)
	move(revoke("op-second", "signed out everywhere"))
	third := rig.openSession(owner, expires)
	move(func() error {
		_, err := rig.writer.InvalidateAll(rig.t.Context(), "op-invalidate",
			"restored from a backup")
		return err
	})
	move(revoke("op-third", "suspended by jane"))
	sessions, err := rig.reader(t).Sessions(t.Context(), owner)
	if err != nil {
		t.Fatalf("list the sessions: %v", err)
	}
	why := map[string]string{}
	at := map[string]time.Time{}
	for _, s := range sessions {
		why[s.Lineage], at[s.Lineage] = s.EndedWhy, s.EndedAt
	}
	for lineage, want := range map[string]string{
		first:  "changed their own password",
		second: "signed out everywhere",
		third:  "ended with every session in the company",
	} {
		if why[lineage] != want {
			t.Errorf("the session %s is listed ended because %q, want %q",
				lineage, why[lineage], want)
		}
	}
	if at[first].IsZero() || at[second].IsZero() || !at[first].Before(at[second]) {
		t.Errorf("the sessions two moves ended are listed ended at %v and %v, "+
			"want each move's own instant", at[first], at[second])
	}
}

// A MOVE NEVER RE-DESCRIBES A SESSION AN EARLIER MOVE ENDED.
//
// `ended_at = 0` does not mean live: a session an earlier move ended without
// writing it on the row holds it too (a start decided before the move whose
// record landed after it, or a row predating moves writing reasons). A later
// move restamping it gives a permanent, false account. The case clears the
// first move's reason off the row to build that state; the control is a
// session opened after it, ended by the second move with its reason.
//
// Mutation: end every session below the new epoch rather than those at the one
// it leaves, and the stale session reads the second move.
func TestAMoveLeavesASessionAnEarlierMoveEnded(t *testing.T) {
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
	stale := rig.openSession(owner, expires)
	revoke("op-first", "changed their own password")
	if err := rig.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE iam_sessions
			SET ended_at = 0, ended_reason = '' WHERE lineage = ?`, stale)
		return err
	}); err != nil {
		t.Fatalf("take the first move's reason off its row: %v", err)
	}
	live := rig.openSession(owner, expires)
	revoke("op-second", "suspended by jane")

	sessions, err := rig.reader(t).Sessions(t.Context(), owner)
	if err != nil {
		t.Fatalf("list the sessions: %v", err)
	}
	listed := map[string]iamdomain.SessionRecord{}
	for _, s := range sessions {
		listed[s.Lineage] = s
	}
	if got := listed[stale]; got.EndedWhy != "ended with every session they held" ||
		!got.EndedAt.IsZero() {
		t.Errorf("the session the first move ended is listed ended at %v "+
			"because %q, want no instant and %q", got.EndedAt, got.EndedWhy,
			"ended with every session they held")
	}
	if got := listed[live]; got.EndedWhy != "suspended by jane" || got.EndedAt.IsZero() {
		t.Errorf("the session the second move ended is listed ended at %v "+
			"because %q, want that move's instant and reason",
			got.EndedAt, got.EndedWhy)
	}
}
