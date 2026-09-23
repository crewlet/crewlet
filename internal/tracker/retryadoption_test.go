package tracker_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE THAT ADOPTED A SNAPSHOT JUDGES A WRITE BY THE LEDGER THAT TRAVELLED
// WITH IT — through a real snapshot, a real transfer, a real install, and the
// real writer, publisher and ledger on the node that adopted.
//
// The adopter holds a row for every operation its donor applied, so a retry of
// one — a turn re-run under its derived operation id, a caller repeating an
// `unknown` — is answered from it with the first copy's position and never
// published again. And an operation neither ledger holds never applied, so a
// write whose id was minted before the join — the FIRST attempt of a turn
// woken by a trigger from before it, which derives its ids from that trigger's
// instant — is decided and published like anyone's. With the ledger scrubbed
// out of the snapshot, both were answered `unknown`: the second is a
// recovering node refusing its own backlog.
func TestANodeThatAdoptedJudgesAWriteByTheLedgerThatTravelled(t *testing.T) {
	t.Parallel()
	donor := newRoundTrip(t)
	donor.applyWhileWriting()
	created := donor.createTask("who owns the rollback")
	minted := time.Now().Add(-time.Hour)
	landed := statelog.NewOpID(minted, "comment-"+created.ID)
	first, err := commentOn(donor, created.ID, landed, "cm-1")
	if err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the donor's application = (%+v, %v), want applied", first.Result, err)
	}
	donor.drain()

	joiner := adoptFrom(t, donor, tracker.Domain{})
	joiner.applyWhileWriting()
	end := joiner.logEnd(t)

	// THE RETRY, answered from the ledger that travelled.
	retry, err := commentOn(joiner, created.ID, landed, "cm-1")
	if err != nil {
		t.Fatalf("the retry on the node that adopted: %v", err)
	}
	if retry.Outcome != statelog.OutcomeApplied || !retry.Collapsed ||
		retry.Position != first.Position {
		t.Fatalf("the retry = %+v, want applied at the donor's %s, answered and "+
			"not decided", retry.Result, first.Position)
	}
	if got := joiner.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log — a second application of "+
			"an operation the donor's ledger holds", got-end)
	}

	// THE FIRST ATTEMPT minted before the join, published.
	fresh, err := commentOn(joiner, created.ID,
		statelog.NewOpID(minted, "comment-"+created.ID+"-2"), "cm-2")
	if err != nil {
		t.Fatalf("a first attempt minted before the join: %v", err)
	}
	if fresh.Outcome != statelog.OutcomeApplied || fresh.Collapsed {
		t.Fatalf("a first attempt minted before the join = %+v, want its own "+
			"record applied — no ledger holds it, so it never applied, and "+
			"answering it `unknown` is the node refusing its own backlog",
			fresh.Result)
	}
	if got := joiner.logEnd(t); got != end+1 {
		t.Fatalf("the log grew by %d, want the first attempt's one record", got-end)
	}
}

// A NODE THAT ADOPTED FROM A DONOR THAT SCRUBBED ITS LEDGER DOES NOT APPLY A
// RETRY A SECOND TIME — which is what a rolling upgrade puts in front of it: a
// peer on a build from before the ledger travelled.
//
// Its artefact holds none of the ledger's rows, and its manifest says so. The
// joiner writes the join's own start into the artefact as the ledger's
// watermark before installing it, so an operation minted before the join —
// a retry of one the donor applied, and, unavoidably, a first attempt too — is
// answered `unknown` rather than decided against rows that may already hold
// it. An operation minted after the join is judged by its row as ever.
func TestANodeThatAdoptedFromAScrubbingDonorDoesNotApplyARetryTwice(t *testing.T) {
	t.Parallel()
	donor := newRoundTrip(t)
	donor.applyWhileWriting()
	created := donor.createTask("who owns the rollback")
	landed := statelog.NewOpID(time.Now().Add(-time.Hour), "comment-"+created.ID)
	if first, err := commentOn(donor, created.ID, landed, "cm-1"); err != nil ||
		first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the donor's application = (%+v, %v), want applied", first.Result, err)
	}
	donor.drain()

	joiner := adoptFrom(t, donor, scrubbingTracker{})
	joiner.applyWhileWriting()
	end := joiner.logEnd(t)

	retry, err := commentOn(joiner, created.ID, landed, "cm-1")
	if err != nil {
		t.Fatalf("the retry: %v — an operation this node cannot vouch for is "+
			"answered, not refused as a broken applier", err)
	}
	if retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry answered %q, want unknown — the donor scrubbed the "+
			"ledger that would say whether the first application landed, and "+
			"deciding again applies it twice", retry.Outcome)
	}
	if got := joiner.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log", got-end)
	}
	detail, err := joiner.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(detail.Comments) != 1 {
		t.Fatalf("the thread holds %d comments, want the one", len(detail.Comments))
	}

	// AND THE CONTROL: minted after the join, judged by its row.
	after, err := commentOn(joiner, created.ID,
		statelog.NewOpID(time.Now(), "comment-"+created.ID+"-2"), "cm-2")
	if err != nil || after.Outcome != statelog.OutcomeApplied {
		t.Fatalf("an operation minted after the join = (%+v, %v), want applied",
			after.Result, err)
	}
}

// scrubbingTracker is the tracker domain as a build from before the ledger
// travelled declared it: its operation ledger classed as this node's own, and
// so scrubbed out of every snapshot it takes.
type scrubbingTracker struct{ tracker.Domain }

func (scrubbingTracker) Tables() map[string]statelog.TableClass {
	tables := tracker.Domain{}.Tables()
	tables[tracker.Domain{}.OpsTable()] = statelog.Local
	return tables
}

// commentOn posts one comment on a task under op, as ana.
func commentOn(r *roundTrip, taskID, op, id string) (tracker.WriteResult, error) {
	return r.writer.UpdateTask(r.t.Context(), op, taskID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: id, Task: taskID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "I do.",
			CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil)
}

// adoptFrom is a second node joining the donor's log by adopting its snapshot:
// the donor takes one as the domain `declared` declares its tables — this
// build's, or an older build's that scrubbed the ledger — and serves it; a
// fresh store adopts it through the real join; and the harness's node is built
// over what arrived, resuming at the artefact's position.
func adoptFrom(t *testing.T, donor *roundTrip, declared statelog.Domain) *roundTrip {
	t.Helper()
	stream := tracker.Domain{}.Stream().Name
	at, _, _, err := statelog.CursorFor(t.Context(), donor.db.Replicated(), stream)
	if err != nil {
		t.Fatalf("read the donor's checkpoint: %v", err)
	}
	lag := uint64(0)
	dir := t.TempDir()
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain: declared,
			Health: func() statelog.Health {
				return statelog.Health{Position: at, Drained: true, Lag: &lag}
			},
		}},
		DB: donor.db, Dir: dir, NodeID: "node-a", EngineVersion: "v0.0.0-test",
		Counted:  func(context.Context) (int, error) { return 3, nil },
		Interval: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	manifest, err := snapper.Take(t.Context())
	if err != nil {
		t.Fatalf("the donor's snapshot: %v", err)
	}
	server, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "node-a",
		Dial: func(context.Context) (*nats.Conn, error) {
			return donor.broker.Conn(), nil
		},
		Newest: func() (statelog.Manifest, bool) { return manifest, true },
		Path:   func(statelog.Manifest) string { return filepath.Join(dir, manifest.Artifact) },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })

	nodePath := filepath.Join(t.TempDir(), "node.db")
	joiner, err := store.Open(t.Context(), nodePath, store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the joiner's store: %v", err)
	}
	t.Cleanup(func() { _ = joiner.Close() })
	adopter, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  map[string]statelog.Registered{"tracker": {Domain: tracker.Domain{}}},
		LivePath: joiner.ReplicatedPath(),
		NodeID:   "node-b",
		Conn:     donor.broker.Conn(),
		Need: func(context.Context) (statelog.OfferRequest, error) {
			return statelog.OfferRequest{Need: map[string]uint64{"tracker": at.Seq}}, nil
		},
		Hold: func(context.Context, map[string]uint64) (func(), error) {
			return func() {}, nil
		},
		Close: func(context.Context) error { return joiner.Close() },
		Reopen: func(ctx context.Context) error {
			reopened, err := store.Open(ctx, nodePath, store.Options{PinnedWriters: 1})
			if err != nil {
				return err
			}
			joiner = reopened
			return nil
		},
		Record: func(ctx context.Context, began time.Time, from string,
			m statelog.Manifest, phase statelog.AdoptionPhase) error {
			return statelog.RecordAdoption(ctx, joiner, began, from, m, phase)
		},
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	// THE DONOR MAY NOT BE LISTENING YET: its serve loop starts on its own
	// goroutine, and a join that asked first would find no offer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = adopter.Join(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the join: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	r := newRoundTripOn(t, donor.broker, donor.log, joiner, "node-b")
	r.consumed = at.Seq
	r.waiter.reach(at)
	return r
}

// A WRITE RETRIED AFTER ITS LEDGER ROW WAS SWEPT IS NOT APPLIED A SECOND TIME,
// through the real writer, the real publisher, the real ledger and the real
// sweep.
//
// The ledger keeps a row for thirty days, and a retry is judged by it — so a
// retry of an operation whose row the sweep deleted used to find the same
// silence as an operation that never ran, decide again on rows that already
// held the first application, and publish a second copy. The sweep now
// records how far back it forgot, and the publisher reads that before it
// trusts the silence.
func TestAWriteRetriedAfterItsLedgerRowWasSweptIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	created := r.createTask("who owns the rollback")

	comment := func(op, id string) (tracker.WriteResult, error) {
		return r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: id, Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman, Body: "I do.",
				CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
	}
	op := statelog.NewOpID(time.Now().Add(-time.Hour), "comment-"+created.ID)
	if first, err := comment(op, "cm-1"); err != nil || first.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the first application = (%+v, %v), want applied", first.Result, err)
	}
	r.drain()

	// THE SWEEP, with a cutoff past the row — the arithmetic of a month
	// passing, done by the job that runs it.
	sweep, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: tracker.Domain{}, Applier: r.applier, Fetch: noFetch{}, Log: r.log,
		DB: r.db.Replicated(),
	})
	if err != nil {
		t.Fatalf("build the ledger's owner: %v", err)
	}
	if n, err := sweep.PurgeOps(t.Context(), time.Now()); err != nil || n == 0 {
		t.Fatalf("the sweep = (%d, %v), want the ledger's rows", n, err)
	}
	end := r.logEnd(t)

	retry, err := comment(op, "cm-1")
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if retry.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the retry answered %q, want unknown — the row that would say "+
			"whether the first application landed was swept", retry.Outcome)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log — a second application "+
			"of one operation, applied by every node", got-end)
	}

	// AND THE CONTROL: an operation minted after the sweep's cutoff is
	// judged by its row as ever, or the sweep would refuse every write.
	fresh, err := comment(statelog.NewOpID(time.Now(), "comment-"+created.ID), "cm-2")
	if err != nil || fresh.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a fresh operation after the sweep = (%+v, %v), want applied",
			fresh.Result, err)
	}
}

// noFetch is a log nothing is pulled from: the sweep's owner is built only to
// sweep.
type noFetch struct{}

func (noFetch) Fetch(context.Context, int, int, time.Duration) ([]statelog.Message, error) {
	return nil, nil
}

func (noFetch) Pending(context.Context) (uint64, error) { return 0, nil }
