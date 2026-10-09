package search_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A RECORD ABOUT AN INDEX IS RETAINED BEHIND THAT INDEX'S DEFERRED CENTROIDS.
//
// A later build that reshapes the centroids record writes it above this
// build's version, and its rollout and measurements may keep the version this
// build reads. Applied here, those would be no-ops against the index this node
// still holds — acknowledged and consumed — and once the node could read the
// centroids its rows would stay filed under the old index for ever, read in
// full by every search, while the duty's own node saw nothing to re-publish.
// Their scope paths nest under the centroids record's, so the framework's own
// deferral probe retains them behind it; a document's embed, scoped elsewhere,
// still applies.
func TestAnIndexsRecordsWaitBehindItsDeferredCentroids(t *testing.T) {
	t.Parallel()
	const dim, model = 16, "nested-embed"
	rng := rand.New(rand.NewPCG(9, 9))
	generation := statelog.Position{Stream: search.Domain{}.Stream().Name,
		Generation: 1, Seq: 1}.Packed()
	later := indexRecordOver(model, dim)
	later.V = search.RecordVersion + 1 // a later build's reshaped centroids
	var payloads [][]byte
	for _, rec := range []search.VectorRecord{
		later,
		reassignRecord(generation, 0, 1),
		measureRecord(generation, 1),
		embedRecord(search.SourceTask, "t1", model, randomEmbedding(rng, dim)),
	} {
		payload, err := rec.Encode()
		if err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	for _, subject := range []search.Subject{search.ReassignSubject(generation, 0),
		search.MeasureSubject(generation)} {
		if !statelog.Covers(search.IndexScopePath(search.IndexCentroids),
			search.IndexScopePath(subject)) {
			t.Fatalf("%s's scope %q does not nest under the centroids record's %q",
				subject, search.IndexScopePath(subject),
				search.IndexScopePath(search.IndexCentroids))
		}
	}

	db := openPinned(t)
	runLog(t, db, versionDomain{reads: search.RecordVersion}, payloads)
	if retained := retainedSeqs(t, db); fmt.Sprint(retained) != "[1 2 3]" {
		t.Fatalf("behind a deferred centroids record this node retained %v — "+
			"its rollout batch and its measurement must wait behind it rather "+
			"than apply as no-ops against the index this node still holds",
			retained)
	}
	if got := queryStrings(t, db, `SELECT source_id FROM kb_vectors`); len(got) != 1 {
		t.Fatalf("a document's embed behind a deferred index record was not "+
			"applied: %v", got)
	}
}

// openPinned is a store with the one pinned writer an applier loop holds.
func openPinned(t *testing.T) *store.DB {
	t.Helper()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// indexRecordOver is an installable centroids record of one list.
func indexRecordOver(model string, dim int) search.VectorRecord {
	measurement := search.Measurement{Sources: 2, Recall: 1, Floor: 0.98,
		Shape: search.ShapeAll}
	return search.VectorRecord{
		RecordEnvelope: search.RecordEnvelope{
			Subject: search.IndexCentroids, Op: search.OpCentroids, Gen: 1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope: statelog.ScopeSet{Paths: []string{
				search.IndexScopePath(search.IndexCentroids)}},
		},
		Model: model, Dim: dim,
		Index: &search.IndexRecord{Log: "S", Lists: 1, Probes: 1, TrainedOn: 2,
			Measurement: &measurement, Centroids: make([]byte, 8*search.CodeWords(dim)),
			Rollout: search.RolloutRanges(nil, search.IVFReassignBatch)},
	}
}

// versionDomain is the vector domain as a build reading only up to reads
// declares it.
type versionDomain struct {
	search.Domain
	reads int
}

func (d versionDomain) RecordVersion() int { return d.reads }

// retainedSeqs is the sequence of every record this node retained, ascending.
func retainedSeqs(t *testing.T, db *store.DB) []uint64 {
	t.Helper()
	var out []uint64
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT position FROM vectors_log_deferred ORDER BY position`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var position int64
			if err := rows.Scan(&position); err != nil {
				return err
			}
			out = append(out, uint64(position%statelog.GenerationStride))
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// runLog runs a statelog.Runner for domain over payloads at sequences 1…n
// until it has committed the last.
func runLog(t *testing.T, db *store.DB, domain statelog.Domain, payloads [][]byte) {
	t.Helper()
	log := &sliceLog{stored: time.Unix(1_700_000_000, 0).UTC(),
		messages: append([][]byte(nil), payloads...)}
	recorder, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Spec: domain.Stream(), Applier: search.NewApplier(), Fetch: log, Log: log,
		Node: db, DB: db.Replicated(),
		Checkpoint: statelog.Position{Generation: 1}, Metrics: recorder,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	last := uint64(len(payloads))
	for runner.Committed().Seq < last {
		select {
		case err := <-done:
			t.Fatalf("the applier stopped at %s: %v", runner.Committed(), err)
		case <-ctx.Done():
			t.Fatalf("the applier reached %s of %d", runner.Committed(), last)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// sliceLog is a log of fixed records, delivered once each in order.
type sliceLog struct {
	mu        sync.Mutex
	messages  [][]byte
	delivered int
	stored    time.Time
}

func (l *sliceLog) storedAt(seq uint64) time.Time {
	return l.stored.Add(time.Duration(seq) * time.Second)
}

func (l *sliceLog) Fetch(ctx context.Context, maxMessages, _ int, wait time.Duration) ([]statelog.Message, error) {
	l.mu.Lock()
	var out []statelog.Message
	for l.delivered < len(l.messages) && len(out) < maxMessages {
		seq := uint64(l.delivered + 1)
		out = append(out, statelog.Message{Seq: seq, StoredAt: l.storedAt(seq),
			Payload: l.messages[l.delivered], Ack: func() error { return nil }})
		l.delivered++
	}
	l.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(wait, 10*time.Millisecond)):
		}
	}
	return out, nil
}

func (l *sliceLog) Pending(context.Context) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.messages) - l.delivered), nil
}

func (l *sliceLog) At(_ context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq == 0 || seq > uint64(len(l.messages)) {
		return "", nil, time.Time{}, false, nil
	}
	return "vectors", l.messages[seq-1], l.storedAt(seq), true, nil
}

func (l *sliceLog) Bounds(context.Context) (uint64, uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.messages) == 0 {
		return 0, 0, nil
	}
	return 1, uint64(len(l.messages)), nil
}
