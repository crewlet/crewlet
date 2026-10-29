package code_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BACKLOG RAISES THE BUSY READING FOR ITS WINDOW, AND NOTHING ELSE DOES.
//
// A repository's queue full behind a batch held at its publish turns the next
// proposal away busy at once, and the committer's reading — what the engine
// carries into code_commit_busy — counts it and names the repository, for
// the whole CommitBusyWindow and no longer: fourteen minutes on it stands,
// sixteen on it is gone. A proposal its asker cannot wait for, and a record
// that lost every compare-and-set round, are each counted by the busy counter
// under their own reason and are no part of the reading: neither says this
// committer turns proposals away. Past the node's whole bound, every
// repository's proposal is a backlog the reading counts as the node's, and
// names the repository with the most of them; and the node's log says it a
// repository a minute, once the minute has closed — never a line a refusal.
//
// Mutations: count `late` (or `contended`) as a backlog, and the reading
// counts the deadline (or the lost race); keep a window of one bucket, and the
// refusal is gone fourteen minutes on; file every refusal as `queue`, and the
// node's bound reads as none; write a line a refusal, or the closed minute's
// lines at every reading, and the log holds more than one a repository.
func TestABacklogRaisesTheBusyReadingForItsWindow(t *testing.T) {
	t.Parallel()
	t.Run("a full queue", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		gate := newGate(n.pub)
		c, rec := committerOn(t, n, gate)
		clock := &fakeClock{at: time.Unix(1_900_000_000, 0)}
		code.SetCommitterClock(c, clock.now)
		if got := c.AlarmReading(clock.now()); got != (code.BusyReading{}) {
			t.Fatalf("a committer that refused nothing reads %+v", got)
		}

		// STALE, every one, so nothing is appended once the gate opens.
		stale := func(ref string) code.Proposal { return proposal(repo, update(ref, oidOf("X"), oidOf("Y"))) }
		lead := proposeAsync(t.Context(), c, stale("refs/heads/lead"))
		eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
		_, busy, queued := fill(t, c, repo, func(i int) code.Proposal {
			return stale(fmt.Sprintf("refs/heads/q%04d", i))
		})
		if busy.Answer.Refused != code.RefusalBusy {
			t.Fatalf("the proposal past the queue answered %+v, want busy", busy.Answer)
		}

		// A DEADLINE IT CANNOT MEET, on the committer's clock: no more than a
		// batch and its answer left, so it is answered busy before it queues.
		short, cancel := context.WithDeadline(t.Context(), clock.now().Add(code.CommitBudget))
		defer cancel()
		late, err := c.Propose(short, stale("refs/heads/late"))
		if err != nil || late.Answer.Refused != code.RefusalBusy {
			t.Fatalf("a push its asker cannot wait for answered %+v, %v; want busy", late.Answer, err)
		}

		want := code.BusyReading{Refused: 1, Repo: repo, OfRepo: 1}
		if got := c.AlarmReading(clock.now()); got != want {
			t.Fatalf("the reading is %+v, want the full queue's one refusal of %s: %+v", got, repo, want)
		}
		if got := c.AlarmReading(clock.now().Add(14 * time.Minute)); got != want {
			t.Errorf("fourteen minutes on the reading is %+v, want it to stand for the window: %+v", got, want)
		}
		if got := c.AlarmReading(clock.now().Add(16 * time.Minute)); got != (code.BusyReading{}) {
			t.Errorf("sixteen minutes on the reading is %+v, want nothing: the window is %s", got,
				code.CommitBusyWindow)
		}
		for _, reason := range []string{"queue", "late"} {
			if got := pushesBusy(rec, reason); got != 1 {
				t.Errorf("the busy counter reads %d pushes refused for %s, want 1", got, reason)
			}
		}

		gate.open()
		for _, ch := range append(queued, lead) {
			if a := await(t, ch); a.err != nil {
				t.Fatalf("a held proposal: %v", a.err)
			}
		}
	})

	t.Run("a lost race", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		c, rec := committerOn(t, n, refusing{fmt.Errorf("publish: %w", statelog.ErrConflict)})
		res, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B"))))
		if err != nil || res.Answer.Refused != code.RefusalBusy {
			t.Fatalf("a batch that lost every round answered %+v, %v; want busy", res.Answer, err)
		}
		if got := c.AlarmReading(time.Now()); got != (code.BusyReading{}) {
			t.Errorf("a lost race reads %+v, want nothing: it is no backlog of this committer", got)
		}
		if got := pushesBusy(rec, "contended"); got != 1 {
			t.Errorf("the busy counter reads %d pushes refused for a lost race, want 1", got)
		}
	})

	t.Run("the node's whole bound", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		web, api := repoOn(t, n, "web"), repoOn(t, n, "api")
		logs := &lineLog{}
		c, rec := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
			o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		})
		clock := &fakeClock{at: time.Unix(1_900_000_000, 0)}
		code.SetCommitterClock(c, clock.now)
		// A NODE THAT HOLDS NOTHING: every proposal is past its bound.
		code.SetCommitterNodeBytes(c, 1)
		for i, repo := range []string{api, web, api} {
			res, err := c.Propose(t.Context(), proposal(repo,
				update(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf("B"))))
			if err != nil || res.Answer.Refused != code.RefusalBusy {
				t.Fatalf("a push past the node's bytes answered %+v, %v; want busy", res.Answer, err)
			}
		}
		want := code.BusyReading{Refused: 3, Repo: api, OfRepo: 2, Node: 3}
		if got := c.AlarmReading(clock.now()); got != want {
			t.Fatalf("the reading is %+v, want %+v", got, want)
		}
		if got := pushesBusy(rec, "node"); got != 3 {
			t.Errorf("the busy counter reads %d at the node's bound, want 3", got)
		}

		// THE MINUTE STILL OPEN: nothing said yet.
		if lines := logs.lines("code_commit_busy"); len(lines) != 0 {
			t.Fatalf("the open minute was logged already: %v", lines)
		}
		// ONE LINE A REPOSITORY, once the minute has closed, and once only.
		c.AlarmReading(clock.now().Add(time.Minute))
		c.AlarmReading(clock.now().Add(2 * time.Minute))
		lines := logs.lines("code_commit_busy")
		if len(lines) != 2 {
			t.Fatalf("the minute's refusals were logged in %d line(s), want one a repository (2): %v",
				len(lines), lines)
		}
		said := map[any]map[string]any{}
		for _, line := range lines {
			said[line["repo"]] = line
		}
		for repo, refused := range map[string]float64{api: 2, web: 1} {
			line := said[repo]
			if line["level"] != "WARN" || line["refused"] != refused || line["node"] != refused ||
				line["queue"] != float64(0) || line["late"] != float64(0) || line["contended"] != float64(0) {
				t.Errorf("%s's line is %v, want a warning of %v refused at the node's bound", repo, line, refused)
			}
		}
	})
}
