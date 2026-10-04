package pages_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// AN EVICTION IS WRITTEN TO THIS LOG BY ITS OWN STORE, AND READ BACK FROM THIS
// LOG'S OWN ROWS — install, lift, and install again.
//
// For as long as only the tracker's writer could publish an eviction, this
// domain had an applier, a fence and a table for one and no way to write it:
// nothing in production ever filled `pages_evictions`, the pages applier never
// dropped an evicted node's records, and the trim — reading the tracker's
// table on this log's behalf — never stopped counting the node here. The store
// writes it now, and [pages.Domain.Evictions] is what the trim reads, so what
// that answers after each step is the contract.
func TestAnEvictionIsWrittenToThisLogAndReadBackFromIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	operator := pages.Actor{Handle: "founder", Kind: pages.AuthorOperator,
		OperatorID: "founder"}
	standing := func(node string) (row statelog.EvictionRow, held bool) {
		t.Helper()
		rows, err := pages.Domain{}.Evictions(t.Context(), r.db.Reader())
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		for _, row := range rows {
			if row.NodeID == node {
				return row, true
			}
		}
		return statelog.EvictionRow{}, false
	}
	// AND THE SAME STANDING READ OFF THE LOG ITSELF ([statelog.EvictedOnLog]),
	// which is how a node a peer re-anchored past sees an eviction its
	// stopped applier never reaches.
	onLog := func(want bool) {
		t.Helper()
		evicted, found, err := statelog.EvictedOnLog(t.Context(), pages.Domain{}, statelog.EstateStream(pages.Domain{}), r.log, "node-b")
		if err != nil || !found || evicted != want {
			t.Fatalf("node-b's standing read off the log = evicted %v, found %v (%v), "+
				"want evicted %v", evicted, found, err, want)
		}
	}

	res, err := r.store.EvictNode(t.Context(), operator, "op-evict", "node-b")
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	if res.Position.Seq == 0 || res.OpID != "op-evict" {
		t.Fatalf("the eviction answered %+v — a gate write reports where it "+
			"landed and under which operation, or a partial gesture could not "+
			"be retried", res)
	}
	r.drain()
	row, held := standing("node-b")
	switch {
	case !held:
		t.Fatal("the eviction landed and this log's rows hold no row for the node")
	case row.Back:
		t.Fatalf("node-b reads as back straight after its eviction: %+v", row)
	case row.From != uint64(res.Position.Packed()):
		t.Fatalf("the eviction takes effect above %d, want its own position %d — "+
			"the gate drops records by comparing against exactly this",
			row.From, res.Position.Packed())
	case row.By != operator.Name():
		t.Fatalf("the eviction was run by %q, want %q", row.By, operator.Name())
	case row.At.IsZero():
		t.Fatal("the eviction carries no instant, and the fence window is " +
			"measured from it")
	}

	onLog(true)
	back, err := r.store.ReadmitNode(t.Context(), operator, "op-back", "node-b")
	if err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	r.drain()
	if row, held = standing("node-b"); !held || !row.Back ||
		row.Readmitted != uint64(back.Position.Packed()) {
		t.Fatalf("node-b after its readmission reads %+v (held %v), want its row "+
			"kept and back at %d — a readmission is an inverse commit, not a "+
			"delete", row, held, back.Position.Packed())
	}

	onLog(false)
	if _, err := r.store.EvictNode(t.Context(), operator, "op-again", "node-b"); err != nil {
		t.Fatalf("EvictNode again: %v", err)
	}
	r.drain()
	if row, held = standing("node-b"); !held || row.Back {
		t.Fatalf("node-b evicted a second time reads %+v (held %v) — a "+
			"re-eviction clears the readmission", row, held)
	}
	onLog(true)
}

// A GATE WRITE REFUSES WHAT IT CANNOT MEAN, before it forms a record.
func TestAnEvictionNamingNothingIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	operator := pages.Actor{Kind: pages.AuthorOperator, OperatorID: "founder"}
	if _, err := r.store.EvictNode(t.Context(), operator, "op", ""); err == nil {
		t.Fatal("an eviction naming no node was written")
	}
	if _, err := r.store.EvictNode(t.Context(), operator, "", "node-b"); err == nil {
		t.Fatal("an eviction with no operation id was written — a retry of it " +
			"could never be told from a second eviction")
	}
	if last, err := r.log.End(t.Context()); err != nil || last != 0 {
		t.Fatalf("the log's end is %d (err %v) after two refused gate writes", last, err)
	}
}

// A RELEASE RACING A WRITER: THE WRITE THE LEAVING NODE HAD IN FLIGHT IS
// DROPPED ON EVERY HOLDER, AND ITS CALLER IS TOLD SO — the knowledge base's
// half of the rule the tracker's log keeps.
//
// The leave runs between the in-flight write's last question about serving and
// its landing, so the write lands above the node's release and is refused
// `released`; after the release the node decides nothing on the log
// (`not_holder`); the row records the release as the node's own gate, which
// its write fence does not read as an eviction; and a release naming another
// node is refused before the log.
func TestAReleaseRacingAWriterDropsTheWriteItHadInFlight(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	r.ensure(activation(0), "ENG", "Engineering", "")
	r.write(author("ana"), pages.NewPage{Title: "before the release"})
	engine := pages.Actor{Handle: "engine", Kind: pages.AuthorOperator, OperatorID: "engine"}

	var released statelog.Result
	r.race.Before(func(subject string) bool { return !strings.Contains(subject, ".eviction.") },
		func() {
			// THE LEAVE, between the in-flight write's last question and
			// its landing: the node stops serving, then releases the log.
			r.holding.Stop(statelog.EstatePartition)
			var err error
			if released, err = r.store.ReleaseLog(t.Context(), engine, "op-release", "node-a"); err != nil {
				t.Errorf("ReleaseLog: %v", err)
			}
		})
	_, err := r.store.Create(t.Context(), author("ana"),
		pages.NewPage{Container: "ENG", Title: "racing the release"})
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonReleased {
		t.Fatalf("the write that landed after its node's release was answered %v, "+
			"want a refusal %q — it is on the log and applies nowhere", err,
			statelog.ReasonReleased)
	}
	r.drain()
	rows, err := pages.Domain{}.Evictions(t.Context(), r.db.Reader())
	if err != nil {
		t.Fatalf("read the gates: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != "node-a" || rows[0].Kind != statelog.EvictionKindRelease ||
		rows[0].From != uint64(released.Position.Packed()) || rows[0].Back || rows[0].By != "node-a" {
		t.Fatalf("the release is recorded as %+v, want node-a's own release above "+
			"its position %d", rows, released.Position.Packed())
	}
	evicted, err := pages.NewFence(r.db.Reader(), "node-a").Evicted(t.Context())
	if err != nil || evicted {
		t.Fatalf("the node's fence reads its own release as an eviction (%v, %v) — "+
			"a node that left a partition is not one the fleet removed", evicted, err)
	}
	var heads int
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM pages_heads WHERE container = 'ENG'`).Scan(&heads)
	}); err != nil {
		t.Fatalf("count the pages: %v", err)
	}
	if heads != 1 {
		t.Fatalf("ENG holds %d page(s), want only the one written before the release", heads)
	}

	end := r.logEnd()
	_, err = r.store.Create(t.Context(), author("ana"),
		pages.NewPage{Container: "ENG", Title: "after the release"})
	if !errors.Is(err, statelog.ErrNotHolder) {
		t.Fatalf("a write asked of the node after it left was answered %v, want %v",
			err, statelog.ErrNotHolder)
	}
	if _, err := r.store.ReleaseLog(t.Context(), engine, "op-release-other", "node-b"); err == nil {
		t.Fatal("a node published a release naming another node — an eviction " +
			"nobody judged")
	}
	if after := r.logEnd(); after != end {
		t.Fatalf("a refused write reached the log: its end moved from %d to %d", end, after)
	}
}

// A RELEASE IS WRITTEN AT THE VERSION THAT ADDED IT, AND A BUILD FROM BEFORE IT
// HALTS THERE — through the real framework loop, over the real log.
//
// A release carries an eviction's bytes under the eviction's kind, so a build
// that predates it would decode it and record an EVICTION: the same gate, in a
// row naming the wrong one, which every newer node holds as a release — two
// builds' rows differing for ever. Its kind is a gate's, so written at version
// 3 that build halts at it rather than apply it the old way; the eviction
// beside it stays at the version every build reads, because its apply did not
// change.
func TestAReleaseIsWrittenAtItsOwnVersionAndAnOlderBuildHaltsAtIt(t *testing.T) {
	t.Parallel()
	if got := (pages.Domain{}).RecordVersion(); got < 3 {
		t.Fatalf("this build reads record version %d, want at least the 3 that "+
			"added a node's release of the log", got)
	}
	r := newRoundTrip(t)
	r.applyWhileWriting()
	operator := pages.Actor{Handle: "founder", Kind: pages.AuthorOperator, OperatorID: "founder"}
	if _, err := r.store.EvictNode(t.Context(), operator, "op-evict", "node-gone"); err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	evicted := r.logEnd()
	engine := pages.Actor{Handle: "engine", Kind: pages.AuthorOperator, OperatorID: "engine"}
	// A NODE RELEASES A LOG ONCE IT HAS STOPPED SERVING ITS PARTITION.
	r.holding.Stop(statelog.EstatePartition)
	if _, err := r.store.ReleaseLog(t.Context(), engine, "op-release", "node-a"); err != nil {
		t.Fatalf("ReleaseLog: %v", err)
	}
	released := r.logEnd()
	if v := r.envelopeAt(evicted).V; v != 1 {
		t.Errorf("the eviction carries version %d, want 1 — its apply is every "+
			"build's, and a higher version would halt nodes that can apply it", v)
	}
	if v := r.envelopeAt(released).V; v != 3 {
		t.Errorf("the release carries version %d, want 3 — a build from before it "+
			"would record it as an eviction", v)
	}

	olderNode, older := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "older.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = olderNode.Close() })
	build := beforeReleaseBuild{}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: build, Spec: statelog.EstateStream(build),
		Layout: statelog.EstateLayout(build.Name()), LogID: statelog.EstateLog(build),
		Applier: pages.NewApplier("node-older", nil, nil),
		Fetch:   &logFetch{log: r.log, next: 1},
		Log:     r.log,
		Node:    olderNode,
		DB:      older,
	})
	if err != nil {
		t.Fatalf("build the older node's applier: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := runner.Run(ctx); !errors.Is(err, statelog.ErrStopped) {
		t.Fatalf("the older node's loop ended with %v, want it stopped at the release", err)
	}
	// BELOW THE RELEASE, wherever in the batch the stop left it: the loop
	// applies a run of records in one transaction, and a stop inside it
	// commits none of them.
	if at := runner.Committed().Seq; at >= released {
		t.Errorf("the older node stands at %d, past the release at %d it cannot "+
			"apply", at, released)
	}
	var gates int
	if err := older.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM pages_evictions WHERE node_id = 'node-a'`).Scan(&gates)
	}); err != nil {
		t.Fatalf("read the older node's gates: %v", err)
	}
	if gates != 0 {
		t.Errorf("the older node recorded the release it halted at (%d row(s))", gates)
	}
}

// beforeReleaseBuild is this domain as a build from before the release — one
// that reads up to record version 2 — sees it.
type beforeReleaseBuild struct{ pages.Domain }

func (beforeReleaseBuild) RecordVersion() int { return 2 }
