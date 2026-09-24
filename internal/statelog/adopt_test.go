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
)

// joinHarness is a donor holding a real replicated estate and a joiner about
// to replace its own with it.
type joinHarness struct {
	t        *testing.T
	nc       *nats.Conn
	donorDB  *store.DB
	joiner   *store.DB
	joinPath string
	manifest statelog.Manifest
	snapPath string

	held     atomic.Int64
	released atomic.Int64
	phases   []statelog.AdoptionPhase
	closes   atomic.Int64
	reopens  atomic.Int64

	// began is every start Record was handed, one per call.
	began []time.Time
	// record, nil by default, is what Record also does: a case that needs
	// the row really written sets it to [statelog.RecordAdoption].
	record func(ctx context.Context, began time.Time, donor string,
		m statelog.Manifest, phase statelog.AdoptionPhase) error
	// answered is when the harness's donor first answered an ask since the
	// adopter was built, in Unix nanoseconds: the latest instant it can
	// have finished the artefact it offered the join. Cleared when the
	// adopter is built, because the harness's own readiness probes are asks
	// too; a later call is the transfer's, which re-reads what to send.
	answered atomic.Int64

	// stillUsable is step 7's re-check, nil by default. A case sets it to
	// stage the one thing the hold is a belt against: a fleet that
	// trimmed past the artefact while it was in flight.
	stillUsable func(context.Context, statelog.Manifest) error

	// onClose runs as step 8 closes the live database, nil by default:
	// the last moment before the install, which is where a case stages
	// an interruption the rename must survive.
	onClose func()
	// closeErr is what step 8's close reports AFTER it has closed the live
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
	return newJoinHarnessFrom(t, joinDonor{domain: probeDomain{}})
}

// joinDonor is how the harness's donor is built: the domain it declares — the
// probe as this build declares it, or as a build from before the ledger
// travelled did — and anything written into its estate before the snapshot.
type joinDonor struct {
	domain statelog.Domain
	seed   func(t *testing.T, db *store.DB)
}

// scrubbingProbe is the probe domain as a build from before the ledger
// travelled declared it: its operation ledger classed as this node's own, and
// so scrubbed out of every snapshot it takes.
type scrubbingProbe struct{ probeDomain }

func (scrubbingProbe) Tables() map[string]statelog.TableClass {
	tables := probeDomain{}.Tables()
	tables["probe_ops"] = statelog.Local
	return tables
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
	donorDB, err := store.Open(t.Context(), filepath.Join(donorDir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the donor's store: %v", err)
	}
	t.Cleanup(func() {
		if err := donorDB.Close(); err != nil {
			t.Errorf("close the donor's store: %v", err)
		}
	})
	if err := donorDB.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
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
				(stream, generation, seq, stream_created_at, updated_at)
				VALUES (?, 1, 4200, 0, 0)`, probeStream)
		return err
	}); err != nil {
		t.Fatalf("seed the donor: %v", err)
	}
	if from.seed != nil {
		from.seed(t, donorDB)
	}

	h := &joinHarness{t: t, nc: q.Conn(), broker: q}
	snapDir := filepath.Join(donorDir, "snapshots")
	lag := uint64(0)
	snapper, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains: []statelog.Registered{{
			Domain: from.domain,
			Health: func() statelog.Health {
				return statelog.Health{
					Position: statelog.Position{Stream: probeStream, Generation: 1, Seq: 4_200},
					Drained:  true,
					Lag:      &lag,
				}
			},
		}},
		DB:            donorDB,
		Dir:           snapDir,
		NodeID:        "donor",
		EngineVersion: "v0.0.0-test",
		Counted:       func(context.Context) (int, error) { return 3, nil },
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
		Newest: func() (statelog.Manifest, bool) {
			h.answered.CompareAndSwap(0, time.Now().UnixNano())
			return h.manifest, true
		},
		Path: func(statelog.Manifest) string { return h.snapPath },
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
	joiner, err := store.Open(t.Context(), filepath.Join(joinDir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the joiner's store: %v", err)
	}
	h.joiner = joiner
	h.joinPath = joiner.ReplicatedPath()
	t.Cleanup(func() { _ = h.joiner.Close() })
	return h
}

func (h *joinHarness) adopter(t *testing.T) *statelog.Adopter {
	t.Helper()
	h.answered.Store(0)
	a, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  map[string]statelog.Registered{"probe": {Domain: probeDomain{}}},
		LivePath: h.joinPath,
		NodeID:   "joiner",
		Conn:     h.nc,
		Need: func(context.Context) (statelog.OfferRequest, error) {
			return statelog.OfferRequest{
				Need:        map[string]uint64{"probe": 4_000},
				Generations: map[string]uint32{"probe": 1},
			}, nil
		},
		Hold: func(context.Context, map[string]uint64) (func(), error) {
			h.held.Add(1)
			return func() { h.released.Add(1) }, nil
		},
		Close: func(ctx context.Context) error {
			h.closes.Add(1)
			if h.onClose != nil {
				h.onClose()
			}
			if err := h.joiner.Close(); err != nil {
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
			db, err := store.Open(ctx, filepath.Join(filepath.Dir(h.joinPath), "node.db"),
				store.Options{})
			if err != nil {
				return err
			}
			h.joiner = db
			return nil
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

// answeredAt is when the harness's donor answered the join's ask, at the
// precision the store keeps an instant.
func (h *joinHarness) answeredAt(t *testing.T) time.Time {
	t.Helper()
	n := h.answered.Load()
	if n == 0 {
		t.Fatal("the donor never answered an ask, so this case has no answer " +
			"to hold the adoption's start against")
	}
	return time.Unix(0, n).UTC().Truncate(time.Microsecond)
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

	m, err := h.adopter(t).Join(t.Context())
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
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
	if err := h.joiner.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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

	// THE HOLD WAS TAKEN BEFORE THE FETCH AND RELEASED AFTER.
	if h.held.Load() != 1 || h.released.Load() != 1 {
		t.Fatalf("the replay tail was held %d time(s) and released %d — without "+
			"the hold the trim can pass the artefact while it is in flight, and "+
			"this node installs a snapshot whose tail is already gone",
			h.held.Load(), h.released.Load())
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
	// ALL UNDER ONE START, so one join is one row — and a start that
	// follows the donor's answer, which is what makes it the bound for a
	// join that stops partway: see
	// TestAJoinThatStoppedAfterItsInstallStillBoundsTheLedger.
	for _, began := range h.began {
		if !began.Equal(h.began[0]) {
			t.Fatalf("one join recorded its phases under the starts %v — each "+
				"is a row of its own", h.began)
		}
	}
	if asked := h.answeredAt(t); h.began[0].Before(asked) {
		t.Fatalf("the adoption starts at %s, before its donor answered at %s",
			h.began[0], asked)
	}
	// AND NOTHING OF THE PART FILE SURVIVES, the lock its inspection took
	// included.
	if left := h.debris(t); len(left) != 0 {
		t.Errorf("%v survive beside the live database", left)
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
// The cancellation lands at step 7's re-check, which is the last step before
// the install: late enough that a transfer really happened, early enough that
// the live database was never touched.
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
// Step 8 closes the live database and then runs the install, and when the
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
	if err := h.joiner.Replicated().SQL().PingContext(t.Context()); err != nil {
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

			_, err := h.adopter(t).Join(ctx)
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
			if h.held.Load() != h.released.Load() {
				t.Fatalf("the tail was held %d time(s) and released %d",
					h.held.Load(), h.released.Load())
			}
		})
	}
}

// A DONOR THAT SCRUBBED ITS LEDGER LEAVES THE ARTEFACT WITH A WATERMARK, WRITTEN
// BEFORE THE INSTALL, AT A START THAT FOLLOWS ITS ANSWER.
//
// A build from before the ledger travelled scrubs it out of every snapshot,
// and a rolling upgrade puts such a donor in front of this build's joiner. Its
// artefact holds none of the ledger's rows, so the adopter has to say how far
// back that ledger may have lost them — and say it IN THE FILE, before the
// rename: written into the live estate after the reopen, it would leave a
// window in which a publisher reads the empty ledger's silence as conclusive,
// and a join that installed and then failed to reopen would leave the file
// with no watermark at all. The start is the bound because it follows the
// moment the donor answered: the donor offers what it holds when it answers,
// so it may have finished the artefact an instant before, and an operation
// this node published earlier than THAT can be inside it with its row
// scrubbed.
func TestADonorThatScrubbedItsLedgerLeavesTheArtefactAWatermark(t *testing.T) {
	t.Parallel()

	t.Run("an adoption that completes", func(t *testing.T) {
		t.Parallel()
		h := newJoinHarnessFrom(t, joinDonor{domain: scrubbingProbe{}})
		if !slices.Contains(h.manifest.Scrubbed, "probe_ops") {
			t.Fatalf("the scrubbing donor's manifest lists %v scrubbed, without "+
				"its ledger — the case is not staging a donor that scrubs",
				h.manifest.Scrubbed)
		}
		if _, err := h.adopter(t).Join(t.Context()); err != nil {
			t.Fatalf("Join: %v", err)
		}
		if ops := h.joinerOps(t); ops != 0 {
			t.Fatalf("the adopted ledger holds %d row(s) from a donor that "+
				"scrubbed it", ops)
		}
		before, lost := h.joinerLostBefore(t)
		if !lost || !before.Equal(h.began[0].Truncate(time.Microsecond)) {
			t.Fatalf("the adopted ledger's watermark = (%s, %v), want the join's "+
				"own start %s — without it the empty ledger's silence reads as "+
				"conclusive, and every retry of an operation the donor applied is "+
				"decided a second time", before, lost, h.began[0])
		}
		if asked := h.answeredAt(t); before.Before(asked) {
			t.Fatalf("the watermark is %s, before the donor answered at %s — an "+
				"operation minted between the two can be inside the artefact with "+
				"its row scrubbed, and is re-decided", before, asked)
		}
	})

	// AND ONE THAT STOPPED AFTER ITS INSTALL: whatever opens the estate next
	// finds the donor's file current, with the watermark already in it,
	// because it was written before the rename rather than after the reopen
	// that never happened.
	t.Run("an adoption that stopped after its install", func(t *testing.T) {
		t.Parallel()
		h := newJoinHarnessFrom(t, joinDonor{domain: scrubbingProbe{}})
		h.reopenErr = errors.New("the artefact did not open")
		if _, err := h.adopter(t).Join(t.Context()); !errors.Is(err, statelog.ErrEstateNotRestored) {
			t.Fatalf("Join = %v, want the installed artefact not opening", err)
		}
		later, err := store.Open(t.Context(), filepath.Join(filepath.Dir(h.joinPath), "node.db"),
			store.Options{})
		if err != nil {
			t.Fatalf("open the node's store again: %v", err)
		}
		t.Cleanup(func() { _ = later.Close() })
		h.joiner = later
		at, _, _, err := statelog.CursorFor(t.Context(), later.Replicated(), probeStream)
		if err != nil {
			t.Fatalf("read the reopened estate's checkpoint: %v", err)
		}
		if at.Seq != 4_200 {
			t.Fatalf("the reopened estate is at %d, want the artefact's 4200 — the "+
				"case did not reach an installed artefact", at.Seq)
		}
		before, lost := h.joinerLostBefore(t)
		if !lost {
			t.Fatal("the installed artefact carries no watermark — its ledger is " +
				"the donor's scrubbed one, and every operation minted before the " +
				"join would be decided again")
		}
		if asked := h.answeredAt(t); before.Before(asked) {
			t.Fatalf("the watermark is %s, before the donor answered at %s", before, asked)
		}
	})
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
		domain: probeDomain{},
		seed: func(t *testing.T, db *store.DB) {
			if err := statelog.RecordLedgerLoss(t.Context(), db.Replicated(),
				probeDomain{}, swept); err != nil {
				t.Fatalf("record the donor's sweep: %v", err)
			}
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

// joinerOps is how many rows the joiner's operation ledger holds.
func (h *joinHarness) joinerOps(t *testing.T) int64 {
	t.Helper()
	var ops int64
	if err := h.joiner.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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
	rows, err := statelog.NewRows(h.joiner, probeDomain{}, nil)
	if err != nil {
		t.Fatalf("build the read seam: %v", err)
	}
	before, lost, err := rows.LostBefore(t.Context())
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
