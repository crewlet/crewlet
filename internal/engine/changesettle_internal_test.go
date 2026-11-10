package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

const settledPull = "https://github.com/acme/web/pull/12"

// changeNode is a running node on GitHub at github.com with two tasks held by
// ceo, each waiting on settledPull.
func changeNode(t *testing.T) (*Engine, []string) {
	t.Helper()
	e, _ := aRunningNodeOf(t, nativeCleanupCompany)
	e.githubPulls.wire(&github.SeatLookup{Opts: github.SeatAppOptions{WebBase: "https://github.com"}})
	writer := e.native.Load().writer
	at := time.Now().UTC()
	key := fmt.Sprintf("C%05d", seededProjects.Add(1))
	mustApply(t, "the project", func() (worklet.WriteResult, error) {
		return writer.WriteDocument(t.Context(), statelog.NewOpID(at, "project"),
			worklet.ProjectSubject(key), "", worklet.Project{V: worklet.DocumentVersion, Key: key,
				Name: "Changes " + key, CreatedAt: at, UpdatedAt: at}, worklet.ChangeProjectCreated, nil)
	})
	var ids []string
	for i := range 2 {
		id := fmt.Sprintf("%s-t%d", key, i)
		mustApply(t, "task "+id, func() (worklet.WriteResult, error) {
			return writer.CreateTask(t.Context(), statelog.NewOpID(at, "create"), worklet.Task{
				V: worklet.DocumentVersion, ID: id, Project: key, Type: "task", Title: "ship it",
				Assignee: "ceo", Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
				Priority: worklet.PriorityNormal, CreatedAt: at, UpdatedAt: at,
			}, nil)
		})
		if _, err := writer.Depend(t.Context(), statelog.NewOpID(at, "depend"), worklet.DependencyChange{
			Task: id, Project: key, WaitingOnAdd: []string{settledPull}}, nil); err != nil {
			t.Fatalf("wait %s on the pull request: %v", id, err)
		}
		ids = append(ids, id)
	}
	return e, ids
}

// settledOn is how each task records settledPull, and whether it reads blocked.
func settledOn(t *testing.T, e *Engine, ids []string) ([]worklet.ChangeSettlement, []bool) {
	t.Helper()
	var states []worklet.ChangeSettlement
	var blocked []bool
	for _, id := range ids {
		got, err := e.native.Load().trackerReader.Task(t.Context(), id, worklet.DetailWants{},
			statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		states, blocked = append(states, got.Task.Changes[settledPull]), append(blocked, got.Blocked)
	}
	return states, blocked
}

// settleRecords is how many settle records the tracker holds.
func settleRecords(t *testing.T, e *Engine) int {
	t.Helper()
	var n int
	if err := e.backends.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM tracker_history WHERE kind = 'change_settled'`).Scan(&n)
	}); err != nil {
		t.Fatalf("count the settle records: %v", err)
	}
	return n
}

// A MERGE THE WEBHOOK DELIVERS SETTLES EVERY TASK WAITING ON IT, once however
// many apps' copies arrive, under the host's own spelling of the repository.
func TestAMergeTheWebhookDeliversSettlesTheTasksWaitingOnIt(t *testing.T) {
	t.Parallel()
	e, ids := changeNode(t)
	if _, blocked := settledOn(t, e, ids); !blocked[0] || !blocked[1] {
		t.Fatalf("tasks waiting on an open pull request read unblocked: %v", blocked)
	}
	merged := time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC)
	body, err := json.Marshal(map[string]any{"action": "closed", "pull_request": map[string]any{
		"number": 12, "html_url": "https://github.com/Acme/Web/pull/12", "merged": true,
		"merged_at": merged, "closed_at": merged}})
	if err != nil {
		t.Fatal(err)
	}
	delivery := types.RawWebhook{BodyRaw: body, Headers: map[string]string{"x-github-event": "pull_request"}}
	for range 2 {
		e.observeInbound(context.Background(), github.Backend, delivery)
	}
	states, blocked := settledOn(t, e, ids)
	for i := range ids {
		if blocked[i] || states[i].State != worklet.ChangeMerged || states[i].Source != worklet.SettledByWebhook ||
			!states[i].At.Equal(merged) {
			t.Errorf("%s after the merge: blocked %v, recorded %+v; want unblocked, merged by the webhook",
				ids[i], blocked[i], states[i])
		}
	}
	if n := settleRecords(t, e); n != len(ids) {
		t.Errorf("%d settle records for %d tasks, want one each however many copies arrived", n, len(ids))
	}
}

// THE POLL SETTLES WHAT NO DELIVERY DID, once the change is past the webhook's
// grace, and leaves a change settled more recently to its webhook.
func TestThePollSettlesWhatNoDeliveryDid(t *testing.T) {
	t.Parallel()
	e, ids := changeNode(t)
	closed := time.Now().UTC().Add(-time.Minute)
	ref, _, _ := github.ChangeRef("https://github.com", settledPull)
	e.githubPulls.mu.Lock()
	e.githubPulls.entries = map[github.PullRef]pullPreview{ref: {at: time.Now(),
		pr: github.PullRequest{Number: 12, State: "closed", ClosedAt: &closed}}}
	e.githubPulls.mu.Unlock()
	if pending, err := e.openChangesPending(t.Context()); err != nil || !pending {
		t.Fatalf("the poll's gate answered %v (%v) with two tasks waiting", pending, err)
	}
	if n, err := e.pollChanges(t.Context(), time.Now(), time.Time{}); err != nil || n != 0 {
		t.Fatalf("a pass inside the webhook's grace settled %d (%v), want none", n, err)
	}
	n, err := e.pollChanges(t.Context(), closed.Add(changePollGrace+time.Minute), time.Time{})
	if err != nil || n != int64(len(ids)) {
		t.Fatalf("a pass past the grace settled %d (%v), want %d", n, err, len(ids))
	}
	states, blocked := settledOn(t, e, ids)
	for i := range ids {
		if blocked[i] || states[i].State != worklet.ChangeClosed || states[i].Source != worklet.SettledByPoll {
			t.Errorf("%s after the poll: blocked %v, recorded %+v; want unblocked, closed by the poll",
				ids[i], blocked[i], states[i])
		}
	}
	if pending, err := e.openChangesPending(t.Context()); err != nil || pending {
		t.Errorf("the poll's gate answered %v (%v) with nothing left open, want no pass", pending, err)
	}
}
