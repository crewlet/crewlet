package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CHANGE NOBODY IS TOLD ABOUT STILL NAMES WHAT IT WAS.
//
// This is the whole finding as one case. `tracker_history.kind` is what the
// activity feed's `kinds=` filter, every report window and the unblocked
// repair's own scan select on — and it used to be read off the NOTIFICATION,
// so a record that carried none had its kind guessed from the OPERATION
// instead. The file that writes the row opens by stating the principle that
// breaks: the feed is "a complete account of what HAPPENED rather than an
// account of what was ANNOUNCED, and `notified` is how a reader tells
// 'nothing was announced' from 'nothing happened'".
//
// A CATALOGUE EDIT AND A VIEW SAVE ARE THE SHARPEST INSTANCES, because they
// are quiet BY DESIGN and on every path: a wake per catalogue edit would page
// the whole company for a renamed dropdown, and a saved view is read from its
// own strip rather than woken into anybody's inbox. Both therefore reached the
// feed as `patch` — which is an [tracker.OpKind], not a [tracker.ChangeKind],
// so `kinds=catalogue_updated` and `kinds=view_saved` found nothing at all
// while the rows sat there.
func TestAQuietChangeStillNamesWhatItWas(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []tracker.TaskType{
		{Slug: "incident", Name: "Incident", Plural: "Incidents"},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()
	if _, err := r.writer.WriteView(t.Context(), "op-view", aView("v-1", nil)); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	for _, tc := range []struct {
		subject, id string
		want        tracker.ChangeKind
	}{
		{"catalogue", "types", tracker.ChangeCatalogue},
		{"view", "v-1", tracker.ChangeViewSaved},
	} {
		got := historyKind(t, r, tc.subject, tc.id)
		if got != string(tc.want) {
			t.Errorf("a %s edit filed under %q, want %q — nobody was told, "+
				"which is not the same as nothing having happened",
				tc.subject, got, tc.want)
		}
		// AND IT IS STILL MARKED AS ANNOUNCED TO NOBODY, so the two
		// facts stay separate: `kind` says what, `notified` says whether.
		if notified := historyNotified(t, r, tc.subject, tc.id); notified {
			t.Errorf("the %s edit reports itself notified — it carries no "+
				"notification at all", tc.subject)
		}
	}
}

// EVERY HISTORY ROW NAMES A KIND THIS BUILD KNOWS.
//
// The fallback used to end at `ChangeKind(op)` — the OPERATION, cast — and
// four of the nine [tracker.OpKind]s are not [tracker.ChangeKind]s at all.
// Three are near-misses of one: a quiet removal filed as `tombstone` while the
// filter spells it `removed`, a restore as `restore` against `restored`, a
// purge as `purge` against `purged`. A word one letter from the right one is
// the worst possible value here, because it reads correct in a database dump
// and matches nothing.
func TestEveryHistoryRowNamesAKindThisBuildKnows(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)

	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-quiet-status", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("a quiet status change: %v", err)
	}
	r.drain()
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-1", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	r.drain()
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-1", "ENG",
		nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()

	kinds := historyKinds(t, r)
	if len(kinds) < 4 {
		t.Fatalf("%d history rows, want the create and the three writes", len(kinds))
	}
	for _, got := range kinds {
		if !tracker.ChangeKind(got).Valid() {
			t.Errorf("a history row is filed under %q, which is not a change "+
				"kind this build knows — every reader of this column selects "+
				"on it by name, so a value outside the set is a row nobody "+
				"can ever find", got)
		}
	}
	// AND THE TWO THAT USED TO BE NEAR-MISSES ARE THE RIGHT WORDS.
	for _, want := range []tracker.ChangeKind{
		tracker.ChangeRemoved, tracker.ChangeRestored,
	} {
		if !containsKind(kinds, string(want)) {
			t.Errorf("no history row is filed under %q; the rows are %v — "+
				"the operation's own word (`tombstone`, `restore`) is one "+
				"letter from this and matches no filter", want, kinds)
		}
	}
}

// A RECORD THAT WRITES A HISTORY ROW IS REFUSED WITHOUT A KIND, and one that
// writes none is refused WITH one.
//
// Both halves, because the rule is a partition rather than a requirement: a
// barrier, a turn, a rank move and an alias produce no row for a kind to
// describe, and a change kind on one of those would be a word about a record
// nobody reads. The guard is what makes a kind added later fail the writer
// rather than publish a row filed under a guess.
func TestARecordStatesItsKindExactlyWhenItWritesOne(t *testing.T) {
	t.Parallel()
	for _, kind := range tracker.ObjectKinds {
		records := kind.RecordsHistory()
		switch kind {
		case tracker.KindTask, tracker.KindProject, tracker.KindSprint,
			tracker.KindTags, tracker.KindCatalogue, tracker.KindView,
			tracker.KindGoal, tracker.KindPerson:
			if !records {
				t.Errorf("%s writes a history row and says it does not", kind)
			}
		default:
			if records {
				t.Errorf("%s writes no history row and says it does — a kind "+
					"on such a record is a word about a row nobody reads", kind)
			}
		}
	}
}

// THE KIND AND THE NOTIFICATION MUST AGREE, because they are one fact with two
// carriers — and a disagreement is invisible: the feed files the row under one
// word while the card renders the other, and nothing compares them.
func TestARecordCannotSayOneThingAndAnnounceAnother(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)

	done := tracker.StatusDone
	_, err := r.writer.UpdateTask(t.Context(), "op-mismatch", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus,
		&tracker.Notify{
			Kind:     tracker.ChangeAssignee,
			Snapshot: tracker.Snapshot{Key: "ENG-1", Project: "ENG", Assignee: "ana"},
		})
	if err == nil {
		t.Fatal("a record claiming `status` while announcing `assignee` was " +
			"accepted — the feed would file it one way and the card render " +
			"the other, and nothing anywhere compares the two")
	}
	for _, want := range []string{"status", "assignee"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// THE ONE IRREVERSIBLE OPERATION TELLS THE PROJECT LEAD.
//
// It told nobody. Every purge published with a nil notification, while
// [tracker.Candidates] carried a `purged` branch and `ReasonPurged` sat in the
// reason list — dead code on one side and silence on the other, each looking
// like the other's explanation. A task and every comment, revision, history
// row and turn record on it were destroyed and the only person accountable for
// that project heard nothing.
func TestAPurgeTellsTheProjectLead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	if got := historyKind(t, r, "task", "t-1"); got != string(tracker.ChangePurged) {
		t.Errorf("the purge filed under %q, want %q — `purge` is the "+
			"OPERATION and no filter spells it that way",
			got, tracker.ChangePurged)
	}
	told := notifiedHandles(t, r, "t-1")
	if !containsKind(told, "eng-lead") {
		t.Errorf("the purge notified %v, want the project lead — this is the "+
			"one operation with no inverse, and the person accountable for "+
			"the project is who has to know it happened", told)
	}
}

// THE EXCERPT NAMES THE KEY AND NOT THE CONTENT, which is the one rule a purge
// notification has that no other does: an excerpt quoting the title or the
// body would keep a copy of exactly what the purge destroyed, on the log, for
// its whole retention window.
func TestAPurgeExcerptKeepsNoCopyOfWhatItDestroyed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	secret := "the merger with Contoso"
	task := newTask("t-1")
	task.Title = secret
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	excerpt := historyExcerpt(t, r, "task", "t-1")
	if strings.Contains(excerpt, secret) {
		t.Errorf("the purge excerpt quotes the title it destroyed: %q — the "+
			"record outlives the rows, so this is the content surviving the "+
			"operation that exists to remove it", excerpt)
	}
	for _, want := range []string{"purged", "cannot be undone", "asked for by legal"} {
		if !strings.Contains(excerpt, want) {
			t.Errorf("the purge excerpt does not say %q: %q — an irreversible "+
				"act with no reason on it is the shape nobody can audit",
				want, excerpt)
		}
	}
}

// AN UNBLOCKED NOTICE WITH NOBODY TO TELL IS REFUSED, never published.
//
// The assignee is the whole recipient list, so such a notice reaches nobody —
// and it does not merely waste a record: the apply stamps `unblocked_told_at`
// from it, and [tracker.ScanUnblocked] selects on that column being older than
// the clearing instant. A notice nobody received would mark the task TOLD and
// the repair would never look at it again.
func TestAnUnblockedNoticeWithNobodyToTellIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "t-1", nil)

	_, err := r.writer.TellUnblocked(t.Context(), "op-tell", tracker.Unblock{
		Task: "t-1", Key: "ENG-1", Project: "ENG",
	})
	if err == nil {
		t.Fatal("an unblocked notice with no assignee was published — it " +
			"tells nobody and stamps the task told, so the repair never " +
			"looks at it again")
	}
	if !strings.Contains(err.Error(), "assignee") {
		t.Errorf("the refusal does not name the missing assignee: %v", err)
	}
	// AND THE NOTICE STILL WORKS WITH ONE, so this refuses a case rather
	// than the verb.
	if _, err := r.writer.TellUnblocked(t.Context(), "op-tell-2", tracker.Unblock{
		Task: "t-1", Key: "ENG-1", Project: "ENG", Assignee: "ana",
	}); err != nil && !errors.Is(err, statelog.ErrConflict) {
		t.Errorf("an unblocked notice naming its assignee was refused: %v", err)
	}
}

// historyKind reads the kind of the newest history row about one subject.
func historyKind(t *testing.T, r *roundTrip, kind, id string) string {
	t.Helper()
	got := r.strings(`SELECT kind FROM tracker_history
		WHERE subject_kind = ? AND subject_id = ?
		ORDER BY log_seq DESC LIMIT 1`, kind, id)
	if len(got) == 0 {
		t.Fatalf("no history row for %s %s", kind, id)
	}
	return got[0]
}

// historyExcerpt reads the excerpt of the newest history row about one subject.
func historyExcerpt(t *testing.T, r *roundTrip, kind, id string) string {
	t.Helper()
	got := r.strings(`SELECT excerpt FROM tracker_history
		WHERE subject_kind = ? AND subject_id = ?
		ORDER BY log_seq DESC LIMIT 1`, kind, id)
	if len(got) == 0 {
		t.Fatalf("no history row for %s %s", kind, id)
	}
	return got[0]
}

// historyKinds is every kind this company's log has produced.
func historyKinds(t *testing.T, r *roundTrip) []string {
	t.Helper()
	return r.strings(`SELECT kind FROM tracker_history ORDER BY log_seq`)
}

// notifiedHandles is everybody the applier wrote a notification row for about
// one subject.
func notifiedHandles(t *testing.T, r *roundTrip, subjectID string) []string {
	t.Helper()
	return r.strings(`SELECT recipient FROM tracker_notifications
		WHERE subject_id = ? ORDER BY recipient`, subjectID)
}

func containsKind(all []string, want string) bool {
	for _, got := range all {
		if got == want {
			return true
		}
	}
	return false
}

// historyNotified reads whether the newest history row about one subject
// carried a notification.
func historyNotified(t *testing.T, r *roundTrip, kind, id string) bool {
	t.Helper()
	got := r.strings(`SELECT notified FROM tracker_history
		WHERE subject_kind = ? AND subject_id = ?
		ORDER BY log_seq DESC LIMIT 1`, kind, id)
	if len(got) == 0 {
		t.Fatalf("no history row for %s %s", kind, id)
	}
	return got[0] == "1"
}

// EVERY ROUTABLE CHANGE KIND RENDERS A "What happened" LINE.
//
// `changeLead` is a switch with no default, so a kind it does not name renders
// an EMPTY string and `promptHeader` silently omits the line. The reader then
// gets an opener, a key and a **By:** with nothing between them saying what
// the person did — which is the one thing the wake exists to carry.
//
// It covered twenty of the thirty-two kinds and nothing connected the two
// lists, so the gap was invisible from both ends. Of the twelve missing,
// eleven never reach it — `Prompt.Build` dispatches on [MetaObject] first, so
// a goal, a sprint and a person's queue render through [buildObjectPrompt],
// and the project, policy, view and catalogue kinds are not routable at all.
// The twelfth was `purged`: task-subject, routable, and rendering no line for
// the one operation in this engine that cannot be undone.
//
// THE WALK IS OVER [tracker.ChangeKinds] rather than a list here, so a kind
// added later is covered without anybody remembering to — which is the half
// that was missing rather than the case itself.
func TestEveryRoutableChangeKindRendersWhatHappened(t *testing.T) {
	t.Parallel()
	// The kinds whose wake is a TASK wake, which are the ones that reach
	// changeLead. Everything else renders through the object prompt or is
	// not routed at all; both are asserted below so this list cannot
	// quietly shrink.
	throughTheTaskPrompt := map[tracker.ChangeKind]bool{
		tracker.ChangeCreated: true, tracker.ChangeFields: true,
		tracker.ChangeStatus: true, tracker.ChangeAssignee: true,
		tracker.ChangeCollaborators: true, tracker.ChangeWatchers: true,
		tracker.ChangeTags: true, tracker.ChangeRelations: true,
		tracker.ChangeRouted: true, tracker.ChangeMoved: true,
		tracker.ChangeReparented: true, tracker.ChangeSprint: true,
		tracker.ChangeChecklist: true, tracker.ChangeArchived: true,
		tracker.ChangeComment: true, tracker.ChangeCommentEdited: true,
		tracker.ChangeCommentResolved: true, tracker.ChangeCommentRemoved: true,
		tracker.ChangeRemoved: true, tracker.ChangeRestored: true,
		tracker.ChangePurged: true,
	}
	for _, kind := range tracker.ChangeKinds {
		if !throughTheTaskPrompt[kind] {
			continue
		}
		got := tracker.Prompt{}.Build(notify.Inbound{
			Subject:   "ENG-1 a task",
			EventType: string(kind),
			Metadata: map[string]string{
				tracker.MetaChangeKind: string(kind),
				tracker.MetaTaskKey:    "ENG-1",
				tracker.MetaVia:        string(tracker.ReasonWatcher),
				notify.ActorField:      "ana",
			},
		}, nil)
		if !strings.Contains(got, "**What happened:**") {
			t.Errorf("a %q wake renders no `What happened` line — the reader "+
				"gets an opener, a key and a By: with nothing between them "+
				"saying what was done, which is the one thing the wake "+
				"carries:\n%s", kind, got)
		}
	}
}

// AND A PURGE DOES NOT READ AS AN ORDINARY EDIT.
//
// Three separate places in the prompt assume the task still exists: the
// opener says it "changed", the context block sends the reader to
// `get_work_item`, and the handling block describes deciding whether to act on
// it. For a purge all three are wrong — and the third is worse than wrong,
// because a removal's block offers a restore that a purge has no equivalent
// of.
func TestAPurgeWakeDoesNotSendAnybodyToReadTheTask(t *testing.T) {
	t.Parallel()
	got := tracker.Prompt{}.Build(notify.Inbound{
		Subject:   "ENG-1 a task",
		EventType: string(tracker.ChangePurged),
		Metadata: map[string]string{
			tracker.MetaChangeKind: string(tracker.ChangePurged),
			tracker.MetaTaskKey:    "ENG-1",
			tracker.MetaVia:        string(tracker.ReasonPurged),
			notify.ActorField:      "ops-1",
		},
	}, nil)

	if strings.Contains(got, tracker.GetWorkItemTool) {
		t.Errorf("a purge wake sends the reader to %s — the row is gone from "+
			"every node, so that is a wasted round and a failed tool call:\n%s",
			tracker.GetWorkItemTool, got)
	}
	if strings.Contains(got, "you are watching changed") {
		t.Errorf("a purge wake opens as an ordinary edit — a reader who "+
			"expects a change goes looking for what moved:\n%s", got)
	}
	for _, want := range []string{"destroyed", "cannot be undone"} {
		if !strings.Contains(got, want) {
			t.Errorf("a purge wake does not say %q — it is the one operation "+
				"in this engine with no inverse:\n%s", want, got)
		}
	}
	// AND IT DOES NOT OFFER THE REMOVAL'S OWN REMEDY, which is the sharp
	// edge: a removed task is in the trash and comes back, a purged one
	// does not exist anywhere.
	if strings.Contains(got, tracker.RestoreWorkItemTool) {
		t.Errorf("a purge wake offers a restore:\n%s", got)
	}
}
