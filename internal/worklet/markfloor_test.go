package worklet_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A MARK WAITS FOR THE POSITION ITS ASKER READ TO, rather than refusing a
// notice the person was shown as one the company never recorded.
//
// A mark is unkeyed, so the estate router asks whichever data node it reaches,
// and the inbox page that showed the notice may have been read from a peer
// ahead of that node. Node b here is that node: on the same log as node a,
// which wrote and applied the change that reached ana, and not yet at it. With
// no floor its decide finds no history row for the notice and refuses the mark
// `invalid` — "change the arguments", which is false: the same call lands once
// b catches up. With the notice's own position as the floor, a b that cannot
// reach it answers `behind`, a condition, and appends nothing; once its
// applier runs the same mark lands.
func TestAMarkWaitsForTheAskersFloor(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node-b.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = dbNode.Close() })
	b := newRoundTripOn(t, a.broker, a.log, dbNode, db, "node-b")
	b.drain()

	notice := noticesFor(t, a, "ana", 1)[0]
	seen := *positionOf(notice)
	read := inbox.Gesture[worklet.Reason]{Read: []string{notice.RecordID}}
	end := a.logEnd(t)

	// BLIND, node b refuses what it has not reached as none of the
	// company's — the refusal this floor exists to retire.
	_, err := b.writer.MarkInbox(t.Context(), "op-blind", "ana", statelog.Position{}, read,
		worklet.PersonAuthority{})
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), notice.RecordID) {
		t.Fatalf("a mark with no floor on a node behind the notice answered %v, want it refused "+
			"invalid naming the notice — the state this test is about", err)
	}

	// HELD TO THE FLOOR, a node that cannot reach it says it is behind,
	// which the same call clears, and writes nothing.
	_, err = b.writer.MarkInbox(t.Context(), "op-held", "ana", seen, read, worklet.PersonAuthority{})
	var refusal *statelog.Unavailable
	switch {
	case err == nil:
		t.Fatal("a mark held to a floor its node had not reached was decided anyway")
	case errors.Is(err, worklet.ErrInvalid):
		t.Fatalf("a mark held to its asker's floor was refused invalid: %v — the floor never "+
			"reached the decide", err)
	case !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBehind:
		t.Fatalf("a mark held to a floor its node had not reached answered %v, want %q", err,
			statelog.ReasonBehind)
	}
	if got := a.logEnd(t); got != end {
		t.Fatalf("the held mark put %d record(s) on the log", got-end)
	}

	// AND ONCE ITS APPLIER RUNS, the same mark lands on node b.
	b.applyWhileWriting()
	if _, err := b.writer.MarkInbox(t.Context(), "op-held", "ana", seen, read,
		worklet.PersonAuthority{}); err != nil {
		t.Fatalf("the floored mark on a node that reached its floor: %v", err)
	}
	b.drain()
	if got := b.person("ana"); len(got.Read) != 1 || got.Read[0].RecordID != notice.RecordID {
		t.Fatalf("ana's read list on node b is %v, want the notice she marked", got.Read)
	}

	// A FLOOR ON ANOTHER LOG is refused before anything is asked: it says
	// nothing of how far this one was read.
	foreign := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: seen.Seq}
	if _, err := b.writer.MarkInbox(t.Context(), "op-foreign", "ana", foreign, read,
		worklet.PersonAuthority{}); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a mark held to a position on the code log answered %v, want it refused invalid", err)
	}
}
