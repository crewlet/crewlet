package worklet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PURGE THIS BUILD WRITES LEAVES NO COPY OF THE TASK BEHIND, in any table.
//
// History rows, inbox notices, turn records and dependency mirrors all hold
// copies of the task's content or id, so a purge that removed only the object
// tables left a purged task readable on every data node.
//
// What survives is the purge's own account, the lead's `purged` notice, and
// the census's SKELETON of the task (creation, status, assignee and project
// changes, no content): the flow census answers the past from those rows, and
// a purge leaves it only from its own instant.
// [TestAPurgeDestroysWhatItsTaskWroteThroughTheFramework] holds the same apply
// through the real framework loop.
func TestAPurgeLeavesNoCopyOfTheTaskBehind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const secret = "the merger with Contoso"
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	// The content, in every place a record puts it: a comment mentioning
	// somebody other than the writer, whom no notice names
	// ([worklet.Reason.WakesActor]); a turn's spend; and a dependency whose
	// mirror is a row on the task.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Comment: &worklet.Comment{ID: "c-1", Body: secret, Author: "jane"},
		}, worklet.ChangeComment, &worklet.Notify{
			Kind: worklet.ChangeComment, Excerpt: secret, Mentions: []string{"bo"},
		}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn", worklet.TurnRecord{
		Task: "t-1", Seat: "swe", TurnID: "run-1",
		Spend: worklet.TurnSpend{Turns: 1, Input: 10},
	}); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "t-2", Project: "ENG", WaitingOnAdd: []string{"t-1"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("depend: %v", err)
	}
	r.drain()
	if held := oneTask(t, r, "t-1"); len(held.Dependents) == 0 {
		t.Fatalf("t-1 lists no dependents after the depend: %+v", held.Dependents)
	}

	about := func(query string) []string {
		t.Helper()
		return r.strings(query, "t-1")
	}
	const (
		histories = `SELECT id FROM tracker_history WHERE subject_id = ?`
		notices   = `SELECT record_id FROM tracker_notifications WHERE subject_id = ?`
		turns     = `SELECT id FROM tracker_turns WHERE task_id = ?`
		mirrors   = `SELECT task_id FROM tracker_task_dependents
			WHERE task_id = ?1 OR dependent_id = ?1`
		copies = `SELECT id FROM tracker_history
			WHERE subject_id = ? AND (CAST(document AS TEXT) LIKE '%Contoso%'
			                          OR excerpt LIKE '%Contoso%')`
	)
	// The fixture holds what it says, or the assertions below pass
	// vacuously.
	for name, query := range map[string]string{
		"history": histories, "a notice": notices, "a turn": turns,
		"a mirror": mirrors, "a copy of the comment": copies,
	} {
		if len(about(query)) == 0 {
			t.Fatalf("the fixture wrote no %s about t-1", name)
		}
	}

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	// Only the purge's own row and the census skeleton remain: kind,
	// instant and the three deltas, nothing else.
	if got := about(histories); !slices.Contains(got, "op-purge") {
		t.Errorf("after the purge the task's history is %v, with no purge row", got)
	}
	if got := about(`SELECT id FROM tracker_history WHERE subject_id = ?
		AND kind = 'created'`); len(got) != 1 {
		t.Errorf("the task's creation is %v after the purge, want its one row — "+
			"the census needs it to take the task out of the days before it existed", got)
	}
	if got := about(`SELECT id FROM tracker_history WHERE subject_id = ?
		AND id <> 'op-purge' AND (excerpt <> '' OR comment_id <> ''
		     OR CAST(document AS TEXT) <> '{}' OR kind = 'comment'
		     OR json_extract(fields_json, '$.title') IS NOT NULL)`); len(got) != 0 {
		t.Errorf("history rows %v still carry content after the purge", got)
	}
	if got := about(copies); len(got) != 0 {
		t.Errorf("the purged comment survives in history rows %v", got)
	}
	for _, id := range about(notices) {
		if id != "op-purge" {
			t.Errorf("an inbox notice from %s survives the purge", id)
		}
	}
	if got := notifiedHandles(t, r, "t-1"); !containsKind(got, "eng-lead") {
		t.Errorf("the purge's own notice is gone (%v) — the lead's `purged` "+
			"wake is the one account of the task that must survive", got)
	}
	if got := about(turns); len(got) != 0 {
		t.Errorf("turn records %v survive the purge", got)
	}
	if got := about(mirrors); len(got) != 0 {
		t.Errorf("dependency mirror rows %v survive the purge", got)
	}
	if excerpt := historyExcerpt(t, r, "task", "t-1"); !strings.Contains(excerpt, "purged") {
		t.Errorf("the surviving row is not the purge's: %q", excerpt)
	}
}

// A PURGE DESTROYS WHAT ITS TASK'S OWN RECORDS WROTE — through the real
// framework loop, over the real log, from the record exactly as the writer put
// it there.
func TestAPurgeDestroysWhatItsTaskWroteThroughTheFramework(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const secret = "the merger with Contoso"
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	filedTask(t, r, "t-3")
	// The content, in every place a record puts it: a mentioning comment,
	// a turn's spend, a dependent (edge and mirror row), and a child.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Comment: &worklet.Comment{ID: "c-1", Body: secret, Author: "jane"},
		}, worklet.ChangeComment, &worklet.Notify{
			Kind: worklet.ChangeComment, Excerpt: secret, Mentions: []string{"bo"},
		}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn", worklet.TurnRecord{
		Task: "t-1", Seat: "swe", TurnID: "run-1",
		Spend: worklet.TurnSpend{Turns: 1, Input: 10},
	}); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	r.waitOn(oneTask(t, r, "t-2"), oneTask(t, r, "t-1"))
	r.fileUnder(oneTask(t, r, "t-3"), oneTask(t, r, "t-1"))

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	purgeAt := r.logEnd(t)
	written := r.recordAt(t, purgeAt)
	if written.Op != worklet.OpPurge {
		t.Fatalf("the premise: record %d is a %s, want the purge", purgeAt, written.Op)
	}

	body, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("encode the purge: %v", err)
	}
	// Sealed as the writer seals it: the loop verifies every frame before
	// decoding, so a bare replacement would be refused as unsigned.
	before, after := applyPurgeThroughARunner(t, r, purgeAt,
		testSigner(t, worklet.Domain{}).Seal(body))
	if _, held := after["tracker_tasks"]; !held {
		t.Fatal("the dump read no task table")
	}
	if slices.ContainsFunc(after["tracker_tasks"], func(row string) bool {
		return strings.HasPrefix(row, `["t-1",`)
	}) {
		t.Fatal("the purged task's row survives the purge")
	}
	// The premise, so a failure below has something to read against.
	for _, table := range []string{"tracker_turns", "tracker_task_dependents"} {
		if len(before[table]) == 0 {
			t.Errorf("the premise: the fixture wrote no %s row", table)
		}
	}
	assertTheTasksRecordsAreGone(t, after, secret)
}

// assertTheTasksRecordsAreGone fails unless nothing the purged task's own
// records wrote survives beside the purge's own account and the census's
// skeleton of it.
func assertTheTasksRecordsAreGone(t *testing.T, after map[string][]string, secret string) {
	t.Helper()
	for _, row := range after["tracker_history"] {
		if !strings.Contains(row, `"t-1"`) || strings.HasPrefix(row, `["op-purge",`) {
			continue
		}
		// Only the census skeleton may stay: the comment's row goes
		// whole, and a kept row holds no title, excerpt or document.
		if strings.HasPrefix(row, `["op-comment",`) || strings.Contains(row, `title`) {
			t.Errorf("a purge left content in a history row about "+
				"the task: %s", row)
		}
	}
	for table, rows := range after {
		for _, row := range rows {
			if strings.Contains(row, secret) {
				t.Errorf("the purged comment survives a purge in %s: %s",
					table, row)
			}
		}
	}
	for _, table := range []string{"tracker_turns", "tracker_task_dependents"} {
		for _, row := range after[table] {
			if strings.Contains(row, `"t-1"`) {
				t.Errorf("a purge left a %s row naming the task: %s", table, row)
			}
		}
	}
	for _, row := range after["tracker_notifications"] {
		if strings.Contains(row, `"t-1"`) && !strings.Contains(row, `"op-purge"`) {
			t.Errorf("a purge left an inbox notice about the task: %s", row)
		}
	}
}

// applyPurgeThroughARunner runs a fresh node's framework loop over r's log,
// with the record at purgeAt replaced by purge, and dumps every table the
// identity claim covers just before that record and just after it.
func applyPurgeThroughARunner(t *testing.T, r *roundTrip, purgeAt uint64,
	purge []byte) (before, after map[string][]string) {

	t.Helper()
	node, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	fetch := &heldLogFetch{log: r.log, next: 1, hold: purgeAt,
		replace: map[uint64][]byte{purgeAt: purge}}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:   worklet.Domain{},
		Spec:     worklet.Domain{}.Stream(),
		Applier:  worklet.NewApplier(nil),
		Fetch:    fetch,
		Log:      r.log,
		Node:     node,
		DB:       estate,
		Verifier: r.verifier,
	})
	if err != nil {
		t.Fatalf("build the node's applier: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the node's loop ended with %v", err)
		}
	}()
	reach := func(seq uint64) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for runner.Committed().Seq < seq {
			select {
			case err := <-done:
				t.Fatalf("the node's loop stopped at %d of %d: %v",
					runner.Committed().Seq, seq, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("the node reached %d of %d", runner.Committed().Seq, seq)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	reach(purgeAt - 1)
	before = dumpReproducible(t, estate)
	fetch.release()
	reach(purgeAt)
	return before, dumpReproducible(t, estate)
}

// dumpReproducible reads every row of every table the identity claim covers,
// each row rendered as a JSON array of its columns, sorted.
func dumpReproducible(t *testing.T, db store.ReplicatedHandle) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		for _, table := range worklet.ReproducibleTables {
			rows, err := tx.QueryContext(t.Context(), `SELECT * FROM `+table)
			if err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
			columns, err := rows.Columns()
			if err != nil {
				_ = rows.Close()
				return err
			}
			dumped := []string{}
			for rows.Next() {
				values := make([]any, len(columns))
				targets := make([]any, len(columns))
				for i := range values {
					targets[i] = &values[i]
				}
				if err := rows.Scan(targets...); err != nil {
					_ = rows.Close()
					return err
				}
				for i, v := range values {
					if b, ok := v.([]byte); ok {
						values[i] = string(b)
					}
				}
				encoded, err := json.Marshal(values)
				if err != nil {
					_ = rows.Close()
					return err
				}
				dumped = append(dumped, string(encoded))
			}
			if err := rows.Close(); err != nil {
				return err
			}
			slices.Sort(dumped)
			out[table] = dumped
		}
		return nil
	}); err != nil {
		t.Fatalf("dump the node's rows: %v", err)
	}
	return out
}

// heldLogFetch hands a framework loop a harness's log in order, holding back
// every record from hold on until released, and handing a replacement payload
// for any sequence named in replace.
type heldLogFetch struct {
	log     *js.DomainLog
	mu      sync.Mutex
	next    uint64
	hold    uint64
	open    bool
	replace map[uint64][]byte
}

func (f *heldLogFetch) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = true
}

func (f *heldLogFetch) Fetch(ctx context.Context, maxMessages, _ int,
	wait time.Duration) ([]statelog.Message, error) {

	end, err := f.log.End(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if !f.open {
		end = min(end, f.hold-1)
	}
	var out []statelog.Message
	for f.next <= end && len(out) < maxMessages {
		_, payload, storedAt, ok, err := f.log.At(ctx, f.next)
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		if replaced, held := f.replace[f.next]; held {
			payload = replaced
		}
		if ok {
			out = append(out, statelog.Message{
				Seq: f.next, StoredAt: storedAt, Payload: payload,
				Ack: func() error { return nil },
			})
		}
		f.next++
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(wait, 50*time.Millisecond)):
		}
	}
	return out, nil
}

func (f *heldLogFetch) Pending(ctx context.Context) (uint64, error) {
	end, err := f.log.End(ctx)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		end = min(end, f.hold-1)
	}
	if f.next > end {
		return 0, nil
	}
	return end - f.next + 1, nil
}
