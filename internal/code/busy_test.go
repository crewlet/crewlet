package code_test

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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
// sixteen on it is gone. A proposal that queued with time to spare and lost it
// behind a held batch — withdrawn as it waits for its asker's deadline, or
// refused as its batch forms after a whole budget asked with none — met the
// queue's TIME bound, the same backlog, and the reading counts it too. A
// proposal that ARRIVED with too little of its asker's attempt left, and a
// record that lost every compare-and-set round, are each counted by the busy
// counter under their own reason and are no part of the reading: neither says
// this committer turns proposals away. Past the node's whole bound, every
// repository's proposal is a backlog the reading counts as the node's, and
// names the repository with the most of them; and the node's log says it a
// repository a minute, once the minute has closed — never a line a refusal.
//
// Mutations: count `late` (or `contended`) as a backlog, and the reading
// counts the arrival too late (or the lost race); file a wait that outlasted
// its asker, or one past a whole budget, as `late`, and the reading misses the
// committer that turned it away; keep a window of one bucket, and the refusal
// is gone fourteen minutes on; file every refusal as `queue`, and the node's
// bound reads as none; write a line a refusal, or the closed minute's lines at
// every reading, and the log holds more than one a repository.
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

	t.Run("a wait that outlasted its asker", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		gate := newGate(n.pub)
		c, rec := committerOn(t, n, gate)
		lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
		eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })

		// QUEUED WITH TIME TO SPARE — a batch, its answer and a second —
		// and the second spent behind the held batch.
		ctx, cancel := context.WithTimeout(t.Context(), code.CommitBudget+code.ReplyMargin+time.Second)
		defer cancel()
		waiting := proposeAsync(ctx, c, proposal(repo, update("refs/heads/waited", code.ZeroOID, oidOf("W"))))
		eventually(t, "the push queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
		if a := await(t, waiting); a.err != nil || a.res.Answer.Refused != code.RefusalBusy {
			t.Fatalf("a push that waited out its asker's attempt answered %+v, %v; want busy", a.res.Answer, a.err)
		}
		want := code.BusyReading{Refused: 1, Repo: repo, OfRepo: 1, Waited: 1}
		if got := c.AlarmReading(time.Now()); got != want {
			t.Errorf("the reading is %+v, want the push that waited out its time: %+v", got, want)
		}
		if got := pushesBusy(rec, "waited"); got != 1 {
			t.Errorf("the busy counter reads %d pushes that waited out their time, want 1", got)
		}
		if got := pushesBusy(rec, "late"); got != 0 {
			t.Errorf("the busy counter reads %d pushes too late on arrival, want none: it queued in time", got)
		}
		gate.open()
		requireOK(t, await(t, lead))
	})

	t.Run("a wait past a whole budget", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		gate := newGate(n.pub)
		c, rec := committerOn(t, n, gate)
		clock := &fakeClock{at: time.Unix(1_900_000_000, 0)}
		code.SetCommitterClock(c, clock.now)
		lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
		eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })

		// ASKED WITH NO DEADLINE, and still queued a whole budget later:
		// refused as the batch it would lead forms.
		waiting := proposeAsync(t.Context(), c, proposal(repo,
			update("refs/heads/waited", code.ZeroOID, oidOf("W"))))
		eventually(t, "the push queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
		clock.advance(code.CommitBudget)
		gate.open()
		requireOK(t, await(t, lead))
		if a := await(t, waiting); a.err != nil || a.res.Answer.Refused != code.RefusalBusy {
			t.Fatalf("a push queued a whole budget answered %+v, %v; want busy", a.res.Answer, a.err)
		}
		want := code.BusyReading{Refused: 1, Repo: repo, OfRepo: 1, Waited: 1}
		if got := c.AlarmReading(clock.now()); got != want {
			t.Errorf("the reading is %+v, want the push that waited a whole budget: %+v", got, want)
		}
		if got := pushesBusy(rec, "waited"); got != 1 {
			t.Errorf("the busy counter reads %d pushes that waited out their time, want 1", got)
		}
		if got := pushesBusy(rec, "late"); got != 0 {
			t.Errorf("the busy counter reads %d pushes too late on arrival, want none: it queued in time", got)
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
				line["queue"] != float64(0) || line["waited"] != float64(0) || line["late"] != float64(0) ||
				line["contended"] != float64(0) {
				t.Errorf("%s's line is %v, want a warning of %v refused at the node's bound", repo, line, refused)
			}
		}
	})
}

// A BUSY REFUSAL IS FILED IN ITS OWN MINUTE, WHATEVER THE INSTANT. The epoch's
// first minute, the minutes before it and the zero time's are minutes a clock
// can read like any other — a node's clock reset to 1970, a case's clock never
// set — so a refusal stamped in one is counted for the window, and its
// minute's line is written once, naming that minute.
//
// Mutations: tell an empty slot by its minute being zero, and a refusal in
// the epoch's first minute — or one moved there — panics on the slot's
// missing map, or is never counted; start the newest minute at zero rather
// than at the first filing, and the zero time's refusals are filed in the
// epoch's minute; divide the seconds toward zero, and the half minute before
// the epoch is filed in the minute after it.
func TestABusyRefusalIsFiledInItsOwnMinuteAtAnyInstant(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	epoch := time.Unix(0, 0)
	for name, tc := range map[string]struct {
		// at is when each refusal is stamped, in order.
		at []time.Time
		// minutes is each minute's line: its minute and its refusals.
		minutes map[string]float64
	}{
		"the zero time": {at: []time.Time{{}, {}},
			minutes: map[string]float64{"0001-01-01T00:00:00Z": 2}},
		"the epoch's first minute": {at: []time.Time{epoch, epoch.Add(30 * time.Second)},
			minutes: map[string]float64{"1970-01-01T00:00:00Z": 2}},
		"a half minute either side of the epoch": {
			at:      []time.Time{epoch.Add(-30 * time.Second), epoch.Add(30 * time.Second)},
			minutes: map[string]float64{"1969-12-31T23:59:00Z": 1, "1970-01-01T00:00:00Z": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logs := &lineLog{}
			c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
				o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
			})
			clock := &fakeClock{at: tc.at[0]}
			code.SetCommitterClock(c, clock.now)
			// A NODE THAT HOLDS NOTHING: every proposal is refused at once,
			// stamped by the committer's clock.
			code.SetCommitterNodeBytes(c, 1)
			for i, at := range tc.at {
				clock.advance(at.Sub(clock.now()))
				res, err := c.Propose(t.Context(), proposal(repo,
					update(fmt.Sprintf("refs/heads/b%d", i), code.ZeroOID, oidOf("B"))))
				if err != nil || res.Answer.Refused != code.RefusalBusy {
					t.Fatalf("a push past the node's bytes at %v answered %+v, %v; want busy", at, res.Answer, err)
				}
			}
			last := tc.at[len(tc.at)-1]
			refused := len(tc.at)
			want := code.BusyReading{Refused: refused, Repo: repo, OfRepo: refused, Node: refused}
			if got := c.AlarmReading(last); got != want {
				t.Fatalf("the reading at %v is %+v, want %+v", last, got, want)
			}
			// EACH MINUTE SAID ONCE, by its own minute, once it has closed.
			c.AlarmReading(last.Add(time.Minute))
			c.AlarmReading(last.Add(2 * time.Minute))
			said := map[string]float64{}
			for _, line := range logs.lines("code_commit_busy") {
				minute, _ := line["minute"].(string)
				if _, twice := said[minute]; twice {
					t.Errorf("the minute %s was logged twice", minute)
				}
				said[minute], _ = line["refused"].(float64)
			}
			if !maps.Equal(said, tc.minutes) {
				t.Errorf("the minutes logged are %v, want %v", said, tc.minutes)
			}
		})
	}
}
