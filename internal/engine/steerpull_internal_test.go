package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A JOB WHOSE CLI ASKED FOR ITS NOTES IS RECORDED AS HAVING ASKED — by a Claude
// Code hook's take and by an OpenCode plugin's poll alike, on the run's real
// bridge session — and its collection says nothing; a job whose CLI never asked
// is said, at its collection, to have run no reader (steer_reader_never_ran).
// That line tells an operator something in the box switched the CLI's reading
// off, so it must be said of exactly the job that never asked: said of every
// job that did, it would send them hunting for a policy that is not there.
//
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: drop the record of the ask from either the hook's take or the
// plugin's poll, and that job is said never to have run its reader.
func TestAJobsCLIAskingForItsNotesIsOnItsRecord(t *testing.T) {
	logs := captureSteerLogs(t)
	client := httpxtest.Pool(t)
	for _, tc := range []struct {
		name string
		ask  func(ctx context.Context, req sandbox.RunRequest) error
	}{
		{"a hook's take", func(ctx context.Context, req sandbox.RunRequest) error {
			session, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "1"}, nil).
				Connect(ctx, &mcp.StreamableClientTransport{
					Endpoint: req.MCPServers[config.ToolServerName].URL, HTTPClient: client}, nil)
			if err != nil {
				return err
			}
			defer func() { _ = session.Close() }()
			_, failed, err := takeAt(ctx, session, map[string]any{"event": "PostToolBatch", "agent": ""})
			if failed {
				return errors.New("the hook's take was answered as an error")
			}
			return err
		}},
		{"a plugin's poll", func(ctx context.Context, req sandbox.RunRequest) error {
			if req.Steer.URL == "" {
				return errors.New("the run was handed no notes route")
			}
			// A NOTE WAITS, so the poll answers at once rather than holding.
			poll, err := http.NewRequestWithContext(ctx, http.MethodGet, req.Steer.URL+"/notes?after=0", nil)
			if err != nil {
				return err
			}
			res, err := client.Do(poll)
			if err != nil {
				return err
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("the poll answered %d", res.StatusCode)
			}
			return nil
		}},
		{"nothing asked", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &reviewWatch{}
			e, _ := indicatingWith(t, notify.StatusAlways, model)
			steerable(t, e)
			script := modelPlayer(t, model.run)
			var (
				launch string
				pulled string
				askErr error
			)
			rig := rigRuns(t, e, func(ctx context.Context, req sandbox.RunRequest) sandbox.Result {
				if tc.ask != nil {
					askErr = tc.ask(ctx, req)
					// RECORDED OFF THE PATH THAT ASKED, so it lands a moment
					// after the answer.
					deadline := time.Now().Add(5 * time.Second)
					for pulled != launch && time.Now().Before(deadline) {
						if row, found, err := e.sandbox.Load().pending.Get(ctx, "run-1"); err == nil && found {
							pulled = row.Steer.Pulled
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				return script(ctx, req)
			})
			// THE CLI SAYS IT READS NOTES, as Claude Code's and OpenCode's
			// runners do: a run of it that never asks is a sign.
			for _, runner := range rig.runners {
				runner.TakesNotes = true
			}
			parkOnARun(t, e, rig, "run-1")
			c := &steerCase{e: e, rig: rig, model: model}
			c.file(t, "run-1", "req-1", "use the staging database")
			launch = rig.row(t, "run-1").LaunchID
			if err := rig.collect(t, "run-1"); err != nil {
				t.Fatalf("collect: %v", err)
			}
			if askErr != nil {
				t.Fatalf("the CLI's ask: %v", askErr)
			}
			said := false
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "steer_reader_never_ran") && strings.Contains(line, launch) {
					said = true
				}
			}
			switch {
			case tc.ask != nil && pulled != launch:
				t.Errorf("the record says job %q asked for its notes, want job %s", pulled, launch)
			case tc.ask != nil && said:
				t.Errorf("job %s asked for its notes and was said never to have run its reader", launch)
			case tc.ask == nil && !said:
				t.Errorf("job %s never asked for its notes and nothing said so", launch)
			}
		})
	}
}

// EVERY FILING ON A RUN'S RECORD IS ANSWERED IN ITS OWN WORDS. The record's
// answer to a filing is the store's (`full`, `spent`, `closed`), and each is a
// different fact for the person sending the note: room for it after the turn
// reads, nothing more this turn ever, or the turn is over. Translated wrongly, a
// person whose turn has taken all ten notes is told to wait for a read that
// changes nothing.
//
// Mutation: answer the store's `spent` as full, or its `full` as spent, and this
// goes red.
func TestEveryFilingOnARunsRecordIsAnsweredInItsOwnWords(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	file := func(id string) steer.Status {
		t.Helper()
		filed, err := runs.File(ctx, "run-1", steer.Note{ID: id, Text: "note " + id, By: "founder",
			ByKind: iam.ActorHuman})
		if err != nil {
			t.Fatalf("File %s: %v", id, err)
		}
		return filed.Status
	}
	for i := range steer.MaxPendingNotes {
		if got := file(fmt.Sprintf("n-%d", i+1)); got != steer.StatusAccepted {
			t.Fatalf("note %d of %d waiting answered %s, want accepted", i+1, steer.MaxPendingNotes, got)
		}
	}
	if got := file("one-too-many"); got != steer.StatusFull {
		t.Errorf("a note past the waiting bound answered %s, want full", got)
	}
	// A READ MAKES ROOM, and the turn takes notes until it has taken ten.
	read := func() {
		t.Helper()
		run, found, err := store.Get(ctx, "run-1")
		if err != nil || !found {
			t.Fatalf("Get = %v, %v", found, err)
		}
		var ids []string
		for _, n := range run.Steer.Notes {
			if n.Waiting() {
				ids = append(ids, n.ID)
			}
		}
		if _, err := store.TakeNotes(ctx, "run-1", sandbox.NoteTake{IDs: ids, Launch: launch,
			Statuses: steerTakeStatuses, Read: sandbox.NoteRead{Phase: "execute", Boundary: "tool_batch",
				Launch: launch}}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	for i := steer.MaxPendingNotes; i < steer.MaxTurnNotes; i++ {
		if got := file(fmt.Sprintf("n-%d", i+1)); got != steer.StatusAccepted {
			t.Fatalf("note %d of the turn's %d answered %s, want accepted", i+1, steer.MaxTurnNotes, got)
		}
	}
	read()
	if got := file("the-eleventh"); got != steer.StatusSpent {
		t.Errorf("the turn's eleventh note answered %s, want spent", got)
	}
	if got := file("n-1"); got != steer.StatusAccepted {
		t.Errorf("a retry of a note the turn took answered %s, want accepted", got)
	}
	// THE ENDING DECIDED, and a run with no record: nothing will read a note.
	if _, ok, err := store.DecideEnding(ctx, "run-1", sandbox.Decision{License: sandbox.License{
		WhileIn: sandbox.Active, Launch: sandbox.EveryLaunch}}); err != nil || !ok {
		t.Fatalf("DecideEnding = %v, %v", ok, err)
	}
	if got := file("after-the-end"); got != steer.StatusClosed {
		t.Errorf("a note to a run whose ending is decided answered %s, want closed", got)
	}
	filed, err := runs.File(ctx, "no-such-run", steer.Note{ID: "x", Text: "x", By: "founder"})
	if err != nil || filed.Status != steer.StatusClosed {
		t.Errorf("a note to a run with no record answered (%s, %v), want closed", filed.Status, err)
	}
}
