package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// dispatchesDump is every row of code_check_targets and code_check_dispatches,
// in key order, as text: two estates hold one state of the home's dispatches
// iff their dumps are equal.
func (e *estate) dispatchesDump() string {
	e.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT * FROM code_check_targets ORDER BY repo_id, target_ref`,
		`SELECT * FROM code_check_dispatches ORDER BY repo_id, commit_oid, target_ref`,
	} {
		if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(context.Background(), q)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			for rows.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				fmt.Fprintf(&b, "%v\n", values)
			}
			return rows.Err()
		}); err != nil {
			e.t.Fatalf("dump the dispatches: %v", err)
		}
	}
	return b.String()
}

// TWO READINGS OF ONE TARGET CONVERGE IN EITHER ORDER: a home's dispatch and a
// failover's second home's land in either order — the subject carries no
// expectation — and each row keeps the reading of the LATER TIP, then the
// later record, so the two orders leave the target's reading and the head's
// dispatch byte for byte, both naming the checks file the later tip held.
//
// Mutation: keep whichever record a row holds first (first in log order
// wins) — the two orders keep two readings, red.
func TestTwoDispatchesOfOneTargetConvergeInEitherOrder(t *testing.T) {
	t.Parallel()
	tipAt := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: seq}
	}
	head := DispatchedHead{OID: oid("head"), ChangeID: "0190f3c4-0000-7000-8000-00000000c0de", Revision: 1}
	reading := func(tip string, seq uint64, defs string) built {
		return checkRec(t, OpCheckDispatched, CheckDispatched{RepoID: checkRepo, Target: "refs/heads/main",
			Tip: oid(tip), TipPosition: tipAt(seq), DefsOID: oid(defs), Heads: []DispatchedHead{head},
			DispatchedAt: applyClock})
	}
	// THE EARLIER TIP'S READING LANDS LATER in the log in one order: a
	// home that read main before a push moved it, recording after the home
	// that read it after.
	records := map[uint64]built{20: reading("tip-2", 3, "defs-2"), 21: reading("tip-1", 2, "defs-1")}
	dumps := map[string]string{}
	for name, order := range map[string][]uint64{"in log order": {20, 21}, "reversed": {21, 20}} {
		e := openChecksEstate(t)
		e.applyAt(records, order...)
		dumps[name] = e.dispatchesDump()
	}
	if dumps["in log order"] != dumps["reversed"] {
		t.Fatalf("two readings of main joined to two states:\n%s\nand\n%s", dumps["in log order"],
			dumps["reversed"])
	}
	if got := dumps["in log order"]; !strings.Contains(got, oid("defs-2")) || strings.Contains(got, oid("defs-1")) {
		t.Fatalf("the rows keep\n%s\nwant the later tip's checks file, %s, on the target and the head", got,
			oid("defs-2"))
	}
}

// THE WIDEST HEAD'S DISPATCH FITS ONE RECORD, so the home never splits a head
// between records: a checks file of MaxChecksPerFile checks of the longest
// name, filling its MaxChecksFileBytes with text JSON writes six bytes a byte
// (`<`, as <), queued at one head of a long target, as the attempts of
// one dispatch record naming that head, encodes within MaxDispatchBytes and
// is a dispatch Validate passes.
//
// Mutation: halve MaxDispatchBytes — the widest head's dispatch no longer
// fits one record, red.
func TestTheWidestHeadsDispatchFitsOneRecord(t *testing.T) {
	t.Parallel()
	const perCheck = 64 // a check's YAML beside its run and name: its keys, indentation and timeout
	run := strings.Repeat("<", (MaxChecksFileBytes-len("checks:\n"))/MaxChecksPerFile-MaxCheckNameBytes-perCheck)
	var b strings.Builder
	b.WriteString("checks:\n")
	for i := range MaxChecksPerFile {
		name := fmt.Sprintf("%s%02d", strings.Repeat("n", MaxCheckNameBytes-2), i)
		fmt.Fprintf(&b, "  - name: %s\n    run: m%s\n    timeout_seconds: 60\n", name, run)
	}
	raw := b.String()
	if len(raw) > MaxChecksFileBytes || len(raw) < MaxChecksFileBytes-MaxChecksPerFile*perCheck {
		t.Fatalf("the widest checks file is %d bytes, want it within %d of MaxChecksFileBytes (%d)", len(raw),
			MaxChecksPerFile*perCheck, MaxChecksFileBytes)
	}
	file, err := ParseChecks([]byte(raw))
	if err != nil || len(file.Checks) != MaxChecksPerFile {
		t.Fatalf("the widest checks file parsed to %d checks (%v), want %d", len(file.Checks), err,
			MaxChecksPerFile)
	}
	target := "refs/heads/" + strings.Repeat("t", 200)
	head := DispatchedHead{OID: oid("head"), ChangeID: "0190f3c4-0000-7000-8000-00000000c0de", Revision: 1}
	at := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: statelog.MaxSeq}
	d := CheckDispatched{RepoID: checkRepo, Target: target, Tip: oid("tip"), TipPosition: at, DefsOID: oid("defs"),
		Heads: []DispatchedHead{head}, DispatchedAt: applyClock}
	for _, def := range file.Checks {
		d.Attempts = append(d.Attempts, CheckQueued{CheckKey: CheckKey{RepoID: checkRepo, OID: head.OID,
			Target: target, Name: def.Name, Attempt: MaxCheckAttempts}, Purpose: PurposeHead,
			ChangeID: head.ChangeID, Revision: head.Revision, DefsOID: d.DefsOID, Def: &def, Required: true,
			Reason: QueueDispatch, QueuedAt: applyClock})
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxDispatchBytes {
		t.Fatalf("the widest head's dispatch encodes to %d bytes, past MaxDispatchBytes (%d)", len(encoded),
			MaxDispatchBytes)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the widest head's dispatch is refused: %v", err)
	}
	t.Logf("the widest head's dispatch encodes to %d of MaxDispatchBytes' %d", len(encoded), MaxDispatchBytes)
}
