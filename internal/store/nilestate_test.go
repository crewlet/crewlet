package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A HANDLE THAT IS NOT OPEN ANSWERS. It does not take the process with it.
//
// # The contract, and why it needed a gate
//
// [store.ErrNoEstate]'s own doc states it: "IT IS A STATE, NOT A BUG IN THE
// CALLER. DB.PartitionDB answers it while an adoption holds a partition closed
// between its rename and its reopen, for a partition this node does not hold,
// and after DB.Close — all documented and deliberate — so a goroutine that was
// already in flight when one of those happened reaches here legitimately. What
// it must NOT reach is a nil dereference: a maintenance tick racing a shutdown
// panicked the engine."
//
// It reached one anyway, a second time, and the stack is worth keeping because
// every layer above it was already correct:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	[signal SIGSEGV: segmentation violation code=0x1 addr=0x0]
//	store.(*DB).Read(0x0, …)                      store/pinned.go:167
//	tracker.Evictions(…, 0x0, …)                  tracker/evictions.go:66
//	engine.(*retention).tombstones(…)             engine/retention.go:591
//	engine.(*retention).tick(…)                   engine/retention.go:267
//	created by engine.(*Engine).startRetention
//
// `internal/engine`'s caller already switched on [store.ErrNoEstate] with a
// comment explaining that a tick in flight reaches a closing estate;
// `internal/tracker`'s already documented that it goes through the store
// handle rather than a `*sql.DB` precisely so it would get that error. Both
// were right. The guard was in [DB.txOpts] — one frame too deep — and
// `pooled(d.busy)` in [DB.Read] and [DB.Tx] is an ARGUMENT, evaluated before
// the call it guards. So the nil was dereferenced on the way in and the guard
// never ran. Ten of this type's eighteen exported methods behaved that way.
//
// # Why the roster is reflected rather than listed
//
// A hand-written table certifies the methods somebody remembered, and the next
// method added to this type is unguarded and silent — which is how the first
// fix came to cover `txOpts` and miss the two callers that reach it. So the
// table below is checked AGAINST the type's own method set, and a method
// nobody classified fails the build. The same shape `internal/solo`'s roster
// guard and `internal/skipgate`'s allowlist take, for the same reason.
func TestAHandleThatIsNotOpenAnswersRatherThanPanics(t *testing.T) {
	t.Parallel()

	var d *store.DB
	ctx := t.Context()

	// EVERY EXPORTED METHOD, and what "not open" means for it. A method that
	// can report an error owes [store.ErrNoEstate]; one that returns a bare
	// value owes the zero that means "no estate", which is what CLAUDE.md's
	// "zero values must be meaningful" asks of it.
	answers := map[string]func(t *testing.T){
		// ---- the read and write paths: ErrNoEstate ----------------------
		"Read": func(t *testing.T) {
			wantErrNoEstate(t, d.Read(ctx, func(*sql.Tx) error {
				t.Error("the body ran against a handle that is not open")
				return nil
			}))
		},
		"Tx": func(t *testing.T) {
			wantErrNoEstate(t, d.Tx(ctx, func(*sql.Tx) error {
				t.Error("the body ran against a handle that is not open")
				return nil
			}))
		},
		"Writer": func(t *testing.T) {
			w, err := d.Writer(ctx)
			wantErrNoEstate(t, err)
			if w != nil {
				t.Error("a writer was handed out on a handle that is not open")
			}
		},
		"AppliedMigrations": func(t *testing.T) {
			_, err := d.AppliedMigrations(ctx)
			wantErrNoEstate(t, err)
		},
		"EncodeVector": func(t *testing.T) {
			_, err := d.EncodeVector([]float32{1, 2, 3})
			wantErrNoEstate(t, err)
		},
		"UsageMark": func(t *testing.T) {
			_, err := d.UsageMark(ctx, store.UsageWindow{})
			wantErrNoEstate(t, err)
		},
		"UsageForDay": func(t *testing.T) {
			_, err := d.UsageForDay(ctx, store.UsageWindow{})
			wantErrNoEstate(t, err)
		},
		"Backup": func(t *testing.T) {
			_, err := d.Backup(ctx, t.TempDir()+"/copy.db")
			if err == nil {
				t.Error("a backup of a handle that is not open reported success")
			}
		},

		// ---- the accessors: a zero that means "no estate" ----------------
		"Estate": func(t *testing.T) {
			if got := d.Estate(); got != "" {
				t.Errorf("Estate() = %q, want the empty estate", got)
			}
		},
		"Path": func(t *testing.T) {
			if got := d.Path(); got != "" {
				t.Errorf("Path() = %q, want no file", got)
			}
		},
		"PartitionPath": func(t *testing.T) {
			if got := d.PartitionPath(storetest.LayoutZero(1)); got != "" {
				t.Errorf("PartitionPath() = %q, want no file", got)
			}
		},
		"File": func(t *testing.T) {
			if got := d.File(); got != (store.PartitionFile{}) {
				t.Errorf("File() = %+v, want no partition", got)
			}
		},
		"EmbeddingDim": func(t *testing.T) {
			if got := d.EmbeddingDim(); got != 0 {
				t.Errorf("EmbeddingDim() = %d, want no configured width", got)
			}
		},
		"Caps": func(t *testing.T) { _ = d.Caps() },
		"PartitionDB": func(t *testing.T) {
			part, err := d.PartitionDB("estate.000")
			wantErrNoEstate(t, err)
			if part != nil {
				t.Error("a closed handle answered a partition")
			}
		},
		"OpenPartitions": func(t *testing.T) {
			if got := d.OpenPartitions(); got != nil {
				t.Errorf("OpenPartitions() = %v, want none", got)
			}
		},
		"PartitionHandle": func(t *testing.T) {
			h := d.PartitionHandle("estate.000")
			if !h.IsZero() {
				t.Error("a closed handle built a partition handle that names a node")
			}
			wantErrNoEstate(t, h.Read(ctx, func(*sql.Tx) error {
				t.Error("the body ran through a partition of a handle that is not open")
				return nil
			}))
		},
		"SQL": func(t *testing.T) {
			if pool := d.SQL(); pool != nil {
				t.Error("a closed handle answered a connection pool, which is " +
					"the shape that panics three frames inside database/sql")
			}
		},

		// ---- the lifecycle: idempotent, because a close may lose a race ---
		"Close": func(t *testing.T) { _ = d.Close() },
		"OpenPartition": func(t *testing.T) {
			part, err := d.OpenPartition(ctx, storetest.LayoutZero(1))
			wantErrNoEstate(t, err)
			if part != nil {
				t.Error("a handle that is not open opened a partition")
			}
		},
		"ClosePartition": func(t *testing.T) { wantErrNoEstate(t, d.ClosePartition("estate.000")) },
		"DropPartition": func(t *testing.T) {
			wantErrNoEstate(t, d.DropPartition(ctx, storetest.LayoutZero(1)))
		},
		"LearnEmbeddingDim": func(t *testing.T) { d.LearnEmbeddingDim(768) },

		// ---- the sub-handles: BUILDING one must not panic -----------------
		//
		// Their own methods reach `x.db.sql` directly and would panic on a nil
		// db — and that is not a gap here, because every one of them is built
		// from the NODE's own file, and only a partition is ever not open.
		// `engine/backends.go` takes `db.Events()`, `engine/reconcile.go` and
		// `configapi` take `opts.Store.Configs()`, `maintenance/jobs.go` takes
		// both: the node handle, which is open for as long as the process is.
		// What IS asserted is that a caller holding a closed handle can ask
		// for one without dying, since the accessor costs nothing and the
		// refusal belongs at the statement.
		//
		// What is not open is a PARTITION, and it is reached through a
		// [store.PartitionHandle] — whose own roster is the next test — at
		// every site across internal/search, internal/tracker and
		// internal/engine that once took a nil peer and segfaulted.
		"Configs":       func(t *testing.T) { _ = d.Configs() },
		"Events":        func(t *testing.T) { _ = d.Events() },
		"SecretValues":  func(t *testing.T) { _ = d.SecretValues(nil) },
		"ThreadFollows": func(t *testing.T) { _ = d.ThreadFollows() },
	}

	// THE ROSTER IS THE TYPE'S OWN. A method added to *DB and not classified
	// here fails, which is the only way this gate covers the method that has
	// not been written yet — and the method that has not been written yet is
	// exactly the one the first fix missed.
	var missing []string
	typ := reflect.TypeOf(d)
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if _, held := answers[name]; !held {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("*store.DB exports %v with nothing here saying what they "+
			"answer on a handle that is not open. Every one of them can be "+
			"reached by a goroutine already in flight when an adoption or a "+
			"Close ends a handle — see store.ErrNoEstate — so classify it: "+
			"ErrNoEstate if it can report one, a meaningful zero if it "+
			"cannot.", missing)
	}
	// AND THE OTHER DIRECTION: an entry naming a method this type no longer
	// has is an excuse that outlived what it excused.
	for name := range answers {
		if _, held := typ.MethodByName(name); !held {
			t.Errorf("this table classifies %q and *store.DB has no such "+
				"method any more", name)
		}
	}

	for name, check := range answers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s panicked on a handle that is not open: %v — "+
						"this is the shape that took the engine down, twice",
						name, p)
				}
			}()
			check(t)
		})
	}
}

func wantErrNoEstate(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("err = %v, want store.ErrNoEstate — a caller cannot tell a "+
			"closing estate from a real fault without it", err)
	}
}

// A PARTITION HANDLE THAT NAMES NOTHING OPEN ANSWERS, EVERY METHOD OF IT.
//
// The handle is what a holder keeps, and it outlives every file it resolves to:
// the zero value a holder given no estate keeps, a partition the node never
// held, one an adoption holds closed, and every partition of a node that has
// closed. Each must answer [store.ErrNoEstate] or a meaningful zero, and
// neither of its two types may grow a method this roster has not classified,
// for [TestAHandleThatIsNotOpenAnswersRatherThanPanics]' reason.
func TestAPartitionHandleThatNamesNothingOpenAnswers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	closed, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	if err := closed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	notHeld, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = notHeld.Close() })

	for name, h := range map[string]store.PartitionHandle{
		"the zero handle":                 {},
		"a partition the node never held": notHeld.PartitionHandle("tracker.007"),
		"a partition of a closed node":    storetest.EstateOf(closed),
	} {
		answers := map[string]func(t *testing.T){
			"Read": func(t *testing.T) {
				wantErrNoEstate(t, h.Read(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a partition that is not open")
					return nil
				}))
			},
			"Tx": func(t *testing.T) {
				wantErrNoEstate(t, h.Tx(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a partition that is not open")
					return nil
				}))
			},
			"Writer": func(t *testing.T) {
				w, err := h.Writer(ctx)
				wantErrNoEstate(t, err)
				if w != nil {
					t.Error("a writer was pinned on a partition that is not open")
				}
			},
			"DB": func(t *testing.T) {
				db, err := h.DB()
				wantErrNoEstate(t, err)
				if db != nil {
					t.Error("a partition that is not open answered a file")
				}
			},
			"Caps": func(t *testing.T) {
				if got := h.Caps(); !reflect.DeepEqual(got, store.Capabilities{}) {
					t.Errorf("Caps() = %+v, want the zero probe", got)
				}
			},
			"Name":   func(t *testing.T) { _ = h.Name() },
			"IsZero": func(t *testing.T) { _ = h.IsZero() },
			"Reader": func(t *testing.T) {
				r := h.Reader()
				if r.IsZero() != h.IsZero() || r.Name() != h.Name() {
					t.Errorf("the reader of %q is %q, zero %v", h.Name(), r.Name(), r.IsZero())
				}
				wantErrNoEstate(t, r.Read(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a partition that is not open")
					return nil
				}))
			},
		}
		for _, typ := range []reflect.Type{
			reflect.TypeFor[store.PartitionHandle](), reflect.TypeFor[store.PartitionReader](),
		} {
			for i := range typ.NumMethod() {
				if _, held := answers[typ.Method(i).Name]; !held {
					t.Errorf("%s exports %s with nothing here saying what it answers "+
						"for a partition that is not open", typ.Name(), typ.Method(i).Name)
				}
			}
		}
		for method, check := range answers {
			t.Run(name+"/"+method, func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("%s panicked for %s: %v", method, name, p)
					}
				}()
				check(t)
			})
		}
	}
	if !(store.PartitionHandle{}).IsZero() {
		t.Error("the zero handle does not say it names no partition")
	}
}
