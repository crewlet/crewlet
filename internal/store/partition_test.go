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
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A NODE'S OWN STORE HOLDS NO PARTITION UNTIL ONE IS OPENED ON IT.
//
// A node without the `data` role holds no partition at all, and a data node
// holds only the ones it has joined — so a path that reaches for a partition
// the node does not hold must be told so, rather than handed an empty database
// that reads as a company with nothing in it, which is the answer a seat acts
// on by filing a duplicate. And opening the node must create nothing beside
// it: a partition's file exists because a partition was opened.
func TestANodesStoreHoldsNoPartitionUntilOneIsOpened(t *testing.T) {
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
	if held := node.OpenPartitions(); len(held) != 0 {
		t.Errorf("a node nobody opened a partition on holds %v", held)
	}
	file := storetest.LayoutZero(1)
	if _, err := node.PartitionDB(file.Name); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a partition the node does not hold = %v, want ErrNoEstate", err)
	}
	estate := node.PartitionHandle(file.Name)
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through a partition the node does not hold = %v, want ErrNoEstate", err)
	}
	if _, err := os.Stat(node.PartitionPath(file)); !os.IsNotExist(err) {
		t.Errorf("opening the node created %s: %v", node.PartitionPath(file), err)
	}
}

// A PARTITION IS A FILE OF ITS OWN, opened on the node and carrying the
// partition sequence.
//
// The boundary is a file because of what rests on it: a snapshot is a copy of
// one partition, and a partition is the unit a node joins and leaves — taken
// from one file of everything, each of those would be a copy followed by a
// delete, and with no in-place VACUUM the deleted pages would ride along in the
// artefact, the transfer, the checksum and the integrity check.
func TestAPartitionIsAFileOfItsOwn(t *testing.T) {
	t.Parallel()
	node, part := openPartitioned(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 2)
	defer func() { _ = node.Close() }()

	if part.Estate() != store.EstatePartition {
		t.Errorf("the partition's handle is the %s estate", part.Estate())
	}
	if part.Path() == node.Path() {
		t.Fatal("the node and its partition name one file, so there is no boundary at all")
	}
	if _, err := os.Stat(part.Path()); err != nil {
		t.Errorf("the partition's file is not on disk: %v", err)
	}
	if got, want := part.File(), storetest.LayoutZero(2); got != want {
		t.Errorf("the partition's handle is the file %+v, want %+v", got, want)
	}
	if node.File() != (store.PartitionFile{}) {
		t.Errorf("the node's own handle names the partition file %+v", node.File())
	}
	if got := node.OpenPartitions(); !slices.Equal(got, []string{"estate.000"}) {
		t.Errorf("the node holds %v, want [estate.000]", got)
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
	partApplied, err := part.AppliedMigrations(t.Context())
	if err != nil {
		t.Fatalf("partition AppliedMigrations: %v", err)
	}
	if !slices.Equal(partApplied, store.SchemaVersions(store.EstatePartition)) {
		t.Errorf("the partition applied %v, want the partition sequence", partApplied)
	}

	// A PARTITION IS NOT A NODE: it holds no partition of its own, so a
	// caller holding one cannot reach a sibling through it and lose track
	// of which file it is writing.
	if _, err := part.OpenPartition(t.Context(), storetest.LayoutZero(1)); !errors.Is(err, store.ErrNotANode) {
		t.Errorf("a partition opened a partition: %v, want ErrNotANode", err)
	}
	if _, err := part.PartitionDB("estate.000"); !errors.Is(err, store.ErrNotANode) {
		t.Errorf("a partition answered a partition: %v, want ErrNotANode", err)
	}
}

// THE NODE AND EVERY PARTITION ARE DIFFERENT DATABASES, not handles on one.
//
// The whole design rests on it: no transaction spans two of them, no read joins
// across them, and a snapshot of one carries nothing of another. A table
// created in one must therefore be invisible in every other.
func TestEveryPartitionIsADifferentDatabase(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	seven, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 2})
	if err != nil {
		t.Fatalf("open tracker.007: %v", err)
	}
	eight, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.008", Logs: 2})
	if err != nil {
		t.Fatalf("open tracker.008: %v", err)
	}
	if err := seven.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE crewlet_estate_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("create in tracker.007: %v", err)
	}
	for name, other := range map[string]*store.DB{"the node": node, "tracker.008": eight} {
		err := other.Read(t.Context(), func(tx *sql.Tx) error {
			var n int
			return tx.QueryRowContext(t.Context(), `SELECT count(*) FROM crewlet_estate_probe`).Scan(&n)
		})
		if err == nil {
			t.Errorf("a table created in tracker.007 is readable from %s, so the two "+
				"handles are one database", name)
		} else if !strings.Contains(strings.ToLower(err.Error()), "crewlet_estate_probe") {
			t.Errorf("%s's refusal does not name the missing table: %v", name, err)
		}
	}
}

// LAYOUT 0's FILE IS WHERE THE REPLICATED ESTATE HAS ALWAYS BEEN, and a
// partitioned layout's files are beside it with the layout in their names.
//
// Layout 0's is the one [store.ReplicatedPath] names — beside the node's own,
// or wherever store.replicated_path puts it — so no node running layout 0 ever
// has its file moved. A partitioned layout's carry the layout number, so a
// repartition's files never share a name with the ones it replaces, and they
// are kept beside layout 0's because an operator who moved the replicated
// estate to a faster volume moved the estate.
func TestAPartitionIsWhereItsLayoutPutsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodePath := filepath.Join(dir, "node.db")
	zero := storetest.LayoutZero(1)
	one := store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 2}

	if got, want := store.PartitionPath(dir, 0, "estate.000"), store.ReplicatedPath(nodePath, ""); got != want {
		t.Errorf("layout 0's partition is at %q, and the replicated estate has always been at %q", got, want)
	}
	if got, want := store.PartitionPath(dir, 1, "tracker.007"), filepath.Join(dir, "l1-tracker.007.db"); got != want {
		t.Errorf("tracker.007 of layout 1 is at %q, want %q", got, want)
	}
	if store.PartitionPath(dir, 1, "tracker.007") == store.PartitionPath(dir, 2, "tracker.007") {
		t.Error("tracker.007 of two layouts is one file, so a repartition would reuse a name for a second history")
	}

	for _, c := range []struct {
		name       string
		configured bool
	}{
		{"derived beside the node's", false},
		{"named explicitly", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			nodeDir, elsewhere := t.TempDir(), t.TempDir()
			var opts store.Options
			wantZero := filepath.Join(nodeDir, "crewlet-replicated.db")
			if c.configured {
				opts.ReplicatedPath = filepath.Join(elsewhere, "estate.db")
				wantZero = opts.ReplicatedPath
			}
			node, err := store.OpenNode(t.Context(), filepath.Join(nodeDir, "node.db"), opts)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = node.Close() }()
			if got := node.PartitionPath(zero); got != wantZero {
				t.Errorf("layout 0's partition is at %q, want %q", got, wantZero)
			}
			wantOne := filepath.Join(filepath.Dir(wantZero), "l1-tracker.007.db")
			if got := node.PartitionPath(one); got != wantOne {
				t.Errorf("tracker.007 is at %q, want %q beside layout 0's", got, wantOne)
			}
			part, err := node.OpenPartition(t.Context(), one)
			if err != nil {
				t.Fatalf("open tracker.007: %v", err)
			}
			if part.Path() != wantOne {
				t.Errorf("tracker.007 opened at %q, not where the node says it lives (%q)",
					part.Path(), wantOne)
			}
		})
	}
}

// A CLOSED PARTITION STILL NAMES ITS FILE, and reopens at it.
//
// A partition is closed between an adoption's close and its reopen, and stays
// closed after a join that could not open it again — the one state in which an
// operator is told to go and look at that file. An empty path there logged the
// lost estate against no file at all, and a snapshot measuring its size
// stat'ed the empty string.
func TestAClosedPartitionStillNamesItsFile(t *testing.T) {
	t.Parallel()
	node, part := openPartitioned(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	defer func() { _ = node.Close() }()
	file := storetest.LayoutZero(1)
	want := part.Path()
	estate := node.PartitionHandle(file.Name)

	if err := node.ClosePartition(file.Name); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := node.PartitionPath(file); got != want {
		t.Fatalf("with the partition closed its path is %q, want %q — the file it reopens at", got, want)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through a closed partition = %v, want ErrNoEstate", err)
	}
	// ALREADY CLOSED IS NOT AN ERROR, so an unwind need not know how far
	// the join it undoes got.
	if err := node.ClosePartition(file.Name); err != nil {
		t.Errorf("closing a closed partition = %v, want nil", err)
	}

	reopened, err := node.OpenPartition(t.Context(), file)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Path() != want {
		t.Fatalf("the partition reopened at %q, not the %q it was at", reopened.Path(), want)
	}
	// THE HANDLE A HOLDER KEPT FOLLOWS THE REOPEN, because it resolves on
	// every call rather than capturing the file it saw first.
	if db, err := estate.DB(); err != nil || db != reopened {
		t.Errorf("the kept handle resolves to (%p, %v), want the reopened file %p", db, err, reopened)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a read through the reopened partition = %v", err)
	}
}

// OPENING WHAT IS OPEN ANSWERS IT; OPENING IT AS ANOTHER FILE IS REFUSED.
//
// A join that retries after a partial failure opens what is already open, and
// must get the handle it is open as rather than a second pool on one file. But
// the same name as a different file — another log count, so another pool — or
// a partition of another layout while this node holds one's, is a caller
// confused about what the node holds: a partition is keyed by its name, and two
// layouts name two histories alike.
func TestOpeningAPartitionTwiceAnswersTheOneOpen(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	seven := store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 2}
	first, err := node.OpenPartition(t.Context(), seven)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	again, err := node.OpenPartition(t.Context(), seven)
	if err != nil || again != first {
		t.Errorf("opening tracker.007 again = (%p, %v), want the open handle %p", again, err, first)
	}
	if _, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 1}); err == nil {
		t.Error("tracker.007 was opened again as a file carrying another log count")
	}
	if _, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 2, Name: "tracker.008", Logs: 2}); err == nil ||
		!strings.Contains(err.Error(), "layout 1") {
		t.Errorf("a partition of layout 2 opened beside layout 1's: %v", err)
	}
}

// A PARTITION FILE THIS PACKAGE CANNOT NAME OR SIZE IS REFUSED, saying why.
func TestAPartitionFileIsValidatedWhereItEnters(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		file store.PartitionFile
		ok   bool
	}{
		{"layout 0's one partition", store.PartitionFile{Layout: 0, Name: "estate.000", Logs: 3}, true},
		{"a partitioned layout's", store.PartitionFile{Layout: 1, Name: "tracker.255", Logs: 2}, true},
		{"layout 0 with another partition", store.PartitionFile{Layout: 0, Name: "tracker.000", Logs: 1}, false},
		{"estate.000 in a partitioned layout", store.PartitionFile{Layout: 1, Name: "estate.000", Logs: 1}, false},
		{"a negative layout", store.PartitionFile{Layout: -1, Name: "tracker.000", Logs: 1}, false},
		{"a name that is not a partition's", store.PartitionFile{Layout: 1, Name: "tracker.7", Logs: 1}, false},
		{"no name at all", store.PartitionFile{Layout: 1, Logs: 1}, false},
		{"no log to pin a writer for", store.PartitionFile{Layout: 1, Name: "tracker.001"}, false},
		{"the zero value", store.PartitionFile{}, false},
	} {
		if err := c.file.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate(%+v) = %v, want ok=%v", c.name, c.file, err, c.ok)
		}
	}
}

// DROPPING A PARTITION DELETES ITS FILE AND EVERY SIDECAR, and only its own.
//
// The last step of leaving a partition, once every loop that wrote it has
// stopped. A -wal left beside a deleted file is applied to whatever is created
// at that name next, so the sidecars go with it; a retry after a leave
// interrupted between its close and its delete finds the file not held and
// deletes it all the same; and a drop naming another file under the held name
// deletes nothing.
//
// The store's LOCK file is the one thing beside it that stays, and on purpose
// (see lock.go's release): unlinking it races a peer about to lock it. It is
// empty, holds no data, and is the one the next open of the name locks.
func TestDroppingAPartitionDeletesItsFileAndItsSidecars(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	seven := store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 2}
	part, err := node.OpenPartition(t.Context(), seven)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := part.Path()
	if err := part.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TABLE crewlet_drop_probe (id INTEGER PRIMARY KEY)`)
		return err
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := node.DropPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.007", Logs: 1}); err == nil {
		t.Fatal("a drop naming another file under the held name was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a refused drop deleted the held file: %v", err)
	}
	if _, err := node.PartitionDB(seven.Name); err != nil {
		t.Fatalf("a refused drop let go of the partition: %v", err)
	}

	if err := node.DropPartition(t.Context(), seven); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := node.PartitionDB(seven.Name); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a dropped partition = %v, want ErrNoEstate", err)
	}
	left, _ := filepath.Glob(path + "*")
	left = slices.DeleteFunc(left, func(p string) bool { return p == path+".lock" })
	if len(left) != 0 {
		t.Errorf("the drop left %v behind", left)
	}

	// A FILE THE NODE NO LONGER HOLDS IS DELETED ALL THE SAME.
	if _, err := node.OpenPartition(t.Context(), seven); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if err := node.ClosePartition(seven.Name); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := node.DropPartition(t.Context(), seven); err != nil {
		t.Fatalf("drop a partition the node no longer holds: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a partition dropped after its close is still on disk: %v", err)
	}
}

// A DROP NEVER DELETES A FILE ANOTHER HANDLE HOLDS.
//
// The claim on a path is shared inside this process, so the lock alone would
// let one node handle delete a file another handle here is reading; a drop is
// refused naming the file instead, and deletes nothing.
func TestADropNeverDeletesAFileAnotherHandleHolds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	node, _ := openPartitioned(t, path, store.Options{}, 1)
	defer func() { _ = node.Close() }()
	other, held := openPartitioned(t, path, store.Options{}, 1)
	defer func() { _ = other.Close() }()

	if err := node.DropPartition(t.Context(), storetest.LayoutZero(1)); err == nil {
		t.Fatal("a partition another handle in this process holds was dropped")
	}
	if err := held.Read(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("the other handle's partition is unreadable after a refused drop: %v", err)
	}
}

// PENDING REPORTS THE NODE AND EVERY PARTITION IT IS NAMED, because a deploy
// gate that named some of a node's files would pass while another is behind.
func TestPendingReportsTheNodeAndEveryPartition(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	files := []store.PartitionFile{storetest.LayoutZero(3)}
	schemas, err := store.Pending(t.Context(), path, store.Options{}, files...)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(schemas) != 1+len(files) {
		t.Fatalf("Pending reported %d files, want the node and %d partition(s)", len(schemas), len(files))
	}
	for i, sch := range schemas {
		if len(sch.Applied) != 0 {
			t.Errorf("a fresh %s file reports %d applied", sch.Estate, len(sch.Applied))
		}
		if !slices.Equal(sch.Pending, store.SchemaVersions(sch.Estate)) {
			t.Errorf("the %s file has %v pending, want its whole sequence", sch.Estate, sch.Pending)
		}
		if sch.Path == "" {
			t.Errorf("the %s file's report names no file", sch.Estate)
		}
		wantPartition := ""
		if i > 0 {
			wantPartition = files[i-1].Name
		}
		if sch.Partition != wantPartition {
			t.Errorf("report %d names the partition %q, want %q", i, sch.Partition, wantPartition)
		}
	}
	if schemas[1].Path != store.ReplicatedPath(path, "") {
		t.Errorf("layout 0's report is for %s, not the file the node opens", schemas[1].Path)
	}
}

// ONE FILE FOR THE NODE AND A PARTITION IS REFUSED BY THE CONFIG, and this is
// the engine-side half: two exclusive claims on one path do not deadlock, they
// migrate the partition sequence into the node's own database.
func TestOneFileForTheNodeAndAPartitionIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.db")
	node, err := store.OpenNode(t.Context(), path, store.Options{ReplicatedPath: path})
	if err != nil {
		t.Fatalf("open the node: %v", err)
	}
	defer func() { _ = node.Close() }()
	if _, err := node.OpenPartition(t.Context(), storetest.LayoutZero(1)); !errors.Is(err, store.ErrOneFile) {
		t.Errorf("layout 0's partition at the node's own path = %v, want ErrOneFile", err)
	}
}

// CLOSING THE NODE CLOSES EVERY PARTITION, and gives every lock back.
//
// A process holding a partition's lock with no handle left to release it is a
// state nothing asked for and nothing can recover from: the next process to
// open it is refused for a file nobody is using, and the error names this one.
func TestClosingANodeClosesEveryPartition(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	node, err := store.OpenNode(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	files := []store.PartitionFile{
		{Layout: 1, Name: "tracker.000", Logs: 2},
		{Layout: 1, Name: "pages.000", Logs: 2},
	}
	for _, f := range files {
		if _, err := node.OpenPartition(t.Context(), f); err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
	}
	estate := node.PartitionHandle("tracker.000")
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := estate.Read(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("a read through a partition of a closed node = %v, want ErrNoEstate", err)
	}
	// AND THE CLAIMS: each file reopens in a second node handle — which
	// proves little on its own, since a claim is shared inside a process,
	// and is why TestClosingANodeGivesBackEveryPartitionsClaim asserts the
	// claims themselves.
	again, err := store.OpenNode(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatalf("reopen the node: %v", err)
	}
	defer func() { _ = again.Close() }()
	for _, f := range files {
		if _, err := again.OpenPartition(t.Context(), f); err != nil {
			t.Errorf("reopen %s after its node closed: %v", f.Name, err)
		}
	}
}

// A PARTITION IS OPENED AT THE WIDTH THE NODE HAS LEARNED, and learns what
// the node learns afterwards.
//
// The width describes the vectors a node holds, whichever file they are in, so
// a file that did not know it would leave the dimension guard off for its rows
// alone — the two-widths state [store.DB.LearnEmbeddingDim] exists to prevent.
func TestEveryPartitionLearnsTheNodesEmbeddingWidth(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	early, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	node.LearnEmbeddingDim(64)
	if got := early.EmbeddingDim(); got != 64 {
		t.Errorf("a partition open when the node learned its width reports %d, want 64", got)
	}
	late, err := node.OpenPartition(t.Context(), store.PartitionFile{Layout: 1, Name: "tracker.001", Logs: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := late.EmbeddingDim(); got != 64 {
		t.Errorf("a partition opened after the node learned its width reports %d, want 64", got)
	}
}

// A PARTITION CLOSED UNDER A CALLER ANSWERS ErrNoEstate, like one never open.
//
// A partition's database is taken for one operation, and a leave or an
// adoption can close it while that operation is on its way in. The pool is
// then closed rather than nil, and database/sql's own "database is closed"
// would reach a caller whose branch for a partition that is not open never
// sees it — a reader that reports an unreadable estate on every tick of a
// normal shutdown, where it should report the state it is in.
func TestAPartitionClosedUnderACallerAnswersNoEstate(t *testing.T) {
	t.Parallel()
	node, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = node.Close() }()
	file := store.PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1}
	part, err := node.OpenPartition(t.Context(), file)
	if err != nil {
		t.Fatalf("open %s: %v", file.Name, err)
	}
	if err := node.ClosePartition(file.Name); err != nil {
		t.Fatalf("close %s: %v", file.Name, err)
	}
	for name, call := range map[string]func() error{
		"Read": func() error { return part.Read(t.Context(), func(*sql.Tx) error { return nil }) },
		"Tx":   func() error { return part.Tx(t.Context(), func(*sql.Tx) error { return nil }) },
		"Writer": func() error {
			_, err := part.Writer(t.Context())
			return err
		},
		"AppliedMigrations": func() error {
			_, err := part.AppliedMigrations(t.Context())
			return err
		},
		"Backup": func() error {
			_, err := part.Backup(t.Context(), filepath.Join(t.TempDir(), "copy.db"))
			return err
		},
	} {
		if err := call(); !errors.Is(err, store.ErrNoEstate) {
			t.Errorf("%s on a partition closed under its caller = %v, want ErrNoEstate", name, err)
		}
	}
}
