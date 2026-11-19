package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// THE ORDER IS THE CRASH MATRIX, and it is asserted rather than described.
//
// Each of the five steps exists because the state a crash after it leaves is
// one the next open can make sense of, and no other arrangement has that
// property. Reversed anywhere, the prose above [AdoptFile] would still read
// correctly and the code would be wrong — which is the shape a comment cannot
// protect.
func TestTheAdoptsStepsRunInTheOrderItsCrashMatrixAssumes(t *testing.T) {
	t.Parallel()
	var names []string
	for _, step := range adoptSteps("live.db", "prepared.db") {
		names = append(names, step.name)
	}
	want := []string{
		"checkpoint the prepared file",
		"remove the prepared file's sidecars",
		"checkpoint the live file",
		"remove the live file's sidecars",
		"rename prepared over live",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("the install runs\n  %v\nand its crash matrix assumes\n  %v\n\n"+
			"The prepared file is made self-contained BEFORE the live one is "+
			"touched, so a crash in between leaves the old database intact; "+
			"the live file's sidecars go BEFORE the rename, so a crash in "+
			"between never leaves a -wal whose pages belong to a database "+
			"that is gone", names, want)
	}
}

// AN INTERRUPTED ADOPTION LEAVES ONE DATABASE OR THE OTHER, NEVER A MIXTURE.
//
// A crash leaves whatever the NEXT open finds. Acceptable: the old database,
// the new one, or no live path yet (the caller re-fetches). Not: the new
// database with the old -wal applied, or pages lost from a sidecar, a store
// that opens and answers wrongly. The sequence is cut after each step of
// [adoptSteps], so a step added or reordered is covered automatically.
//
// It cannot see a reordering whose cuts stay coherent BY ACCIDENT: swapping the
// last two steps passes, as the live file is already checkpointed.
// [TestTheAdoptsStepsRunInTheOrderItsCrashMatrixAssumes] catches that; moving
// the rename to the FRONT is caught here.
func TestAnInterruptedAdoptionLeavesOneDatabaseOrTheOther(t *testing.T) {
	t.Parallel()
	steps := len(adoptSteps("a", "b"))
	for cut := range steps + 1 {
		t.Run(cutName(cut, steps), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			live := filepath.Join(dir, "live.db")
			prepared := filepath.Join(dir, "prepared.db")
			seedProbe(t, live, "the-old-one")
			seedProbe(t, prepared, "the-donor")

			// THE CRASH: run the prefix and stop, leaving whatever that
			// step left on disk.
			for _, step := range adoptSteps(live, prepared)[:cut] {
				if err := step.run(t.Context()); err != nil {
					t.Fatalf("step %q: %v", step.name, err)
				}
			}

			// THE SIDECARS FIRST, BEFORE ANYTHING OPENS THE FILE. A
			// -wal beside the live path past the rename would hold
			// pages of the database that was replaced, and the next
			// open applies them — but opening it to read the marker
			// creates a fresh one, so the check has to come first.
			if cut == steps {
				for _, suffix := range []string{"-wal", "-shm"} {
					if _, err := os.Stat(live + suffix); !os.IsNotExist(err) {
						t.Errorf("%s survived a complete adoption: a sidecar "+
							"beside a replaced database holds pages of the "+
							"database that is gone", live+suffix)
					}
				}
			}

			mark, found := readProbe(t, live)
			switch {
			case !found:
				// A live path that does not exist is only legal before
				// there was ever one — this fixture seeds it, so any
				// cut must leave a database behind.
				t.Fatalf("cut after %d step(s) left no live database at all — "+
					"the file this node was replacing is the only copy of its "+
					"state it has", cut)
			case mark == "the-old-one" || mark == "the-donor":
				// Either is coherent. Which one it is depends on
				// whether the rename ran, and both are states the next
				// open can act on.
			default:
				t.Fatalf("cut after %d step(s) left the live database saying "+
					"%q — neither the old database nor the new one, which is "+
					"a store that opens, answers, and answers wrongly",
					cut, mark)
			}

			if cut == steps && mark != "the-donor" {
				t.Fatalf("a complete adoption left %q, want the donor's", mark)
			}
		})
	}
}

// cutName says where the crash happened, in the failure's own words.
func cutName(cut, steps int) string {
	switch cut {
	case 0:
		return "before_any_step"
	case steps:
		return "after_every_step"
	default:
		return "after_" + adoptSteps("a", "b")[cut-1].name
	}
}

// seedProbe writes one marker row and closes with a HOT -wal, which is the
// state every step of the install is arranged around.
func seedProbe(t *testing.T, path, mark string) {
	t.Helper()
	db, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`CREATE TABLE crewlet_adopt_probe (mark TEXT NOT NULL)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO crewlet_adopt_probe (mark) VALUES (?)`, mark)
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// readProbe opens the live path the way the next boot would and reads the
// marker, reporting whether there was a database there at all.
func readProbe(t *testing.T, path string) (string, bool) {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", false
	}
	db, err := OpenNode(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("the next open of %s failed: %v — an interrupted adoption "+
			"must leave a database somebody can open", path, err)
	}
	defer func() { _ = db.Close() }()
	var mark string
	if err := db.SQL().QueryRowContext(context.WithoutCancel(t.Context()),
		`SELECT mark FROM crewlet_adopt_probe`).Scan(&mark); err != nil {
		t.Fatalf("read %s after an interrupted adoption: %v", path, err)
	}
	return mark, true
}
