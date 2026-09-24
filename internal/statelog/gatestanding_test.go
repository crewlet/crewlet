package statelog_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A RETRIED GATE OPERATION THE LEDGER HOLDS STANDS ONLY WHILE ITS RECORD IS
// STILL THE GATE IN FORCE — and every other finding is a refusal or an error,
// never "applied".
//
// A ledger hit says the operation's record landed; it does not say the record
// is what the node's standing on the log still rests on. An eviction retried
// after a readmission was answered "applied" at its old position while every
// row counted the node. Each case is the node's own row against where the
// operation landed, over values.
func TestAGateRetryStandsOnlyWhileItsRecordIsInForce(t *testing.T) {
	t.Parallel()
	const node = "node-away"
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: probeStream, Generation: 1, Seq: seq}
	}
	packed := func(seq uint64) uint64 { return uint64(at(seq).Packed()) }
	evicted := func(from uint64) statelog.EvictionRow {
		return statelog.EvictionRow{NodeID: node, From: packed(from)}
	}
	back := func(from, readmitted uint64) statelog.EvictionRow {
		return statelog.EvictionRow{NodeID: node, From: packed(from),
			Readmitted: packed(readmitted), Back: readmitted > from}
	}

	for _, tc := range []struct {
		name    string
		readmit bool
		landed  uint64
		row     statelog.EvictionRow
		found   bool

		wantReason statelog.Reason
		wantErr    string
	}{
		{name: "an eviction whose record is the one in force stands",
			landed: 7, row: evicted(7), found: true},
		{name: "an eviction a readmission has undone since is superseded",
			landed: 7, row: back(7, 9), found: true,
			wantReason: statelog.ReasonSuperseded},
		{name: "an eviction a later eviction replaced is superseded",
			landed: 7, row: evicted(12), found: true,
			wantReason: statelog.ReasonSuperseded},
		{name: "an eviction whose own row is missing is not judged at all",
			landed: 7, wantErr: "no eviction row"},
		{name: "an eviction row older than the record is not judged at all",
			landed: 7, row: evicted(3), found: true, wantErr: "earlier composed position"},
		{name: "a readmission whose record is the one in force stands",
			readmit: true, landed: 9, row: back(7, 9), found: true},
		{name: "a readmission of a node this log never evicted stands",
			readmit: true, landed: 9},
		{name: "a readmission a later eviction undid is superseded",
			readmit: true, landed: 9, row: evicted(14), found: true,
			wantReason: statelog.ReasonSuperseded},
		{name: "a readmission a later readmission replaced is superseded",
			readmit: true, landed: 9, row: back(12, 15), found: true,
			wantReason: statelog.ReasonSuperseded},
		{name: "a readmission repeated under a later gesture is superseded",
			readmit: true, landed: 9, row: back(7, 15), found: true,
			wantReason: statelog.ReasonSuperseded},
		{name: "a readmission the row never recorded is not judged at all",
			readmit: true, landed: 9, row: evicted(7), found: true,
			wantErr: "readmitted at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := statelog.GateStanding("op-1.evict.probe:node-away", node,
				tc.readmit, at(tc.landed), tc.row, tc.found)
			var refusal *statelog.Unavailable
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) ||
					errors.As(err, &refusal) {
					t.Fatalf("GateStanding answered %v, want an error naming %q — "+
						"an estate whose row and ledger disagree is not one to "+
						"judge a retry against", err, tc.wantErr)
				}
			case tc.wantReason != "":
				if !errors.As(err, &refusal) || refusal.Reason != tc.wantReason {
					t.Fatalf("GateStanding answered %v, want a %s refusal",
						err, tc.wantReason)
				}
				if refusal.Position != at(tc.landed) {
					t.Errorf("the refusal names %s, want where the operation "+
						"landed, %s", refusal.Position, at(tc.landed))
				}
			case err != nil:
				t.Fatalf("GateStanding: %v", err)
			}
		})
	}
}

// A HELD OPERATION IS JUDGED INSIDE THE SNAPSHOT'S OWN TRANSACTION, AND A
// JUDGEMENT OR A LEDGER THAT CANNOT BE READ IS AN ERROR — never "never landed",
// and never a decision.
//
// A gate's standing is read from the node's row, and it has to be the row the
// same transaction's ledger read saw: judged in another, a readmission landing
// between the two reads is judged against a ledger from before it. And the
// lookup this replaced read `err == nil && done`, so an unreadable ledger was
// a ledger that said no and the gate record went out again.
func TestAHeldOperationIsJudgedInsideTheSnapshot(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	const opID = "op-2.evict.probe:node-away"
	subject := statelog.Subject{Kind: "eviction", ID: "node-away"}
	ledger(t, h.db, opID, probePrefix+"."+subject.String(),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 4})
	unread := errors.New("the node's row could not be read")

	for name, tc := range map[string]struct {
		domain statelog.Domain
		judge  func(*sql.Tx, statelog.OpEntry) error
		held   bool
	}{
		"a held operation is handed to its judge with the transaction": {
			domain: probeDomain{}, held: true,
			judge: func(tx *sql.Tx, entry statelog.OpEntry) error {
				if tx == nil || entry.Position.Seq != 4 {
					return errors.New("judged outside the snapshot")
				}
				return nil
			}},
		"a judge that cannot read the node's row": {
			domain: probeDomain{},
			judge:  func(*sql.Tx, statelog.OpEntry) error { return unread }},
		"a ledger that cannot be read": {
			domain: missingLedger{},
			judge:  func(*sql.Tx, statelog.OpEntry) error { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := statelog.NewRows(h.db, tc.domain, nil)
			if err != nil {
				t.Fatalf("NewRows: %v", err)
			}
			decided := false
			snap, err := rows.Snapshot(t.Context(), subject,
				statelog.ScopeSet{Paths: []string{subject.String()}}, opID, tc.judge,
				func(*sql.Tx, statelog.Position) (statelog.Decision, error) {
					decided = true
					return statelog.Decision{}, nil
				})
			switch {
			case tc.held:
				if err != nil || !snap.HeldOK || snap.Held.Seq != 4 || decided {
					t.Fatalf("the snapshot answered (%+v, %v, decided=%v), want the "+
						"held operation and no decision", snap, err, decided)
				}
			case err == nil || snap.HeldOK || decided:
				t.Fatalf("an unreadable judgement answered (%+v, %v, decided=%v) — "+
					"read as \"never landed\", the gate record goes out a second "+
					"time", snap, err, decided)
			}
		})
	}
}

// missingLedger is the probe domain naming an operation ledger its estate does
// not hold, so every read of it fails.
type missingLedger struct{ probeDomain }

func (missingLedger) OpsTable() string { return "probe_ops_absent" }

// ledger writes one operation row, as this node's applier would have.
func ledger(t *testing.T, db *store.DB, opID, subject string, at statelog.Position) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO probe_ops (op_id, subject, position, applied_at)
			VALUES (?, ?, ?, ?)`, opID, subject, at.Packed(),
			store.EncodeTime(time.Unix(1_700_000_000, 0).UTC()))
		return err
	}); err != nil {
		t.Fatalf("write the operation row: %v", err)
	}
}
