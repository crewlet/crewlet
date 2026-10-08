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

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
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

// A DOMAIN THE ARTEFACT NAMES AND THIS NODE DOES NOT RUN GETS NO ROW — one a
// newer build of this node registered before a rollback.
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
			"newer":   {Seq: 5, Generation: 1},
		}},
	})
	if _, invented := row.Domains["newer"]; invented {
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
//
// THE MANIFEST NAMES ITS FILE, as a take's does: written before the name was
// set, it named none, and the bytes check passed on the directory itself.
func writeTestSnapshot(t *testing.T, dir string, m statelog.Manifest) {
	t.Helper()
	m.V = statelog.ManifestVersion
	var newest uint64
	for _, at := range m.Domains {
		if at.Seq > newest {
			newest = at.Seq
		}
	}
	base := filepath.Join(dir, fmt.Sprintf("snapshot-%d", newest))
	m.Artifact = filepath.Base(base) + ".db"
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
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
//
// THE DECLINE IS `deferred`, a skip the retry waits out flat at thirty
// seconds. A boot skip retries a second later ([snapshotWaits.after]), and a
// nudged tick could not be told from that one.
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
				Reason: statelog.SkipDeferred, Detail: "a newer peer's record"}
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
				(&Engine{}).snapshotLoop(s, taker, t.TempDir(), snapshotWaitsFor(24*time.Hour))
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
			if s.snapshot.Load() == nil {
				t.Fatal("the nudged tick published nothing to the register row")
			}
		})
	}
}

// A SKIP THAT CLEARS AS A BOOT SETTLES IS RETRIED SOON, AND LESS OFTEN EACH
// TIME.
//
// `unhydrated`, `sole_node` and `lagging` each end seconds into a healthy boot,
// and a flat thirty-second retry put a restarted node's first artefact — and
// the donor the fleet counts with it — half a minute after it could have been
// taken. Doubling, so a node that stays in one for its whole life (a single
// node is `sole_node` for ever) settles on the ceiling rather than asking
// every second. Each gap is held to at least the wait it was scheduled for —
// a timer never fires early, so the lower bound is the one that cannot flake.
func TestABootSkipIsRetriedOnADoublingWait(t *testing.T) {
	t.Parallel()
	waits := snapshotWaits{Interval: 24 * time.Hour,
		FirstRetry: 50 * time.Millisecond, Retry: 1600 * time.Millisecond}
	ctx, stop := context.WithCancel(t.Context())
	s := &stateLog{run: ctx, snapshotNudge: make(chan struct{}, 1)}
	// EACH ATTEMPT STAMPED WHEN IT STARTS, inside the loop's own call: the
	// gap between two starts is the wait between them plus the work of the
	// first, never less, however late this goroutine is scheduled.
	started := make(chan time.Time, 16)
	taker := &countingTaker{took: make(chan struct{}, 16), take: func() (statelog.Manifest, error) {
		started <- time.Now()
		return statelog.Manifest{}, &statelog.ErrSkipped{Reason: statelog.SkipSoleNode}
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Engine{}).snapshotLoop(s, taker, t.TempDir(), waits)
	}()
	t.Cleanup(func() { stop(); <-done })

	taker.await(t, "the boot's tick")
	last := <-started
	for i, want := range []time.Duration{50, 100, 200, 400, 800} {
		taker.await(t, fmt.Sprintf("retry %d", i+1))
		at := <-started
		gap := at.Sub(last)
		last = at
		if want *= time.Millisecond; gap < want {
			t.Fatalf("retry %d came %v after the attempt before it, want at "+
				"least %v: the retry does not wait out its doubling", i+1, gap, want)
		}
		// AND SOON AT FIRST: a retry that waited the ceiling from the start
		// is the flat half-minute this replaced.
		if i == 0 && gap >= waits.Retry/2 {
			t.Fatalf("the first retry came %v after the boot's tick, want well "+
				"inside the %v ceiling: a boot skip is retried soon", gap, waits.Retry)
		}
	}
}

// EVERY ANSWER A TICK CAN GIVE WAITS AS LONG AS ITS NEXT ANSWER IS AWAY.
//
// The schedule is the whole of what makes a restarted node a donor promptly
// and an idle one quiet, so it is pinned answer by answer, over the
// production values.
func TestEachSnapshotAnswerWaitsUntilItCanChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	day := snapshotWaitsFor(24 * time.Hour)
	skip := func(r statelog.SkipReason) error { return &statelog.ErrSkipped{Reason: r} }
	holding := func(age time.Duration) snapshotHeld {
		return snapshotHeld{Have: true, Manifest: statelog.Manifest{TakenAt: now.Add(-age)}}
	}
	for _, c := range []struct {
		name       string
		waits      snapshotWaits
		err        error
		held       snapshotHeld
		streak     int
		want       time.Duration
		wantStreak int
	}{
		{"a taken snapshot waits the interval", day, nil, holding(0), 3,
			24 * time.Hour, 0},
		{"a boot skip's first retry is a second", day, skip(statelog.SkipUnhydrated),
			snapshotHeld{}, 0, time.Second, 1},
		{"each boot skip in a row doubles it", day, skip(statelog.SkipSoleNode),
			snapshotHeld{}, 3, 8 * time.Second, 4},
		{"a lagging node backs off as a booting one does", day, skip(statelog.SkipLagging),
			snapshotHeld{}, 1, 2 * time.Second, 2},
		{"the doubling settles on the ceiling", day, skip(statelog.SkipSoleNode),
			snapshotHeld{}, 40, snapshotSkipRetry, 41},
		{"a skip that waits on an operator retries flat", day,
			skip(statelog.SkipInsufficientSpace), snapshotHeld{}, 4, snapshotSkipRetry, 0},
		{"a node holding a record it cannot read retries flat", day,
			skip(statelog.SkipDeferred), snapshotHeld{}, 0, snapshotSkipRetry, 0},
		{"a take that failed retries flat", day, errors.New("read-only file system"),
			snapshotHeld{}, 2, snapshotSkipRetry, 0},
		{"a recent artefact waits until it is stale", day, skip(statelog.SkipRecent),
			holding(20 * time.Hour), 0, 4 * time.Hour, 0},
		{"one just past stale is asked again soon", day, skip(statelog.SkipRecent),
			holding(24*time.Hour + time.Minute), 0, time.Second, 0},
		{"one stamped in the future waits no longer than the interval", day,
			skip(statelog.SkipRecent), holding(-time.Hour), 0, 24 * time.Hour, 0},
		{"no retry outlasts a short interval",
			snapshotWaitsFor(10 * time.Second), skip(statelog.SkipDeferred),
			snapshotHeld{}, 0, 10 * time.Second, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, streak := c.waits.after(c.err, c.held, c.streak, now)
			if got != c.want || streak != c.wantStreak {
				t.Errorf("after(%v) = (%v, streak %d), want (%v, streak %d)",
					c.err, got, streak, c.want, c.wantStreak)
			}
		})
	}
}

// A TAKEN SNAPSHOT IS PUBLISHED AT ONCE, NOT A HEARTBEAT LATER.
//
// The register row naming the artefact is what makes this node a donor the
// trim and a joiner count, and the heartbeat's next beat is up to ten seconds
// away; so a take wakes it, and a skip — which changes no artefact — does not.
func TestATakenSnapshotWakesTheHeartbeat(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		take  func() (statelog.Manifest, error)
		wakes bool
	}{
		{"a take", func() (statelog.Manifest, error) {
			return statelog.Manifest{TakenAt: time.Now().UTC(), NodeID: "node-a"}, nil
		}, true},
		{"a skip", func() (statelog.Manifest, error) {
			return statelog.Manifest{}, &statelog.ErrSkipped{Reason: statelog.SkipDeferred}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, stop := context.WithCancel(t.Context())
			s := &stateLog{run: ctx, snapshotNudge: make(chan struct{}, 1),
				heartbeatNudge: make(chan struct{}, 1)}
			taker := &countingTaker{took: make(chan struct{}, 8), take: c.take}
			done := make(chan struct{})
			go func() {
				defer close(done)
				(&Engine{}).snapshotLoop(s, taker, t.TempDir(), snapshotWaitsFor(24*time.Hour))
			}()
			t.Cleanup(func() { stop(); <-done })
			taker.await(t, "the boot's tick")
			select {
			case <-s.heartbeatNudge:
				if !c.wakes {
					t.Fatal("a tick that took nothing woke the heartbeat")
				}
			case <-time.After(2 * time.Second):
				if c.wakes {
					t.Fatal("a taken snapshot left its register row to the next beat")
				}
			}
		})
	}
}

// THE HEARTBEAT PUBLISHES AT ONCE, AGAIN ON EVERY TICK, AND ON A NUDGE.
//
// Every tick is what notices a RUNNING node fall below the floor or behind a
// peer's reanchor ([stateLog.publishPositions]), so the loop that re-invokes
// the publish is pinned here under an interval a test can wait out — the
// cases that drive the publish directly certify what one publish does, and
// none of them would notice a heartbeat that published once and stopped.
func TestTheHeartbeatPublishesOnEveryTickAndNudge(t *testing.T) {
	t.Parallel()
	t.Run("on every tick", func(t *testing.T) {
		t.Parallel()
		published := beating(t, &stateLog{}, 50*time.Millisecond)
		for i := range 4 {
			awaitPublish(t, published, fmt.Sprintf("publish %d", i+1))
		}
	})
	t.Run("on a nudge", func(t *testing.T) {
		t.Parallel()
		s := &stateLog{heartbeatNudge: make(chan struct{}, 1)}
		published := beating(t, s, time.Hour)
		awaitPublish(t, published, "the heartbeat's first publish")
		s.nudgeHeartbeat()
		awaitPublish(t, published, "the publish a nudge asked for, with the tick an hour away")
	})
}

// beating runs s's heartbeat loop on interval for the rest of the test, over a
// publish that reports each call.
func beating(t *testing.T, s *stateLog, interval time.Duration) <-chan struct{} {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	published := make(chan struct{}, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.beat(ctx, interval, func(context.Context) { published <- struct{}{} })
	}()
	t.Cleanup(func() { stop(); <-done })
	return published
}

// awaitPublish waits for the heartbeat's next publish.
func awaitPublish(t *testing.T, published <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatalf("waited 10s for %s: the heartbeat stopped publishing", what)
	}
}

// AN APPLIER'S FIRST DRAIN WAKES THE SNAPSHOT LOOP.
//
// `unhydrated` is the commonest boot skip and an applier's first drain is the
// moment it ends, so the engine hands every applier it builds the loop's nudge
// rather than leaving the loop to find out on its retry. Built here through
// the engine's own [stateLog.start], over an empty log: the loop's first fetch
// comes back with nothing, which is the drain.
func TestAnAppliersFirstDrainWakesTheSnapshotLoop(t *testing.T) {
	t.Parallel()
	s, q, appendTo := aProvisionedTrackerLog(t)
	s.snapshotNudge = make(chan struct{}, 1)
	running, err := s.start(t.Context(), t.Context(), q, tracker.Domain{}, appendTo, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errs := make(chan error, 1)
	go func() { errs <- running.runner.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-errs })
	select {
	case <-s.snapshotNudge:
	case <-time.After(15 * time.Second):
		t.Fatalf("the applier drained (%v) and the snapshot loop was not woken",
			running.runner.Drained())
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
// took, so the only thing that can run it again inside the test is a nudge
// the event gives it: its own, or the first drain of an applier it relaunched
// ([stateLog.snapshotNudge]) — two paths to one wake, and this fails only when
// the event takes neither.
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
// interval — a day — behind a snapshot it has just taken, and returns what the
// loop last published. From then on nothing but a nudge runs the loop again.
//
// A PEER IS COUNTED so a tick can take one — a row naming the tracker's log,
// which is what the trim counts it on: a node alone declines as `sole_node`
// and keeps retrying, which would put a tick of its own inside any window a
// test watched.
//
// AFTER THE BOOT'S OWN TICK, and then nudged, so the take comes at once
// rather than on the boot skip's retry. Before that tick has concluded a nudge
// could land while it was still declining, which wastes it. Once one has, the
// take comes from the nudge or from the retry, whichever is first; a nudge
// that arrived while that take was running runs one more tick at once, which
// declines as `recent` and waits until the artefact is stale — a day — like
// the take does ([snapshotWaits.after]). So whichever tick was last, the loop
// is parked behind the interval, and what this returns is the value it parked
// behind: it waits for the nudge to have been taken and the value to hold.
func parkSnapshotLoop(t *testing.T, e *Engine) *snapshotHeld {
	t.Helper()
	s := e.native.Load().log
	waitUntil(t, 20*time.Second, "the boot's snapshot tick", func() bool {
		return s.snapshot.Load() != nil
	})
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
	s.nudgeSnapshot()
	waitUntil(t, 20*time.Second, "the snapshot loop to take one", func() bool {
		held := s.snapshot.Load()
		return held != nil && held.Have && held.Skip == "" &&
			!held.Manifest.TakenAt.Before(counted)
	})
	// SETTLED: no nudge left to take, and the value unchanged across a pause
	// longer than a tick that declines as `recent` — a few reads — takes.
	var parked *snapshotHeld
	waitUntil(t, 20*time.Second, "the snapshot loop to park", func() bool {
		held := s.snapshot.Load()
		if len(s.snapshotNudge) > 0 {
			return false
		}
		time.Sleep(250 * time.Millisecond)
		if len(s.snapshotNudge) > 0 || s.snapshot.Load() != held {
			return false
		}
		parked = held
		return true
	})
	return parked
}

// THE DONOR OFFERS THE NEWEST ARTEFACT IN THE SNAPSHOT ROOT, BY THE NAME ITS
// MANIFEST CARRIES.
//
// Every data node keeps the one estate, so the donor has one thing to offer:
// the newest complete artefact in the directory its loop writes to, never one
// whose bytes are gone, and fetched from the file the manifest names rather
// than a name derived beside it. And it is told nothing of this node's write
// standing: a barred machine back with its files serves no writes from the
// copy it keeps, and that copy may be the fleet's only one.
func TestTheDonorOffersTheNewestArtefactInTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := &stateLog{nodeID: "node-x"}
	deps := s.donorDeps(root, func(context.Context) (*nats.Conn, error) {
		return nil, errors.New("no transfer in this case")
	})
	if _, err := statelog.NewDonor(deps); err != nil {
		t.Fatalf("the donor's deps are refused: %v", err)
	}
	if m, found := deps.Newest(); found {
		t.Fatalf("an empty root offers %+v", m)
	}
	writeTestSnapshot(t, root, statelog.Manifest{
		TakenAt: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
		Domains: map[string]statelog.DomainPosition{"tracker": {Seq: 120, Generation: 3}},
	})
	writeTestSnapshot(t, root, statelog.Manifest{
		TakenAt: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		Domains: map[string]statelog.DomainPosition{"tracker": {Seq: 140, Generation: 3}},
	})
	m, found := deps.Newest()
	if !found || m.Domains["tracker"].Seq != 140 {
		t.Fatalf("the donor offers (%+v, %v), want the newest artefact", m, found)
	}
	if got, want := deps.Path(m), filepath.Join(root, m.Artifact); got != want {
		t.Errorf("the donor streams %s, want the file its manifest names: %s", got, want)
	}
	// AN ARTEFACT WHOSE BYTES ARE GONE IS NOT OFFERED: a joiner choosing it
	// over every other offer would fail after the transfer began.
	if err := os.Remove(deps.Path(m)); err != nil {
		t.Fatal(err)
	}
	if again, found := deps.Newest(); !found || again.Domains["tracker"].Seq != 120 {
		t.Errorf("with the newest artefact's bytes gone the donor offers (%+v, %v), "+
			"want the one before it", again, found)
	}
}

// A SNAPSHOT'S RECIPIENTS ARE THE COUNTED NODES OTHER THAN THIS ONE — and this
// one need not be counted at all.
//
// A machine an eviction barred, back with its files, is not counted on the logs
// its eviction gates once the fence window has passed, and the copy it keeps
// may be the fleet's only one. Judged as "fewer than two counted", the one
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
		s := &stateLog{fleet: fleet, nodeID: node}
		waitUntil(t, 10*time.Second, "the presence view to name node-j", func() bool {
			got, err := e.recipients(ctx, s, time.Now())
			return err == nil && got == want
		})
	}
}
