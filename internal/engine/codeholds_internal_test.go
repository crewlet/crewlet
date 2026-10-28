package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE LIVE TURNS' HOLDS ARE READ FROM EVERY ROW AND EVERY SEAT HOLDER — the
// code-retention pass's [code.Retention.Holds], over a running node: a key a
// run row holds, whatever the row's status (a newer build's included), a key a
// running turn of a seat holder holds in its memory, and nothing else. And
// never an answer on an unknown: a seat holder that does not answer inside the
// budget is named in an error, never read as holding nothing.
//
// Mutations: read the run store through ListActive — the newer build's row's
// key is unheld, red; read the rows alone — the running turn's key is unheld,
// red; read a silent holder as holding nothing — no error, red.
func TestCodeHoldsReadsRowsAndEverySeatHolder(t *testing.T) {
	t.Parallel()
	boot := testBootstrap(t)
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &boot)
	company, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &boot, Company: company})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(e.node.Attached()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(e.node.Attached()) == 0 {
		t.Fatal("the node attached no seat in 10s, so no seat holder would be asked")
	}

	// A PARKED RUN'S ROW, holding A, and a row a newer build wrote at a
	// status this build does not know, holding C — each of a seat this
	// company does not place, so no recovery of this node's seats reads them.
	for _, row := range []sandbox.PendingRun{
		{TurnID: "run-a", AgentHandle: "elsewhere", Status: sandbox.StatusAwaiting, CodeHolds: []string{"repo-a"}},
		{TurnID: "run-c", AgentHandle: "elsewhere", Status: "rehearsing", CodeHolds: []string{"repo-c"}},
	} {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if created, err := e.backends.Fleet.CreateSandboxRun(t.Context(), row.TurnID, raw); err != nil || !created {
			t.Fatalf("seed %s's row: %v %v", row.TurnID, created, err)
		}
	}
	// A RUNNING TURN OF THIS SEAT HOLDER, holding B in its memory.
	running := turnctx.NewHolds(nil)
	if _, err := running.Take(t.Context(), "repo-b"); err != nil {
		t.Fatal(err)
	}
	done := e.turnHolds.running(running)
	defer done()

	got, err := e.codeHolds(t.Context(), []string{"repo-a", "repo-b", "repo-c", "repo-d"})
	if err != nil {
		t.Fatalf("codeHolds: %v", err)
	}
	if !got["repo-a"] || !got["repo-b"] || !got["repo-c"] || got["repo-d"] || len(got) != 3 {
		t.Fatalf("held %v, want repo-a (a row), repo-b (a running turn) and repo-c (a newer build's row)", got)
	}

	// A TURN THAT RETURNED holds nothing in memory any more.
	done()
	if got, err = e.codeHolds(t.Context(), []string{"repo-b"}); err != nil || got["repo-b"] {
		t.Fatalf("after its turn returned, repo-b reads %v, %v — want unheld", got, err)
	}

	// A SEAT HOLDER THAT DOES NOT ANSWER is an unknown, named.
	if _, _, err := e.backends.Coord.TryAcquire(t.Context(), coord.SeatResource(uuid.New()),
		coord.AcquireOptions{Owner: "silent-node:boot-1", TTL: time.Minute}); err != nil {
		t.Fatalf("seat a silent holder: %v", err)
	}
	if _, err := e.codeHolds(t.Context(), []string{"repo-b"}); err == nil ||
		!strings.Contains(err.Error(), "silent-node:boot-1") {
		t.Fatalf("a silent seat holder = %v, want an error naming it", err)
	}
}

// failingRuns is a run store that cannot be listed, and failingLeases a lease
// listing that cannot be read.
type failingRuns struct{}

func (failingRuns) ListRuns(context.Context) ([]sandbox.PendingRun, error) {
	return nil, errors.New("the coordination store refused the listing")
}

type failingLeases struct{}

func (failingLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	return nil, errors.New("the lease bucket refused the listing")
}

type listedRuns []sandbox.PendingRun

func (r listedRuns) ListRuns(context.Context) ([]sandbox.PendingRun, error) { return r, nil }

type listedLeases []coord.Lease

func (l listedLeases) ListLive(context.Context, coord.Class) ([]coord.Lease, error) { return l, nil }

// AN UNREAD SOURCE IS AN ERROR, NEVER "NOTHING HELD": a run store that cannot
// be listed, seat leases that cannot be read, and a holder whose answer this
// build cannot read — a version it does not speak — each fail the read naming
// what was not read, so the pass expires nothing that page.
//
// Mutation: skip a reply at another version as answered — the old build's
// holder reads as holding nothing, red.
func TestAnUnreadHoldsSourceIsAnError(t *testing.T) {
	t.Parallel()
	var self runningTurns
	holder := []coord.Lease{{Resource: coord.SeatResource(uuid.New()), Owner: "node-b:boot-1"}}
	answer := func(version int) func(context.Context, string, []byte, int) ([][]byte, error) {
		return func(context.Context, string, []byte, int) ([][]byte, error) {
			raw, err := json.Marshal(codeHoldsAnswer{Version: version, Owner: "node-b:boot-1"})
			return [][]byte{raw}, err
		}
	}
	for name, tc := range map[string]struct {
		r    holdsReader
		want string
	}{
		"a run store that cannot be listed": {holdsReader{runs: failingRuns{}, leases: listedLeases(holder),
			ask: answer(codeHoldsProtocol), self: &self, budget: time.Second}, "run store"},
		"seat leases that cannot be read": {holdsReader{runs: listedRuns(nil), leases: failingLeases{},
			ask: answer(codeHoldsProtocol), self: &self, budget: time.Second}, "seat leases"},
		"a holder answering at another version": {holdsReader{runs: listedRuns(nil),
			leases: listedLeases(holder), ask: answer(codeHoldsProtocol + 1), self: &self,
			budget: time.Second}, "node-b:boot-1"},
	} {
		if _, err := tc.r.holds(t.Context(), []string{"repo-a"}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s = %v, want an error naming %q", name, err, tc.want)
		}
	}
	// AND A NODE WITH NO BROKER IS THE FLEET: its own turns are every turn.
	h := turnctx.NewHolds([]string{"repo-a"})
	done := self.running(h)
	defer done()
	got, err := holdsReader{runs: listedRuns(nil), self: &self}.holds(t.Context(), []string{"repo-a", "repo-z"})
	if err != nil || !got["repo-a"] || got["repo-z"] {
		t.Fatalf("a node with no broker read %v, %v — want its own turn's repo-a", got, err)
	}
}

// A TURN HOLDS WHAT ITS ROW MAY: the most one turn may hold
// (turnctx.MaxHolds) is the most its run row carries (sandbox.MaxCodeHolds),
// so a turn's whole set always fits the row a launch writes it on — and
// neither package imports the other, so this is where the two meet.
func TestATurnHoldsWhatItsRowMay(t *testing.T) {
	t.Parallel()
	if turnctx.MaxHolds != sandbox.MaxCodeHolds {
		t.Fatalf("a turn may hold %d keys and its row carry %d", turnctx.MaxHolds, sandbox.MaxCodeHolds)
	}
}
