package store_test

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A HANDLE THAT IS NOT OPEN ANSWERS. It does not take the process with it.
//
// # The contract, and why it needed a gate
//
// [store.ErrNoEstate]'s own doc states it: "IT IS A STATE, NOT A BUG IN THE
// CALLER. DB.Replicated answers nil while an adoption holds the peer closed
// between its rename and its reopen, and again after DB.Close — both
// documented and deliberate — so a goroutine that was already in flight when
// one of those happened reaches here legitimately. What it must NOT reach is a
// nil dereference: a maintenance tick racing a shutdown panicked the engine."
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
		"ReplicatedPath": func(t *testing.T) {
			if got := d.ReplicatedPath(); got != "" {
				t.Errorf("ReplicatedPath() = %q, want no file", got)
			}
		},
		"EmbeddingDim": func(t *testing.T) {
			if got := d.EmbeddingDim(); got != 0 {
				t.Errorf("EmbeddingDim() = %d, want no configured width", got)
			}
		},
		"Caps": func(t *testing.T) { _ = d.Caps() },
		"Replicated": func(t *testing.T) {
			if peer := d.Replicated(); peer != nil {
				t.Error("a closed handle answered a peer")
			}
		},
		"SQL": func(t *testing.T) {
			if pool := d.SQL(); pool != nil {
				t.Error("a closed handle answered a connection pool, which is " +
					"the shape that panics three frames inside database/sql")
			}
		},

		// ---- the lifecycle: idempotent, because a close may lose a race ---
		"Close":           func(t *testing.T) { _ = d.Close() },
		"CloseReplicated": func(t *testing.T) { _ = d.CloseReplicated() },
		"ReopenReplicated": func(t *testing.T) {
			if err := d.ReopenReplicated(ctx); err == nil {
				t.Error("a handle that is not open reported a successful reopen")
			}
		},
		"LearnEmbeddingDim": func(t *testing.T) { d.LearnEmbeddingDim(768) },

		// ---- the sub-handles: BUILDING one must not panic -----------------
		//
		// Their own methods reach `x.db.sql` directly and would panic on a nil
		// db — and that is not a gap here, because every one of them is built
		// from the NODE estate and only the REPLICATED peer is ever nil.
		// `engine/backends.go` takes `db.Events()`, `engine/reconcile.go` and
		// `configapi` take `opts.Store.Configs()`, `maintenance/jobs.go` takes
		// both: the node handle, which is open for as long as the process is.
		// What IS asserted is that a caller holding a closed handle can ask
		// for one without dying, since the accessor costs nothing and the
		// refusal belongs at the statement.
		//
		// The path that IS nil is `DB.Replicated()`, and it is reached as
		// `Replicated().Read(...)` at ten sites across internal/search,
		// internal/tracker and internal/engine — every one of them a segfault
		// before the guard above, not merely the one the retention tick hit.
		"Configs":       func(t *testing.T) { _ = d.Configs() },
		"Events":        func(t *testing.T) { _ = d.Events() },
		"ThreadFollows": func(t *testing.T) { _ = d.ThreadFollows() },
		"SecretValues":  func(t *testing.T) { _ = d.SecretValues(nil) },
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
			"Close nils the peer — see store.ErrNoEstate — so classify it: "+
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
