package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE REGISTER IS THE ONLY PLACE A FLEET CAN SEE ANOTHER NODE'S ARTEFACT.
//
// These cases exist because the five snapshot fields on [coord.NodePositions]
// had three readers and no writer at all. The trim's snapshot term counts
// donors from them and refuses to remove anything until two counted nodes hold
// one — so on every fleet of two or more, the term was permanently unknown and
// the log of every domain grew for the life of the deployment, while the
// applied term, the backup term and every operator surface reported a healthy
// fleet.

// A TAKEN SNAPSHOT REACHES THE ROW, per domain and with its own generation.
func TestTheRowCarriesTheSnapshotThisNodeHolds(t *testing.T) {
	t.Parallel()
	taken := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	row := coord.NodePositions{
		NodeID: "node-a",
		Domains: map[string]coord.DomainPosition{
			"tracker": {Seq: 900, Generation: 2, AppliedThrough: 900},
			"pages":   {Seq: 40, Generation: 2, AppliedThrough: 40},
		},
	}
	stampSnapshot(&row, heldSnapshots{statelog.EstatePartition: {
		Have: true,
		Manifest: statelog.Manifest{
			TakenAt: taken,
			Bytes:   4 << 20,
			Domains: map[string]statelog.DomainPosition{
				"tracker": {Seq: 850, Generation: 2},
				"pages":   {Seq: 33, Generation: 2},
			},
		},
	}})

	if row.SnapshotBytes != 4<<20 {
		t.Errorf("snapshot_bytes = %d, want %d — Settings › Nodes renders what "+
			"this node costs to transfer from this field", row.SnapshotBytes, 4<<20)
	}
	if row.SnapshotSkip != "" {
		t.Errorf("snapshot_skip = %q on a node that just took one — the field "+
			"answers \"why can this node not donate\", and it can", row.SnapshotSkip)
	}
	if got := row.Domains["tracker"]; got.SnapshotSeq != 850 ||
		got.SnapshotGeneration != 2 || !got.SnapshotAt.Equal(taken) {
		t.Errorf("tracker snapshot = seq %d gen %d at %s, want 850/2/%s — the "+
			"trim's snapshot term counts a donor from exactly these three",
			got.SnapshotSeq, got.SnapshotGeneration, got.SnapshotAt, taken)
	}
	if got := row.Domains["pages"].SnapshotSeq; got != 33 {
		t.Errorf("pages snapshot seq = %d, want 33 — one artefact covers every "+
			"domain, so a row naming only some of them under-reports the donor",
			got)
	}
	// AND THE COMMITTED POSITION IS UNTOUCHED. It is the applied term's
	// input and a snapshot is a different fact about the same node;
	// overwriting it would license removing records nobody applied.
	if got := row.Domains["tracker"].Seq; got != 900 {
		t.Errorf("the committed position moved to %d — the applied term reads "+
			"it, and a snapshot is not a position", got)
	}
}

// A DOMAIN THE ARTEFACT NAMES AND THIS NODE NO LONGER RUNS GETS NO ROW.
func TestAnArtefactDoesNotInventADomainThisNodeDoesNotRun(t *testing.T) {
	t.Parallel()
	row := coord.NodePositions{
		NodeID:  "node-a",
		Domains: map[string]coord.DomainPosition{"tracker": {Seq: 10, Generation: 1}},
	}
	stampSnapshot(&row, heldSnapshots{statelog.EstatePartition: {
		Have: true,
		Manifest: statelog.Manifest{Domains: map[string]statelog.DomainPosition{
			"tracker": {Seq: 8, Generation: 1},
			"retired": {Seq: 5, Generation: 1},
		}},
	}})
	if _, invented := row.Domains["retired"]; invented {
		t.Error("a domain this node does not run reached the register — it would " +
			"carry a committed position of zero, which the trim reads as a node " +
			"holding that log back at the floor for ever")
	}
}

// NOTHING CONCLUDED YET IS NOT A REFUSAL.
func TestABootingNodeSaysNothingRatherThanNo(t *testing.T) {
	t.Parallel()
	row := coord.NodePositions{
		NodeID:  "node-a",
		Domains: map[string]coord.DomainPosition{"tracker": {Seq: 10}},
	}
	stampSnapshot(&row, nil)
	if row.SnapshotSkip != "" || row.SnapshotBytes != 0 ||
		row.Domains["tracker"].SnapshotSeq != 0 {
		t.Errorf("a node whose snapshot loop has not concluded published "+
			"skip=%q bytes=%d seq=%d — an operator reads a skip as a known "+
			"answer, and this one is not known yet", row.SnapshotSkip,
			row.SnapshotBytes, row.Domains["tracker"].SnapshotSeq)
	}
}

// A SKIP DOES NOT ERASE WHAT IS ON DISK, and `recent` is the skip a node gets
// BECAUSE it holds a current artefact.
func TestWhatANodeHoldsSurvivesASkip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestSnapshot(t, dir, statelog.Manifest{
		TakenAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		Bytes:   7,
		Domains: map[string]statelog.DomainPosition{"tracker": {Seq: 120, Generation: 3}},
	})

	for _, tc := range []struct {
		name string
		err  error
		skip statelog.SkipReason
	}{
		{"recent", &statelog.ErrSkipped{Reason: statelog.SkipRecent},
			""},
		{"deferred", &statelog.ErrSkipped{Reason: statelog.SkipDeferred},
			statelog.SkipDeferred},
		{"a hard failure", errors.New("copy the replicated estate: disk is read-only"),
			statelog.SkipFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := heldAfter(statelog.Manifest{}, tc.err, dir, 0, statelog.EstatePartition)
			if !held.Have || held.Manifest.Domains["tracker"].Seq != 120 {
				t.Errorf("a %s tick dropped the artefact on disk (have=%v) — the "+
					"node can still donate it, and a fleet that stops counting it "+
					"stops trimming", tc.name, held.Have)
			}
			if held.Skip != tc.skip {
				t.Errorf("skip = %q, want %q", held.Skip, tc.skip)
			}
		})
	}
}

// A NODE WITH NEITHER AN ARTEFACT NOR A REASON IS THE ONE STATE THE FIELD
// EXISTS TO PREVENT.
func TestASkipWithNothingOnDiskStillPublishesItsReason(t *testing.T) {
	t.Parallel()
	held := heldAfter(statelog.Manifest{},
		&statelog.ErrSkipped{Reason: statelog.SkipSoleNode}, t.TempDir(), 0, statelog.EstatePartition)
	if held.Have {
		t.Fatal("an empty directory reported an artefact")
	}
	if held.Skip != statelog.SkipSoleNode {
		t.Errorf("skip = %q, want %q — an absent position is silent about "+
			"whether that is a full disk, a lagging node or a loop that has not "+
			"run", held.Skip, statelog.SkipSoleNode)
	}
}

// A SNAPSHOT OF AN EMPTY DOMAIN IS STILL A SNAPSHOT.
//
// One artefact covers every domain, so a node that snapshots while its newest
// domain's log is still empty covers that domain at position zero — a real
// file a joiner adopts. Reading "holds none" off the sequence would leave the
// fleet permanently one donor short of the two the trim's snapshot term needs,
// and it would do so for every domain rather than only the empty one.
func TestADomainSnapshottedWhileEmptyStillCountsAsADonor(t *testing.T) {
	t.Parallel()
	taken := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	rows := []coord.NodePositions{
		{NodeID: "has-one", Domains: map[string]coord.DomainPosition{
			"pages": {Seq: 0, SnapshotSeq: 0, SnapshotAt: taken},
		}},
		{NodeID: "has-none", Domains: map[string]coord.DomainPosition{
			"pages": {Seq: 0},
		}},
	}
	got := reportedPositions(rows, "pages")
	if len(got) != 2 {
		t.Fatalf("reported %d node(s), want 2", len(got))
	}
	if !got[0].HasSnapshot {
		t.Error("a node whose artefact covers an empty domain reports holding " +
			"none — the trim then waits for a second donor that will not " +
			"arrive until that log has records in it")
	}
	if got[1].HasSnapshot {
		t.Error("a node that has never snapshotted reports holding one, which " +
			"licenses removing records no artefact covers")
	}
}

// writeTestSnapshot lays down a complete artefact — the manifest AND the bytes
// beside it, because [newestSnapshot] refuses one without the other on the
// rule the backup manifest states: the manifest is written last, so a
// directory entry without its file is the debris of a run that did not finish.
func writeTestSnapshot(t *testing.T, dir string, m statelog.Manifest) {
	t.Helper()
	m.V = statelog.ManifestVersion
	// LAYOUT 0's ONE PARTITION unless the case says otherwise: an artefact
	// names the partition it is a copy of, and only one of the partition
	// asked about is held.
	if m.Partition == "" {
		m.Partition = statelog.EstatePartition.String()
	}
	var newest uint64
	for _, at := range m.Domains {
		if at.Seq > newest {
			newest = at.Seq
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
	base := filepath.Join(dir, fmt.Sprintf("snapshot-%d", newest))
	m.Artifact = filepath.Base(base) + ".db"
	if err := os.WriteFile(base+".db", []byte("store bytes"), 0o600); err != nil {
		t.Fatalf("write the artefact: %v", err)
	}
	if err := os.WriteFile(base+".json", body, 0o600); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

// WHAT A SKIPPED TICK SAYS, which is the whole of [reportForSkip].
//
// The loop retries a skip every thirty seconds for as long as it holds, so
// "report it" and "report it every time" are not the same instruction — and
// the second one is what the default topology got. `sole_node` is the steady
// state of a ONE-NODE company, which is the supported shape rather than a
// degraded fleet, so warned per tick it is a line every thirty seconds for the
// life of a healthy deployment.
func TestASkippedTickSaysSomethingOnlyWhenTheReasonChanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		reason   statelog.SkipReason
		reported statelog.SkipReason
		holds    bool
		event    string
		warn     bool
	}{
		{
			// The restart path, and the reason `holds` is an argument
			// rather than a branch at the call site.
			name:   "a node holding a current artefact says nothing",
			reason: statelog.SkipRecent,
			holds:  true,
		},
		{
			name:   "whatever it skipped for",
			reason: statelog.SkipSoleNode,
			holds:  true,
		},
		{
			name:   "a solo node states its posture, at info",
			reason: statelog.SkipSoleNode,
			event:  "statelog_snapshot_sole_node",
		},
		{
			name:     "and says it once, however long it holds",
			reason:   statelog.SkipSoleNode,
			reported: statelog.SkipSoleNode,
		},
		{
			// A fleet WITH peers holding no donor is the condition the
			// warning was written for, and it keeps it.
			name:   "a node that cannot catch up warns",
			reason: statelog.SkipUnhydrated,
			event:  "statelog_no_snapshot_yet",
			warn:   true,
		},
		{
			name:     "and is not repeated either",
			reason:   statelog.SkipUnhydrated,
			reported: statelog.SkipUnhydrated,
		},
		{
			// The reason MOVING is news in both directions: a fleet that
			// gained a peer now has a real precondition to report, and
			// one that lost its last peer is no longer in trouble.
			name:     "a changed reason is reported again",
			reason:   statelog.SkipDeferred,
			reported: statelog.SkipSoleNode,
			event:    "statelog_no_snapshot_yet",
			warn:     true,
		},
		{
			name:     "including back to sole_node",
			reason:   statelog.SkipSoleNode,
			reported: statelog.SkipLagging,
			event:    "statelog_snapshot_sole_node",
		},
		{
			// A hard failure stamps SkipFailed, so the next skip after
			// one is news whatever it is.
			name:     "and after a failure, whatever comes next",
			reason:   statelog.SkipSoleNode,
			reported: statelog.SkipFailed,
			event:    "statelog_snapshot_sole_node",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			say := reportForSkip(tc.reason, tc.reported, tc.holds)
			if say.Event != tc.event {
				t.Errorf("event = %q, want %q", say.Event, tc.event)
			}
			if say.Warn != tc.warn {
				t.Errorf("warn = %v, want %v — the level is the difference "+
					"between a fleet with no donor and a company that needs "+
					"none", say.Warn, tc.warn)
			}
			if tc.event != "" && say.Detail == "" {
				t.Error("no detail: an operator reading this line has only " +
					"the event name to go on")
			}
		})
	}
}

// The sole-node line must not repeat the claim the warning makes, because on a
// node with no peers it is false: nothing about being alone clears "as its
// peers publish their positions".
func TestTheSoleNodeLineDoesNotPromiseThatPeersWillFixIt(t *testing.T) {
	t.Parallel()
	say := reportForSkip(statelog.SkipSoleNode, "", false)
	if strings.Contains(say.Detail, "peers publish") {
		t.Errorf("sole-node detail claims peers will clear it: %q", say.Detail)
	}
	// It has to say what DOES cover a single node, or the reader is left
	// believing their company has no recovery path at all.
	if !strings.Contains(say.Detail, "backup") {
		t.Errorf("sole-node detail does not name the artefact that covers a "+
			"single node: %q", say.Detail)
	}
}

// A RESTART IS NOT A NODE THAT HAS NEVER SNAPSHOTTED.
//
// The loud branch asks whether this node HOLDS an artefact, and it has to,
// because the obvious spelling — a flag set when this process took one — is
// the same question only until the first restart. A node that snapshotted
// yesterday and was restarted skips for `recent`: its artefact is inside the
// operator's interval, its register row advertises it, and a peer can adopt
// from it. A process-scoped flag is false at that moment, so every restart
// warned that the node had never taken a snapshot while simultaneously
// publishing the one it holds.
//
// `Have` is read from the DIRECTORY, so it survives a restart the way the
// artefact does.
func TestARestartDoesNotClaimTheNodeHasNeverSnapshotted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestSnapshot(t, dir, statelog.Manifest{
		TakenAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		Domains: map[string]statelog.DomainPosition{"tracker": {Seq: 120, Generation: 3}},
	})

	// Exactly the first tick after a restart: nothing taken in THIS
	// process, and the gate declines because what is on disk is current.
	held := heldAfter(statelog.Manifest{},
		&statelog.ErrSkipped{Reason: statelog.SkipRecent}, dir, 0, statelog.EstatePartition)
	if !held.Have {
		t.Fatal("the artefact on disk was not seen, so the loop cannot tell " +
			"this restart from a node that has never snapshotted")
	}
	// `Have` is the predicate the loop branches on, so a true here is a
	// tick that says nothing rather than one that warns.
	if say := reportForSkip(statelog.SkipRecent, "", held.Have); say.Event != "" {
		t.Errorf("a `recent` skip on a node holding a current artefact "+
			"reports %q — the one reading here that is simply false", say.Event)
	}
}

// AND A NODE THAT HOLDS NOTHING STILL SAYS SO.
//
// The other direction of the same predicate: `Have` false is the state the
// fleet cannot recover from, and it is the one the branch is for.
func TestANodeHoldingNothingIsStillReported(t *testing.T) {
	t.Parallel()
	held := heldAfter(statelog.Manifest{},
		&statelog.ErrSkipped{Reason: statelog.SkipUnhydrated}, t.TempDir(), 0, statelog.EstatePartition)
	if held.Have {
		t.Fatal("an empty directory reported an artefact")
	}
	say := reportForSkip(statelog.SkipUnhydrated, "", held.Have)
	if !say.Warn || say.Event != "statelog_no_snapshot_yet" {
		t.Errorf("report = %+v, want the warning: a fleet with peers and no "+
			"donor is exactly what it is for", say)
	}
}

// A NUDGE WAKES THE SNAPSHOT LOOP, AND NOTHING ELSE DOES BEFORE ITS WAIT IS UP.
//
// A reanchor, an adoption and the restore of an installed artefact each leave
// this node holding an artefact no joiner will take, and each wakes the loop
// rather than leaving the fleet without a donor for the length of whichever
// wait the loop is in: the interval — a day — after a snapshot it took, and
// the skip retry after one it declined. Both waits are exercised here with the
// interval at a day, so a tick that comes early can only be the nudge.
func TestANudgeWakesTheSnapshotLoopOutOfEitherWait(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		take func() (statelog.Manifest, error)
	}{
		{"after a snapshot it took", func() (statelog.Manifest, error) {
			return statelog.Manifest{TakenAt: time.Now().UTC(), NodeID: "node-a"}, nil
		}},
		{"after a tick it declined", func() (statelog.Manifest, error) {
			return statelog.Manifest{}, &statelog.ErrSkipped{
				Reason: statelog.SkipLagging, Detail: "behind"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, stop := context.WithCancel(t.Context())
			s := &stateLog{run: ctx, snapshotNudge: make(chan struct{}, 1)}
			taker := &countingTaker{took: make(chan struct{}, 8), take: c.take}
			done := make(chan struct{})
			go func() {
				defer close(done)
				dir := t.TempDir()
				(&Engine{}).snapshotLoop(s, snapshotPlan{
					kept: func() snapshotScope {
						return snapshotScope{kept: []statelog.PartitionID{statelog.EstatePartition}}
					},
					dir: func(statelog.PartitionID) string { return dir },
					taker: func(statelog.PartitionID) (snapshotTaker, error) {
						return taker, nil
					},
				}, 24*time.Hour)
			}()
			t.Cleanup(func() { stop(); <-done })

			taker.await(t, "the boot's tick")
			select {
			case <-taker.took:
				t.Fatal("the loop ticked again with nothing to wake it")
			case <-time.After(300 * time.Millisecond):
			}
			s.nudgeSnapshot()
			taker.await(t, "the nudged tick")
			if _, held := s.snapshotOf(statelog.EstatePartition); !held {
				t.Fatal("the nudged tick published nothing to the register row")
			}
		})
	}
}

// countingTaker is a snapshotter that reports every attempt.
type countingTaker struct {
	took chan struct{}
	take func() (statelog.Manifest, error)
}

func (c *countingTaker) Take(context.Context) (statelog.Manifest, error) {
	m, err := c.take()
	c.took <- struct{}{}
	return m, err
}

// await waits for the loop's next attempt.
func (c *countingTaker) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.took:
	case <-time.After(5 * time.Second):
		t.Fatalf("waited 5s for %s", what)
	}
}

// A REANCHOR, AN ADOPTION AND A RESTORE EACH WAKE THE LOOP ON A RUNNING NODE.
//
// Each leaves the node holding an artefact at a generation a joiner refuses —
// or, for the restore, possibly the donor's file under an artefact of its own —
// and until the loop takes another, the peers the event left behind have no
// donor. The loop is first left waiting out its interval behind a snapshot it
// took, so the only thing that can run it again inside the test is the nudge
// the event gives it.
func TestEveryEventThatStrandsTheArtefactWakesTheSnapshotLoop(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		event func(t *testing.T) (*Engine, func())
	}{
		{"a reanchor", func(t *testing.T) (*Engine, func()) {
			e, js := aRunningNode(t)
			return e, func() {
				running := e.native.Load().log.Domain(tracker.Domain{}.Name())
				spec := running.spec
				if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
					res.Outcome != statelog.OutcomeApplied {
					t.Fatalf("a write before the rebuild: %+v, %v", res, err)
				}
				rebuildLog(t, js, spec)
				e.native.Load().log.publishPositions(t.Context())
				view, err := e.ReanchorStatus(t.Context(), spec.Name)
				if err != nil {
					t.Fatalf("ReanchorStatus: %v", err)
				}
				if _, err := e.Reanchor(t.Context(), ReanchorRequest{
					Stream: spec.Name, Confirm: statelog.ConfirmationOf(view.CreatedAt),
					By: "ops-1",
				}); err != nil {
					t.Fatalf("Reanchor: %v", err)
				}
			}
		}},
		{"an adoption", func(t *testing.T) (*Engine, func()) {
			e, back, q := bootRejoinNode(t)
			waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
			quietHeartbeat(e.native.Load().log)
			return e, func() {
				running, at, last := pushBelowTheFloor(t, e, q)
				rows := filepath.Join(t.TempDir(), "crewlet-replicated.db")
				copyAdvancedTo(t, back, running, at, last, rows)
				standUpDonor(t, q, rows, statelog.Position{
					Stream: at.Stream, Generation: at.Generation, Seq: last,
				}, running.runner.KeyedTo())
				if err := e.rejoin(e.native.Load().log.run, e.native.Load().log); err != nil {
					t.Fatalf("rejoin: %v", err)
				}
			}
		}},
		{"the restore of an estate a failed adoption left closed", func(t *testing.T) (*Engine, func()) {
			e, back, _ := bootRejoinNode(t)
			waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
			quietHeartbeat(e.native.Load().log)
			return e, func() {
				s := e.native.Load().log
				s.haltAppliers()
				if err := closeEstateZero(back.Store); err != nil {
					t.Fatalf("close the replicated estate: %v", err)
				}
				if err := s.restoreEstate(s.run); err != nil {
					t.Fatalf("restore: %v", err)
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, event := c.event(t)
			parked := parkSnapshotLoop(t, e)
			event()
			waitUntil(t, 10*time.Second, "the snapshot loop to wake", func() bool {
				return e.native.Load().log.snapshots.Load() != parked
			})
		})
	}
}

// parkSnapshotLoop leaves a running node's snapshot loop waiting out its
// interval — a day — behind a snapshot it has just taken, and returns what that
// tick published. From then on nothing but a nudge runs the loop again.
//
// A PEER IS COUNTED so the tick can take one — a row naming the partition's
// tracker log, which is what the trim counts it on: a node alone declines as
// `sole_node` and retries every thirty seconds, which would put a tick of its
// own inside any window a test watched. And the loop is NOT nudged here: a
// nudge that arrived while a tick was taking would run it again at once, and
// that tick — declining as `recent` — goes back to the thirty-second retry.
func parkSnapshotLoop(t *testing.T, e *Engine) *heldSnapshots {
	t.Helper()
	s := e.native.Load().log
	counted := time.Now().UTC()
	// AT THIS NODE'S OWN GENERATION: a peer's row a generation ahead reads as
	// a peer that re-anchored the log, which strands this node's rows.
	generation := s.Domain(tracker.Domain{}.Name()).runner.Committed().Generation
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "a-counted-peer", At: counted,
		Domains: map[string]coord.DomainPosition{
			tracker.Domain{}.Name(): {Generation: generation}},
	}); err != nil {
		t.Fatalf("publish a counted peer: %v", err)
	}
	var parked *heldSnapshots
	waitUntil(t, 75*time.Second, "the snapshot loop to take one and park", func() bool {
		set := s.snapshots.Load()
		if set == nil {
			return false
		}
		held, ok := (*set)[statelog.EstatePartition]
		if !ok || !held.Have || held.Skip != "" || held.Manifest.TakenAt.Before(counted) {
			return false
		}
		parked = set
		return true
	})
	return parked
}

// THE LOOP TAKES EVERY PARTITION IT SERVES, ONE AT A TIME, THE OLDEST FIRST.
//
// A snapshot is a copy of one partition's file, so a node holding many takes
// many, never two at once; and among those due, the partition whose newest
// artefact is oldest goes first — one holding none before any — because its
// donors are the stalest and a joiner of it replays furthest. What the loop
// concluded about each is held apart, per partition.
func TestTheLoopTakesEachServedPartitionOldestFirst(t *testing.T) {
	t.Parallel()
	p0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	p1 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	p2 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 2}
	ctx, stop := context.WithCancel(t.Context())
	s := &stateLog{run: ctx, layout: statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 3, Domains: []string{"tracker"}},
	}}, snapshotNudge: make(chan struct{}, 1)}
	earlier := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	s.holdSnapshot(p0, snapshotHeld{Have: true, Manifest: statelog.Manifest{TakenAt: earlier.Add(time.Hour)}})
	s.holdSnapshot(p2, snapshotHeld{Have: true, Manifest: statelog.Manifest{TakenAt: earlier}})

	var mu sync.Mutex
	var order []statelog.PartitionID
	took := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Engine{}).snapshotLoop(s, snapshotPlan{
			kept: func() snapshotScope {
				return snapshotScope{kept: []statelog.PartitionID{p0, p1, p2}}
			},
			dir: func(statelog.PartitionID) string { return t.TempDir() },
			taker: func(p statelog.PartitionID) (snapshotTaker, error) {
				return takerFunc(func() (statelog.Manifest, error) {
					mu.Lock()
					order = append(order, p)
					mu.Unlock()
					took <- struct{}{}
					return statelog.Manifest{TakenAt: time.Now().UTC(), Partition: p.String()}, nil
				}), nil
			},
		}, 24*time.Hour)
	}()
	t.Cleanup(func() { stop(); <-done })
	for range 3 {
		select {
		case <-took:
		case <-time.After(5 * time.Second):
			t.Fatal("the loop did not take every partition it serves")
		}
	}
	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	if want := []statelog.PartitionID{p1, p2, p0}; !slices.Equal(got, want) {
		t.Errorf("the loop took %v, want the oldest artefact's partition first %v", got, want)
	}
	for _, p := range []statelog.PartitionID{p0, p1, p2} {
		if held, ok := s.snapshotOf(p); !ok || !held.Have {
			t.Errorf("what the loop concluded about %s is not held (%+v)", p, held)
		}
	}
	select {
	case <-took:
		t.Fatal("a partition whose snapshot was just taken was taken again before its interval")
	case <-time.After(200 * time.Millisecond):
	}
}

// takerFunc is a snapshotter over a function.
type takerFunc func() (statelog.Manifest, error)

func (f takerFunc) Take(context.Context) (statelog.Manifest, error) { return f() }

// A PARTITION THIS NODE NO LONGER KEEPS A COPY OF IS NO LONGER ITS DONATION.
//
// What the loop concluded about a partition is what the row advertises a joiner
// may adopt; once the node has begun to give its copy up, keeping the entry
// would go on advertising a copy it no longer offers.
func TestAPartitionNoLongerKeptIsForgotten(t *testing.T) {
	t.Parallel()
	p0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	p1 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	s := &stateLog{}
	s.holdSnapshot(p0, snapshotHeld{Have: true})
	s.holdSnapshot(p1, snapshotHeld{Have: true})
	s.keepSnapshotsOf(snapshotScope{kept: []statelog.PartitionID{p1}})
	if _, held := s.snapshotOf(p0); held {
		t.Error("a partition no longer kept is still this node's donation")
	}
	if _, held := s.snapshotOf(p1); !held {
		t.Error("the partition still kept lost what the loop concluded about it")
	}
}

// A PARTITION THIS NODE COMES TO KEEP AFTER ITS LOOP STARTED IS TAKEN WITHIN
// THE RETRY, NOT A DAY LATER.
//
// A node keeps a partition's copy only once it is established — after the loop
// has started, at every boot — so the loop's first pass finds a partition it
// runs a log of and keeps no copy of yet. Waiting only on the partitions kept
// then sat out the whole interval, and the node took no artefact of what it
// came to keep for a day. While any partition is unsettled, the loop looks
// again within the retry.
func TestAPartitionKeptAfterTheLoopStartsIsTakenSoon(t *testing.T) {
	t.Parallel()
	p := statelog.PartitionID{Space: statelog.SpaceTracker}
	ctx, stop := context.WithCancel(t.Context())
	s := &stateLog{run: ctx, snapshotNudge: make(chan struct{}, 1)}
	var keeps atomic.Bool
	taker := &countingTaker{took: make(chan struct{}, 8), take: func() (statelog.Manifest, error) {
		return statelog.Manifest{TakenAt: time.Now().UTC(), Partition: p.String()}, nil
	}}
	passes := make(chan struct{}, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Engine{}).snapshotLoop(s, snapshotPlan{
			kept: func() snapshotScope {
				select {
				case passes <- struct{}{}:
				default:
				}
				if keeps.Load() {
					return snapshotScope{kept: []statelog.PartitionID{p}}
				}
				// A JOINER: it runs the partition's log and its copy is
				// not established yet.
				return snapshotScope{unsettled: true}
			},
			dir:   func(statelog.PartitionID) string { return t.TempDir() },
			taker: func(statelog.PartitionID) (snapshotTaker, error) { return taker, nil },
			retry: 20 * time.Millisecond,
		}, 24*time.Hour)
	}()
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-passes:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never looked at what it keeps")
	}
	keeps.Store(true)
	taker.await(t, "the partition it came to keep")
}

// WHAT THE LOOP FINDS IS WHAT THIS NODE RUNS, SORTED BY WHETHER IT KEEPS A COPY.
//
// The snapshot loop's scope comes from the partitions this node runs a log of,
// each asked of the copies the node keeps: a partition it keeps is taken; one
// it runs and keeps no copy of yet — a joiner still installing one — and one
// whose copy could not be told this pass each leave the loop UNSETTLED, so it
// looks again within the retry rather than a day later; and the one it could
// not tell about is named, so its report is kept rather than dropped on a
// moment's blip.
//
// AND NEVER BY WHETHER IT MAY WRITE THEM: the loop is told nothing of this
// node's write standing. A copy nobody may write — a machine an eviction
// barred, back with its files — may be its partition's only one, and a loop
// that took artefacts only of what the node could write gave that partition's
// joiner nothing to fetch.
func TestTheLoopsScopeIsWhatThisNodeRunsAndKeeps(t *testing.T) {
	t.Parallel()
	layout := partitionedTestLayout()
	parts := layout.Partitions()
	if len(parts) < 3 {
		t.Fatalf("the premise: the test layout has %d partitions, want three", len(parts))
	}
	kept, joining, unknown := parts[0], parts[1], parts[2]
	var runs []*runningLog
	for _, id := range layout.AllLogs() {
		runs = append(runs, &runningLog{id: id, key: id.String()})
	}
	scopeOf := func(c partitionAnswers) snapshotScope {
		s := &stateLog{run: t.Context(), layout: layout, copies: c}
		s.logs = newLogSet(layout, runs)
		return s.keptPartitions()
	}
	stale := errors.New("the estate view is stale")

	scope := scopeOf(partitionAnswers{kept: {yes: true}, joining: {}, unknown: {err: stale}})
	if !slices.Equal(scope.kept, []statelog.PartitionID{kept}) {
		t.Errorf("the loop takes %v, want the one partition this node keeps a copy of %v",
			scope.kept, kept)
	}
	if want := []copyUnknown{{partition: unknown, err: stale}}; !slices.Equal(scope.unknown, want) {
		t.Errorf("the loop names %v unknown, want the one whose copy could not be "+
			"told, with why: %v", scope.unknown, want)
	}
	if !scope.unsettled {
		t.Error("a pass with a partition joining and one unknown is settled: the loop " +
			"would sleep its whole interval before either")
	}

	for name, c := range map[string]struct {
		copies partitionAnswers
		want   bool
	}{
		"a partition joining": {want: true, copies: partitionAnswers{
			kept: {yes: true}, joining: {}, unknown: {yes: true}}},
		"a partition whose copy is unknown": {want: true, copies: partitionAnswers{
			kept: {yes: true}, joining: {yes: true}, unknown: {err: stale}}},
		"every partition kept": {copies: partitionAnswers{
			kept: {yes: true}, joining: {yes: true}, unknown: {yes: true}}},
	} {
		if got := scopeOf(c.copies).unsettled; got != c.want {
			t.Errorf("%s: the pass is unsettled %v, want %v", name, got, c.want)
		}
	}
}

// THE DONOR OFFERS WHAT THIS NODE KEEPS, NOT WHAT IT MAY WRITE.
//
// A barred machine back with its files serves no writes from the copy it
// keeps, and that copy may be its partition's only one. A donor that answered
// only for the partitions its node serves left that partition's joiner nothing
// to fetch — the partition never had a serving holder again, and a readmission
// of that very machine, which has to reach the partition's log through one,
// could never be written. The node here keeps one copy, and its donor is told
// nothing of its write standing.
func TestTheDonorOffersTheCopiesThisNodeKeeps(t *testing.T) {
	t.Parallel()
	layout := partitionedTestLayout()
	parts := layout.Partitions()
	kept, other := parts[0], parts[1]
	s := &stateLog{layout: layout, nodeID: "node-x", copies: statelog.KeepsOnly(kept)}
	deps := s.donorDeps(t.TempDir(), func(context.Context) (*nats.Conn, error) {
		return nil, errors.New("no transfer in this case")
	})
	if _, err := statelog.NewDonor(deps); err != nil {
		t.Fatalf("the donor's deps are refused: %v", err)
	}
	for p, want := range map[statelog.PartitionID]bool{kept: true, other: false} {
		got, err := deps.Keeps(p)
		if err != nil || got != want {
			t.Errorf("the donor of a node keeping one copy answers %s (%v, %v), want "+
				"%v: it donates what it keeps a copy of", p, got, err, want)
		}
	}
}

// AN UNKNOWN COPY IS SAID WHEN IT BEGINS AND WHEN IT ENDS, NOT ON EVERY PASS.
//
// The loop asks again every thirty seconds while whether a partition's copy is
// kept is withheld, and a stale estate view withholds every partition at once:
// a warning per pass was a line per held partition every thirty seconds for
// the whole outage. So two passes that cannot tell about the same partition
// warn once, the pass that can again says so once at info — whether it keeps
// the copy or not — a pass after that says nothing, and the partition withheld
// again later is a new warning.
func TestAnUnknownCopyIsSaidWhenItChangesNotEveryPass(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 3}
	q := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 5}
	stale := errors.New("the estate view is stale")
	both := snapshotScope{unknown: []copyUnknown{{p, stale}, {q, stale}}, unsettled: true}
	recovered := snapshotScope{kept: []statelog.PartitionID{p}, unsettled: true}

	lines := func() []map[string]any {
		var got []map[string]any
		for line := range strings.Lines(out.String()) {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("a log line that is not JSON: %q", line)
			}
			got = append(got, rec)
		}
		out.Reset()
		return got
	}
	said := func(pass string, want ...string) {
		t.Helper()
		var got []string
		for _, rec := range lines() {
			got = append(got, fmt.Sprintf("%s %s %s", rec["level"], rec["msg"], rec["partition"]))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s said %q, want %q", pass, got, want)
		}
	}

	var was map[statelog.PartitionID]struct{}
	was = reportCopies(t.Context(), logger, was, both)
	said("the first pass that cannot tell",
		"WARN statelog_snapshot_copy_unknown tracker.003",
		"WARN statelog_snapshot_copy_unknown tracker.005")
	for range 3 {
		was = reportCopies(t.Context(), logger, was, both)
	}
	said("three more passes that cannot tell")
	was = reportCopies(t.Context(), logger, was, recovered)
	said("the pass that can tell again",
		"INFO statelog_snapshot_copy_known tracker.003",
		"INFO statelog_snapshot_copy_known tracker.005")
	was = reportCopies(t.Context(), logger, was, recovered)
	said("a settled pass after it")
	reportCopies(t.Context(), logger, was, both)
	said("the copy withheld again",
		"WARN statelog_snapshot_copy_unknown tracker.003",
		"WARN statelog_snapshot_copy_unknown tracker.005")
}

// THE LOOP CARRIES WHAT ONE PASS COULD NOT TELL INTO THE NEXT.
//
// [reportCopies] says a transition only against the passes before it, and
// that is the LOOP's to hand it: the case above drives the function, and a
// loop that dropped the answer — or kept it only inside one pass — compiled,
// passed it, and warned on every pass of a coordination outage again. So this
// runs the loop itself, its retry short, over a copy withheld for four
// passes and then told: one warning and one line saying it is known again,
// however many passes either state lasted.
//
// NOT PARALLEL: the loop logs through the package's own logger, which follows
// the process-wide one this swaps — and a parallel case's loop logging the
// same partition would be counted here, which is why the partition is one no
// other case uses.
func TestTheSnapshotLoopWarnsOfAnUnknownCopyOnce(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 47}
	const withheld = 4
	var passes atomic.Int64
	ctx, stop := context.WithCancel(t.Context())
	s := &stateLog{run: ctx, snapshotNudge: make(chan struct{}, 1)}
	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Engine{}).snapshotLoop(s, snapshotPlan{
			// WITHHELD, THEN TOLD — and told NOT kept, so every pass
			// stays unsettled and the loop keeps asking at the retry.
			kept: func() snapshotScope {
				if passes.Add(1) <= withheld {
					return snapshotScope{unsettled: true, unknown: []copyUnknown{
						{partition: p, err: coord.ErrUnavailable}}}
				}
				return snapshotScope{unsettled: true}
			},
			dir: func(statelog.PartitionID) string { return dir },
			taker: func(statelog.PartitionID) (snapshotTaker, error) {
				return nil, errors.New("no partition is kept, so none is taken")
			},
			retry: 5 * time.Millisecond,
		}, 24*time.Hour)
	}()
	// TWO PASSES PAST THE ONE THAT TOLD, so a loop that forgot what it had
	// said would have said it again by now — and the loop is stopped and
	// waited for before a line is read, so every pass's report is written.
	waitUntil(t, 10*time.Second, "the snapshot loop's passes",
		func() bool { return passes.Load() >= withheld+3 })
	stop()
	<-done

	said := map[string]int{}
	for _, record := range logs.records(t) {
		if record["partition"] == p.String() {
			said[fmt.Sprint(record["msg"])]++
		}
	}
	want := map[string]int{
		"statelog_snapshot_copy_unknown": 1,
		"statelog_snapshot_copy_known":   1,
	}
	if !maps.Equal(said, want) {
		t.Errorf("%d passes withheld and %d told said %v about %s, want %v — the "+
			"loop has to carry each pass's unknown partitions into the next",
			withheld, passes.Load()-withheld, said, p, want)
	}
}

// partitionAnswers answers each partition as it is told to — as the copies a
// node keeps — and a partition it was told nothing of as no.
type partitionAnswers map[statelog.PartitionID]struct {
	yes bool
	err error
}

func (h partitionAnswers) Keeps(p statelog.PartitionID) (bool, error) {
	a := h[p]
	return a.yes, a.err
}

// A PARTITION WHOSE COPY IS UNKNOWN KEEPS ITS REPORT, and one whose copy this
// node does not keep loses it: not knowing for a pass whether this node keeps a
// partition's copy is not having let it go, and a report dropped on that read
// stopped the row naming an artefact still on the node's disk.
func TestAPartitionWhoseCopyIsUnknownKeepsItsReport(t *testing.T) {
	t.Parallel()
	p := statelog.PartitionID{Space: statelog.SpaceTracker}
	s := &stateLog{}
	s.holdSnapshot(p, snapshotHeld{Have: true})
	s.keepSnapshotsOf(snapshotScope{unknown: []copyUnknown{{partition: p, err: coord.ErrUnavailable}},
		unsettled: true})
	if _, held := s.snapshotOf(p); !held {
		t.Fatal("a partition whose copy is unknown for a pass lost its report")
	}
	s.keepSnapshotsOf(snapshotScope{unsettled: true})
	if _, held := s.snapshotOf(p); held {
		t.Error("a partition whose copy this node does not keep kept its report")
	}
}

// A DIVIDED LAYOUT'S ROW REPORTS EACH PARTITION'S ARTEFACT IN ITS OWN REPORT.
//
// A partitioned layout's row carries a report per partition it holds, and each
// artefact's size and skip land in its partition's report — never on the row,
// which is layout 0's one partition's place — with each log's artefact
// position on the log's own entry. A partition the row does not report, one
// this node no longer runs a log of, is written nowhere.
func TestADividedRowReportsEachPartitionsArtefact(t *testing.T) {
	t.Parallel()
	p0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	p1 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	taken := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	row := coord.NodePositions{
		NodeID: "node-a", Layout: 1,
		Domains: map[string]coord.DomainPosition{"tracker@tracker.000": {Seq: 9, Generation: 1}},
		Partitions: map[string]coord.PartitionReport{
			p0.String(): {State: "serving"},
		},
	}
	stampSnapshot(&row, heldSnapshots{
		p0: {Have: true, Manifest: statelog.Manifest{TakenAt: taken, Bytes: 10,
			Domains: map[string]statelog.DomainPosition{"tracker@tracker.000": {Seq: 5, Generation: 1}}}},
		p1: {Skip: statelog.SkipLagging},
	})
	if err := row.Validate(); err != nil {
		t.Fatalf("the stamped row is not one a node may write: %v", err)
	}
	if got := row.Partitions[p0.String()]; got.SnapshotBytes != 10 || got.State != "serving" {
		t.Errorf("tracker.000's report is %+v, want its artefact's 10 bytes beside its state", got)
	}
	if _, reported := row.Partitions[p1.String()]; reported {
		t.Error("a partition the row does not report was added to it")
	}
	if got := row.Domains["tracker@tracker.000"]; got.SnapshotSeq != 5 || !got.SnapshotAt.Equal(taken) {
		t.Errorf("the log's artefact position is %d at %s, want 5 at %s", got.SnapshotSeq,
			got.SnapshotAt, taken)
	}
}

// AN ARTEFACT OF ANOTHER PARTITION IS NOT HELD.
//
// A partition's directory is where its artefacts are, but what a node holds of
// a partition is only an artefact naming that partition: one of another — a
// directory an operator moved, a layout's leftovers — is no copy a joiner of
// this partition can adopt.
func TestAnArtefactOfAnotherPartitionIsNotHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestSnapshot(t, dir, statelog.Manifest{
		Layout: 1, Partition: "tracker.007",
		TakenAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		Domains: map[string]statelog.DomainPosition{"tracker@tracker.007": {Seq: 120, Generation: 3}},
	})
	if _, found := newestSnapshot(dir, 0, statelog.EstatePartition); found {
		t.Error("an artefact of tracker.007 was held as the estate's")
	}
	if _, found := newestSnapshot(dir, 2, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}); found {
		t.Error("an artefact of layout 1's tracker.007 was held as layout 2's")
	}
	if _, found := newestSnapshot(dir, 1, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}); !found {
		t.Error("tracker.007's own artefact was not held")
	}
}

// A SNAPSHOT'S RECIPIENTS ARE THE COUNTED NODES OTHER THAN THIS ONE — and this
// one need not be counted at all.
//
// A machine an eviction barred, back with its files, is not counted on the logs
// its eviction gates once the fence window has passed, and the copy it keeps
// may be its partition's only one. Judged as "fewer than two counted", the one
// joiner beside it read as nobody to donate to: no artefact was taken, and the
// joiner waited on a donor that never had one. So the count subtracts this node
// only where it is counted: one joiner is one recipient whether or not the node
// asking is counted beside it, and a node counted alone has none.
func TestASnapshotsRecipientsAreTheCountedNodesOtherThanThisOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{"node-j": {"data", "seats"}})
	view, err := coord.NewLeaseView(backend, coord.ClassNode,
		coord.ViewOptions{Every: time.Hour, Trust: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = view.Run(runCtx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	e := &Engine{backends: &Backends{Coord: backend}, dataView: view}
	fleet := coordmem.NewFleet()
	for node, want := range map[string]int{
		"node-x": 1, // counted on nothing: its one recipient is the joiner
		"node-j": 0, // the joiner itself, counted alone: nobody to donate to
	} {
		s := &stateLog{layout: LayoutZero(), fleet: fleet, nodeID: node}
		waitUntil(t, 10*time.Second, "the presence view to name node-j", func() bool {
			got, err := e.recipientsOn(ctx, s, statelog.EstatePartition, time.Now())
			return err == nil && got == want
		})
	}
}
