package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	turso "turso.tech/database/tursogo"
)

// A DRIVER MEASURED ONCE IS NOT ASKED AGAIN — by a pool on the driver itself.
//
// The measurement here holds answers no probe of the pinned driver gives, so a
// pool answered with them was answered from the measurement and not by a
// probe. A pool on a WRAPPED driver is asking a different driver and must hear
// its own answers, and no answer handed out may share its Gated list with the
// measurement, or one handle could rewrite every later handle's.
//
// Mutation: probe whatever the measurement holds, and the sentinel answers are
// lost; answer a wrapped pool from it, and the wrapped pool reports them; hand
// out the measurement's own slice, and the rewrite below reaches it.
func TestADriverMeasuredOnceIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := probePool(t, Options{})
	m := &driverMeasurement{caps: &Capabilities{MaxVariables: 7, Gated: []string{"sentinel"}}}

	got, err := m.capabilities(ctx, pool, false)
	if err != nil {
		t.Fatalf("an answer from the measurement reported %v", err)
	}
	if got.MaxVariables != 7 || got.VectorFunctions {
		t.Errorf("a measured driver was probed again: MaxVariables=%d VectorFunctions=%v, "+
			"want the measurement's 7 and false", got.MaxVariables, got.VectorFunctions)
	}
	got.Gated[0] = "rewritten"
	if m.caps.Gated[0] != "sentinel" {
		t.Error("a handle's Gated list is the measurement's own: rewriting one " +
			"rewrote every later handle's")
	}

	own, err := m.capabilities(ctx, pool, true)
	if err != nil {
		t.Fatalf("a wrapped pool's own probe: %v", err)
	}
	if own.MaxVariables == 7 || !own.VectorFunctions {
		t.Errorf("a pool on a wrapped driver was answered from the measurement "+
			"(MaxVariables=%d VectorFunctions=%v) rather than by its own driver",
			own.MaxVariables, own.VectorFunctions)
	}
}

// ONLY A PROBE THE DRIVER ANSWERED IN FULL IS KEPT, because every probe turns a
// failure into the conservative answer and a kept one is every later handle's.
//
// A busy file is the realistic transient: the probe's CREATE TABLEs wait out
// the busy timeout behind another connection's write and answer "absent" for
// a reason that has nothing to do with the driver. A context that ended and a
// wrapped driver are the other two answers that are not the driver's. None of
// them may become the measurement; the first full probe of the plain driver
// must, and must be what that probe answered.
//
// Mutation: keep a probe whatever it heard, or keep a wrapped pool's, and the
// measurement is filled before the last step.
func TestOnlyAProbeTheDriverAnsweredInFullIsKept(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	m := &driverMeasurement{}

	busy := probePool(t, Options{BusyTimeout: 50 * time.Millisecond})
	holdWrite(t, busy)
	if _, err := m.capabilities(ctx, busy, false); !errors.Is(err, turso.ErrTursoBusy) {
		t.Fatalf("a probe of a file held by another writer reported %v, want the "+
			"driver's busy status — this case staged nothing", err)
	}
	if m.caps != nil {
		t.Fatalf("a probe that waited out a busy file was kept: %+v", *m.caps)
	}

	pool := probePool(t, Options{})
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.capabilities(ended, pool, false); err == nil {
		t.Error("a probe under a context that had ended reported every question answered")
	}
	if m.caps != nil {
		t.Fatalf("a probe under a context that had ended was kept: %+v", *m.caps)
	}

	if _, err := m.capabilities(ctx, pool, true); err != nil {
		t.Fatalf("a wrapped pool's probe: %v", err)
	}
	if m.caps != nil {
		t.Fatalf("a wrapped driver's answers were kept as the driver's: %+v", *m.caps)
	}

	got, err := m.capabilities(ctx, pool, false)
	if err != nil {
		t.Fatalf("a probe of an idle file: %v", err)
	}
	if m.caps == nil {
		t.Fatal("a probe the driver answered in full was not kept, so every open " +
			"pays for it again")
	}
	if !reflect.DeepEqual(*m.caps, got) {
		t.Errorf("the measurement is %+v, want what the probe that filled it "+
			"answered: %+v", *m.caps, got)
	}
}

// A REFUSAL IS AN ANSWER AND A FAILURE IS NOT, and the line between them is
// drawn on what the driver actually returns.
//
// Mutation: count any driver error as an answer, and a busy file's refusal is
// kept; count the generic status alone, and a failure the driver had no status
// for is kept with it.
func TestARefusalIsTheDriversAnswerAndAFailureIsNot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := probePool(t, Options{BusyTimeout: 50 * time.Millisecond})
	var d float64
	refused := pool.QueryRowContext(ctx, `SELECT crewlet_no_such_function(1)`).Scan(&d)

	held := probePool(t, Options{BusyTimeout: 50 * time.Millisecond})
	holdWrite(t, held)
	_, locked := held.ExecContext(ctx, `CREATE TABLE crewlet_probe_busy (a INTEGER)`)

	ended, cancel := context.WithCancel(ctx)
	cancel()
	_, cancelled := pool.PrepareContext(ended, `SELECT 1`)

	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"the parser refusing a statement", refused, true},
		{"a file another writer holds", locked, false},
		{"a context that ended", cancelled, false},
		{"the generic status without a parse refusal",
			fmt.Errorf("%w: database disk image is malformed", turso.ErrTursoGeneric), false},
	} {
		if c.err == nil {
			t.Fatalf("%s: the driver reported no error, so this case staged nothing", c.name)
		}
		if got := answered(c.err); got != c.want {
			t.Errorf("%s (%v): answered = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// A STORE ON A WRAPPED DRIVER PROBES FOR ITSELF, through the open a test
// fixture actually uses.
//
// The process's measurement is filled first, by a plain open, so the wrapped
// open below has one to take; its wrapper answers the vector functions' probe
// with the refusal a driver without them gives, which only its own probe can
// hear.
//
// Mutation: answer every pool from the measurement, and the wrapped store
// reports vector functions its driver refused.
func TestAStoreOnAWrappedDriverProbesForItself(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	plain, err := OpenNode(ctx, filepath.Join(t.TempDir(), "plain.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if !plain.Caps().VectorFunctions {
		t.Fatal("the pinned driver reports no vector functions, so this case cannot " +
			"tell a wrapped driver's answer from the measurement")
	}

	refuse := &driverHooks{query: func(q string) error {
		if strings.Contains(q, "vector_distance_cos") {
			return fmt.Errorf("%w: Parse error: no such function: vector_distance_cos",
				turso.ErrTursoGeneric)
		}
		return nil
	}}
	wrapped, err := OpenNode(ctx, filepath.Join(t.TempDir(), "wrapped.db"),
		Options{WrapDriver: refuse.wrap})
	if err != nil {
		t.Fatalf("open on a wrapped driver: %v", err)
	}
	defer func() { _ = wrapped.Close() }()
	if wrapped.Caps().VectorFunctions {
		t.Error("a store on a driver that refuses the vector functions reports them: " +
			"it was answered from the process's measurement of another driver")
	}
}

// probePool is a live pool on a fresh file of its own, with this package's
// session state and no schema, closed when the test ends.
func probePool(t *testing.T, opts Options) *sql.DB {
	t.Helper()
	pool, err := openPrepared(t.Context(), filepath.Join(t.TempDir(), "probe.db"), opts)
	if err != nil {
		t.Fatalf("open a pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// holdWrite takes the file's write lock on one of pool's connections and keeps
// it until the test ends, so every other write on the file waits out its busy
// timeout.
func holdWrite(t *testing.T, pool *sql.DB) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin the holding write: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), `CREATE TABLE crewlet_probe_holder (a INTEGER)`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
}
