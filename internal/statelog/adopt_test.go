package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// joinHarness is a donor holding a real replicated estate and a joiner about
// to replace its own with it.
type joinHarness struct {
	t       *testing.T
	nc      *nats.Conn
	donorDB *store.DB
	joiner  *store.DB
	// joinEstate is the joiner's replicated estate, as the runtime would
	// hand it: resolved through the joiner's node handle on every call, so
	// it reads whatever file the adoption left open.
	joinEstate store.ReplicatedHandle
	joinPath   string
	manifest   statelog.Manifest
	snapPath   string

	// donors is every donor the harness runs, which its adopter names to
	// the join so the collection ends on their answers rather than at the
	// offer window ([statelog.AdoptDeps.Donors]).
	donors []string
	// listDonors, nil by default, is how the adopter lists them instead
	// of answering donors at once: a case that times the listing sets it.
	listDonors func(context.Context) ([]string, error)

	held     atomic.Int64
	released atomic.Int64
	phases   []statelog.AdoptionPhase
	closes   atomic.Int64
	reopens  atomic.Int64

	// holdErr is what every hold reports instead of pinning, nil by
	// default — a register this node cannot write to. holdAsks counts every
	// attempt, a refused one included, which is one per offer adopted.
	holdErr  error
	holdAsks atomic.Int64

	// began is every start Record was handed, one per call.
	began []time.Time
	// record, nil by default, is what Record also does: a case that needs
	// the row really written sets it to [statelog.RecordAdoption].
	record func(ctx context.Context, began time.Time, donor string,
		m statelog.Manifest, phase statelog.AdoptionPhase) error

	// stillUsable is step 7's re-check, nil by default. A case sets it to
	// stage the one thing the hold is a belt against: a fleet that
	// trimmed past the artefact while it was in flight.
	stillUsable func(context.Context, statelog.Manifest) error

	// onClose runs as step 9 closes the live database, nil by default:
	// the last moment before the install, which is where a case stages
	// an interruption the rename must survive.
	onClose func()
	// closeErr is what step 9's close reports AFTER it has closed the live
	// database, nil by default — which is the engine's close exactly: it
	// takes the handle out of service before it closes it, so a failure
	// there still leaves nothing open.
	closeErr error
	// reopenErr is what every reopen reports instead of opening anything,
	// nil by default.
	reopenErr error
	// reopenCtxErr is what the context the last reopen ran under said
	// about itself when it was called.
	reopenCtxErr error
	// debrisAtReopen is what of the fetched artefact was still beside the
	// live file when the last reopen ran — the part file and anything the
	// verification's reads grew beside it.
	debrisAtReopen []string

	// broker is the one the donor serves on, for a case that stands up
	// another.
	broker *js.Queue

	// logger is what the adopter writes through, nil by default — the
	// package's own. A case that reads the lines sets it.
	logger *slog.Logger
}

func newJoinHarness(t *testing.T) *joinHarness {
	t.Helper()
	return newJoinHarnessFrom(t, joinDonor{})
}

// joinDonor is how the harness's donor is built: anything written into its
// estate before the snapshot.
type joinDonor struct {
	seed func(t *testing.T, estate store.ReplicatedHandle)
}

func newJoinHarnessFrom(t *testing.T, from joinDonor) *joinHarness {
	t.Helper()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})

	// The DONOR, with a snapshot of its own replicated estate.
	donorDir := t.TempDir()
	donorDB, donorEstate := storetest.OpenEstate(t, filepath.Join(donorDir, "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := donorDB.Close(); err != nil {
			t.Errorf("close the donor's store: %v", err)
		}
	})
	if err := donorEstate.Tx(t.Context(), func(tx *sql.Tx) error {
		// A statement BATCH takes no arguments on this driver, so the
		// schema and the parameterised row go separately.
		if _, err := tx.ExecContext(t.Context(), probeDDL+`
			INSERT INTO probe_rows (position, kind, stored_at) VALUES (1, 'edit', 0);
			INSERT INTO probe_ops (op_id, subject, position, applied_at)
				VALUES ('op-1', 's', 1, 0);`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at, applied_version)
				VALUES (?, 1, 4200, 0, 0, 0)`, probeStream)
		return err
	}); err != nil {
		t.Fatalf("seed the donor: %v", err)
	}
	if from.seed != nil {
		from.seed(t, donorEstate)
	}

	h := &joinHarness{t: t, nc: q.Conn(), broker: q, donors: []string{"donor"}}
	snapDir := filepath.Join(donorDir, "snapshots")
	lag := uint64(0)
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain: probeDomain{}, Spec: specOf(probeDomain{}),
			Health: func() statelog.Health {
				return statelog.Health{
					Position: statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200},
					Drained:  true,
					Lag:      &lag,
				}
			},
		}},
		File:          donorEstate,
		Dir:           snapDir,
		NodeID:        "donor",
		EngineVersion: "v0.0.0-test",
		Recipients:    func(context.Context) (int, error) { return 2, nil },
		Interval:      24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	m, err := snapper.Take(t.Context())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	h.manifest = m
	// THE NAME THE MANIFEST CARRIES, which is how the engine's donor
	// finds it too: the name is unique per take, so nothing outside can
	// derive it.
	h.snapPath = filepath.Join(snapDir, m.Artifact)
	h.donorDB = donorDB

	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "donor",
		Dial:   func(context.Context) (*nats.Conn, error) { return q.Conn(), nil },
		Newest: func() (statelog.Manifest, bool) { return h.manifest, true },
		Path:   func(statelog.Manifest) string { return h.snapPath },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	waitForSubject(t, h.nc, statelog.SubjectOffer)

	// The JOINER, whose own replicated estate is about to be replaced.
	joinDir := t.TempDir()
	joiner, joinEstate := storetest.OpenEstate(t, filepath.Join(joinDir, "node.db"), store.Options{}, 1)
	h.joiner, h.joinEstate = joiner, joinEstate
	h.joinPath = storetest.ReplicatedDB(t, joinEstate).Path()
	t.Cleanup(func() { _ = h.joiner.Close() })
	return h
}

func (h *joinHarness) adopter(t *testing.T) *statelog.Adopter {
	t.Helper()
	a, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  map[string]statelog.Registered{"probe": {Domain: probeDomain{}, Spec: specOf(probeDomain{})}},
		LivePath: h.joinPath,
		NodeID:   "joiner",
		Conn:     h.nc,
		Donors: func(ctx context.Context) ([]string, error) {
			if h.listDonors != nil {
				return h.listDonors(ctx)
			}
			return slices.Clone(h.donors), nil
		},
		Need: func(context.Context) (statelog.OfferRequest, error) {
			return statelog.OfferRequest{
				Need:        map[string]uint64{"probe": 4_000},
				Generations: map[string]uint32{"probe": 1},
			}, nil
		},
		Hold: func(context.Context, map[string]statelog.DomainPosition) (func(), error) {
			h.holdAsks.Add(1)
			if h.holdErr != nil {
				return nil, h.holdErr
			}
			h.held.Add(1)
			return func() { h.released.Add(1) }, nil
		},
		Close: func(ctx context.Context) error {
			h.closes.Add(1)
			if h.onClose != nil {
				h.onClose()
			}
			if err := h.joiner.CloseReplicated(); err != nil {
				return err
			}
			return h.closeErr
		},
		Reopen: func(ctx context.Context) error {
			h.reopens.Add(1)
			h.reopenCtxErr = ctx.Err()
			h.debrisAtReopen = h.debris(t)
			if h.reopenErr != nil {
				return h.reopenErr
			}
			_, err := h.joiner.OpenReplicated(ctx, 1)
			return err
		},
		Record: func(ctx context.Context, began time.Time, donor string,
			m statelog.Manifest, phase statelog.AdoptionPhase) error {

			h.phases = append(h.phases, phase)
			h.began = append(h.began, began)
			if h.record != nil {
				return h.record(ctx, began, donor, m, phase)
			}
			return nil
		},
		StillUsable: h.stillUsable,
		Logger:      h.logger,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	return a
}

// debris is every file of the fetched artefact's set beside the live file: the
// part file, and whatever opening it grew beside it.
func (h *joinHarness) debris(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob(h.joinPath + statelog.AdoptPartSuffix + "*")
	if err != nil {
		t.Fatalf("list the artefact's files: %v", err)
	}
	return found
}

// A NODE BELOW THE FLOOR ADOPTS A PEER'S SNAPSHOT WHOLESALE, and every claim
// the artefact makes is checked before it is installed.
//
// A node that has fallen below the trim floor cannot replay its way back: the
// records it is missing are gone. The transfer is what replaces the replay,
// and the verifications are what make accepting somebody else's database file
// safe at all.
func TestANodeBelowTheFloorAdoptsAVerifiedArtefact(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	logs := &lockedBuffer{}
	h.logger = slog.New(slog.NewJSONHandler(logs, nil))

	adoption, err := h.adopter(t).Join(t.Context())
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	m := adoption.Manifest
	if m.NodeID != "donor" {
		t.Fatalf("adopted an artefact from %q, want the donor's", m.NodeID)
	}

	// ONE LINE SAYS SO, naming which artefact: the engine wrote a second
	// `statelog_adopted` carrying the checksum and the instant, and one
	// adoption read as two. The adopter's line carries both now.
	adopted := logRecords(t, logs.Bytes(), "statelog_adopted")
	if len(adopted) != 1 {
		t.Fatalf("%d statelog_adopted lines for one adoption, want one", len(adopted))
	}
	for key, want := range map[string]any{
		"donor":    "donor",
		"sha256":   m.SHA256,
		"taken_at": m.TakenAt.Format(time.RFC3339Nano),
	} {
		if adopted[0][key] != want {
			t.Errorf("statelog_adopted %s = %v, want %v — it is the one line "+
				"that says which artefact this node installed", key, adopted[0][key], want)
		}
	}

	// THE ROWS ARRIVED and the checkpoint with them, which is the whole
	// point: the position inside the file is what describes the file.
	var rows, seq int64
	if err := h.joinEstate.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM probe_rows`).Scan(&rows); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT seq FROM statelog_cursor WHERE stream = ?`, probeStream).Scan(&seq)
	}); err != nil {
		t.Fatalf("read the adopted estate: %v", err)
	}
	if rows != 1 || seq != 4_200 {
		t.Fatalf("the adopted estate holds %d row(s) at position %d, want 1 at 4200",
			rows, seq)
	}

	// THE DONOR'S OPERATION LEDGER CAME WITH IT, and lost nothing on the
	// way: its rows are what this node's own applier would have written
	// from the same records, so a retry of anything the donor applied is
	// answered from them here — and an operation neither ledger holds is
	// one that never applied, which this node may decide like any other.
	if ops := h.joinerOps(t); ops != 1 {
		t.Fatalf("the adopted ledger holds %d row(s), want the donor's one — "+
			"without it this node cannot tell a first attempt from a retry of "+
			"anything the donor applied, and must answer both `unknown`", ops)
	}
	if before, lost := h.joinerLostBefore(t); lost {
		t.Fatalf("the adopted ledger may have lost rows before %s — a donor that "+
			"scrubs nothing loses nothing, and a watermark here refuses every "+
			"first attempt minted before it", before)
	}

	// THE HOLD WAS TAKEN BEFORE THE FETCH AND HANDED TO THE CALLER, NOT
	// RELEASED: the installed artefact's tail is what this node replays
	// next, and nothing in the fleet counts that until a position of the
	// node's own is published — which only the caller can see.
	if h.held.Load() != 1 || h.released.Load() != 0 {
		t.Fatalf("the replay tail was held %d time(s) and released %d by the "+
			"join — without the hold the trim can pass the artefact while it is "+
			"in flight, and released with the join it can pass the tail the node "+
			"has just installed itself in front of", h.held.Load(), h.released.Load())
	}
	if adoption.Release == nil {
		t.Fatal("an installed adoption carries no release, so the pin on its " +
			"tail is either gone already or held for ever")
	}
	adoption.Release()
	if h.released.Load() != 1 {
		t.Fatalf("the adoption's release released %d hold(s), want its own one",
			h.released.Load())
	}

	// AND THE ADOPTION WAS RECORDED THROUGH ITS PHASES. A node that
	// crashed mid-adoption looks, from its checkpoint alone, exactly like
	// one that is caught up — the checkpoint came from the artefact.
	want := []statelog.AdoptionPhase{
		statelog.AdoptionScrubbed, statelog.AdoptionInstalled, statelog.AdoptionComplete,
	}
	if len(h.phases) != len(want) {
		t.Fatalf("the adoption recorded %v, want %v", h.phases, want)
	}
	for i := range want {
		if h.phases[i] != want[i] {
			t.Fatalf("the adoption recorded %v, want %v", h.phases, want)
		}
	}
	// ALL UNDER ONE START, so one join is one row.
	for _, began := range h.began {
		if !began.Equal(h.began[0]) {
			t.Fatalf("one join recorded its phases under the starts %v — each "+
				"is a row of its own", h.began)
		}
	}
	// AND NOTHING OF THE PART FILE SURVIVES, the lock its inspection took
	// included.
	if left := h.debris(t); len(left) != 0 {
		t.Errorf("%v survive beside the live database", left)
	}
}

// A JOIN LISTS THE DONORS IT EXPECTS WHILE THEIR OFFERS ARRIVE, NOT BEFORE.
//
// The listing is a coordination read, and a joining node refuses every read
// and write until its join ends: listed first, a store slow to answer held the
// node for the whole listing and then the whole offer window after it. Here
// the listing answers only once the joiner's ask is on the wire — which a join
// that listed first never sends — and with its answer in, the collection ends
// as soon as the donor it names has offered rather than at the window.
func TestAJoinListsItsDonorsWhileTheirOffersArrive(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	asks, err := h.nc.SubscribeSync(statelog.SubjectOffer)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = asks.Unsubscribe() })
	if err := h.nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	h.listDonors = func(ctx context.Context) ([]string, error) {
		if _, err := asks.NextMsgWithContext(ctx); err != nil {
			return nil, err
		}
		return slices.Clone(h.donors), nil
	}
	// BOUNDED, so a join that lists first — whose listing waits for an ask
	// it has not sent — fails here rather than hanging the run.
	ctx, cancel := context.WithTimeout(t.Context(), 2*statelog.OfferWindow)
	defer cancel()
	started := time.Now()
	if _, err := h.adopter(t).Join(ctx); err != nil {
		t.Fatalf("Join: %v", err)
	}
	// THE INSTANT THE ADOPTION BEGAN is when the collection ended, and
	// half the window separates a collection that ended on its listing
	// from one that waited the window out.
	if len(h.began) == 0 {
		t.Fatal("the join recorded no adoption")
	}
	if collected := h.began[0].Sub(started); collected >= statelog.OfferWindow/2 {
		t.Fatalf("the join collected offers for %v: its listing's answer, after "+
			"every donor it named had offered, did not end the collection", collected)
	}
}

// A CORRUPTED TRANSFER IS REFUSED AND NOTHING IS INSTALLED.
//
// A transfer that dropped or reordered a chunk arrives looking exactly like
// one that did not, so the checksum is the only thing between that and an
// installed database with a hole in it.
func TestACorruptedArtefactIsRefusedAndTheLiveDatabaseSurvives(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	// The manifest claims a checksum the bytes do not have, which is what
	// a corrupted transfer looks like from the recipient's side.
	h.manifest.SHA256 = strings.Repeat("0", 64)

	_, err := h.adopter(t).Join(t.Context())
	if !errors.Is(err, statelog.ErrNoOffer) {
		t.Fatalf("Join = %v, want ErrNoOffer", err)
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("the refusal does not name the checksum: %v", err)
	}
	if h.closes.Load() != 0 {
		t.Fatal("the live database was closed for an artefact that never passed " +
			"verification — the install is the one place a live database is " +
			"replaced, and it must not be reached by a refused offer")
	}
	// NOR ITS SIDECARS: a stale -wal beside the next attempt's artefact is
	// applied to it the moment that attempt's inspection opens it.
	if left := h.debris(t); len(left) != 0 {
		t.Errorf("a refused artefact left %v beside the live database", left)
	}
	// AND THE HOLD WAS RELEASED, so a refused join does not pin the log.
	if h.held.Load() != h.released.Load() {
		t.Fatalf("the tail was held %d time(s) and released %d — a refused join "+
			"that keeps its hold stops the whole fleet trimming",
			h.held.Load(), h.released.Load())
	}
}

// A JOIN ABANDONED MID-ADOPTION SAYS SO, and is never "nobody could donate".
//
// [statelog.ErrNoOffer] is a statement about the fleet, and the engine acts
// on it: a boot comes up on the history it has and logs that no peer could
// donate, a rejoin schedules its next attempt. A cancellation filed among the
// refusals came back as exactly that — every remaining offer ended the same
// way, each donor was logged as refused, and a node being stopped reported a
// fleet with nothing to give.
//
// The cancellation lands at step 7's re-check, the last check of the file
// before the stamp and the install: late enough that a transfer really
// happened, early enough that the live database was never touched.
func TestAJoinAbandonedMidAdoptionIsNotAnEmptyFleet(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.stillUsable = func(joinCtx context.Context, _ statelog.Manifest) error {
		// The engine's own re-check reads the broker under the join's
		// context, and a read under a cancelled one fails with the
		// cancellation.
		cancel()
		return joinCtx.Err()
	}

	_, err := h.adopter(t).Join(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Join = %v, want the cancellation", err)
	}
	if errors.Is(err, statelog.ErrNoOffer) {
		t.Fatalf("an abandoned join reported ErrNoOffer (%v) — the engine reads "+
			"that as a fleet with nothing to donate, and brings a boot up or "+
			"schedules a retry for a node that is being stopped", err)
	}
	if h.closes.Load() != 0 {
		t.Fatal("the live database was closed by a join its caller had abandoned")
	}
	if h.held.Load() != h.released.Load() {
		t.Fatalf("the tail was held %d time(s) and released %d — an abandoned "+
			"join that keeps its hold stops the whole fleet trimming",
			h.held.Load(), h.released.Load())
	}
}

// AN INSTALL INTERRUPTED BEFORE ITS RENAME PUTS THE LIVE DATABASE BACK.
//
// Step 9 closes the live database and then runs the install, and when the
// install fails the live file is still the live file, so reopening it is the
// recovery. It is a ROLLBACK, and the failure it undoes is routinely the
// cancellation itself: a Stop or a signal landing while the install
// checkpoints. Reopened under the join's own context it failed alongside, and
// a node whose install never happened was left with no replicated estate open
// — every read [store.ErrNoEstate] — with nothing in the error to say so.
func TestAnInterruptedInstallReopensTheLiveDatabase(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// THE CANCELLATION LANDS AS THE LIVE DATABASE CLOSES, so the install's
	// first step — checkpointing the prepared file under the join's
	// context — is the one that fails, before any rename.
	h.onClose = cancel

	_, err := h.adopter(t).Join(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Join = %v, want the cancellation that interrupted the install", err)
	}
	if h.reopens.Load() != 1 {
		t.Fatalf("the live database was reopened %d time(s) after a failed "+
			"install, want once", h.reopens.Load())
	}
	if h.reopenCtxErr != nil {
		t.Fatalf("the rollback reopened the live database under a context that "+
			"was already %v — the failure it undoes is that very cancellation, "+
			"so it fails too and the node is left with no estate", h.reopenCtxErr)
	}
	if err := storetest.ReplicatedDB(t, h.joinEstate).SQL().PingContext(t.Context()); err != nil {
		t.Fatalf("the live database is not usable after the rollback: %v", err)
	}
}

// A LIVE DATABASE THAT DID NOT COME BACK IS NEVER "NOBODY COULD DONATE".
//
// Every way a join can close the live database and then fail to open one
// again ends in the same state: no replicated estate open at all, every read
// and every applier answering ErrNoEstate. [statelog.ErrNoOffer] is the one
// thing that must not say so, because the engine answers it by carrying on
// without a snapshot — a boot comes up with nothing open to come up on, and a
// rejoin waits out a widening interval before anything reopens it. So the join
// comes back as [statelog.ErrEstateNotRestored], carrying the failure that
// closed the estate AND the reopen's own, and it stops at that offer: every
// later one would be installed through a bracket around an estate that is not
// there, and each donor logged as refusing a node that could take nothing.
//
// Four doors reach the state, and each is staged with the reopen failing: an
// install that failed while its caller still waited, one the caller's own
// cancellation interrupted, a close that failed after it took the database out
// of service, and an artefact installed and then not opened. A second donor
// offers the same artefact in every case, so a join that moved on would be
// seen closing the live database a second time.
func TestALiveDatabaseThatDidNotComeBackIsNeverAnEmptyFleet(t *testing.T) {
	t.Parallel()
	reopenFailed := errors.New("the disk under the live database is gone")
	closeFailed := errors.New("the close did not finish")
	cases := []struct {
		name string
		// stage arranges the failure that closes the estate for good.
		stage func(h *joinHarness, cancel context.CancelFunc)
		// cause is text the error must carry about that failure.
		cause string
		// cancelled is whether the caller's own end is part of it.
		cancelled bool
		// installed is whether the rename happened, so the reopen is the
		// artefact's own rather than a rollback's.
		installed bool
	}{
		{
			name: "an install that failed",
			stage: func(h *joinHarness, _ context.CancelFunc) {
				// A DIRECTORY WHERE THE PREPARED FILE WAS: the install's
				// first step cannot open it, for a reason of this node's
				// own rather than anything a donor sent.
				h.onClose = func() {
					part := h.joinPath + statelog.AdoptPartSuffix
					if err := os.Remove(part); err != nil {
						h.t.Errorf("remove the prepared file: %v", err)
					}
					if err := os.Mkdir(part, 0o700); err != nil {
						h.t.Errorf("put a directory in its place: %v", err)
					}
				}
			},
			cause: "store: adopt",
		},
		{
			name: "an install the caller interrupted",
			stage: func(h *joinHarness, cancel context.CancelFunc) {
				h.onClose = cancel
			},
			cause:     "store: adopt",
			cancelled: true,
		},
		{
			name: "a close that failed",
			stage: func(h *joinHarness, _ context.CancelFunc) {
				h.closeErr = closeFailed
			},
			cause: closeFailed.Error(),
		},
		{
			name:      "an installed artefact that did not open",
			stage:     func(*joinHarness, context.CancelFunc) {},
			cause:     "the artefact is installed",
			installed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newJoinHarness(t)
			h.addDonor(t, "donor-2")
			h.reopenErr = reopenFailed
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c.stage(h, cancel)

			adoption, err := h.adopter(t).Join(ctx)
			if !errors.Is(err, statelog.ErrEstateNotRestored) {
				t.Fatalf("Join = %v, want ErrEstateNotRestored — the live database "+
					"is not open, and nothing else in the error says so", err)
			}
			if errors.Is(err, statelog.ErrNoOffer) {
				t.Fatalf("a join that lost the live database reported ErrNoOffer (%v) "+
					"— the engine reads that as a fleet with nothing to donate and "+
					"carries on with no estate open", err)
			}
			if !errors.Is(err, reopenFailed) {
				t.Errorf("the error drops why the reopen failed: %v", err)
			}
			if !strings.Contains(err.Error(), c.cause) {
				t.Errorf("the error drops what closed the estate (%q): %v", c.cause, err)
			}
			if c.cancelled && !errors.Is(err, context.Canceled) {
				t.Errorf("the error drops the caller's own cancellation: %v", err)
			}
			if n := h.closes.Load(); n != 1 {
				t.Fatalf("the live database was closed %d time(s), want once — the "+
					"join went on to another offer with no estate open", n)
			}
			if n := h.reopens.Load(); n != 1 {
				t.Fatalf("the live database was reopened %d time(s), want once", n)
			}
			// A ROLLBACK REOPENS WITH THE ARTEFACT ALREADY GONE: a whole
			// copy of the estate on the live file's volume competes with
			// the reopen for the room a full disk has run out of, and is
			// deleted a moment later on the way out anyway.
			if !c.installed && len(h.debrisAtReopen) != 0 {
				t.Fatalf("the rollback reopened the live database beside %v — "+
					"an artefact nothing will install, holding the room the "+
					"reopen may need", h.debrisAtReopen)
			}
			if left := h.debris(t); len(left) != 0 {
				t.Fatalf("the join left %v beside the live database", left)
			}
			// AN INSTALLED ARTEFACT IS AN ADOPTION, open or not: its manifest
			// and its pin go to the caller, since whatever opens the estate
			// next opens the artefact and replays its tail. One never
			// installed keeps no pin.
			if c.installed {
				if adoption.Manifest.SHA256 != h.manifest.SHA256 || adoption.Release == nil ||
					h.released.Load() != 0 {
					t.Fatalf("an installed artefact that did not open came back as "+
						"%q with a release %v, after %d release(s) — the artefact is "+
						"the live file, and its tail is the node's to replay",
						adoption.Manifest.SHA256, adoption.Release != nil, h.released.Load())
				}
			} else if adoption.Release != nil || h.held.Load() != h.released.Load() {
				t.Fatalf("the tail was held %d time(s) and released %d, and a "+
					"release was handed over: %v", h.held.Load(), h.released.Load(),
					adoption.Release != nil)
			}
		})
	}
}

// AN INSTALL IS AN ADOPTION, WHATEVER FAILS AFTER IT.
//
// The rename is the join's point of no return: from it the artefact is the
// live database, and once it is open every applier resumes from its
// checkpoint. What comes after is this node's own HISTORY row of the adoption,
// in the node estate — a separate file whose write can be refused for reasons
// of its own. Filed as the donor's refusal, that refusal sent the join on to
// the next offer, which it fetched and installed OVER the artefact already
// live — a second transfer landing on an artefact no newer than the first —
// and with no offer left it reported that nobody could donate while a donor's
// database was live, so a running node skipped everything an adoption owes.
//
// A second donor offers the same artefact, so a join that moved on is seen
// closing the live database twice; and the write is refused at that phase on
// every attempt, which is what a store fault looks like.
func TestAnInstallWhoseRecordFailsIsStillAnAdoption(t *testing.T) {
	t.Parallel()
	for _, phase := range []statelog.AdoptionPhase{
		statelog.AdoptionInstalled, statelog.AdoptionComplete,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			h := newJoinHarness(t)
			h.addDonor(t, "donor-2")
			logs := &lockedBuffer{}
			h.logger = slog.New(slog.NewJSONHandler(logs, nil))
			refused := errors.New("store: this node's store could not take its write lock in time")
			h.record = func(_ context.Context, _ time.Time, _ string,
				_ statelog.Manifest, at statelog.AdoptionPhase) error {

				if at == phase {
					return refused
				}
				return nil
			}

			adoption, err := h.adopter(t).Join(t.Context())
			m := adoption.Manifest
			if !errors.Is(err, statelog.ErrAdoptionUnrecorded) {
				t.Fatalf("Join = %v, want ErrAdoptionUnrecorded — the artefact is "+
					"installed and open, and only its record is short", err)
			}
			if errors.Is(err, statelog.ErrNoOffer) {
				t.Fatalf("an installed artefact was reported as no usable offer (%v) "+
					"— the engine reads that as nobody could donate, while a donor's "+
					"database is the live one", err)
			}
			if !errors.Is(err, refused) {
				t.Errorf("the error drops why the row could not be advanced: %v", err)
			}
			if m.SHA256 != h.manifest.SHA256 {
				t.Fatalf("Join returned the manifest %q beside the error, want the "+
					"installed artefact's %q — a caller acts on what was installed",
					m.SHA256, h.manifest.SHA256)
			}
			if n := h.closes.Load(); n != 1 {
				t.Fatalf("the live database was closed %d time(s), want once — the "+
					"join installed another offer over the artefact it had installed", n)
			}
			if n := h.held.Load(); n != 1 {
				t.Fatalf("the tail was held %d time(s), want once: a join that moved "+
					"on fetched another offer", n)
			}
			// AND ITS PIN GOES WITH IT, as any installed artefact's does.
			if adoption.Release == nil || h.released.Load() != 0 {
				t.Fatalf("the installed artefact's pin was released %d time(s) by "+
					"the join and handed over %v — it is the caller's, until this "+
					"node is counted", h.released.Load(), adoption.Release != nil)
			}
			if scrubbed := slices.Index(h.phases[1:], statelog.AdoptionScrubbed); scrubbed >= 0 {
				t.Fatalf("the adoption recorded %v — a second offer was verified "+
					"after the first was installed", h.phases)
			}
			// THE ARTEFACT IS LIVE, AND OPEN.
			var seq int64
			if err := h.joinEstate.Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(),
					`SELECT seq FROM statelog_cursor WHERE stream = ?`, probeStream).Scan(&seq)
			}); err != nil || seq != 4_200 {
				t.Fatalf("the live estate reads (%d, %v), want the artefact's 4200", seq, err)
			}
			// AND THE LOG SAYS IT ONCE EACH: an adoption, and the record it
			// could not advance — never a donor refused.
			if n := len(logRecords(t, logs.Bytes(), "statelog_adoption_refused")); n != 0 {
				t.Fatalf("%d statelog_adoption_refused line(s) — no donor refused anything", n)
			}
			if n := len(logRecords(t, logs.Bytes(), "statelog_adopted")); n != 1 {
				t.Fatalf("%d statelog_adopted line(s) for one adoption, want one", n)
			}
			unrecorded := logRecords(t, logs.Bytes(), "statelog_adoption_unrecorded")
			if len(unrecorded) != 1 || unrecorded[0]["phase"] != string(phase) {
				t.Fatalf("statelog_adoption_unrecorded = %v, want one line naming "+
					"the %s phase", unrecorded, phase)
			}
		})
	}
}

// A FAULT OF THIS NODE'S OWN ENDS THE JOIN, AND BLAMES NO DONOR.
//
// Everything a join can fail on before the install is either about the OFFER
// — its transfer, its bytes, its schema, its positions — or about THIS NODE:
// the hold register it pins through, its own store, its own disk. Filed as the
// donor's refusal, the second kind was logged against a peer whose artefact
// was fine, and every remaining offer was then fetched in full only to meet
// the same fault — ending in "nobody could donate" for a fleet that donated
// perfectly well. A second donor offers the same artefact in every case, so a
// join that moved on is seen holding the tail a second time.
func TestAFaultOfThisNodesOwnEndsTheJoin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stage func(t *testing.T, h *joinHarness)
		// closed is whether the live database was closed — and so
		// reopened by the rollback — before the fault.
		closed bool
	}{
		{
			name: "its trim hold is refused",
			stage: func(_ *testing.T, h *joinHarness) {
				h.holdErr = errors.New("the coordination store cannot be reached")
			},
		},
		{
			name: "an earlier attempt's artefact cannot be cleared",
			stage: func(t *testing.T, h *joinHarness) {
				// A DIRECTORY HOLDING A FILE where the copy goes, which
				// nothing that removes a database's files can remove.
				part := h.joinPath + statelog.AdoptPartSuffix
				if err := os.MkdirAll(filepath.Join(part, "held"), 0o700); err != nil {
					t.Fatalf("stage the debris: %v", err)
				}
			},
		},
		{
			name: "its own store refuses the record",
			stage: func(_ *testing.T, h *joinHarness) {
				h.record = func(_ context.Context, _ time.Time, _ string,
					_ statelog.Manifest, at statelog.AdoptionPhase) error {

					if at == statelog.AdoptionScrubbed {
						return errors.New("store: the node estate's disk is full")
					}
					return nil
				}
			},
		},
		{
			name: "its own disk refuses the stamp that names the copy",
			stage: func(t *testing.T, h *joinHarness) {
				// THE COPY GONE FROM UNDER THE STAMP, a directory in its
				// place, as the install's case below stages the rename's:
				// the stamp opens this node's own file, and nothing about
				// the offer is wrong.
				h.stillUsable = func(context.Context, statelog.Manifest) error {
					part := h.joinPath + statelog.AdoptPartSuffix
					if err := os.Remove(part); err != nil {
						t.Errorf("remove the prepared file: %v", err)
					}
					if err := os.Mkdir(part, 0o700); err != nil {
						t.Errorf("put a directory in its place: %v", err)
					}
					return nil
				}
			},
		},
		{
			name: "the close of the live database fails",
			stage: func(_ *testing.T, h *joinHarness) {
				h.closeErr = errors.New("the close did not finish")
			},
			closed: true,
		},
		{
			name: "the install fails",
			stage: func(t *testing.T, h *joinHarness) {
				h.onClose = func() {
					part := h.joinPath + statelog.AdoptPartSuffix
					if err := os.Remove(part); err != nil {
						t.Errorf("remove the prepared file: %v", err)
					}
					if err := os.Mkdir(part, 0o700); err != nil {
						t.Errorf("put a directory in its place: %v", err)
					}
				}
			},
			closed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newJoinHarness(t)
			h.addDonor(t, "donor-2")
			logs := &lockedBuffer{}
			h.logger = slog.New(slog.NewJSONHandler(logs, nil))
			c.stage(t, h)

			adoption, err := h.adopter(t).Join(t.Context())
			if err == nil {
				t.Fatal("a join whose node failed it reported an adoption")
			}
			// THE ONE ANSWER A CALLER BRANCHES ON: the engine comes up
			// behind on it, as it does when nobody could donate, rather
			// than stopping the boot or telling every listener the rows
			// were replaced.
			if !errors.Is(err, statelog.ErrJoinerFault) {
				t.Fatalf("Join = %v, want ErrJoinerFault — a caller cannot tell "+
					"this node's own fault from any other failure", err)
			}
			if errors.Is(err, statelog.ErrNoOffer) {
				t.Fatalf("a fault of this node's own was reported as no usable "+
					"offer (%v) — a statement about the fleet, which donated", err)
			}
			if errors.Is(err, statelog.ErrEstateNotRestored) ||
				errors.Is(err, statelog.ErrAdoptionUnrecorded) {
				t.Fatalf("Join = %v: nothing was installed and the live database "+
					"is open", err)
			}
			if adoption.Manifest.SHA256 != "" || adoption.Release != nil {
				t.Fatalf("Join returned the manifest %q for a join that installed "+
					"nothing — a manifest beside an error means installed",
					adoption.Manifest.SHA256)
			}
			if h.held.Load() != h.released.Load() {
				t.Fatalf("the tail was held %d time(s) and released %d — a join "+
					"that installed nothing keeps no pin", h.held.Load(), h.released.Load())
			}
			if n := h.holdAsks.Load(); n != 1 {
				t.Fatalf("the join tried %d offers, want one — every other offer "+
					"meets the same fault on this node", n)
			}
			if n := len(logRecords(t, logs.Bytes(), "statelog_adoption_refused")); n != 0 {
				t.Fatalf("%d statelog_adoption_refused line(s) blame a donor for "+
					"this node's own fault", n)
			}
			if c.closed && h.reopens.Load() != 1 {
				t.Fatalf("the live database was reopened %d time(s) after the "+
					"failed install, want once", h.reopens.Load())
			}
			if err := storetest.ReplicatedDB(t, h.joinEstate).SQL().PingContext(t.Context()); err != nil {
				t.Fatalf("the live database is not usable after the join: %v", err)
			}
		})
	}
}

// A DONOR'S OWN WATERMARK TRAVELS WITH ITS LEDGER.
//
// The donor's sweep deleted every row it applied more than the ledger's
// retention ago, and recorded how far back. Those rows are missing from the
// artefact too, so an adopter that took the ledger without the watermark
// would read their absence as conclusive and decide a retry of any of them
// again.
func TestADonorsWatermarkTravelsWithItsLedger(t *testing.T) {
	t.Parallel()
	swept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	h := newJoinHarnessFrom(t, joinDonor{
		seed: func(t *testing.T, estate store.ReplicatedHandle) {
			markLedgerLost(t, estate, swept)
		},
	})
	if _, err := h.adopter(t).Join(t.Context()); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if before, lost := h.joinerLostBefore(t); !lost || !before.Equal(swept) {
		t.Fatalf("the adopted ledger's watermark = (%s, %v), want the donor's %s",
			before, lost, swept)
	}
}

// markLedgerLost records, in estate, that the probe's operation ledger may
// have lost every row applied before before — the watermark a sweep leaves,
// written as [tables.markLost] writes it, never backwards.
func markLedgerLost(t *testing.T, estate interface {
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
}, before time.Time) {
	t.Helper()
	if err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_ops_lost (ops_table, lost_before) VALUES (?, ?)
			ON CONFLICT (ops_table) DO UPDATE SET
				lost_before = MAX(lost_before, excluded.lost_before)`,
			probeDomain{}.OpsTable(), store.EncodeTime(before))
		return err
	}); err != nil {
		t.Fatalf("move the operation ledger's watermark: %v", err)
	}
}

// joinerOps is how many rows the joiner's operation ledger holds.
func (h *joinHarness) joinerOps(t *testing.T) int64 {
	t.Helper()
	var ops int64
	if err := h.joinEstate.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM probe_ops`).Scan(&ops)
	}); err != nil {
		t.Fatalf("read the operation ledger: %v", err)
	}
	return ops
}

// joinerLostBefore is the joiner's ledger watermark, through the read seam the
// publisher asks it through.
func (h *joinHarness) joinerLostBefore(t *testing.T) (time.Time, bool) {
	t.Helper()
	rows, err := statelog.NewRows(h.joinEstate.Reader(), probeDomain{}, specOf(probeDomain{}), nil)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	before, lost, err := rows.LostBefore(t.Context(), "")
	if err != nil {
		t.Fatalf("read the ledger's watermark: %v", err)
	}
	return before, lost
}

// addDonor stands up another donor on the harness's broker, offering the same
// artefact, and returns once a joiner asking would hear both.
func (h *joinHarness) addDonor(t *testing.T, nodeID string) {
	t.Helper()
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: nodeID,
		Dial:   func(context.Context) (*nats.Conn, error) { return h.broker.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return h.manifest, true },
		Path:   func(statelog.Manifest) string { return h.snapPath },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = donor.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	h.donors = append(h.donors, nodeID)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		offers, err := statelog.CollectOffers(t.Context(), h.nc,
			statelog.OfferRequest{NodeID: "probe"}, 200*time.Millisecond)
		if err != nil {
			t.Fatalf("CollectOffers: %v", err)
		}
		if len(offers) >= 2 {
			return
		}
	}
	t.Fatalf("%s never answered beside the harness's donor", nodeID)
}

// AN ARTEFACT WHOSE MANIFEST DOES NOT MATCH THE FILE IS REFUSED.
//
// The checkpoint commits in the same transaction as the rows, so the position
// inside the file is the only one that describes the file — a manifest naming
// a different one is describing a different artefact, and adopting it would
// leave this node resuming above rows it does not have.
func TestAManifestTheFileDoesNotKeepIsACorruptSnapshot(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	pos := h.manifest.Domains["probe"]
	pos.Seq = 9_999
	h.manifest.Domains["probe"] = pos

	_, err := h.adopter(t).Join(t.Context())
	if err == nil {
		t.Fatal("an artefact whose manifest the file does not keep was adopted")
	}
	if !strings.Contains(err.Error(), "corrupt snapshot") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if h.closes.Load() != 0 {
		t.Fatal("the live database was closed for an artefact that failed " +
			"verification")
	}
}

// A DONOR THAT CLAIMS A SCRUB IT DID NOT DO IS REFUSED.
//
// The safety argument for accepting somebody else's database file at all is
// that everything still in it is fleet-visible. A claim nobody checks is a
// claim.
func TestADonorsScrubClaimIsCheckedRatherThanTrusted(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	// The manifest says a table was emptied that the artefact still holds.
	h.manifest.Scrubbed = append(h.manifest.Scrubbed, "probe_rows")

	_, err := h.adopter(t).Join(t.Context())
	if err == nil {
		t.Fatal("an artefact claiming a scrub it did not do was adopted")
	}
	if !strings.Contains(err.Error(), "fleet-visible") {
		t.Errorf("the refusal does not say why the claim matters: %v", err)
	}
}

// THE FETCHED ARTEFACT LANDS BESIDE THE LIVE FILE, NOT IN A TEMPORARY
// DIRECTORY.
//
// The install is a rename, and a rename is atomic only WITHIN one filesystem.
// An artefact fetched to /tmp on a host whose data volume is a separate mount
// would have to be copied across at the end — and that copy is the one moment
// an interrupted adoption can leave a mixture, which is the single thing this
// whole sequence is arranged to make impossible.
//
// It is asserted rather than left structural because the failure has no
// symptom on a developer's machine, where /tmp and the working tree are the
// same filesystem and the rename succeeds every time.
//
// The observation is taken at step 7's re-check, which is the one moment the
// artefact is on disk under its own name: before it the transfer has not
// finished, and after it the install has moved or removed it.
func TestTheFetchedArtefactLandsBesideTheLiveFile(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)

	var whereItLanded string
	h.stillUsable = func(context.Context, statelog.Manifest) error {
		part := h.joinPath + statelog.AdoptPartSuffix
		if _, err := os.Stat(part); err == nil {
			whereItLanded = part
		}
		return nil
	}
	if _, err := h.adopter(t).Join(t.Context()); err != nil {
		t.Fatalf("Join: %v", err)
	}

	if whereItLanded == "" {
		t.Fatal("no artefact was on disk when the install was about to run, so " +
			"this case is asserting nothing about where a transfer lands")
	}
	if got, want := filepath.Dir(whereItLanded), filepath.Dir(h.joinPath); got != want {
		t.Fatalf("the artefact was fetched to %s and the live database is in "+
			"%s — the install is a rename, and across a filesystem boundary "+
			"that becomes a copy: the one moment an interrupted adoption can "+
			"leave a mixture", got, want)
	}
}

// AN EARLIER ATTEMPT'S DEBRIS IS CLEARED BEFORE THE FETCH, SIDECARS AND ALL.
//
// Opening a database grows files beside it, so what an interrupted attempt
// leaves at the part path is a SET: the part file, and whatever its opener
// left beside it. Clearing the part file alone was the old shape, and a -wal
// left beside the next attempt's fresh artefact is applied to it the moment
// step 4 opens it — pages of a database that is gone, read as though they
// were the donor's. The case plants exactly that: a truncated transfer, and a
// -wal holding pages written by the last thing that had a database open at
// that path. The artefact beside it is genuine, so a join that kept the -wal
// refuses a good snapshot as corrupt; one that cleared the set adopts it and
// leaves nothing behind.
func TestAnEarlierAttemptsDebrisIsClearedBeforeTheFetch(t *testing.T) {
	t.Parallel()
	h := newJoinHarness(t)
	part := h.joinPath + statelog.AdoptPartSuffix

	// A -WAL THAT HOLDS PAGES, copied while its database is still open:
	// a clean close would fold it in and remove it, and a crash is what
	// does not.
	earlier, err := store.OpenEstate(t.Context(), store.EstateReplicated, part, store.Options{})
	if err != nil {
		t.Fatalf("open a database at the part path: %v", err)
	}
	if err := earlier.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM statelog_cursor`)
		return err
	}); err != nil {
		t.Fatalf("write a page to its -wal: %v", err)
	}
	wal, err := os.ReadFile(part + "-wal")
	if err != nil {
		t.Fatalf("read its -wal: %v", err)
	}
	if err := earlier.Close(); err != nil {
		t.Fatalf("close it: %v", err)
	}
	for _, f := range h.debris(t) {
		if err := os.Remove(f); err != nil {
			t.Fatalf("clear %s: %v", f, err)
		}
	}
	if err := os.WriteFile(part+"-wal", wal, 0o600); err != nil {
		t.Fatalf("plant the -wal: %v", err)
	}
	if err := os.WriteFile(part, []byte("a transfer that stopped"), 0o600); err != nil {
		t.Fatalf("plant the part file: %v", err)
	}

	if _, err := h.adopter(t).Join(t.Context()); err != nil {
		t.Fatalf("Join over an earlier attempt's debris: %v — its -wal was read "+
			"into the artefact this attempt fetched", err)
	}
	if left := h.debris(t); len(left) != 0 {
		t.Fatalf("%v survive the adoption", left)
	}
}

// AN ADOPTED ESTATE IS NAMED A NEW COPY, BEFORE IT IS IN PLACE.
//
// What a node derives from the replicated rows by a forward walk — the chat
// keyword index — cannot see a row below its watermark being replaced, and the
// engine's signal that one was is held in memory, so a restart between the
// install and the next sweep lost it. What survives the restart is the file's
// own name for itself, [statelog.EstateInstance]: minted afresh by the adopter,
// so it is neither the joiner's old copy nor the donor's — a node adopting twice
// from one donor must not come back to an instance a reader already recorded —
// and stamped before the rename, so the file is never at the live path under a
// name it does not own. The donor's copy carries a name of its own, as one a
// join installed would, so the artefact arrives naming something; and the
// joiner adopts the same artefact twice, which a name fixed per build or
// derived from the artefact would answer with the same name both times.
func TestAnAdoptedEstateIsNamedANewCopy(t *testing.T) {
	t.Parallel()
	const donorsOwn = "the-donors-own-copy"
	h := newJoinHarnessFrom(t, joinDonor{seed: func(t *testing.T, estate store.ReplicatedHandle) {
		t.Helper()
		if err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(),
				`UPDATE statelog_estate SET instance = ? WHERE id = 1`, donorsOwn)
			return err
		}); err != nil {
			t.Fatalf("name the donor's copy: %v", err)
		}
	}})
	logs := &lockedBuffer{}
	h.logger = slog.New(slog.NewJSONHandler(logs, nil))
	instanceOf := func(estate interface {
		Read(context.Context, func(*sql.Tx) error) error
	}) string {
		t.Helper()
		var instance string
		if err := estate.Read(t.Context(), func(tx *sql.Tx) error {
			var err error
			instance, err = statelog.EstateInstance(t.Context(), tx)
			return err
		}); err != nil {
			t.Fatalf("read the estate's instance: %v", err)
		}
		return instance
	}
	before := instanceOf(h.joinEstate)
	donor, err := store.OpenEstate(t.Context(), store.EstateReplicated, h.snapPath, store.Options{})
	if err != nil {
		t.Fatalf("open the donor's artefact: %v", err)
	}
	carried := instanceOf(donor)
	if err := donor.Close(); err != nil {
		t.Fatalf("close the donor's artefact: %v", err)
	}
	if carried != donorsOwn {
		t.Fatalf("the premise: the artefact carries the instance %q, want the "+
			"donor's own %q", carried, donorsOwn)
	}
	// Reading the artefact grew sidecars beside it, which a fetch would
	// otherwise serve; the file goes back to the one the manifest hashes.
	if err := store.QuiesceCopy(t.Context(), h.snapPath); err != nil {
		t.Fatalf("quiesce the donor's artefact: %v", err)
	}

	// THE NAME THE ARTEFACT CARRIES AS THE INSTALL BEGINS, read at the
	// last moment before the rename.
	var atInstall string
	h.onClose = func() {
		part, err := store.OpenEstate(t.Context(), store.EstateReplicated,
			h.joinPath+statelog.AdoptPartSuffix, store.Options{})
		if err != nil {
			t.Errorf("open the artefact before its install: %v", err)
			return
		}
		atInstall = instanceOf(part)
		if err := part.Close(); err != nil {
			t.Errorf("close the artefact before its install: %v", err)
		}
	}

	if _, err := h.adopter(t).Join(t.Context()); err != nil {
		t.Fatalf("Join: %v", err)
	}

	after := instanceOf(h.joinEstate)
	if atInstall != after {
		t.Fatalf("the artefact was named %q as its install began and the estate "+
			"is named %q after it — stamped after the rename, a crash between "+
			"the two leaves the donor's rows at the live path under a name a "+
			"reader may already have recorded", atInstall, after)
	}
	if after == before {
		t.Fatalf("the adopted estate is still named %q, the copy it replaced — a "+
			"reader that recorded that name reads the donor's rows as the ones "+
			"it was built from", after)
	}
	if after == carried {
		t.Fatalf("the adopted estate is named %q, the donor's own name — a node "+
			"that adopts from one donor twice comes back to a name a reader has "+
			"already recorded", after)
	}
	adopted := logRecords(t, logs.Bytes(), "statelog_adopted")
	if len(adopted) != 1 || adopted[0]["instance"] != after {
		t.Errorf("statelog_adopted names the instance %v, want the one the "+
			"estate now holds (%s)", adopted, after)
	}

	// THE SAME ARTEFACT AGAIN, from the same donor: a reader that rebuilt
	// from the first install must see the second as another copy.
	if _, err := h.adopter(t).Join(t.Context()); err != nil {
		t.Fatalf("the second Join: %v", err)
	}
	if again := instanceOf(h.joinEstate); again == after || again == carried {
		t.Fatalf("a second adoption of the same artefact named the estate %q, "+
			"after the first named it %q and the donor's is %q — a reader that "+
			"rebuilt from the first reads the second as no change", again, after, carried)
	}
}
