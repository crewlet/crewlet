package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A FOREIGN COMMIT CANNOT ABORT A WRITE TRANSACTION BETWEEN ITS READ AND ITS
// WRITE, which is the shape every state-log applier has and the one the
// driver's own begin does not survive.
//
// # What this is protecting
//
// Turso's BeginTx discards its options and execs a plain, DEFERRED "BEGIN".
// Under that begin a transaction that READS and then WRITES is aborted by any
// commit landing anywhere in the file in between — including into a table it
// never names — with "database snapshot is stale". The applier reads the rows
// below its batch, decides, then writes, so under contention it was being
// aborted and REPLAYING a four-thousand-row batch, once per foreign commit,
// with nothing but a WARN to say so.
//
// # Why it drives the driver directly rather than DB.Tx
//
// Two reasons, and both are about making the claim falsifiable. The retry loop
// sits above DB.Tx and would hide an abort behind a second attempt, so the
// assertion has to be the ERROR rather than a count. And the CONTROL — the
// deferred begin still aborting — is what stops this passing vacuously the
// day the driver gains MVCC and the hazard disappears; running both modes
// through the same wrapper is the only way to state "this one aborts and this
// one does not" as one fact.
func TestTheImmediateBeginIsWhatSurvivesAForeignCommit(t *testing.T) {
	t.Parallel()
	db := openBeginStore(t)
	ctx := t.Context()

	// THE CONTROL. The driver's own begin — a plain, DEFERRED "BEGIN" — and
	// under it the hazard is live. If this stops failing, the case below is
	// no longer evidence of anything and this test says so rather than
	// going quietly green.
	//
	// ISSUED BY HAND on one connection, because this package no longer has
	// a transaction that takes the deferred begin AND lets a write through:
	// its read transaction refuses writes (see beginModeConn.queryOnlyFor),
	// and a read-then-write is exactly what the control has to attempt.
	deferred, err := db.SQL().Conn(ctx)
	if err != nil {
		t.Fatalf("draw a connection: %v", err)
	}
	defer func() { _ = deferred.Close() }()
	if _, err := deferred.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("deferred begin: %v", err)
	}
	var n int
	if err := deferred.QueryRowContext(ctx, `SELECT count(*) FROM mine`).Scan(&n); err != nil {
		t.Fatalf("read under the deferred begin: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO theirs (v) VALUES ('foreign')`); err != nil {
		t.Fatalf("the foreign commit could not land under the deferred begin, so "+
			"this control staged nothing: %v", err)
	}
	_, err = deferred.ExecContext(ctx, `INSERT INTO mine (v) VALUES ('ours')`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "snapshot is stale") {
		t.Errorf("control: a read-then-write under the DEFERRED begin returned %v, "+
			"want the driver's stale-snapshot abort. Either the driver changed or "+
			"the begin is no longer the deferred one — the case below proves "+
			"nothing until this does", err)
	}
	_, _ = deferred.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)

	// AND THE MODE THIS PACKAGE ACTUALLY USES. Nil options is the write
	// mode, so the lock is held from BEGIN and the foreign commit cannot
	// land in the window at all — which is why it is attempted from
	// another goroutine and merely has to not break this transaction,
	// rather than being forced inline where it would simply block.
	immediate, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("immediate begin: %v", err)
	}
	if err := immediate.QueryRowContext(ctx, `SELECT count(*) FROM mine`).Scan(&n); err != nil {
		t.Fatalf("read under the immediate begin: %v", err)
	}
	foreign := make(chan struct{})
	go func() {
		defer close(foreign)
		// It will block on the lock this transaction holds and land
		// once it commits; what matters is that it is TRYING during
		// the window that used to be fatal.
		_, _ = db.SQL().ExecContext(ctx, `INSERT INTO theirs (v) VALUES ('foreign')`)
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := immediate.ExecContext(ctx, `INSERT INTO mine (v) VALUES ('ours')`); err != nil {
		t.Fatalf("a write transaction that read before it wrote was broken by a "+
			"concurrent writer: %v\n"+
			"\tthe begin mode is what prevents this — see internal/store/begin.go", err)
	}
	if err := immediate.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-foreign
}

// AND A READ TRANSACTION STILL TAKES NO WRITE LOCK, which is the other half of
// the same decision and the one that would be easy to lose.
//
// [DB.Read] passes ReadOnly so it gets the DEFERRED begin. If it took the
// immediate one, every multi-statement dashboard read would exclude the
// engine's writers for its whole duration — a correctness fix paid for with
// the throughput it was meant to protect. So this measures what a writer sees
// while a read transaction is open: it must not wait.
func TestAReadTransactionDoesNotExcludeAWriter(t *testing.T) {
	t.Parallel()
	db := openBeginStore(t)

	var wrote time.Duration
	err := db.Read(t.Context(), func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM mine`).Scan(&n); err != nil {
			return err
		}
		start := time.Now()
		if _, err := db.SQL().ExecContext(t.Context(), `INSERT INTO theirs (v) VALUES ('while reading')`); err != nil {
			return err
		}
		wrote = time.Since(start)
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The busy timeout is seconds; a writer that was excluded would spend
	// all of it and then fail. Anything on this side of a second is "did
	// not wait" without pinning a number the machine decides.
	if wrote > time.Second {
		t.Errorf("a writer waited %v while a read transaction was open, so Read is "+
			"taking the write lock at BEGIN; it must pass ReadOnly for the "+
			"deferred begin", wrote)
	}
}

// AND A WRITER THAT LOSES THE RACE LOSES IT HAVING DONE NOTHING.
//
// This is what makes the retry above cheap: under the deferred begin a loser
// discovers the contention at its FIRST WRITE, so its retry replays everything
// the body did up to that point. Taking the lock at BEGIN means the body has
// not run, so the retry is a wait rather than a replay — and the way to assert
// that from outside is that the body runs exactly once even when a second
// writer is hammering the same file.
func TestAContendedWriteRunsItsBodyOnce(t *testing.T) {
	t.Parallel()
	db := openBeginStore(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var foreign atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.SQL().ExecContext(t.Context(), `INSERT INTO theirs (v) VALUES ('hammer')`); err == nil {
				foreign.Add(1)
			}
		}
	}()
	// The writer is given the file first, so a run that staged no
	// contention fails as a staging problem rather than passing as an
	// answer about an idle store.
	deadline := time.Now().Add(10 * time.Second)
	for foreign.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	var bodies int
	err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		bodies++
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM mine`).Scan(&n); err != nil {
			return err
		}
		for i := range 200 {
			if _, err := tx.Exec(`INSERT INTO mine (v) VALUES (?)`, i); err != nil {
				return err
			}
		}
		return nil
	})
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("the write failed under a concurrent writer: %v", err)
	}
	if foreign.Load() == 0 {
		t.Fatal("the concurrent writer never committed, so this run staged no " +
			"contention and its answer would be about an idle store")
	}
	if bodies != 1 {
		t.Errorf("the body ran %d times against %d foreign commits; the lock is "+
			"taken at BEGIN, so a contended writer waits there and its body runs "+
			"once", bodies, foreign.Load())
	}
}

// openBeginStore is a node estate with two tables: one the transaction under
// test writes, and one only the foreign writer touches — which is the point,
// since the driver's write lock is DATABASE-level and a transaction is aborted
// by a commit to a table it never names.
func openBeginStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "begin.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE mine (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE theirs (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO mine (v) VALUES ('seed')`,
	} {
		if _, err := db.SQL().ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return db
}

// A READ TRANSACTION REFUSES A WRITE, and the connection it ran on writes
// again afterwards.
//
// [store.PartitionReader] is handed to every reader of a partition on the
// strength of being unable to write, and a partition written anywhere but its
// applier diverges from every peer that holds it — silently, since the rows
// look right on the node that wrote them. A type with no Tx method keeps a
// caller from asking for a write transaction; it does not keep a statement
// inside a read's own transaction from writing, and one did: an INSERT through
// a reader's Read committed. So the engine refuses it — and the refusal must
// not outlive the read, because pools hand the same connections to reads,
// writes, migrations and copies alike. ONE CONNECTION, so every step below is
// on the connection the read left behind.
func TestAReadTransactionRefusesAWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	node, err := store.OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"),
		store.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })
	if _, err := node.SQL().ExecContext(ctx, `CREATE TABLE mine (v TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	count := func(db interface {
		Read(context.Context, func(*sql.Tx) error) error
	}, table string) int {
		t.Helper()
		var n int
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n)
		}); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// THE NODE'S OWN READ.
	if err := node.Read(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO mine (v) VALUES ('from a read')`)
		return err
	}); err == nil {
		t.Error("a write inside the node's read transaction was not refused")
	}
	if n := count(node, "mine"); n != 0 {
		t.Errorf("a write inside a read transaction left %d row(s)", n)
	}
	// AND THE CONNECTION IT LEFT WRITES — in a transaction, and outside one.
	if err := node.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO mine (v) VALUES ('from a write')`)
		return err
	}); err != nil {
		t.Errorf("a write transaction on the connection a read used was refused: %v", err)
	}
	if _, err := node.SQL().ExecContext(ctx, `INSERT INTO mine (v) VALUES ('bare')`); err != nil {
		t.Errorf("a statement outside a transaction, after a read, was refused: %v", err)
	}
	_ = count(node, "mine")
	if _, err := node.Backup(ctx, filepath.Join(t.TempDir(), "copy.db")); err != nil {
		t.Errorf("a copy taken after a read was refused: %v", err)
	}
	// Every other way a statement reaches the connection outside a
	// transaction: a query that writes, and a statement prepared first.
	_ = count(node, "mine")
	var v string
	if err := node.SQL().QueryRowContext(ctx,
		`INSERT INTO mine (v) VALUES ('queried') RETURNING v`).Scan(&v); err != nil {
		t.Errorf("a query that writes, after a read, was refused: %v", err)
	}
	_ = count(node, "mine")
	stmt, err := node.SQL().PrepareContext(ctx, `INSERT INTO mine (v) VALUES ('prepared')`)
	if err != nil {
		t.Fatalf("prepare after a read: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	if _, err := stmt.ExecContext(ctx); err != nil {
		t.Errorf("a prepared write, after a read, was refused: %v", err)
	}

	// AND A PARTITION'S READER, which is the handle the rule is written on.
	file := store.PartitionFile{Layout: 1, Name: "tracker.000", Logs: 1}
	if _, err := node.OpenPartition(ctx, file); err != nil {
		t.Fatalf("open %s: %v", file.Name, err)
	}
	h := node.PartitionHandle(file.Name)
	if err := h.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE rows (v TEXT)`)
		return err
	}); err != nil {
		t.Fatalf("create in the partition: %v", err)
	}
	if err := h.Reader().Read(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO rows (v) VALUES ('from a reader')`)
		return err
	}); err == nil {
		t.Error("a write through a partition's reader was not refused")
	}
	if n := count(h.Reader(), "rows"); n != 0 {
		t.Errorf("a write through a partition's reader left %d row(s)", n)
	}
}
