package statelog_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A LOST ACKNOWLEDGEMENT THE LEDGER RESOLVES IS COLLAPSED: the row it names may
// be an earlier copy's, whose decision this call never took.
//
// A retry on a node that is behind decides on its stale rows while an earlier
// copy of the same operation — decided elsewhere, against other rows — lands on
// the subject first. The retry's append is refused, its answer is lost, the
// probe finds the earlier copy above the retry's anchor, and this node's
// ledger records the operation there. The publisher answered that as the
// application of THIS call's decision; a key mint handed back the number its
// stale counter implied, and two tasks were filed under one key.
func TestAnAmbiguousPublishTheLedgerResolvesIsCollapsed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := probeSubject("counter")
	prior, err := h.write(subject, statelog.NewOpID(time.Now(), "prior"), "one")
	if err != nil || prior.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the prior write = (%+v, %v)", prior, err)
	}
	if prior.Collapsed {
		t.Fatal("an acknowledged append whose own position the ledger names " +
			"reported itself collapsed — that one IS this call's record")
	}

	op := statelog.NewOpID(time.Now(), "retry")
	// THE EARLIER COPY WINS THE RACE, after this call's snapshot decided and
	// before its append arrives; this node then applies it.
	var once sync.Once
	h.rows.mu.Lock()
	h.rows.afterSnapshot = func() {
		once.Do(func() {
			seq, _, err := h.log.Append(t.Context(), probePrefix+".object.counter",
				op, nil, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"},
					op, "the earlier copy's"))
			if err != nil {
				t.Errorf("land the earlier copy: %v", err)
				return
			}
			h.applier.mu.Lock()
			at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
			h.applier.ops[op] = statelog.OpEntry{
				Position: at, Subject: probePrefix + ".object.counter"}
			h.applier.committed = at
			h.applier.mu.Unlock()
		})
	}
	h.rows.mu.Unlock()
	// ...AND THIS CALL'S APPEND NEVER LANDS, its answer lost.
	h.appends.fail(errors.New("nats: timeout"), true)

	res, err := h.write(subject, op, "this call's")
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the retry answered %q, want applied at the earlier copy", res.Outcome)
	}
	if !res.Collapsed {
		t.Error("the retry was answered from the ledger at a copy it cannot " +
			"prove is its own and does not say so — a caller computing its " +
			"answer inside the decision builds on one nothing published")
	}
}

// AN ACKNOWLEDGED APPEND WHOSE LEDGER ROW NAMES ANOTHER POSITION IS COLLAPSED
// too: the broker stored this call's record, but the operation applied at an
// earlier copy, and this node's applier skipped the second.
//
// Past the duplicate window the broker has no reason to refuse a second copy
// of an additive write, so it acknowledges it cleanly — and the resolution
// that knew the row was another copy's had its answer overwritten by the
// duplicate flag, which was false.
func TestAnAcknowledgedCopyTheLedgerPlacesElsewhereIsCollapsed(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, shortWindowDomain{})
	h.applier.mu.Lock()
	h.applier.auto = false
	h.applier.mu.Unlock()
	subject := probeSubject("note")
	op := statelog.NewOpID(time.Now(), "note")

	var once sync.Once
	h.rows.mu.Lock()
	h.rows.afterSnapshot = func() {
		once.Do(func() {
			seq, _, err := h.log.Append(t.Context(), probePrefix+".object.note", op,
				nil, probeRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"},
					op, "the earlier copy's"))
			if err != nil {
				t.Errorf("land the earlier copy: %v", err)
				return
			}
			// THIS NODE APPLIES THE EARLIER COPY, and will be past
			// whatever this call appends next.
			h.applier.mu.Lock()
			h.applier.ops[op] = statelog.OpEntry{
				Position: statelog.Position{
					Stream: probeStream, Generation: h.gen.Load(), Seq: seq},
				Subject: probePrefix + ".object.note",
			}
			h.applier.committed = statelog.Position{
				Stream: probeStream, Generation: h.gen.Load(), Seq: seq + 100}
			h.applier.mu.Unlock()
			// PAST THE BROKER'S DUPLICATE WINDOW, so this call's append
			// is acknowledged as a record of its own.
			time.Sleep(2 * shortWindow)
		})
	}
	h.rows.mu.Unlock()

	res, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: subject,
		Scope:   statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID:    op,
		Pattern: statelog.PatternAdditive,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: probeRecord(stamp, op, "this call's"), Version: 1}, nil
		},
	})
	if err != nil {
		t.Fatalf("the write: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the write answered %q, want applied at the earlier copy", res.Outcome)
	}
	if !res.Collapsed {
		t.Errorf("the write was acknowledged at a position the ledger does not "+
			"name (%s) and does not say it collapsed onto another copy", res.Position)
	}
}
