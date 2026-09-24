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
// # Why this is the assertion and not "it fails cleanly"
//
// A node adopts because it cannot replay its way back, so the file it is
// replacing is the only copy of its state it has. A power cut is not a
// failure the caller handles — the process is gone, and what matters is
// whatever the NEXT open finds. Three outcomes are acceptable and one is not:
// the old database, the new database, or a live path that does not exist yet
// (the caller re-fetches). A mixture — the new database with the old one's
// -wal applied over it, or either one missing pages that were still in a
// sidecar — is a store that opens, answers, and answers wrongly.
//
// # What "interrupted" means here
//
// The sequence is cut after each step, which is exactly what a crash between
// two of them leaves on disk. It walks [adoptSteps] rather than repeating the
// five calls, so a step added or reordered is covered by this case without
// anybody remembering to extend it.
//
// # What it cannot see
//
// A reordering whose cuts stay coherent BY ACCIDENT. Swapping the last two
// steps puts the rename before the live file's sidecars are removed — but the
// live file was already checkpointed by then, so its -wal holds nothing and
// removing it later changes no byte. That reordering is wrong and this case
// passes on it; [TestTheAdoptsStepsRunInTheOrderItsCrashMatrixAssumes] is what
// catches it, which is why both exist. Measured, so the boundary is a fact
// rather than a caveat: moving the rename to the FRONT is caught here, and
// swapping the last two is not.
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
	db, err := Open(t.Context(), path, Options{})
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
	db, err := Open(t.Context(), path, Options{})
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

// THE REPLICATED PATH STAYS CLAIMED ACROSS THE BRACKET.
//
// The install between CloseReplicated and ReopenReplicated checkpoints and
// replaces the file at that path. Released for that window, the path would
// admit another process, and that process's claim would then refuse the
// reopen, leaving the node with no replicated estate.
//
// ANOTHER PROCESS is a second open file description on the sidecar: flock
// excludes it exactly as it would a peer's, where a second claim taken through
// this process's refcount would only share the first.
func TestTheReplicatedPathStaysClaimedAcrossTheBracket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "node.db")
	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	replicated := ReplicatedPath(path, "")

	lockableByAnotherProcess := func() bool {
		t.Helper()
		f, err := os.OpenFile(replicated+lockSuffix, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open the sidecar as a peer would: %v", err)
		}
		defer func() { _ = f.Close() }()
		held, err := tryLock(f)
		if err != nil {
			t.Fatalf("lock the sidecar as a peer would: %v", err)
		}
		if held {
			_ = unlock(f)
		}
		return held
	}
	shares := func() int {
		t.Helper()
		locksHeld.mu.Lock()
		defer locksHeld.mu.Unlock()
		if held := locksHeld.by[replicated]; held != nil {
			return held.holds
		}
		return 0
	}

	if err := db.CloseReplicated(); err != nil {
		t.Fatalf("CloseReplicated: %v", err)
	}
	if lockableByAnotherProcess() {
		t.Fatalf("with the peer closed for the install, another process could claim %s, "+
			"and its claim would refuse the reopen that follows", replicated)
	}

	if err := db.ReopenReplicated(t.Context()); err != nil {
		t.Fatalf("ReopenReplicated: %v", err)
	}
	if got := shares(); got != 1 {
		t.Errorf("%s has %d share(s) of the claim after the reopen, want the reopened "+
			"handle's alone: the share kept across the swap was never given back", replicated, got)
	}

	// AND A NODE CLOSED WHILE ITS PEER IS CLOSED GIVES THE PATH BACK: the share
	// kept across the swap has nothing else to release it.
	if err := db.CloseReplicated(); err != nil {
		t.Fatalf("second CloseReplicated: %v", err)
	}
	closed = true
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := shares(); got != 0 {
		t.Errorf("%s still has %d share(s) of the claim after the node closed", replicated, got)
	}
	if !lockableByAnotherProcess() {
		t.Errorf("after the node closed, another process still cannot claim %s", replicated)
	}
}
