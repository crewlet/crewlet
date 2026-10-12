package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A NODE'S OWN STORE HOLDS NO REPLICATED ESTATE UNTIL IT IS OPENED ON IT.
//
// A node without the `data` role holds no replicated estate at all — so a
// path that reaches for it there must be told so, rather than handed an empty
// database that reads as a company with nothing in it, which is the answer a
// seat acts on by filing a duplicate. And opening the node must create nothing
// beside it: the replicated estate's file exists because it was opened.
func TestANodesStoreHoldsNoReplicatedEstateUntilItIsOpened(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	node, err := store.OpenNode(t.Context(), filepath.Join(dir, "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()

	if node.Estate() != store.EstateNode {
		t.Errorf("OpenNode returned the %s estate, want the node's", node.Estate())
	}
	if _, err := node.ReplicatedDB(); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a replicated estate the node never opened = %v, want ErrNoEstate", err)
	}
	estate := node.Replicated()
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through a replicated estate the node never opened = %v, want ErrNoEstate", err)
	}
	if _, err := os.Stat(node.ReplicatedFile()); !os.IsNotExist(err) {
		t.Errorf("opening the node created %s: %v", node.ReplicatedFile(), err)
	}
}

// THE REPLICATED ESTATE IS A FILE OF ITS OWN, opened on the node, carrying its
// own sequence, and a DIFFERENT DATABASE from the node's rather than a second
// handle on one.
//
// The boundary is a file because of what rests on it: a snapshot is a copy of
// the replicated estate, and taken from one file of everything it would be a
// copy followed by a delete, and with no in-place VACUUM the deleted pages
// would ride along in the artefact, the transfer, the checksum and the
// integrity check. No transaction spans the two and no read joins across them,
// so a table created in one must be invisible in the other.
func TestTheReplicatedEstateIsAFileOfItsOwn(t *testing.T) {
	t.Parallel()
	node, replicated := openReplicated(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 2)
	defer func() { _ = node.Close() }()

	if replicated.Estate() != store.EstateReplicated {
		t.Errorf("the replicated estate's handle is the %s estate", replicated.Estate())
	}
	if replicated.Path() == node.Path() {
		t.Fatal("the node and its replicated estate name one file, so there is no boundary at all")
	}
	if _, err := os.Stat(replicated.Path()); err != nil {
		t.Errorf("the replicated estate's file is not on disk: %v", err)
	}
	if got, err := node.ReplicatedDB(); err != nil || got != replicated {
		t.Errorf("the node answers its replicated estate with (%p, %v), want the open %p",
			got, err, replicated)
	}

	// TWO SEQUENCES, INDEPENDENTLY NUMBERED. schema_migrations is per file,
	// so `0001` in one and `0001` in the other are two migrations and
	// neither can mask the other.
	nodeApplied, err := node.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("node AppliedMigrations: %v", err)
	}
	if !slices.Equal(nodeApplied, store.SchemaVersions(store.EstateNode)) {
		t.Errorf("the node applied %v, want its own sequence", nodeApplied)
	}
	replicatedApplied, err := replicated.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("replicated AppliedMigrations: %v", err)
	}
	if !slices.Equal(replicatedApplied, store.SchemaVersions(store.EstateReplicated)) {
		t.Errorf("the replicated estate applied %v, want the replicated sequence", replicatedApplied)
	}

	// THE REPLICATED ESTATE IS NOT A NODE: it holds no replicated estate of
	// its own, so a caller holding its handle cannot open a second file
	// through it and lose track of which one it is writing.
	if _, err := replicated.OpenReplicated(t.Context(), 1); !errors.Is(err, store.ErrNotANode) {
		t.Errorf("the replicated estate opened a replicated estate: %v, want ErrNotANode", err)
	}
	if _, err := replicated.ReplicatedDB(); !errors.Is(err, store.ErrNotANode) {
		t.Errorf("the replicated estate answered a replicated estate: %v, want ErrNotANode", err)
	}

	if err := replicated.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE crewlet_estate_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create in the replicated estate: %v", err)
	}
	err = node.Read(t.Context(), func(tx *sql.Tx) error {
		var n int
		return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM crewlet_estate_probe`).Scan(&n)
	})
	if err == nil {
		t.Error("a table created in the replicated estate is readable from the node's " +
			"own file, so the two handles are one database")
	} else if !strings.Contains(strings.ToLower(err.Error()), "crewlet_estate_probe") {
		t.Errorf("the node's refusal does not name the missing table: %v", err)
	}
}

// THE REPLICATED ESTATE IS WHERE [store.ReplicatedPath] PUTS IT — beside the
// node's own, or wherever store.replicated_path puts it, under the name it
// gives — and the node answers that path rather than one derived from a
// directory alone.
func TestTheReplicatedEstateIsWhereTheNodesConfigurationPutsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		configured bool
	}{
		{"derived beside the node's", false},
		// UNDER A NAME OF ITS OWN, which is the case a path derived from
		// a directory alone gets wrong: the file is wherever
		// store.replicated_path says, called whatever it is called there.
		{"named explicitly", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			nodeDir, elsewhere := t.TempDir(), t.TempDir()
			var opts store.Options
			nodePath := filepath.Join(nodeDir, "node.db")
			want := filepath.Join(nodeDir, "crewlet-replicated.db")
			if c.configured {
				opts.ReplicatedPath = filepath.Join(elsewhere, "estate.db")
				want = opts.ReplicatedPath
			}
			if got := store.ReplicatedPath(nodePath, opts.ReplicatedPath); got != want {
				t.Errorf("the replicated estate is at %q, want %q", got, want)
			}
			node, err := store.OpenNode(t.Context(), nodePath, opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = node.Close() }()
			if got := node.ReplicatedFile(); got != want {
				t.Errorf("the node keeps its replicated estate at %q, want %q", got, want)
			}
			replicated, err := node.OpenReplicated(t.Context(), 1)
			if err != nil {
				t.Fatalf("open the replicated estate: %v", err)
			}
			if replicated.Path() != want {
				t.Errorf("the replicated estate opened at %q, not where the node says it "+
					"lives (%q)", replicated.Path(), want)
			}
			if got := replicated.ReplicatedFile(); got != want {
				t.Errorf("the replicated estate's own handle names %q, want %q", got, want)
			}
		})
	}
}

// A CLOSED REPLICATED ESTATE STILL NAMES ITS FILE, and reopens at it.
//
// The estate is closed between an adoption's close and its reopen, and stays
// closed after a join that could not open it again — the one state in which an
// operator is told to go and look at that file. An empty path there logged the
// lost estate against no file at all, and a snapshot measuring its size
// stat'ed the empty string.
func TestAClosedReplicatedEstateStillNamesItsFile(t *testing.T) {
	t.Parallel()
	node, replicated := openReplicated(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = node.Close() }()
	want := replicated.Path()
	estate := node.Replicated()

	if err := node.CloseReplicated(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := node.ReplicatedFile(); got != want {
		t.Fatalf("with the replicated estate closed its path is %q, want %q — the file it reopens at", got, want)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through a closed replicated estate = %v, want ErrNoEstate", err)
	}
	// ALREADY CLOSED IS NOT AN ERROR, so an unwind need not know how far
	// the join it undoes got.
	if err := node.CloseReplicated(); err != nil {
		t.Errorf("closing a closed replicated estate = %v, want nil", err)
	}

	reopened, err := node.OpenReplicated(t.Context(), 1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Path() != want {
		t.Fatalf("the replicated estate reopened at %q, not the %q it was at", reopened.Path(), want)
	}
	// THE HANDLE A HOLDER KEPT FOLLOWS THE REOPEN, because it resolves on
	// every call rather than capturing the file it saw first.
	if db, err := estate.DB(); err != nil || db != reopened {
		t.Errorf("the kept handle resolves to (%p, %v), want the reopened file %p", db, err, reopened)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a read through the reopened replicated estate = %v", err)
	}
}

// OPENING WHAT IS OPEN ANSWERS IT; OPENING IT FOR ANOTHER COUNT IS REFUSED,
// AND SO IS A COUNT OF NO LOGS.
//
// A boot and a restore that both open the estate open what is already open,
// and must get the handle it is open as rather than a second pool on one file.
// But another log count is another pool, sized for writers the open one has no
// room for. And an estate carrying no log has no apply loop to pin a writer,
// which is a caller that has not said what runs on it.
func TestOpeningTheReplicatedEstateTwiceAnswersTheOneOpen(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	if _, err := node.OpenReplicated(t.Context(), 0); err == nil {
		t.Error("the replicated estate was opened for no log at all")
	}
	first, err := node.OpenReplicated(t.Context(), 2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	again, err := node.OpenReplicated(t.Context(), 2)
	if err != nil || again != first {
		t.Errorf("opening the replicated estate again = (%p, %v), want the open handle %p", again, err, first)
	}
	if _, err := node.OpenReplicated(t.Context(), 1); err == nil {
		t.Error("the replicated estate was opened again for another log count")
	}
}

// PENDING REPORTS BOTH FILES, the node's own and its replicated estate where
// the node opens it, because a deploy gate that named one of them would pass
// while the other is behind.
func TestPendingReportsBothFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "node.db")
	opts := store.Options{ReplicatedPath: filepath.Join(t.TempDir(), "elsewhere.db")}
	schemas, err := store.Pending(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(schemas) != 2 {
		t.Fatalf("Pending reported %d files, want the node's and the replicated estate's", len(schemas))
	}
	for i, want := range []struct {
		estate store.Estate
		path   string
	}{
		{store.EstateNode, path},
		// WHERE THE NODE OPENS IT, store.replicated_path included: a
		// report derived from store.path alone gated a fresh file nothing
		// opens and left the real one as far behind as it found it.
		{store.EstateReplicated, opts.ReplicatedPath},
	} {
		sch := schemas[i]
		if sch.Estate != want.estate || sch.Path != want.path {
			t.Errorf("report %d is the %s file at %s, want the %s file at %s",
				i, sch.Estate, sch.Path, want.estate, want.path)
		}
		if len(sch.Applied) != 0 {
			t.Errorf("a fresh %s file reports %d applied", sch.Estate, len(sch.Applied))
		}
		if !slices.Equal(sch.Pending, store.SchemaVersions(sch.Estate)) {
			t.Errorf("the %s file has %v pending, want its whole sequence", sch.Estate, sch.Pending)
		}
	}
}

// ONE FILE FOR THE NODE AND THE REPLICATED ESTATE IS REFUSED BY THE CONFIG,
// and this is the engine-side half: two exclusive claims on one path do not
// deadlock, they migrate the replicated sequence into the node's own database.
func TestOneFileForTheNodeAndTheReplicatedEstateIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	node, err := store.OpenNode(t.Context(), path, store.Options{ReplicatedPath: path})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	if _, err := node.OpenReplicated(t.Context(), 1); !errors.Is(err, store.ErrOneFile) {
		t.Errorf("the replicated estate at the node's own path = %v, want ErrOneFile", err)
	}
}

// CLOSING THE NODE CLOSES ITS REPLICATED ESTATE, and gives its lock back.
//
// A process holding the replicated file's lock with no handle left to release
// it is a state nothing asked for and nothing can recover from: the next
// process to open it is refused for a file nobody is using, and the error
// names this one.
func TestClosingANodeClosesItsReplicatedEstate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	node, _ := openReplicated(t, path, store.Options{}, 2)
	estate := node.Replicated()
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through the replicated estate of a closed node = %v, want ErrNoEstate", err)
	}
	// AND THE CLAIM: the file reopens in a second node handle — which
	// proves little on its own, since a claim is shared inside a process,
	// and is why TestClosingANodeGivesBackItsReplicatedEstatesClaim asserts
	// the claims themselves.
	again, err := store.OpenNode(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen the node: %v", err)
	}
	defer func() { _ = again.Close() }()
	if _, err := again.OpenReplicated(t.Context(), 2); err != nil {
		t.Errorf("reopen the replicated estate after its node closed: %v", err)
	}
}

// THE REPLICATED ESTATE IS OPENED AT THE WIDTH THE NODE HAS LEARNED, and
// learns what the node learns afterwards.
//
// The width describes the vectors a node holds, whichever file they are in, so
// a file that did not know it would leave the dimension guard off for its rows
// alone — the two-widths state [store.DB.LearnEmbeddingDim] exists to prevent.
// An adoption closes and reopens the file under the node, so the reopen must
// come back at the width the node learned while it was open.
func TestTheReplicatedEstateLearnsTheNodesEmbeddingWidth(t *testing.T) {
	t.Parallel()
	node, early := openReplicated(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = node.Close() }()
	node.LearnEmbeddingDim(64)
	if got := early.EmbeddingDim(); got != 64 {
		t.Errorf("a replicated estate open when the node learned its width reports %d, want 64", got)
	}
	if err := node.CloseReplicated(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := node.OpenReplicated(t.Context(), 1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := reopened.EmbeddingDim(); got != 64 {
		t.Errorf("a replicated estate reopened after the node learned its width reports %d, want 64", got)
	}
}

// A REPLICATED ESTATE CLOSED UNDER A CALLER ANSWERS ErrNoEstate, like one
// never open.
//
// The estate's database is taken for one operation, and an adoption can close
// it while that operation is on its way in. The pool is then closed rather
// than nil, and database/sql's own "database is closed" would reach a caller
// whose branch for an estate that is not open never sees it — a reader that
// reports an unreadable estate on every tick of a normal shutdown, where it
// should report the state it is in.
func TestAReplicatedEstateClosedUnderACallerAnswersNoEstate(t *testing.T) {
	t.Parallel()
	node, replicated := openReplicated(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = node.Close() }()
	if err := node.CloseReplicated(); err != nil {
		t.Fatalf("close the replicated estate: %v", err)
	}
	for name, call := range map[string]func() error{
		"Read": func() error { return replicated.Read(t.Context(), func(*sql.Tx) error { return nil }) },
		"Tx":   func() error { return replicated.Tx(t.Context(), func(*sql.Tx) error { return nil }) },
		"Writer": func() error {
			_, err := replicated.Writer(t.Context())
			return err
		},
		"AppliedMigrations": func() error {
			_, err := replicated.AppliedMigrations(t.Context())
			return err
		},
		"Backup": func() error {
			_, err := replicated.Backup(t.Context(), filepath.Join(t.TempDir(), "copy.db"))
			return err
		},
	} {
		if err := call(); !errors.Is(err, store.ErrNoEstate) {
			t.Errorf("%s on a replicated estate closed under its caller = %v, want ErrNoEstate", name, err)
		}
	}
}

// A REPLICATED ESTATE CLOSED THROUGH ITS OWN HANDLE LEAVES ITS NODE'S SLOT.
//
// The estate is held by its node, and the runtime asks the node whether it
// has lost it — an estate a lookup answers is one it never reopens. So an
// estate closed behind the node's back, through the handle the node gave out,
// must stop being answered as open: a lookup is told ErrNoEstate, and opening
// it again opens it rather than handing back the handle that was closed.
func TestAReplicatedEstateClosedThroughItsOwnHandleLeavesTheSlot(t *testing.T) {
	t.Parallel()
	node, replicated := openReplicated(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = node.Close() }()
	if err := replicated.Close(); err != nil {
		t.Fatalf("close the replicated estate through its own handle: %v", err)
	}

	if got, err := node.ReplicatedDB(); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a replicated estate closed through its own handle is still answered: "+
			"ReplicatedDB = %p, %v, want ErrNoEstate", got, err)
	}
	again, err := node.OpenReplicated(t.Context(), 1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again == replicated {
		t.Fatal("opening the file again handed back the handle that was closed")
	}
	if err := again.Read(t.Context(), func(tx *sql.Tx) error {
		var n int
		return tx.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n)
	}); err != nil {
		t.Errorf("the reopened replicated estate does not read: %v", err)
	}
}
