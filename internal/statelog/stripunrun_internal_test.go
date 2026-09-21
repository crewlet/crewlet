package statelog

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/store"
)

// A NODE ADOPTS AN ARTEFACT FROM A DONOR THAT RUNS MORE THAN IT DOES, and what
// it installs must not carry the domains it declined.
//
// Both halves are the case. The ROWS are the point — a satellite told not to
// run a directory of people must not end up holding every one of them on its
// own disk because a donor happened to have them — and the CHECKPOINT is the
// half that is easy to forget and worse to get wrong: left behind it says this
// node applied up to a position, so the day an operator gives the node that
// role its applier resumes above every record whose rows were just deleted.
//
// And the TABLES STAY. Dropping them reads like the same thing and is not:
// migrations key on their filename, so a table dropped out of a file is one no
// migration ever recreates, and the node that later declares the role finds
// the migration applied and the table gone for good.
func TestAnArtefactCarryingADomainThisNodeDoesNotRunIsInstalledStripped(t *testing.T) {
	t.Parallel()
	path := donorFile(t)

	a := &Adopter{
		deps: AdoptDeps{Unrun: []Domain{unrunDomain{}}},
		log:  slog.New(slog.DiscardHandler),
	}
	stripped, err := a.stripUnrun(t.Context(), path)
	if err != nil {
		t.Fatalf("stripUnrun: %v", err)
	}
	if !slices.Equal(stripped, []string{"pages"}) {
		t.Errorf("stripped = %v, want the one domain this node does not run", stripped)
	}

	// THE ROWS ARE GONE and the neighbouring domain's are untouched: a
	// strip that emptied the file would pass a check that only looked at
	// what it meant to remove.
	if got := rowsIn(t, path, "pages_titles"); got != 0 {
		t.Errorf("pages_titles holds %d rows after the strip", got)
	}
	if got := rowsIn(t, path, "tracker_history"); got != 1 {
		t.Errorf("tracker_history holds %d rows, want the row this node DOES run", got)
	}

	// THE CHECKPOINT IS GONE, and only that domain's.
	if _, ok := cursorRow(t, path, "CREWLET_PAGES_LOG"); ok {
		t.Error("the stripped domain kept its checkpoint, so an applier started " +
			"later resumes above every record whose rows this just deleted")
	}
	if _, ok := cursorRow(t, path, "CREWLET_TRACKER_LOG"); !ok {
		t.Error("the strip removed the checkpoint of a domain this node runs")
	}

	// AND THE TABLES ARE STILL THERE, empty, which is what a node that
	// never adopted anything has.
	if got := rowsIn(t, path, "pages_titles"); got != 0 {
		t.Errorf("pages_titles is unreadable after the strip (%d)", got)
	}
}

// TestNothingToStripTouchesTheFileAtAll, because a node that runs every domain
// is the ordinary case and opening, deleting from and checkpointing an
// artefact it has no business editing is a write nobody asked for.
func TestNothingToStripTouchesTheFileAtAll(t *testing.T) {
	t.Parallel()
	path := donorFile(t)
	before, err := store.FileDigest(path)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	a := &Adopter{log: slog.New(slog.DiscardHandler)}
	stripped, err := a.stripUnrun(t.Context(), path)
	if err != nil {
		t.Fatalf("stripUnrun: %v", err)
	}
	if len(stripped) != 0 {
		t.Errorf("stripped = %v on a node that runs everything", stripped)
	}
	after, err := store.FileDigest(path)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if before != after {
		t.Error("a node with nothing to strip rewrote the artefact, which is a " +
			"write between the donor's checksum and the install")
	}
}

// TestADomainDeclaredBothRunAndUnrunIsRefusedAtConstruction. The two answers
// are opposite and the wrong one is silent: the node would strip a domain it
// is about to start an applier for and come up on an empty table at position
// zero, which looks exactly like a node that is merely new.
func TestADomainDeclaredBothRunAndUnrunIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	deps := AdoptDeps{
		Domains:  map[string]Registered{"pages": {Domain: unrunDomain{}}},
		Unrun:    []Domain{unrunDomain{}},
		LivePath: filepath.Join(t.TempDir(), "node.db"),
		Conn:     &nats.Conn{},
		Need: func(context.Context) (OfferRequest, error) {
			return OfferRequest{}, nil
		},
		Hold: func(context.Context, map[string]uint64) (func(), error) {
			return func() {}, nil
		},
		Close:  func(context.Context) error { return nil },
		Reopen: func(context.Context) error { return nil },
		Record: func(context.Context, string, Manifest, AdoptionPhase) error { return nil },
	}
	if _, err := NewAdopter(deps); err == nil {
		t.Fatal("a domain declared both run and not run built an adopter")
	}

	// THE CONTROL: the same deps without the contradiction build.
	deps.Unrun = nil
	if _, err := NewAdopter(deps); err != nil {
		t.Fatalf("the same deps without the contradiction were refused: %v", err)
	}
}

// donorFile is a replicated estate as a donor's copy would arrive: rows and a
// checkpoint for two domains, one of which this node does not run.
func donorFile(t *testing.T) string {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := db.ReplicatedPath()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stream := range []string{"CREWLET_PAGES_LOG", "CREWLET_TRACKER_LOG"} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at)
				VALUES (?, 1, 42, ?, ?)`,
				stream, time.Unix(1_700_000_000, 0).UnixMicro(),
				time.Unix(1_700_000_000, 0).UnixMicro()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO pages_titles (container, title_token, title_norm, page_id, created_at, version)
			VALUES ('c', 'tok', 'a title', 'p', 0, 1)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_history
			    (id, subject_kind, subject_id, kind, log_seq, log_stream, created_at, document)
			VALUES ('h', 'task', 'x', 'created', 1, 'CREWLET_TRACKER_LOG', 0, x'7b7d')`)
		return err
	}); err != nil {
		t.Fatalf("seed the donor's copy: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func rowsIn(t *testing.T, path, table string) int {
	t.Helper()
	db, err := store.OpenEstate(t.Context(), store.EstateReplicated, path, store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func cursorRow(t *testing.T, path, stream string) (FileCursor, bool) {
	t.Helper()
	cursors, err := CursorsInFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read the checkpoints: %v", err)
	}
	c, ok := cursors[stream]
	return c, ok
}

// unrunDomain stands in for a domain this node does not run. It names REAL
// tables, because the strip refuses a name the file does not have — which is
// the property that stops a renamed table travelling under an old name.
type unrunDomain struct{}

func (unrunDomain) Name() string { return "pages" }
func (unrunDomain) Stream() StreamSpec {
	return StreamSpec{Name: "CREWLET_PAGES_LOG", Replay: ReplayStrict}
}
func (unrunDomain) RecordVersion() int                { return 1 }
func (unrunDomain) Envelope([]byte) (Envelope, error) { return Envelope{}, nil }
func (unrunDomain) InstallsGate(Envelope) bool        { return false }
func (unrunDomain) Tables() map[string]TableClass {
	return map[string]TableClass{
		"pages_titles":             Replicated,
		"pages_log_deferred":       Local,
		"pages_log_deferred_scope": Local,
		"pages_ops":                Local,
	}
}
func (unrunDomain) DeferredTable() string { return "pages_log_deferred" }
func (unrunDomain) ScopeIndex() string    { return "pages_log_deferred_scope" }
func (unrunDomain) OpsTable() string      { return "pages_ops" }
func (unrunDomain) ReadinessInput() bool  { return true }
func (unrunDomain) ClaimsIdentity() bool  { return true }

// AN ARTEFACT FROM A BUILD THAT PREDATES A DOMAIN IS STILL ADOPTED.
//
// Its file has no tables for that domain at all, and refusing it would mean
// this node declined a snapshot over rows for a domain IT DOES NOT EVEN RUN.
// The donor-side scrub refuses a table it cannot find, deliberately — a
// drifted list there means private rows travel under a claim they were
// removed — and this is the one caller for which that is the wrong answer.
func TestAnArtefactWithoutTheDeclinedDomainsTablesIsStillAdopted(t *testing.T) {
	t.Parallel()
	path := donorFile(t)
	dropPagesTables(t, path)

	a := &Adopter{
		deps: AdoptDeps{Unrun: []Domain{unrunDomain{}}},
		log:  slog.New(slog.DiscardHandler),
	}
	stripped, err := a.stripUnrun(t.Context(), path)
	if err != nil {
		t.Fatalf("an artefact with no tables for the declined domain was refused: %v", err)
	}
	if !slices.Equal(stripped, []string{"pages"}) {
		t.Errorf("stripped = %v, want the declined domain named anyway — its "+
			"checkpoint was there even though its tables were not", stripped)
	}
	if _, ok := cursorRow(t, path, "CREWLET_PAGES_LOG"); ok {
		t.Error("the checkpoint survived, so an applier started later would resume " +
			"above records whose rows this file never had")
	}
	if got := rowsIn(t, path, "tracker_history"); got != 1 {
		t.Errorf("tracker_history holds %d rows, want the row this node DOES run", got)
	}
}

// TestStrippingTwiceIsStrippingOnce, because the step runs between a crash and
// a retry like every other one in the join: the second pass finds the tables
// already empty, and a refusal there would leave a node unable to adopt
// anything for as long as that artefact was the best on offer.
func TestStrippingTwiceIsStrippingOnce(t *testing.T) {
	t.Parallel()
	path := donorFile(t)
	a := &Adopter{
		deps: AdoptDeps{Unrun: []Domain{unrunDomain{}}},
		log:  slog.New(slog.DiscardHandler),
	}
	if _, err := a.stripUnrun(t.Context(), path); err != nil {
		t.Fatalf("first strip: %v", err)
	}
	if _, err := a.stripUnrun(t.Context(), path); err != nil {
		t.Fatalf("a second strip of the same file was refused: %v", err)
	}
	if got := rowsIn(t, path, "pages_titles"); got != 0 {
		t.Errorf("pages_titles holds %d rows after two strips", got)
	}
}

// dropPagesTables makes the file look like one a build predating the domain
// wrote: the tables are simply not there.
func dropPagesTables(t *testing.T, path string) {
	t.Helper()
	db, err := store.OpenEstate(t.Context(), store.EstateReplicated, path, store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for table := range (unrunDomain{}).Tables() {
			if _, err := tx.ExecContext(t.Context(), `DROP TABLE `+table); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("drop the declined domain's tables: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
