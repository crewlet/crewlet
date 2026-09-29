package tracker_test

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
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PURGE THIS BUILD WRITES LEAVES NO COPY OF THE TASK BEHIND, in any table.
//
// The object tables went and the history did not: every history row keeps its
// record's whole mutation — each title, body and comment the task was ever
// given — beside an excerpt, an inbox notice keeps an excerpt of its own, and
// turn records and the dependency mirror stayed keyed on an id nothing
// resolves. So a task purged because it held something that must not be kept
// stayed readable on every node for the life of the company, while the purge
// report and the docs said it was destroyed.
//
// What survives is the purge's own account — that it happened, to which key,
// by whom and why — and the lead's `purged` notice, which is read through it.
// The writer puts a purge on the log at record version 4, and it is version 4
// that destroys these rows ([TestAPurgeDestroysWhatItsTaskWroteFromVersionFourOnly]).
func TestAPurgeLeavesNoCopyOfTheTaskBehind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const secret = "the merger with Contoso"
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	// THE CONTENT, in every place a record puts it: a comment that
	// mentions somebody (a history row, its document and an inbox
	// notice), a turn's spend, and a dependency whose mirror is a row on
	// the task.
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Comment: &tracker.Comment{ID: "c-1", Body: secret, Author: "jane"},
		}, tracker.ChangeComment, &tracker.Notify{
			Kind: tracker.ChangeComment, Excerpt: secret, Mentions: []string{"ana"},
		}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn", tracker.TurnRecord{
		Task: "t-1", Seat: "swe", TurnID: "run-1",
		Spend: tracker.TurnSpend{Turns: 1, Input: 10},
	}); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
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
	// THE FIXTURE HOLDS WHAT IT SAYS, or the assertions below pass on
	// a task that never had any of it.
	for name, query := range map[string]string{
		"history": histories, "a notice": notices, "a turn": turns,
		"a mirror": mirrors, "a copy of the comment": copies,
	} {
		if len(about(query)) == 0 {
			t.Fatalf("the fixture wrote no %s about t-1", name)
		}
	}

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	if got := about(histories); len(got) != 1 || got[0] != "op-purge" {
		t.Errorf("after the purge the task's history is %v, want the purge's "+
			"own row alone", got)
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

// A PURGE DESTROYS WHAT ITS TASK'S OWN RECORDS WROTE FROM RECORD VERSION 4, AND
// A VERSION-1 PURGE APPLIES EXACTLY AS EVERY BUILD BEFORE VERSION 4 APPLIED IT —
// through the real framework loop, over the real log.
//
// Destroying the history, the notices, the turn records and the dependency
// mirror changed what a purge's APPLY does, and a record's apply is a function
// of its version ([tracker.RecordVersion]). Applied to a version-1 record the
// new way, the same record would leave these rows on every node that applied
// it before version 4 and remove them on every node replaying it now — after
// adopting a snapshot, or beside an older build in a rolling upgrade — and the
// identity claim says every node holds the same rows.
//
// So the version-1 half holds the rule every earlier build applied, stated as
// what it touched: the thirteen object tables its purge deleted from, the
// census it lowered, and the three tables it only ADDED to (its deletion
// marker, its own history row, the notices that row routed). Every other row
// in every table the identity claim covers is byte-identical before and after.
func TestAPurgeDestroysWhatItsTaskWroteFromVersionFourOnly(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const secret = "the merger with Contoso"
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	filedTask(t, r, "t-3")
	// THE CONTENT, in every place a record puts it: a comment mentioning
	// somebody (a history row, its document and an inbox notice), a turn's
	// spend, a dependent (an edge on it, a mirror row on the purged task),
	// and a child (re-parented by every version).
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Comment: &tracker.Comment{ID: "c-1", Body: secret, Author: "jane"},
		}, tracker.ChangeComment, &tracker.Notify{
			Kind: tracker.ChangeComment, Excerpt: secret, Mentions: []string{"ana"},
		}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn", tracker.TurnRecord{
		Task: "t-1", Seat: "swe", TurnID: "run-1",
		Spend: tracker.TurnSpend{Turns: 1, Input: 10},
	}); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	r.waitOn(oneTask(t, r, "t-2"), oneTask(t, r, "t-1"))
	r.fileUnder(oneTask(t, r, "t-3"), oneTask(t, r, "t-1"))

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	purgeAt := r.logEnd(t)
	written := r.recordAt(t, purgeAt)
	if written.Op != tracker.OpPurge {
		t.Fatalf("the premise: record %d is a %s, want the purge", purgeAt, written.Op)
	}

	for _, version := range []int{1, 4} {
		t.Run("version "+itoa(version), func(t *testing.T) {
			// THE SAME PURGE AT THIS VERSION. A version-1 one carries the
			// scope every writer before version 4 stated: the task and its
			// project.
			rec := written
			rec.V = version
			if version < 4 {
				rec.Scope = tracker.ScopeSet{Subject: true, Container: "ENG"}
			}
			body, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("encode the version-%d purge: %v", version, err)
			}
			before, after := applyPurgeThroughARunner(t, r, purgeAt, body)

			if _, held := after["tracker_tasks"]; !held {
				t.Fatal("the dump read no task table")
			}
			if slices.ContainsFunc(after["tracker_tasks"], func(row string) bool {
				return strings.HasPrefix(row, `["t-1",`)
			}) {
				t.Fatal("the purged task's row survives the purge")
			}
			if version < 4 {
				assertTheRuleBeforeVersionFour(t, before, after)
				return
			}
			assertTheTasksRecordsAreGone(t, after, secret)
		})
	}
}

// assertTheRuleBeforeVersionFour fails unless a purge touched exactly what
// every build before record version 4 did: its thirteen DELETE targets and the
// project census, plus additions (never a removal) to its marker, its history
// row and the notices that row routed.
func assertTheRuleBeforeVersionFour(t *testing.T, before, after map[string][]string) {
	t.Helper()
	deleted := map[string]bool{
		"tracker_references": true, "tracker_task_keys": true,
		"tracker_watchers": true, "tracker_collaborators": true,
		"tracker_task_tags": true, "tracker_relations": true,
		"tracker_task_deps": true, "tracker_checklist_items": true,
		"tracker_field_values": true, "tracker_task_closure": true,
		"tracker_body_revisions": true, "tracker_comments": true,
		"tracker_tasks": true,
		// The census moved down with the row.
		"tracker_projects": true,
	}
	added := map[string]bool{
		"tracker_deletions": true, "tracker_history": true,
		"tracker_notifications": true,
	}
	for _, table := range tracker.ReproducibleTables {
		was, is := before[table], after[table]
		switch {
		case deleted[table]:
			continue
		case added[table]:
			for _, row := range was {
				if !slices.Contains(is, row) {
					t.Errorf("a version-1 purge removed a %s row every build before "+
						"version 4 kept: %s", table, row)
				}
			}
		case !slices.Equal(was, is):
			t.Errorf("a version-1 purge changed %s, which no build before version "+
				"4 touched:\n  before %v\n  after  %v", table, was, is)
		}
	}
	// The rows the fixture is about, named, so a failure above has a
	// premise to read against.
	for _, table := range []string{"tracker_turns", "tracker_task_dependents"} {
		if len(before[table]) == 0 {
			t.Errorf("the premise: the fixture wrote no %s row", table)
		}
	}
}

// assertTheTasksRecordsAreGone fails unless nothing the purged task's own
// records wrote survives beside the purge's own account.
func assertTheTasksRecordsAreGone(t *testing.T, after map[string][]string, secret string) {
	t.Helper()
	for _, row := range after["tracker_history"] {
		if strings.Contains(row, `"t-1"`) && !strings.HasPrefix(row, `["op-purge",`) {
			t.Errorf("a version-4 purge left a history row about the task: %s", row)
		}
	}
	for table, rows := range after {
		for _, row := range rows {
			if strings.Contains(row, secret) {
				t.Errorf("the purged comment survives a version-4 purge in %s: %s",
					table, row)
			}
		}
	}
	for _, table := range []string{"tracker_turns", "tracker_task_dependents"} {
		for _, row := range after[table] {
			if strings.Contains(row, `"t-1"`) {
				t.Errorf("a version-4 purge left a %s row naming the task: %s", table, row)
			}
		}
	}
	for _, row := range after["tracker_notifications"] {
		if strings.Contains(row, `"t-1"`) && !strings.Contains(row, `"op-purge"`) {
			t.Errorf("a version-4 purge left an inbox notice about the task: %s", row)
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
		Domain:  tracker.Domain{},
		Spec:    statelog.EstateStream(tracker.Domain{}),
		Layout:  statelog.EstateLayout(tracker.Domain{}.Name()),
		LogID:   statelog.EstateLog(tracker.Domain{}),
		Applier: tracker.NewApplier("node-b"),
		Fetch:   fetch,
		Log:     r.log,
		Node:    node,
		DB:      estate,
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
func dumpReproducible(t *testing.T, db store.PartitionHandle) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		for _, table := range tracker.ReproducibleTables {
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
