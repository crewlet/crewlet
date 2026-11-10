package statelog_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A RESTART DOES NOT RE-RAISE A STANDING ALARM. A tracker that remembers keeps,
// in the node's own file, every instance whose raise was taken: the first
// evaluation after a restart hands on nothing for an instance still up, one
// clear with the original instant for one that cleared while the node was down,
// and a fire for one that is new, or whose raise the broker never took.
//
// Mutation: drop the recall, or keep untold raises, and a case is red.
func TestARestartDoesNotReRaiseAStandingAlarm(t *testing.T) {
	t.Parallel()
	backup := statelog.Alarm{Kind: statelog.KindBackupAge, Detail: "no backup for 3d"}
	trim := func(logs ...string) statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindTrimBlocked, Detail: "a trim is held"}
		for _, l := range logs {
			a.Domains = append(a.Domains, statelog.AlarmDomain{Domains: []string{l}, Detail: "held on " + l})
		}
		return a
	}
	t0 := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)

	// boot is one process of the node: a tracker over the node's file, whose
	// transitions the broker takes once each evaluation returns, unless lost.
	type boot struct {
		tracker *statelog.Tracker
		said    []statelog.AlarmTransition
		lost    bool
	}
	start := func(t *testing.T, db *store.DB, at time.Time, lost bool) *boot {
		b := &boot{tracker: statelog.NewTracker(nil, func() time.Time { return at }), lost: lost}
		b.tracker.Remember(db)
		b.tracker.OnTransition("node-a", func(tr statelog.AlarmTransition) { b.said = append(b.said, tr) })
		return b
	}
	observe := func(t *testing.T, b *boot, alarms []statelog.Alarm) {
		before := len(b.said)
		b.tracker.Observe(t.Context(), alarms)
		if !b.lost {
			for _, tr := range b.said[before:] {
				b.tracker.Told(t.Context(), tr.ID)
			}
		}
	}
	names := func(trs []statelog.AlarmTransition) []string {
		var out []string
		for _, tr := range trs {
			name := string(tr.Change) + " " + string(tr.Kind)
			if tr.Domain != "" {
				name += "/" + tr.Domain
			}
			out = append(out, name)
		}
		return out
	}
	nodeFile := func(t *testing.T) *store.DB {
		return storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	}

	t.Run("a standing alarm through twenty restarts", func(t *testing.T) {
		t.Parallel()
		db := nodeFile(t)
		first := start(t, db, t0, false)
		observe(t, first, []statelog.Alarm{backup})
		if got := names(first.said); !slices.Equal(got, []string{"fired backup_age"}) {
			t.Fatalf("first boot handed on %v, want one fire", got)
		}
		for i := 1; i <= 20; i++ {
			again := start(t, db, t0.Add(time.Duration(i)*time.Hour), false)
			observe(t, again, []statelog.Alarm{backup})
			if len(again.said) != 0 {
				t.Fatalf("restart %d handed on %v, want nothing: the alarm stood through it", i, names(again.said))
			}
		}
		last := start(t, db, t0.Add(21*time.Hour), false)
		observe(t, last, nil)
		if len(last.said) != 1 || last.said[0].Change != statelog.TransitionCleared ||
			!last.said[0].Since.Equal(t0) {
			t.Fatalf("the clear after the restarts = %+v, want one clear raised at %v", last.said, t0)
		}
	})

	t.Run("cleared while the node was down", func(t *testing.T) {
		t.Parallel()
		db := nodeFile(t)
		observe(t, start(t, db, t0, false), []statelog.Alarm{backup})
		again := start(t, db, t0.Add(time.Hour), false)
		observe(t, again, nil)
		if len(again.said) != 1 || again.said[0].Change != statelog.TransitionCleared ||
			!again.said[0].Since.Equal(t0) {
			t.Fatalf("handed on %v, want one clear with its original instant", names(again.said))
		}
	})

	t.Run("new while the node was down", func(t *testing.T) {
		t.Parallel()
		again := start(t, nodeFile(t), t0, false)
		observe(t, again, []statelog.Alarm{backup})
		if got := names(again.said); !slices.Equal(got, []string{"fired backup_age"}) {
			t.Fatalf("handed on %v, want one fire", got)
		}
	})

	t.Run("a second log joins while the node was down", func(t *testing.T) {
		t.Parallel()
		db := nodeFile(t)
		observe(t, start(t, db, t0, false), []statelog.Alarm{trim("tracker")})
		again := start(t, db, t0.Add(time.Hour), false)
		observe(t, again, []statelog.Alarm{trim("tracker", "pages")})
		if got := names(again.said); !slices.Equal(got, []string{"fired trim_blocked/pages"}) {
			t.Fatalf("handed on %v, want a fire for the new log alone", got)
		}
	})

	t.Run("a raise the broker never took", func(t *testing.T) {
		t.Parallel()
		db := nodeFile(t)
		observe(t, start(t, db, t0, true), []statelog.Alarm{backup})
		again := start(t, db, t0.Add(time.Hour), false)
		observe(t, again, []statelog.Alarm{backup})
		if got := names(again.said); !slices.Equal(got, []string{"fired backup_age"}) {
			t.Fatalf("handed on %v, want the lost raise raised again", got)
		}
	})
}
