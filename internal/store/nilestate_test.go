package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A HANDLE THAT IS NOT OPEN ANSWERS. It does not take the process with it.
//
// [store.ErrNoEstate] is a state, not a caller bug: a node without `data`, an
// adoption between rename and reopen, and DB.Close all answer it, so a
// goroutine in flight (a tick racing a shutdown) reaches it legitimately and
// must not reach a nil dereference. A guard in [DB.txOpts] is too deep:
// `pooled(d.busy)` in [DB.Read] and [DB.Tx] is an ARGUMENT, evaluated first.
//
// The roster is reflected, not listed: a hand-written table certifies only the
// methods somebody remembered. It is checked AGAINST the type's own method set,
// as `internal/solo`'s roster guard is.
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
		"ForeignMigrations": func(t *testing.T) {
			foreign, err := d.ForeignMigrations(ctx)
			wantErrNoEstate(t, err)
			if len(foreign) != 0 {
				t.Errorf("a handle that is not open named %v as foreign", foreign)
			}
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
		"UsageRederive": func(t *testing.T) {
			_, ok, err := d.UsageRederive(ctx)
			wantErrNoEstate(t, err)
			if ok {
				t.Error("a handle that is not open answered a re-derive mark")
			}
		},
		"ClearUsageRederive": func(t *testing.T) {
			cleared, err := d.ClearUsageRederive(ctx, 1)
			wantErrNoEstate(t, err)
			if cleared {
				t.Error("a handle that is not open cleared a re-derive mark")
			}
		},
		"UsageWatermark": func(t *testing.T) {
			_, ok, err := d.UsageWatermark(ctx)
			wantErrNoEstate(t, err)
			if ok {
				t.Error("a handle that is not open answered a usage watermark")
			}
		},
		"SetUsageWatermark": func(t *testing.T) {
			wantErrNoEstate(t, d.SetUsageWatermark(ctx, time.Now()))
		},
		"UsageRepublished": func(t *testing.T) {
			_, ok, err := d.UsageRepublished(ctx)
			wantErrNoEstate(t, err)
			if ok {
				t.Error("a handle that is not open answered the generations it last republished under")
			}
		},
		"SetUsageRepublished": func(t *testing.T) {
			wantErrNoEstate(t, d.SetUsageRepublished(ctx, "node-a@1"))
		},
		"UsageDayWindow": func(t *testing.T) {
			_, ok, err := d.UsageDayWindow(ctx, "2026-09-23")
			wantErrNoEstate(t, err)
			if ok {
				t.Error("a handle that is not open answered a usage day's cut")
			}
		},
		"UsageDayContaining": func(t *testing.T) {
			_, _, ok, err := d.UsageDayContaining(ctx, time.Now())
			wantErrNoEstate(t, err)
			if ok {
				t.Error("a handle that is not open answered the usage day an instant falls in")
			}
		},
		"PinUsageDay": func(t *testing.T) {
			_, err := d.PinUsageDay(ctx, "2026-09-23", store.UsageWindow{Start: time.Now(),
				End: time.Now().Add(time.Hour)})
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
		"ReplicatedFile": func(t *testing.T) {
			if got := d.ReplicatedFile(); got != "" {
				t.Errorf("ReplicatedFile() = %q, want no file", got)
			}
		},
		// False is "keeps what is written here", which a handle that never
		// opened a file cannot contradict: nothing was discarded either.
		"Scratch": func(t *testing.T) {
			if d.Scratch() {
				t.Error("Scratch() = true on a handle that is not open, want false")
			}
		},
		"EmbeddingDim": func(t *testing.T) {
			if got := d.EmbeddingDim(); got != 0 {
				t.Errorf("EmbeddingDim() = %d, want no configured width", got)
			}
		},
		"Caps": func(t *testing.T) { _ = d.Caps() },
		"Copies": func(t *testing.T) {
			if got := d.Copies(); len(got) != 0 {
				t.Errorf("Copies() = %+v, want none: nothing copies a handle that is not open", got)
			}
		},
		"CopyHoldsBack": func(t *testing.T) {
			if got := d.CopyHoldsBack(1 << 30); got != 0 {
				t.Errorf("CopyHoldsBack() = %d, want nothing measured", got)
			}
		},
		"ReplicatedDB": func(t *testing.T) {
			replicated, err := d.ReplicatedDB()
			wantErrNoEstate(t, err)
			if replicated != nil {
				t.Error("a closed handle answered a replicated estate")
			}
		},
		"Replicated": func(t *testing.T) {
			h := d.Replicated()
			if !h.IsZero() {
				t.Error("a closed handle built a replicated-estate handle that names a node")
			}
			wantErrNoEstate(t, h.Read(ctx, func(*sql.Tx) error {
				t.Error("the body ran through the replicated estate of a handle that is not open")
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
		"OpenReplicated": func(t *testing.T) {
			replicated, err := d.OpenReplicated(ctx, 1)
			wantErrNoEstate(t, err)
			if replicated != nil {
				t.Error("a handle that is not open opened a replicated estate")
			}
		},
		"CloseReplicated":   func(t *testing.T) { wantErrNoEstate(t, d.CloseReplicated()) },
		"LearnEmbeddingDim": func(t *testing.T) { d.LearnEmbeddingDim(768) },
		"SetEventSeats": func(t *testing.T) {
			d.SetEventSeats(func(string) (string, bool) { return "", false })
			d.SetEventSeats(nil)
		},

		// ---- the sub-handles: BUILDING one must not panic -----------------
		//
		// Their own methods reach `x.db.sql` and would panic on a nil db, which
		// is no gap: each is built from the NODE's own file, open as long as
		// the process is. What IS asserted is that a caller holding a closed
		// handle can ask for one without dying; the refusal belongs at the
		// statement.
		//
		// The REPLICATED ESTATE is the one not open, and it is reached through
		// a [store.ReplicatedHandle], whose own roster is the next test.
		"Configs": func(t *testing.T) { _ = d.Configs() },
		"Events":  func(t *testing.T) { _ = d.Events() },
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

// A REPLICATED-ESTATE HANDLE WITH NOTHING OPEN BEHIND IT ANSWERS, EVERY
// METHOD OF IT.
//
// The handle is what a holder keeps, and it outlives every file it resolves to:
// the zero value a holder given no estate keeps, the handle of a node that
// never opened its replicated estate (every node without `data`), one an
// adoption holds closed, and the estate of a node that has closed. Each must
// answer [store.ErrNoEstate] or a meaningful zero, and neither of its two types
// may grow a method this roster has not classified — nor this roster keep a
// method neither type has any more — for
// [TestAHandleThatIsNotOpenAnswersRatherThanPanics]' reason.
func TestAReplicatedHandleWithNothingOpenAnswers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	closed, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	if err := closed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	neverOpened, err := store.OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open a node: %v", err)
	}
	t.Cleanup(func() { _ = neverOpened.Close() })

	for name, h := range map[string]store.ReplicatedHandle{
		"the zero handle": {},
		"a node that never opened its replicated estate": neverOpened.Replicated(),
		"the replicated estate of a closed node":         closed.Replicated(),
	} {
		answers := map[string]func(t *testing.T){
			"Read": func(t *testing.T) {
				wantErrNoEstate(t, h.Read(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a replicated estate that is not open")
					return nil
				}))
			},
			"Tx": func(t *testing.T) {
				wantErrNoEstate(t, h.Tx(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a replicated estate that is not open")
					return nil
				}))
			},
			"Writer": func(t *testing.T) {
				w, err := h.Writer(ctx)
				wantErrNoEstate(t, err)
				if w != nil {
					t.Error("a writer was pinned on a replicated estate that is not open")
				}
			},
			"DB": func(t *testing.T) {
				db, err := h.DB()
				wantErrNoEstate(t, err)
				if db != nil {
					t.Error("a replicated estate that is not open answered a file")
				}
			},
			"Caps": func(t *testing.T) {
				if got := h.Caps(); !reflect.DeepEqual(got, store.Capabilities{}) {
					t.Errorf("Caps() = %+v, want the zero probe", got)
				}
			},
			"IsZero": func(t *testing.T) { _ = h.IsZero() },
			"Reader": func(t *testing.T) {
				r := h.Reader()
				if r.IsZero() != h.IsZero() {
					t.Errorf("the reader is zero %v and its handle %v", r.IsZero(), h.IsZero())
				}
				wantErrNoEstate(t, r.Read(ctx, func(*sql.Tx) error {
					t.Error("the body ran against a replicated estate that is not open")
					return nil
				}))
				if got := r.Caps(); !reflect.DeepEqual(got, store.Capabilities{}) {
					t.Errorf("the reader's Caps() = %+v, want the zero probe", got)
				}
			},
			// THE COLLECTOR'S QUESTION, whose answer licenses a deletion: a
			// replicated estate that is not open is ErrNoEstate, which the
			// collector reads as a pass that judges nothing, never as an
			// estate a newer build never migrated.
			"ForeignMigrations": func(t *testing.T) {
				foreign, err := h.Reader().ForeignMigrations(ctx)
				wantErrNoEstate(t, err)
				if len(foreign) != 0 {
					t.Errorf("a replicated estate that is not open named %v as foreign", foreign)
				}
			},
		}
		types := []reflect.Type{
			reflect.TypeFor[store.ReplicatedHandle](), reflect.TypeFor[store.ReplicatedReader](),
		}
		for _, typ := range types {
			for i := range typ.NumMethod() {
				if _, held := answers[typ.Method(i).Name]; !held {
					t.Errorf("%s exports %s with nothing here saying what it answers "+
						"for a replicated estate that is not open", typ.Name(), typ.Method(i).Name)
				}
			}
		}
		for method := range answers {
			if !slices.ContainsFunc(types, func(typ reflect.Type) bool {
				_, has := typ.MethodByName(method)
				return has
			}) {
				t.Errorf("this table classifies %q and neither handle type has "+
					"such a method any more", method)
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
	if !(store.ReplicatedHandle{}).IsZero() {
		t.Error("the zero handle does not say it is on no node")
	}
}
