package tracker_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

func (f fixedRanking) RankItems(context.Context, string, int) ([]tracker.RankedDoc, error) {
	return f, nil
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
		apply(taskRecord(id, tracker.OpPatch, tracker.TaskPatch{
			Comment: &tracker.Comment{ID: "c-" + id, Task: id, Author: "bo",
				AuthorKind: tracker.AuthorHuman, Body: "which one is this?",
				Ask: "ana", CreatedAt: at},
		}, &tracker.Notify{Kind: tracker.ChangeComment,
			Snapshot: tracker.Snapshot{Key: "ENG-7", Assignee: "ana"}}))
	}
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

	hits, err := tracker.NewSearcher(h.db, fixedRanking{{ID: "t-1"}, {ID: "t-2"}}).
		Search(ctx, "which", 0)
	if err != nil {
		t.Fatalf("the ranked search: %v", err)
	}
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
	inbox, err := reader.Inbox(ctx, tracker.InboxQuery{
		Handle: "ana", Level: statelog.ReadStale,
	}, now)
	if err != nil {
		t.Fatalf("the inbox: %v", err)
	}
	noticed := map[string]bool{}
	for _, notice := range inbox.Notices {
		if notice.SubjectKey != "ENG-7" {
			t.Fatalf("a notice about %s stored %q, want ENG-7 — the case "+
				"depends on the key a notice was written with",
				notice.SubjectID, notice.SubjectKey)
		}
		opensAnother := notice.SubjectID != "t-1"
		if notice.SubjectKeyCollision != opensAnother {
			t.Errorf("the notice about %s carries subject_key_collision=%v, "+
				"want %v — ENG-7 opens t-1, so every other holder's notice "+
				"must link by id, the moved task's included", notice.SubjectID,
				notice.SubjectKeyCollision, opensAnother)
		}
		noticed[notice.SubjectID] = true
	}
	if len(noticed) != 3 {
		t.Fatalf("ana's inbox holds notices about %v, want all three holders", noticed)
	}

	mine, err := reader.MyWork(ctx, tracker.MyWorkQuery{
		Handle: "ana", Level: statelog.ReadStale,
	}, now)
	if err != nil {
		t.Fatalf("ana's own work: %v", err)
	}
	if len(mine.Assigned) == 0 || len(mine.AskedOfMe) == 0 ||
		len(mine.ChecklistItems) == 0 {
		t.Fatalf("ana's own work carries %d assigned, %d asks and %d "+
			"checklist items, want rows in every block", len(mine.Assigned),
			len(mine.AskedOfMe), len(mine.ChecklistItems))
	}
	for _, row := range mine.Assigned {
		check("an assigned row", row.ID, row.KeyCollision)
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
