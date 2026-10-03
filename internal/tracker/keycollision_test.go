package tracker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A KEY TWO TASKS HOLD IS FLAGGED ON THE ONES THAT DID NOT CLAIM IT, and the
// key goes on opening the one that did.
//
// The broker cannot refuse the shape: a counter and a task are different
// subjects, so a counter restored beside tasks minted after it hands out
// numbers those tasks already hold, and each create arbitrates only against
// itself. So the applier writes every task, and the attention set is the only
// place anybody learns that one key names several — which it did not, because
// the flag's only producer was a cross-project move nothing called.
//
// The CLAIMANT keeps the key because it is the task every earlier reference,
// link and chat message was written against. Resolving the key to whichever
// row the key index returns first is a different answer wherever the row
// order differs from the claim order, which is exactly what a purge that
// hands the key on produces here — and what a donated snapshot's copy does to
// row order in general. A purge of the claimant HANDS THE KEY ON, or the
// survivors stay flagged, and the key unclaimed, until their own next write.
func TestAKeyTwoTasksHoldIsFlaggedOnTheOnesThatDidNotClaimIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	// CREATED OUT OF ID ORDER, so the task a purge hands the key to (the
	// lowest id) is not the one the key index lists first (the earliest
	// row).
	for _, id := range []string{"t-1", "t-3", "t-2"} {
		task := newTask(id)
		task.Key = "ENG-7"
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, task, nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	flag := func(id string) int64 {
		t.Helper()
		return h.value(`SELECT key_collision FROM tracker_tasks WHERE id = ?`, id)
	}
	if flag("t-1") != 0 {
		t.Error("the task that claimed ENG-7 first is flagged — the claimant " +
			"is the one every earlier reference names, not a duplicate")
	}
	for _, id := range []string{"t-2", "t-3"} {
		if flag(id) != 1 {
			t.Errorf("%s holds ENG-7 beside its claimant and carries no "+
				"key_collision — the attention set is the only place a "+
				"duplicate key is ever named", id)
		}
	}

	log, err := statelogtest.LocalReader(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	opened := func() string {
		t.Helper()
		got, err := reader.Task(t.Context(), "eng-7", tracker.DetailWants{},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read ENG-7: %v", err)
		}
		return got.Task.ID
	}
	if got := opened(); got != "t-1" {
		t.Errorf("ENG-7 opens %s, want t-1 — the claimant", got)
	}

	// A LATER WRITE TO A DUPLICATE KEEPS ITS FLAG: the flag is derived from
	// the directory on every apply, not set once by the create.
	patch := taskRecord("t-3", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("still a duplicate")}, nil)
	patch.OpID = "t-3-rename"
	if _, err := h.apply(patch, at.Add(time.Minute)); err != nil {
		t.Fatalf("patch t-3: %v", err)
	}
	if flag("t-3") != 1 {
		t.Error("an edit to a duplicate cleared its key_collision, although " +
			"the claimant still holds ENG-7")
	}

	purge := taskRecord("t-1", tracker.OpPurge,
		map[string]any{"reason": "the restored duplicate"}, nil)
	if _, err := h.apply(purge, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("purge t-1: %v", err)
	}
	if got := h.value(`SELECT COUNT(*) FROM tracker_task_keys
		WHERE key = 'ENG-7' AND task_id = 't-2'`); got != 1 {
		t.Error("the directory does not name t-2 for ENG-7 after the " +
			"claimant's purge — the lowest id among the holders takes it, so " +
			"every node picks the same one")
	}
	if flag("t-2") != 0 {
		t.Error("t-2 claimed ENG-7 when the claimant was purged and is still " +
			"flagged — the purge did not re-derive the holders' flags")
	}
	if flag("t-3") != 1 {
		t.Error("t-3 still shares ENG-7 with t-2 and lost its key_collision")
	}
	if got := opened(); got != "t-2" {
		t.Errorf("ENG-7 opens %s after the purge, want t-2 — the new claimant, "+
			"whatever row the key index happens to list first", got)
	}
}

// fixedRanking is a [tracker.Ranker] that answers one ranking whatever it is
// asked, so a case about what a HIT carries does not depend on the index.
type fixedRanking []tracker.RankedDoc

func (f fixedRanking) RankItems(context.Context, tracker.SearchQuery) (tracker.RankedDocs, error) {
	return tracker.RankedDocs{Docs: f}, nil
}

func (fixedRanking) Building(context.Context) bool { return false }

// EVERY ROW AN ITEM IS OPENED FROM SAYS WHEN ITS KEY OPENS ANOTHER TASK.
//
// A key two tasks hold resolves to the one that claimed it, on every read
// (see the case above). So a flagged task is reached by its ID, and the only
// way a screen or a model can know to use the id is that the row it is
// opening FROM says so: a row without the flag hands its key onward, and the
// gesture that names the duplicate opens the claimant instead. That held for
// the attention queue's own filter and for nothing a person clicks — the
// board rows, a ranked hit, the item and its links, the feed, the inbox and
// every block of somebody's own work all carried the key alone.
//
// AND A NOTICE IS ASKED ABOUT ITS OWN KEY. A notice stores the key its subject
// held when it was written, so a duplicate moved to another project since
// answers to a fresh key nobody shares — while the key its notice carries
// still opens the claimant. The task's own flag describes the new key; the
// notice's has to describe the one a reader would follow.
func TestEveryRowAnItemIsOpenedFromSaysWhenItsKeyOpensAnotherTask(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	step := 0
	apply := func(rec tracker.MutationRecord) {
		t.Helper()
		step++
		rec.OpID = rec.OpID + "-" + itoa(step)
		if _, err := h.apply(rec, at.Add(time.Duration(step)*time.Second)); err != nil {
			t.Fatalf("apply %s: %v", rec.OpID, err)
		}
	}
	held := func(id, title string) tracker.Task {
		task := newTask(id)
		task.Key, task.Title, task.Assignee = "ENG-7", title, "ana"
		task.Checklists = []tracker.Checklist{{ID: "cl-" + id, Name: "steps",
			Items: []tracker.ChecklistItem{{ID: "it-" + id, Name: "check " + id,
				Assignee: "ana"}}}}
		return task
	}
	// t-1 CLAIMS ENG-7, and t-2 and t-3 hold it beside it.
	for _, task := range []tracker.Task{held("t-1", "the claimant"),
		held("t-2", "the duplicate"), held("t-3", "the moved duplicate")} {
		apply(taskRecord(task.ID, tracker.OpCreate, task, nil))
	}
	neighbour := newTask("t-5")
	neighbour.Key = "ENG-5"
	neighbour.Relations = []tracker.Relation{
		{Kind: tracker.RelationLinked, Other: "t-1"},
		{Kind: tracker.RelationLinked, Other: "t-2"},
	}
	apply(taskRecord("t-5", tracker.OpCreate, neighbour, nil))
	// AN ASK AND A NOTICE ON EACH HOLDER, so the inbox, the asks and the
	// feed all have a row about every one of them.
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		comment := taskRecord(id, tracker.OpPatch, tracker.TaskPatch{
			Comment: &tracker.Comment{ID: "c-" + id, Task: id, Author: "bo",
				AuthorKind: tracker.AuthorHuman, Body: "which one is this?",
				Ask: "ana", CreatedAt: at},
		}, &tracker.Notify{Kind: tracker.ChangeComment,
			Snapshot: tracker.Snapshot{Key: "ENG-7", Assignee: "ana"}})
		// BY BO, who wrote the comment: a change is never news to its own
		// author, and the notice under test is ana's.
		comment.Actor = "bo"
		apply(comment)
	}
	// A LEAD PUTS A HOLDER AT THE TOP OF SOMEBODY'S PRIORITIES — the one
	// notice whose SUBJECT is not the task it is about: the subject is the
	// person, and the key beside it is the task's. bo puts the duplicate at
	// the top of ana's list and the claimant at the top of cy's.
	prioritise := func(person, top string) tracker.MutationRecord {
		body, err := json.Marshal(tracker.Person{V: 1, Handle: person,
			Priorities: []string{top}})
		if err != nil {
			t.Fatalf("encode %s's list: %v", person, err)
		}
		return tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				V: tracker.RecordVersion, OpID: person + "-prioritised",
				Subject: tracker.PersonSubject(person), Op: tracker.OpPatch,
				CreatedAt: at, Writer: "node-a",
				Scope: tracker.ScopeSet{Subject: true},
			},
			Mutation: body, Actor: "bo", ActorKind: tracker.AuthorHuman,
			Notify: &tracker.Notify{Kind: tracker.ChangePrioritised,
				Snapshot: tracker.Snapshot{Person: person, Task: top,
					Key: "ENG-7", Project: "ENG", PrioritisedBy: "bo",
					Position: 1}},
		}
	}
	apply(prioritise("ana", "t-2"))
	apply(prioritise("cy", "t-1"))
	// t-3 MOVES, and the key it moves to is its own.
	former := []string{"ENG-7"}
	apply(taskRecord("t-3", tracker.OpPatch, tracker.TaskPatch{
		Project: ptr("OPS"), FormerKeys: &former, Mint: &tracker.KeyMint{N: 1},
	}, nil))

	log, err := statelogtest.LocalReader(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx, now := t.Context(), at.Add(time.Hour)
	stale := statelog.Freshness{Level: statelog.ReadStale}
	// WHAT EACH HOLDER'S FLAG MUST BE, which is the whole expectation: the
	// claimant answers to its key, the duplicate does not, and the moved
	// duplicate answers to its NEW key.
	want := map[string]bool{"t-1": false, "t-2": true, "t-3": false}
	check := func(surface, id string, got bool) {
		t.Helper()
		if exp, ok := want[id]; ok && got != exp {
			t.Errorf("%s: %s carries key_collision=%v, want %v — a row is how "+
				"a reader knows whether its key opens it or the claimant",
				surface, id, got, exp)
		}
	}

	board, err := reader.Tasks(ctx, tracker.Query{
		Scope: tracker.Scope{Workspace: true}, Level: statelog.ReadStale,
	}, now)
	if err != nil {
		t.Fatalf("the board: %v", err)
	}
	seen := map[string]bool{}
	for _, row := range board.Rows {
		seen[row.ID] = true
		check("a board row", row.ID, row.KeyCollision)
		switch {
		case row.ID == "t-2" && row.Address() != "t-2":
			t.Errorf("the duplicate's row addresses itself as %q — its key "+
				"opens t-1, so the only address that reaches it is its id",
				row.Address())
		case row.ID == "t-1" && row.Address() != "ENG-7":
			t.Errorf("the claimant's row addresses itself as %q, want its key",
				row.Address())
		}
	}
	if !seen["t-1"] || !seen["t-2"] {
		t.Fatalf("the board answered %v, want both holders of ENG-7", seen)
	}

	found, err := tracker.NewSearcher(h.db, fixedRanking{{ID: "t-1"}, {ID: "t-2"}}).
		Search(ctx, tracker.SearchQuery{Text: "which", Limit: 10})
	if err != nil {
		t.Fatalf("the ranked search: %v", err)
	}
	hits := found.Hits
	if len(hits) != 2 {
		t.Fatalf("the ranked search answered %d hits, want both holders", len(hits))
	}
	for _, hit := range hits {
		check("a ranked hit", hit.ID, hit.KeyCollision)
	}

	// THE ITEM ITSELF, by its id and by its key — the two addresses reach
	// two different tasks, and each answer says which it is.
	for ref, id := range map[string]string{"t-2": "t-2", "ENG-7": "t-1"} {
		detail, err := reader.Task(ctx, ref, tracker.DetailWants{}, stale)
		if err != nil {
			t.Fatalf("open %s: %v", ref, err)
		}
		if detail.Task.ID != id {
			t.Fatalf("%s opens %s, want %s", ref, detail.Task.ID, id)
		}
		check("the item answer", id, detail.KeyCollision)
	}
	linked, err := reader.Task(ctx, "t-5", tracker.DetailWants{Links: true}, stale)
	if err != nil {
		t.Fatalf("open t-5: %v", err)
	}
	if len(linked.Links) != 2 {
		t.Fatalf("t-5 answers %d links, want its two", len(linked.Links))
	}
	for _, link := range linked.Links {
		check("a link's other end", link.Other, link.KeyCollision)
	}

	feed, err := reader.Activity(ctx, tracker.ActivityQuery{
		Workspace: true, Level: statelog.ReadStale,
	}, now)
	if err != nil {
		t.Fatalf("the feed: %v", err)
	}
	fed := map[string]bool{}
	for _, record := range feed.Records {
		if _, ok := want[record.SubjectID]; ok {
			fed[record.SubjectID] = true
			check("a feed record's subject", record.SubjectID,
				record.SubjectKeyCollision)
		}
	}
	if len(fed) != len(want) {
		t.Fatalf("the feed carries records about %v, want every holder of ENG-7", fed)
	}

	// THE INBOX ASKS ABOUT THE KEY IT STORED: every notice here carries
	// ENG-7, and ENG-7 opens t-1 — so the moved duplicate's notice is
	// flagged although the task's own flag, about OPS-1, is not.
	//
	// AND IT ASKS ABOUT THE TASK THE NOTICE NAMES, never the subject: a
	// prioritised notice's subject is a PERSON, so a flag asked of the
	// subject read every one of them as a collision and sent the reader to
	// the person's handle as though it were a task. The answer names the
	// task, which is what a reader opens when the key opens another.
	inboxOf := func(handle string) []tracker.InboxNotice {
		t.Helper()
		inbox, err := reader.Inbox(ctx, tracker.InboxQuery{
			Handle: handle, Level: statelog.ReadStale, Snoozed: tracker.SnoozeExclude,
		}, now)
		if err != nil {
			t.Fatalf("%s's inbox: %v", handle, err)
		}
		return inbox.Notices
	}
	noticed := map[string]bool{}
	prioritised := map[string]tracker.InboxNotice{}
	for _, notice := range append(inboxOf("ana"), inboxOf("cy")...) {
		if notice.SubjectKey != "ENG-7" {
			t.Fatalf("a notice about %s stored %q, want ENG-7 — the case "+
				"depends on the key a notice was written with",
				notice.Task, notice.SubjectKey)
		}
		if notice.Kind == tracker.ChangePrioritised {
			prioritised[notice.SubjectID] = notice
		} else if notice.Task != notice.SubjectID {
			t.Errorf("a %s notice on %s names the task %q — a task commit's "+
				"notice is about its own subject", notice.Kind,
				notice.SubjectID, notice.Task)
		}
		opensAnother := notice.Task != "t-1"
		if notice.SubjectKeyCollision != opensAnother {
			t.Errorf("the %s notice about %s carries subject_key_collision=%v, "+
				"want %v — ENG-7 opens t-1, so every other holder's notice "+
				"must link by id, the moved task's included", notice.Kind,
				notice.Task, notice.SubjectKeyCollision, opensAnother)
		}
		noticed[notice.Task] = true
	}
	if len(noticed) != 3 {
		t.Fatalf("the inboxes hold notices about %v, want all three holders", noticed)
	}
	for person, task := range map[string]string{"ana": "t-2", "cy": "t-1"} {
		if got, ok := prioritised[person]; !ok || got.Task != task {
			t.Errorf("%s's prioritised notice names the task %q, want %s — "+
				"its subject is %s, so the task is the only thing that says "+
				"which holder of ENG-7 reached the top of the list",
				person, got.Task, task, person)
		}
	}

	mine, err := reader.MyWork(ctx, tracker.MyWorkQuery{
		Handle: "ana", Level: statelog.ReadStale,
	}, now, time.UTC)
	if err != nil {
		t.Fatalf("ana's own work: %v", err)
	}
	if len(mine.Assigned) == 0 || len(mine.AskedOfMe) == 0 ||
		len(mine.ChecklistItems) == 0 || len(mine.Priorities) == 0 {
		t.Fatalf("ana's own work carries %d assigned, %d asks, %d "+
			"checklist items and %d priorities, want rows in every block",
			len(mine.Assigned), len(mine.AskedOfMe), len(mine.ChecklistItems),
			len(mine.Priorities))
	}
	for _, row := range mine.Assigned {
		check("an assigned row", row.ID, row.KeyCollision)
	}
	for _, row := range mine.Priorities {
		check("a priorities row", row.ID, row.KeyCollision)
	}
	for _, ask := range mine.AskedOfMe {
		check("an ask", ask.ID, ask.KeyCollision)
		// AND THE CALL IT HANDS OVER answers on the task the ask is on.
		if !strings.Contains(ask.Answer, `item: "`+ask.Address()+`"`) {
			t.Errorf("the ask on %s hands over %s — the call must name the "+
				"task's address, or answering it posts on the claimant",
				ask.ID, ask.Answer)
		}
	}
	for _, item := range mine.ChecklistItems {
		check("a checklist item's task", item.Task, item.TaskKeyCollision)
	}
}

// noticeTaskMigration is the replicated migration that gave a notice the id of
// the task it is about, read from the file that ships it.
const noticeTaskMigration = "internal/store/schema/replicated/" +
	"0047_a_notice_names_the_task_it_is_about.sql"

// A NOTICE FROM BEFORE IT NAMED ITS TASK READS AS IT DID THEN, OR BETTER.
//
// The column is DERIVED, so the rows written before it are filled by the
// applier's own re-derivation (derivation version 7) and never by a second
// copy of the rule in the migration's SQL — which this case also holds, by
// reading the migration and refusing a backfill in it. The two kinds of
// notice come out differently on purpose. A task commit's notice is about its
// own subject, so it is filled from there and a duplicate's notice links by id
// exactly as one written today does. A prioritised notice's subject is a
// PERSON, and the task its wake named is in no row the pass reaches — so it
// stays empty, and an empty task must leave the key as the notice's address:
// asked against nothing, every directory row reads as "another task", and the
// reader is sent to an id the notice does not have.
//
// OVER ROWS PUT BACK THE WAY THE BUILD BEFORE THE COLUMN WROTE THEM, and then
// [tracker.Applier.Rederive] — the pass a node runs on its first boot of a
// build whose rules differ from its checkpoint's.
func TestANoticeFromBeforeItNamedItsTaskStillOpensWhatItMeant(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	step := 0
	apply := func(rec tracker.MutationRecord) {
		t.Helper()
		step++
		rec.OpID = rec.OpID + "-" + itoa(step)
		if _, err := h.apply(rec, at.Add(time.Duration(step)*time.Second)); err != nil {
			t.Fatalf("apply %s: %v", rec.OpID, err)
		}
	}
	for _, id := range []string{"t-1", "t-2"} {
		task := newTask(id)
		task.Key, task.Assignee = "ENG-7", "ana"
		apply(taskRecord(id, tracker.OpCreate, task, nil))
	}
	// A COMMENT ON THE DUPLICATE, for ana — a task commit's notice — by bo,
	// since a change is never news to its own author.
	byBo := func(rec tracker.MutationRecord) tracker.MutationRecord {
		rec.Actor = "bo"
		return rec
	}
	apply(byBo(taskRecord("t-2", tracker.OpPatch, tracker.TaskPatch{
		Comment: &tracker.Comment{ID: "c-t-2", Task: "t-2", Author: "bo",
			AuthorKind: tracker.AuthorHuman, Body: "is this the one?",
			CreatedAt: at},
	}, &tracker.Notify{Kind: tracker.ChangeComment,
		Snapshot: tracker.Snapshot{Key: "ENG-7", Assignee: "ana"}})))
	// AND ONE ON A TASK SINCE PURGED, for di: its row is gone, so only its
	// history says the notice was a task's.
	gone := newTask("t-3")
	gone.Key, gone.Assignee = "ENG-9", "di"
	apply(taskRecord("t-3", tracker.OpCreate, gone, nil))
	apply(byBo(taskRecord("t-3", tracker.OpPatch, tracker.TaskPatch{
		Comment: &tracker.Comment{ID: "c-t-3", Task: "t-3", Author: "bo",
			AuthorKind: tracker.AuthorHuman, Body: "still needed?",
			CreatedAt: at},
	}, &tracker.Notify{Kind: tracker.ChangeComment,
		Snapshot: tracker.Snapshot{Key: "ENG-9", Assignee: "di"}})))
	apply(taskRecord("t-3", tracker.OpPurge,
		map[string]any{"reason": "filed twice"}, nil))
	// AND THE CLAIMANT AT THE TOP OF CY'S LIST — a person's notice.
	body, err := json.Marshal(tracker.Person{V: 1, Handle: "cy",
		Priorities: []string{"t-1"}})
	if err != nil {
		t.Fatalf("encode cy's list: %v", err)
	}
	apply(tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "cy-prioritised",
			Subject: tracker.PersonSubject("cy"), Op: tracker.OpPatch,
			CreatedAt: at, Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "bo", ActorKind: tracker.AuthorHuman,
		Notify: &tracker.Notify{Kind: tracker.ChangePrioritised,
			Snapshot: tracker.Snapshot{Person: "cy", Task: "t-1", Key: "ENG-7",
				Project: "ENG", PrioritisedBy: "bo", Position: 1}},
	})

	text, err := os.ReadFile(filepath.Join(sourcetree.Root(t), noticeTaskMigration))
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if strings.Contains(string(text), "UPDATE tracker_notifications") {
		t.Fatalf("%s backfills the column in SQL — a derived column has one "+
			"implementation, the applier's re-derivation, and a second copy of "+
			"the rule drifts the day either is edited", noticeTaskMigration)
	}
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		// AS THE BUILD BEFORE THE COLUMN LEFT THEM — and with ana's
		// notice's history row gone, as a reanchor leaves a notice that
		// outlived it: only the task row it names says it was a task's.
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_notifications SET task_id = ''`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `DELETE FROM tracker_history
			WHERE id IN (SELECT record_id FROM tracker_notifications
			             WHERE recipient = 'ana')`); err != nil {
			return err
		}
		_, err := h.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if tracker.DerivationVersion < 7 {
		t.Error("the applier fills a notice's task_id by re-derivation and " +
			"its derivation version does not say so — a node upgrading onto " +
			"rows written without the column would never fill them")
	}

	log, err := statelogtest.LocalReader(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name})
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	only := func(handle string) tracker.InboxNotice {
		t.Helper()
		inbox, err := reader.Inbox(t.Context(), tracker.InboxQuery{
			Handle: handle, Level: statelog.ReadStale, Snoozed: tracker.SnoozeExclude,
		}, at.Add(time.Hour))
		if err != nil {
			t.Fatalf("%s's inbox: %v", handle, err)
		}
		if len(inbox.Notices) != 1 {
			t.Fatalf("%s's inbox holds %d notices, want the one", handle,
				len(inbox.Notices))
		}
		return inbox.Notices[0]
	}
	if got := only("ana"); got.Task != "t-2" || !got.SubjectKeyCollision {
		t.Errorf("the comment notice on the duplicate came out of the "+
			"re-derivation naming task %q with subject_key_collision=%v, want t-2 "+
			"and true — it is about its own subject, and ENG-7 opens t-1",
			got.Task, got.SubjectKeyCollision)
	}
	if got := only("di"); got.Task != "t-3" {
		t.Errorf("the notice on a purged task came out of the re-derivation "+
			"naming task %q, want t-3 — its row is gone, and its history is "+
			"what says the notice was a task's", got.Task)
	}
	if got := only("cy"); got.Task != "" || got.SubjectKeyCollision ||
		got.SubjectKey != "ENG-7" {

		t.Errorf("the prioritised notice came out of the re-derivation naming "+
			"task %q under key %q with subject_key_collision=%v, want no task, "+
			"ENG-7 and false — its subject is a person, nothing it holds names "+
			"the task, and its key is the only address it has",
			got.Task, got.SubjectKey, got.SubjectKeyCollision)
	}
}
