package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	e.githubPulls.wire(&github.SeatLookup{Opts: github.SeatAppOptions{WebBase: "https://github.com"}},
		[]string{"acme"})
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
	stubPulls(e, map[string]pullRead{settledPull: {m: github.PullMovers{State: "closed", ClosedAt: &closed}}})
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

// waitAlsoOn makes task id of the node's project wait on ref too, written as
// the engine (a wait no tool would accept included).
func waitAlsoOn(t *testing.T, e *Engine, id, ref string) {
	t.Helper()
	project, _, _ := strings.Cut(id, "-")
	if _, err := e.native.Load().writer.Depend(t.Context(), statelog.NewOpID(time.Now(), "depend"),
		worklet.DependencyChange{Task: id, Project: project, WaitingOnAdd: []string{ref}}, nil); err != nil {
		t.Fatalf("wait %s on %s: %v", id, ref, err)
	}
}

// pullRead is one pull request as the stubbed host answers it.
type pullRead struct {
	m   github.PullMovers
	err error
}

// stubPulls stands each pull request's read in for the host's, any other
// answering 404, with no merge asked.
func stubPulls(e *Engine, reads map[string]pullRead) {
	byRef := map[github.PullRef]pullRead{}
	for raw, read := range reads {
		ref, _, _ := github.ChangeRef("https://github.com", raw)
		byRef[ref] = read
	}
	e.changePoll.mu.Lock()
	defer e.changePoll.mu.Unlock()
	e.changePoll.movers = func(ref github.PullRef) (github.PullMovers, error) {
		read, ok := byRef[ref]
		if !ok {
			return github.PullMovers{}, &github.APIError{Status: 404}
		}
		return read.m, read.err
	}
	if e.changePoll.asked == nil {
		e.changePoll.asked = func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }
	}
}

// A PULL REQUEST OUTSIDE THE COMPANY'S OWNERS IS REFUSED, NEVER READ AND NEVER
// SETTLED: the company's credentials are for its own code.
func TestAPullRequestOutsideTheCompanysOwnersIsNeitherAcceptedNorRead(t *testing.T) {
	t.Parallel()
	e, ids := changeNode(t)
	const foreign = "https://github.com/other/lib/pull/3"
	if ref, refused := e.ChangeRef("https://github.com/Acme/Web/pull/12/files"); ref != settledPull || refused != "" {
		t.Errorf("the company's own pull request reads as (%q, %q), want %s", ref, refused, settledPull)
	}
	if ref, refused := e.ChangeRef(foreign); ref != "" || !strings.Contains(refused, "other") ||
		!strings.Contains(refused, "acme") {
		t.Errorf("another owner's pull request reads as (%q, %q), want it refused naming both owners", ref, refused)
	}
	waitAlsoOn(t, e, ids[0], foreign)
	closed := time.Now().UTC().Add(-time.Hour)
	stubPulls(e, map[string]pullRead{foreign: {m: github.PullMovers{State: "closed", ClosedAt: &closed}}})
	if _, err := e.pollChanges(t.Context(), time.Now(), time.Time{}); err != nil {
		t.Fatalf("poll: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"action": "closed", "pull_request": map[string]any{
		"html_url": foreign, "merged": false, "closed_at": closed}})
	e.observeInbound(t.Context(), github.Backend, types.RawWebhook{BodyRaw: body,
		Headers: map[string]string{"x-github-event": "pull_request"}})
	got, err := e.native.Load().trackerReader.Task(t.Context(), ids[0], worklet.DetailWants{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	if err != nil {
		t.Fatal(err)
	}
	if _, settled := got.Task.Changes[foreign]; settled {
		t.Errorf("another owner's pull request was settled on %s: %+v", ids[0], got.Task.Changes)
	}
}

// A RATE LIMIT ENDS A PASS WHERE IT STOOD: the next resumes at the change the
// host refused, so changes past the first batch are still read.
func TestARateLimitedPassResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	e, ids := changeNode(t)
	const thirteen, fourteen = "https://github.com/acme/web/pull/13", "https://github.com/acme/web/pull/14"
	waitAlsoOn(t, e, ids[0], thirteen)
	waitAlsoOn(t, e, ids[1], fourteen)
	e.changePoll.batch = 2
	closed := time.Now().UTC().Add(-time.Hour)
	done := pullRead{m: github.PullMovers{State: "closed", ClosedAt: &closed}}
	stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)},
		thirteen: {err: &github.APIError{Status: 429, RateLimited: true}}, fourteen: done})
	if _, err := e.pollChanges(t.Context(), time.Now(), time.Time{}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if e.changePoll.after != settledPull {
		t.Fatalf("a pass the host stopped at %s resumes after %q, want after %s", thirteen,
			e.changePoll.after, settledPull)
	}
	stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)},
		thirteen: done, fourteen: done})
	if n, err := e.pollChanges(t.Context(), time.Now(), time.Time{}); err != nil || n != 2 {
		t.Fatalf("the next pass settled %d (%v), want the two past the first batch", n, err)
	}
}

// A PULL REQUEST THE HOST WILL NOT SHOW WAITS OUT A GROWING BACK-OFF, and is
// read again once it is over.
func TestAnUnreadablePullRequestBacksOff(t *testing.T) {
	t.Parallel()
	e, _ := changeNode(t)
	closed := time.Now().UTC().Add(-time.Hour)
	stubPulls(e, map[string]pullRead{settledPull: {err: &github.APIError{Status: 404}}})
	now := time.Now()
	if _, err := e.pollChanges(t.Context(), now, time.Time{}); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if held := e.changePoll.unreadable[settledPull]; held.wait != unreadableFirst {
		t.Fatalf("an unreadable pull request waits %v, want %v", held.wait, unreadableFirst)
	}
	stubPulls(e, map[string]pullRead{settledPull: {m: github.PullMovers{State: "closed", ClosedAt: &closed}}})
	if n, _ := e.pollChanges(t.Context(), now.Add(30*time.Minute), time.Time{}); n != 0 {
		t.Errorf("a pass inside the back-off settled %d, want it not read", n)
	}
	if n, _ := e.pollChanges(t.Context(), now.Add(2*time.Hour), time.Time{}); n != 2 {
		t.Errorf("a pass after the back-off settled %d, want both waiting tasks", n)
	}
	if _, held := e.changePoll.unreadable[settledPull]; held {
		t.Error("a pull request read again is still held as unreadable")
	}
}
