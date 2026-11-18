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
// A mark is unkeyed, so it may reach a node (b here) behind the peer the inbox
// page was read from. With no floor b refuses it `invalid`, which is false: the
// same call lands once b catches up. Floored at the notice's position, b
// answers `behind` and appends nothing until its applier runs.
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

	// BLIND, node b refuses what it has not reached: the refusal the floor retires.
	_, err := b.writer.MarkInbox(t.Context(), "op-blind", "ana", statelog.Position{}, read,
		worklet.PersonAuthority{})
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), notice.RecordID) {
		t.Fatalf("a mark with no floor on a node behind the notice answered %v, want it refused "+
			"invalid naming the notice — the state this test is about", err)
	}

	// HELD TO THE FLOOR, a node that cannot reach it answers behind and writes nothing.
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

	// A FLOOR ON ANOTHER LOG is refused: it says nothing of how far this one was read.
	foreign := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: seen.Seq}
	if _, err := b.writer.MarkInbox(t.Context(), "op-foreign", "ana", foreign, read,
		worklet.PersonAuthority{}); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a mark held to a position on the code log answered %v, want it refused invalid", err)
	}
}
