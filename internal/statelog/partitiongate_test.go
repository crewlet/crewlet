package statelog_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// strayKind is a record kind the placing domain files in another partition
// than the probe's one log, and strayPath a scope path it places there too.
const (
	strayKind = "stray"
	strayPath = "stray/"
)

// elsewhere is a partition no log of the probe's layout carries — "another
// partition" from wherever the probe's one log is.
var elsewhere = statelog.PartitionID{Space: statelog.SpaceTracker, Index: 3}

// placingDomain is the probe domain with a partition function that places one
// kind of record, and one family of scope paths, in another partition than the
// log it runs on — what a domain whose writer or router sent a record to the
// wrong log looks like to the framework.
type placingDomain struct{ probeDomain }

func (d placingDomain) PartitionOf(l statelog.Layout, env statelog.Envelope) (statelog.PartitionID, bool) {
	if env.Kind == strayKind {
		return elsewhere, true
	}
	return d.probeDomain.PartitionOf(l, env)
}

func (d placingDomain) ScopePartition(l statelog.Layout, path string) (statelog.PartitionID, bool) {
	if strings.HasPrefix(path, strayPath) {
		return elsewhere, true
	}
	if path == "domain" {
		// THE LOG'S OWN TERM, which lies wherever it is written.
		return statelog.PartitionID{}, false
	}
	return d.probeDomain.ScopePartition(l, path)
}

// probeRows is every probe row the applier wrote, by position.
func probeRows(t *testing.T, h *applyHarness) []int64 {
	t.Helper()
	var out []int64
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT position FROM probe_rows ORDER BY position`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var at int64
			if err := rows.Scan(&at); err != nil {
				return err
			}
			out = append(out, at)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the probe rows: %v", err)
	}
	return out
}

// gatedUnder is how many records the runner counted as gated under reason.
func gatedUnder(h *applyHarness, reason statelog.Reason) uint64 {
	var total uint64
	for _, snapshot := range h.metrics.Read() {
		if snapshot.Name == metrics.StatelogRecordsGated && snapshot.Attrs["gate"] == string(reason) {
			total += snapshot.Total
		}
	}
	return total
}

// GATE 2: A RECORD ITS OWN DOMAIN PLACES IN ANOTHER PARTITION IS DROPPED ON
// EVERY HOLDER OF THE LOG IT IS ON — whatever its version, and identically.
//
// Applied, it would write rows this partition does not own. Retained because a
// build cannot read it, it would be filed under a scope only this partition's
// probe reads — a deferral the partition it belongs to never sees, which is the
// gap clause (i) of the floor theorem cannot close. So the applier asks the
// domain where the record belongs, from the envelope every build decodes, and
// every holder asking the same function of the same bytes drops it alike, under
// a gate the counter names.
func TestARecordItsDomainPlacesInAnotherPartitionIsGatedOnEveryHolder(t *testing.T) {
	t.Parallel()
	offer := func(h *applyHarness) {
		h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
		stray := env(2, strayKind, "b", "op-2", 1)
		h.fetch.offer(2, stray)
		// A VERSION THIS BUILD CANNOT READ, which is retained unless the
		// partition gate is asked first.
		future := env(3, strayKind, "c", "op-3", 9)
		h.fetch.offer(3, future)
		h.fetch.offer(4, env(4, "edit", "d", "op-4", 1))
	}
	var holders [][]int64
	for range 2 {
		h := newApplyHarness(t, placingDomain{})
		offer(h)
		if err := h.run(4); err != nil {
			t.Fatalf("run: %v", err)
		}
		rows := probeRows(t, h)
		want := []int64{
			statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}.Packed(),
			statelog.Position{Stream: probeStream, Generation: 1, Seq: 4}.Packed(),
		}
		if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
			t.Fatalf("the holder applied the rows at %v, want only %v — a record "+
				"its domain places in %s wrote rows on a log of %s", rows, want,
				elsewhere, statelog.EstatePartition)
		}
		if n := h.retainedCount(); n != 0 {
			t.Errorf("the holder retained %d record(s) — a record that belongs to "+
				"another partition was filed under a scope that partition never "+
				"probes", n)
		}
		if n := gatedUnder(h, statelog.ReasonWrongPartition); n != 2 {
			t.Errorf("the holder counted %d record(s) gated %q, want 2 — a dropped "+
				"record's only witness is this counter", n, statelog.ReasonWrongPartition)
		}
		for _, op := range []string{"op-2", "op-3"} {
			held, err := h.ledgerHolds(op)
			if err != nil {
				t.Fatalf("read the ledger: %v", err)
			}
			if held {
				t.Errorf("the ledger holds %s, which applied nowhere", op)
			}
		}
		holders = append(holders, rows)
	}
	if len(holders[0]) != len(holders[1]) {
		t.Fatalf("two holders of one log hold different rows: %v and %v", holders[0], holders[1])
	}
}

// GATE 2 AT ITS SOURCE: THE WRITE AUTHORITY NEVER APPENDS A RECORD ITS DOMAIN
// PLACES IN ANOTHER PARTITION.
//
// Every holder would drop it, so appending it buys a durable record that applies
// nowhere — and its resolution, finding no ledger row and no gate the domain's
// reader knows of, would read the silence as a lost race and decide again until
// the round budget reported a colleague who does not exist.
func TestTheWriteAuthorityRefusesARecordThatBelongsToAnotherPartition(t *testing.T) {
	t.Parallel()
	h := newHarnessFor(t, placingDomain{})
	subject := statelog.Subject{Kind: "object", ID: "a"}
	_, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: subject, Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID: "op-stray", Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return statelog.Decision{Payload: strayRecord(stamp, "op-stray", nil), Version: 1}, nil
		},
	})
	if !errors.Is(err, statelog.ErrWrongPartition) {
		t.Fatalf("a record its domain places in %s was answered %v, want %v",
			elsewhere, err, statelog.ErrWrongPartition)
	}
	if n := h.appends.appends.Load(); n != 0 {
		t.Fatalf("the refused record reached the broker %d time(s)", n)
	}
}

// GATE 2 IN A RESOLUTION: A COPY OF THE OPERATION THAT ITS DOMAIN PLACES IN
// ANOTHER PARTITION IS REFUSED `wrong_partition` — whichever way the write met
// it.
//
// This node's own append is placed before it reaches the broker, so a misplaced
// record carrying its operation is one somebody else put on the log — another
// build, or a writer that routed it here — and every holder drops it. The
// broker holds its operation id for the log's duplicate window, so the same
// operation sent inside it is COLLAPSED onto that record; and a write whose
// answer was lost FINDS it newest on the subject. Either way this node's
// applier passed it and wrote no ledger row, and a resolution that asked only
// the domain's gates and the reanchor's rules found neither, trusted a ledger
// that vouched, and reported a contract violation for a record that was only
// ever gated.
func TestACopyItsDomainPlacesInAnotherPartitionIsRefusedWrongPartition(t *testing.T) {
	t.Parallel()
	const op = "op-misplaced"
	// stray lands node-b's copy of the operation, of the kind the placing
	// domain files elsewhere, on the subject — and has this node's applier
	// pass it, writing no ledger row, as the partition gate does.
	stray := func(t *testing.T, h *harness) statelog.Position {
		h.applier.mu.Lock()
		h.applier.auto = false
		h.applier.mu.Unlock()
		seq, _, err := h.log.Append(t.Context(), probePrefix+".object.a", op, nil,
			strayRecord(statelog.Stamp{Gen: h.gen.Load(), Writer: "node-b"}, op, nil))
		if err != nil {
			t.Errorf("land node-b's misplaced copy: %v", err)
			return statelog.Position{}
		}
		at := statelog.Position{Stream: probeStream, Generation: h.gen.Load(), Seq: seq}
		h.applier.advance(at)
		return at
	}
	for _, tc := range []struct {
		name  string
		found bool
	}{
		{name: "collapsed onto it"},
		{name: "found newest on the subject", found: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, placingDomain{})
			var at statelog.Position
			if tc.found {
				// THE COPY LANDS AFTER THIS WRITE'S SNAPSHOT, and this
				// write's own append never does, its answer lost.
				var once sync.Once
				h.rows.mu.Lock()
				h.rows.afterSnapshot = func() { once.Do(func() { at = stray(t, h) }) }
				h.rows.mu.Unlock()
				h.appends.fail(errors.New("no response from stream"), true)
			} else {
				at = stray(t, h)
			}

			res, err := h.write(probeSubject("a"), op, "mine")
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonWrongPartition {
				t.Fatalf("a write meeting node-b's copy in %s = (%+v, %v), want a "+
					"refusal %q", elsewhere, res, err, statelog.ReasonWrongPartition)
			}
			if refusal.Position != at || refusal.OpID != op || refusal.CopyWriter != "node-b" {
				t.Errorf("refusal = %+v, want %q at %s under %s, naming node-b's copy",
					refusal, statelog.ReasonWrongPartition, at, op)
			}
		})
	}
}

// GATE 1: A WRITE WHOSE SCOPE NAMES ANOTHER PARTITION'S OBJECT IS REFUSED
// BEFORE ANYTHING IS APPENDED — on the request's scope, which step 0 probes, and
// on the record's, which a holder that cannot decode it files a deferral under.
//
// A deferral is filed and probed in one partition's file, so a path naming
// another partition's object is one that partition's probe never sees. What
// passes is a path naming the log itself, which lies wherever it is written.
func TestAWriteWhoseScopeNamesAnotherPartitionIsRefusedBeforeItIsAppended(t *testing.T) {
	t.Parallel()
	subject := statelog.Subject{Kind: "object", ID: "a"}
	for _, tc := range []struct {
		name    string
		request []string
		record  []string
		refused bool
	}{
		{"the request's scope", []string{subject.String(), strayPath + "x"}, nil, true},
		{"the record's scope", []string{subject.String()}, []string{subject.String(), strayPath + "x"}, true},
		{"a path naming the log itself", []string{subject.String(), "domain"}, []string{"domain"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, placingDomain{})
			res, err := h.pub.Publish(t.Context(), statelog.Request{
				Subject: subject, Scope: statelog.ScopeSet{Paths: tc.request},
				OpID: "op-scope", Pattern: statelog.PatternArbitrated,
				Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
					payload := probeRecord(stamp, "op-scope", "body")
					if tc.record != nil {
						payload = scopedRecord(stamp, "op-scope", tc.record)
					}
					return statelog.Decision{Payload: payload, Version: 1}, nil
				},
			})
			switch {
			case tc.refused && !errors.Is(err, statelog.ErrScopeCrossesPartitions):
				t.Fatalf("a scope naming %s was answered (%+v, %v), want %v",
					elsewhere, res, err, statelog.ErrScopeCrossesPartitions)
			case tc.refused && h.appends.appends.Load() != 0:
				t.Fatalf("the refused write reached the broker %d time(s)", h.appends.appends.Load())
			case !tc.refused && errors.Is(err, statelog.ErrScopeCrossesPartitions):
				t.Fatalf("a scope naming only this partition and the log itself "+
					"was refused: %v", err)
			}
		})
	}
}

// strayRecord is a probe record of the kind the placing domain files in
// another partition.
func strayRecord(stamp statelog.Stamp, opID string, scope []string) []byte {
	return encodeProbe(statelog.Envelope{
		V: 1, Kind: strayKind, OpID: opID, Gen: stamp.Gen, Writer: stamp.Writer,
		Scope: statelog.ScopeSet{Paths: scope},
	})
}

// scopedRecord is an ordinary probe record declaring the scope given.
func scopedRecord(stamp statelog.Stamp, opID string, scope []string) []byte {
	return encodeProbe(statelog.Envelope{
		V: 1, Kind: "object", OpID: opID, Gen: stamp.Gen, Writer: stamp.Writer,
		Scope: statelog.ScopeSet{Paths: scope},
	})
}

// encodeProbe is one record in the probe domain's own shape — its envelope,
// which is what [probeDomain.Envelope] reads back.
func encodeProbe(env statelog.Envelope) []byte {
	payload, err := json.Marshal(env)
	if err != nil {
		panic(fmt.Sprintf("encode a probe record: %v", err))
	}
	return payload
}

// upgradedPlacingDomain is the placing domain as a later build reads it: a
// build that reads record version reads, and places the stray kind in another
// partition — which the build before it, [probeDomain], placed on this log.
type upgradedPlacingDomain struct {
	placingDomain
	reads int
}

func (d upgradedPlacingDomain) RecordVersion() int { return d.reads }

// GATE 2 ON A RETAINED RECORD: A BUILD THAT PLACES IT IN ANOTHER PARTITION
// DROPS IT WHEN IT REPROCESSES IT, exactly as it would have dropped it live.
//
// A record is retained by a build that cannot read it, and that build asked
// where the record belongs with the partition function IT has — one that may
// place a kind it does not know yet on this log. The build that reads the
// record may place it elsewhere, and a holder that was on it when the record
// arrived gates it there at once. A holder that retained it and upgraded must
// reach the same answer when it reprocesses the record, or its rows hold what
// every other holder of the log dropped. And what the gated record held back by
// scope is released with it, since nothing applied is left to order it behind.
func TestAReprocessedRecordAnotherPartitionOwnsIsGated(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	// ABOVE THE RETAINING BUILD, which places every record on its one log.
	h.fetch.offer(2, env(2, strayKind, "b", "op-2", 9))
	// ON THE ROWS 2 WOULD HAVE MADE STALE, so retained behind it.
	h.fetch.offer(3, env(3, "edit", "b", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 2 {
		t.Fatalf("the retaining build kept %d record(s), want 2", got)
	}

	// THE UPGRADE: a build that reads the record and places its kind in
	// another partition than this log's.
	h.upgrade(upgradedPlacingDomain{reads: 9})
	if err := h.boot(0); err != nil {
		t.Fatalf("the upgraded build's boot: %v", err)
	}
	seen := h.applier.seen()
	if len(seen) != 1 || seen[0].Seq != 3 {
		t.Fatalf("the upgraded build applied %v, want only the record at 3 — the "+
			"record at 2 belongs to %s and every holder that met it live dropped it",
			seen, elsewhere)
	}
	rows := probeRows(t, h)
	want := []int64{
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 1}.Packed(),
		statelog.Position{Stream: probeStream, Generation: 1, Seq: 3}.Packed(),
	}
	if len(rows) != len(want) || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("the holder holds rows at %v, want %v", rows, want)
	}
	if n := gatedUnder(h, statelog.ReasonWrongPartition); n != 1 {
		t.Errorf("the upgraded build counted %d record(s) gated %q, want 1 — a "+
			"dropped record's only witness is this counter", n, statelog.ReasonWrongPartition)
	}
	for op, applied := range map[string]bool{"op-2": false, "op-3": true} {
		held, err := h.ledgerHolds(op)
		if err != nil {
			t.Fatalf("read the ledger: %v", err)
		}
		if held != applied {
			t.Errorf("the ledger holds %s: %v, want %v", op, held, applied)
		}
	}
	// AND COUNTED AS THE LIVE LOOP COUNTS IT: a record a gate dropped wrote
	// no row, so it is `gated` and not `reprocessed` — a reprocess tally
	// that took every released record for an applied one reported rows
	// this node does not hold.
	for result, want := range map[string]uint64{"reprocessed": 1, "gated": 1} {
		if got := appliedAs(h, result); got != want {
			t.Errorf("the upgraded build counted %d record(s) %q, want %d", got,
				result, want)
		}
	}
}

// appliedAs is how many records the runner counted consumed with result.
func appliedAs(h *applyHarness, result string) uint64 {
	var total uint64
	for _, snapshot := range h.metrics.Read() {
		if snapshot.Name == metrics.StatelogApplyRecords && snapshot.Attrs["result"] == result {
			total += snapshot.Total
		}
	}
	return total
}
