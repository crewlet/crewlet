package signedask

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
)

// took is what a guard answers a read's id: empty where it was taken.
func took(t *testing.T, g *Guard, id string, deadline time.Time, apart time.Duration, now time.Time) string {
	t.Helper()
	_, why := g.take(t.Context(), id, deadline, apart, now, false)
	return why
}

// keptGuard is c's guard over file as a node starting at now builds it.
func keptGuard(t *testing.T, c *Channel[ask, answer], file Ledger, now time.Time) *Guard {
	t.Helper()
	g, err := c.KeptGuard(t.Context(), file, now)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A WRITE TAKEN BEFORE A RESTART IS REFUSED AFTER IT: the guard a node builds
// at boot over its own file finds there every write it took inside its window,
// so a captured write replayed at the restarted node is a replay, not a
// request. A read's id is kept in memory only, so the restarted node takes a
// read again, which costs nothing.
//
// Mutations: Guard.take skips the Keep, or reads Keep's fresh as true: the
// replay is admitted, red.
func TestAWriteTakenBeforeARestartIsRefusedAfterIt(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	file := signedasktest.NewLedger()
	write := ask{ID: "w-1", Deadline: instant.Add(time.Minute), Do: "remove node-c", Write: true}
	read := ask{ID: "r-1", Deadline: instant.Add(time.Minute), Do: "list voters"}
	before := keptGuard(t, c, file, instant)
	for _, req := range []ask{write, read} {
		if got := c.Admit(t.Context(), sealed(t, c, req), before, instant); got.Disposition != Admitted {
			t.Fatalf("%s before the restart = %+v, want it admitted", req.ID, got)
		}
	}
	if !file.Holds("test.channel", write.ID) || file.Holds("test.channel", read.ID) {
		t.Fatalf("the file holds the write: %v, the read: %v; want the write's id alone",
			file.Holds("test.channel", write.ID), file.Holds("test.channel", read.ID))
	}

	later := instant.Add(30 * time.Second)
	after := keptGuard(t, c, file, later)
	replay := c.Admit(t.Context(), sealed(t, c, write), after, later)
	if replay.Disposition != Stale || !strings.Contains(replay.Why, "taken already") {
		t.Fatalf("the write replayed after the restart = %+v, want it refused as taken already", replay)
	}
	if got := c.Admit(t.Context(), sealed(t, c, read), after, later); got.Disposition != Admitted {
		t.Errorf("the read replayed after the restart = %+v, want it admitted: a read's id is "+
			"memory's alone", got)
	}
}

// THE FILE HOLDS NO MORE THAN THE MEMORY: an id is deleted from it when a
// sweep forgets it, and a guard starting past an id's window neither holds it
// nor leaves it in the file. So the file is bounded as the guard is, by the
// writes its channel admits inside one window.
//
// Mutations: Guard.take skips the Forget after a sweep (the file keeps every
// short write), or KeptGuard skips its own (the file keeps the expired ids):
// red.
func TestTheFileForgetsAWriteWhenItsWindowEnds(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	file := signedasktest.NewLedger()
	g := keptGuard(t, c, file, instant)
	long := ask{ID: "w-long", Deadline: instant.Add(time.Hour), Do: "remove node-c", Write: true}
	if got := c.Admit(t.Context(), sealed(t, c, long), g, instant); got.Disposition != Admitted {
		t.Fatalf("the long write = %+v", got)
	}
	// THE GUARD REACHES ITS SWEEP SIZE with the last short write, so the
	// next admission, after every short one's window, sweeps.
	for i := range sweepFloor - 1 {
		short := ask{ID: "w-short-" + strconv.Itoa(i), Deadline: instant.Add(time.Second), Do: "x", Write: true}
		if got := c.Admit(t.Context(), sealed(t, c, short), g, instant); got.Disposition != Admitted {
			t.Fatalf("short write %d = %+v", i, got)
		}
	}
	if n := file.Len("test.channel"); n != sweepFloor {
		t.Fatalf("the file holds %d ids before the sweep, want %d", n, sweepFloor)
	}
	later := instant.Add(time.Minute)
	sweeper := ask{ID: "w-sweeper", Deadline: later.Add(time.Minute), Do: "x", Write: true}
	if got := c.Admit(t.Context(), sealed(t, c, sweeper), g, later); got.Disposition != Admitted {
		t.Fatalf("the sweeping write = %+v", got)
	}
	if n := file.Len("test.channel"); n != 2 {
		t.Errorf("after the sweep the file holds %d ids, want the two still inside their windows", n)
	}

	keptGuard(t, c, file, instant.Add(2*time.Hour))
	if n := file.Len("test.channel"); n != 0 {
		t.Errorf("a guard starting past every window left %d ids in the file", n)
	}
}

// A WRITE THE FILE WILL NOT KEEP RUNS NOTHING: it is refused as unrecorded,
// naming its id so the asker can ask another node, and it is not held either,
// so once the file takes writes again the same request is taken: it never ran,
// so it is no replay.
//
// Mutations: Guard.take ignores the Keep's error (admitted, red), or keeps the
// id in memory after it (the retry is stale, red).
func TestAWriteTheFileWillNotKeepRunsNothing(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	file := signedasktest.NewLedger()
	g := keptGuard(t, c, file, instant)
	write := ask{ID: "w-1", Deadline: instant.Add(time.Minute), Do: "remove node-c", Write: true}
	sent := sealed(t, c, write)
	file.Refuse(errors.New("disk full"))
	got := c.Admit(t.Context(), sent, g, instant)
	if got.Disposition != Unrecorded || got.ID != write.ID || !strings.Contains(got.Why, "disk full") {
		t.Fatalf("a write the file refused = %+v, want it unrecorded, naming its id and the cause", got)
	}
	file.Refuse(nil)
	if again := c.Admit(t.Context(), sent, g, instant); again.Disposition != Admitted {
		t.Errorf("the same write once the file keeps again = %+v, want it admitted", again)
	}
}

// A GUARD WITHOUT A FILE RUNS NO WRITE, so a channel wired without its file
// fails at its first write rather than taking writes a restart forgets; its
// reads are taken as ever.
//
// Mutation: drop the writes-without-a-file case in Guard.take: admitted, red.
func TestAGuardWithoutAFileRunsNoWrite(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	g := NewGuard()
	write := ask{ID: "w-1", Deadline: instant.Add(time.Minute), Do: "remove node-c", Write: true}
	if got := c.Admit(t.Context(), sealed(t, c, write), g, instant); got.Disposition != Unrecorded {
		t.Errorf("a write at a guard without a file = %+v, want it unrecorded", got)
	}
	if got := c.Admit(t.Context(), sealed(t, c, removal), g, instant); got.Disposition != Admitted {
		t.Errorf("a read at a guard without a file = %+v, want it admitted", got)
	}
}

// A GUARD WHOSE FILE REFUSES ITS START DOES NOT START: it could keep no write
// there. And a write the file already holds is a replay, whatever the memory
// says.
func TestAGuardTrustsItsFileOverItsMemory(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	file := signedasktest.NewLedger()
	file.Refuse(errors.New("unreadable"))
	if _, err := c.KeptGuard(t.Context(), file, instant); err == nil ||
		!strings.Contains(err.Error(), "signed_request_taken") {
		t.Fatalf("a guard over a file that refuses = %v, want it refused naming the table", err)
	}
	file.Refuse(nil)
	g := keptGuard(t, c, file, instant)
	write := ask{ID: "w-1", Deadline: instant.Add(time.Minute), Do: "remove node-c", Write: true}
	if _, err := file.Keep(t.Context(), "test.channel", write.ID, write.Deadline); err != nil {
		t.Fatal(err)
	}
	// Mutation: Guard.take reads Keep's fresh as true: admitted, red.
	if got := c.Admit(t.Context(), sealed(t, c, write), g, instant); got.Disposition != Stale {
		t.Errorf("a write the file holds and the memory does not = %+v, want it stale", got)
	}
}

// A REQUEST FOR ANOTHER NODE TAKES NOTHING: a scattered write reaches every
// node, and only the one it names keeps its id, so the others write no row and
// take no memory. And the address is asked only of what the fleet signed, so
// an unsigned request cannot learn which node a request would reach.
//
// Mutations: AdmitTo asks addressed after the take (the file holds the id,
// red), or before the signature check (the predicate sees the forgery, red).
func TestARequestForAnotherNodeTakesNothing(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	file := signedasktest.NewLedger()
	g := keptGuard(t, c, file, instant)
	write := ask{ID: "w-1", Deadline: instant.Add(time.Minute), Do: "stop on node-b", Write: true}
	asked := 0
	elsewhere := func(ask) bool { asked++; return false }
	if got := c.AdmitTo(t.Context(), sealed(t, c, write), g, instant, elsewhere); got.Disposition != NotAddressed {
		t.Fatalf("a write for another node = %+v, want it not addressed", got)
	}
	if file.Len("test.channel") != 0 || len(g.seen) != 0 {
		t.Fatalf("a write for another node left %d rows and %d ids held", file.Len("test.channel"), len(g.seen))
	}
	if got := c.Admit(t.Context(), sealed(t, c, write), g, instant); got.Disposition != Admitted {
		t.Errorf("the same write at the node it names = %+v, want it admitted", got)
	}

	forged := channel(t, "test.channel", recordseal.OneKey("k1", "material outside the fleet"))
	asked = 0
	if got := c.AdmitTo(t.Context(), sealed(t, forged, write), g, instant, elsewhere); got.Disposition != Tampered || asked != 0 {
		t.Errorf("a forged request = %+v with the address asked %d times, want it tampered and never asked", got, asked)
	}
}
