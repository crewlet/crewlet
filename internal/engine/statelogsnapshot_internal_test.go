package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
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
	stampSnapshot(&row, &snapshotHeld{
		Have: true,
		Manifest: statelog.Manifest{
			TakenAt: taken,
			Bytes:   4 << 20,
			Domains: map[string]statelog.DomainPosition{
				"tracker": {Seq: 850, Generation: 2},
				"pages":   {Seq: 33, Generation: 2},
			},
		},
	})

	if row.SnapshotBytes != 4<<20 {
		t.Errorf("snapshot_bytes = %d, want %d — the fleet screen renders what "+
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
	stampSnapshot(&row, &snapshotHeld{
		Have: true,
		Manifest: statelog.Manifest{Domains: map[string]statelog.DomainPosition{
			"tracker": {Seq: 8, Generation: 1},
			"retired": {Seq: 5, Generation: 1},
		}},
	})
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
			held := heldAfter(statelog.Manifest{}, tc.err, dir)
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
		&statelog.ErrSkipped{Reason: statelog.SkipSoleNode}, t.TempDir())
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
		&statelog.ErrSkipped{Reason: statelog.SkipRecent}, dir)
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
		&statelog.ErrSkipped{Reason: statelog.SkipUnhydrated}, t.TempDir())
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
				(&Engine{}).snapshotLoop(s, taker, t.TempDir(), 24*time.Hour)
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
			if held := s.snapshot.Load(); held == nil {
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
				spec := running.domain.Stream()
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
				if err := back.Store.CloseReplicated(); err != nil {
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
				return e.native.Load().log.snapshot.Load() != parked
			})
		})
	}
}

// parkSnapshotLoop leaves a running node's snapshot loop waiting out its
// interval — a day — behind a snapshot it has just taken, and returns what that
// tick published. From then on nothing but a nudge runs the loop again.
//
// A PEER IS COUNTED so the tick can take one: a node alone declines as
// `sole_node` and retries every thirty seconds, which would put a tick of its
// own inside any window a test watched. And the loop is NOT nudged here: a
// nudge that arrived while a tick was taking would run it again at once, and
// that tick — declining as `recent` — goes back to the thirty-second retry.
func parkSnapshotLoop(t *testing.T, e *Engine) *snapshotHeld {
	t.Helper()
	s := e.native.Load().log
	counted := time.Now().UTC()
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "a-counted-peer", At: counted,
	}); err != nil {
		t.Fatalf("publish a counted peer: %v", err)
	}
	var parked *snapshotHeld
	waitUntil(t, 75*time.Second, "the snapshot loop to take one and park", func() bool {
		held := s.snapshot.Load()
		if held == nil || !held.Have || held.Skip != "" ||
			held.Manifest.TakenAt.Before(counted) {
			return false
		}
		parked = held
		return true
	})
	return parked
}
