// A person bound to an operator token is one party with two identities, and
// every personal read has to answer for both.
//
// The WRITE is the other half, and it is what stops the two names growing
// apart: a person's own state is keyed on who they ARE — the seat the
// credential is bound to — while the record's author stays the credential,
// because that is the audit trail. The read's fallback is what still reaches
// the records written before that was true.

package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// founderParty is the shape the API resolves for a human seat whose
// `contact.crewlet_operator_id` names an `api.auth` token: the seat is who
// they are, the token is what their own assistant writes under.
var founderParty = tracker.Party{Handle: "jane-founder", OperatorID: "founder"}

func (r *roundTrip) myWorkAs(who tracker.Party) tracker.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), tracker.MyWorkQuery{
		Who: who, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("MyWork(%s): %v", who, err)
	}
	return out
}

// create files one task and drains it, so a case reads as what it is about
// rather than as four lines of harness per row.
func create(t *testing.T, r *roundTrip, id string, task tracker.Task,
	notify *tracker.Notify) {

	t.Helper()
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, notify); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	r.drain()
}

// EVERY BLOCK MATCHES EITHER NAME.
//
// This is the measured defect: a founder whose token is bound to their seat
// filed eleven items through their own assistant, every one of them
// attributed to `founder` because a tracker whose author field is chosen by
// the writer is not an audit trail — and then opened My work, which asked
// about `jane-founder`, and found seven tabs at zero.
//
// The five blocks below are four different tables —
// `tracker_tasks.assignee`, `tracker_collaborators.handle`,
// `tracker_watchers.handle`, `tracker_comments.ask` and
// `tracker_checklist_items.assignee` — so one of them fixed and the rest left
// is a fix that would look exactly like this one from the screen that still
// says nothing.
func TestMyWorkAnswersForEitherOfAPersonsIdentities(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// ASSIGNED, under the credential: a task the founder's assistant
	// filed at them.
	own := newTask("t-own")
	own.Assignee = "founder"
	create(t, r, "own", own, nil)

	// WATCHING and COLLABORATING, on somebody else's task, both recorded
	// under the credential — which is what a create through the operator
	// MCP leaves behind.
	theirs := newTask("t-theirs")
	theirs.Assignee = "agent-swe"
	theirs.Watchers = []string{"founder"}
	theirs.Collaborators = []string{"founder"}
	create(t, r, "theirs", theirs, nil)

	// AN ASK addressed to the credential.
	askOn(t, r, "op-ask", "t-theirs", tracker.Comment{
		ID: "c-1", Task: "t-theirs", Author: "agent-swe",
		AuthorKind: tracker.AuthorAgent, Body: "which region?",
		Ask: "founder", CreatedAt: wednesday,
	})

	// A CHECKLIST CLAIM, which no assignee filter over tasks reaches.
	lists := []tracker.Checklist{{ID: "l-1", Name: "Release",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "sign off",
			Assignee: "founder"}}}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-list", "t-theirs", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Checklists: &lists},
		tracker.ChangeChecklist, nil); err != nil {
		t.Fatalf("checklist: %v", err)
	}
	r.drain()

	// THE SEAT ALONE SEES NOTHING, which is the bug this is about and the
	// proof that the assertions below are not passing for another reason.
	blind := r.myWorkAs(tracker.PartyOf("jane-founder"))
	if n := len(blind.Assigned) + len(blind.WatchingRecent) +
		len(blind.Collaborating) + len(blind.AskedOfMe) +
		len(blind.ChecklistItems); n != 0 {

		t.Fatalf("the seat alone already answers %d rows, so this case "+
			"proves nothing: %+v", n, blind)
	}

	// EACH BLOCK IS ITS OWN ASSERTION rather than one switch, so a change
	// that fixes some of the five reports the ones it left — which is the
	// failure mode this whole case is about.
	got := r.myWorkAs(founderParty)
	if len(got.Assigned) != 1 || got.Assigned[0].ID != "t-own" {
		t.Errorf("assigned is %+v, want the task filed under the credential",
			got.Assigned)
	}
	if len(got.Collaborating) != 1 || got.Collaborating[0].ID != "t-theirs" {
		t.Errorf("collaborating is %+v, want the task the credential was "+
			"brought onto", got.Collaborating)
	}
	if len(got.WatchingRecent) != 1 || got.WatchingRecent[0].ID != "t-theirs" {
		t.Errorf("watching is %+v, want the task the credential follows",
			got.WatchingRecent)
	}
	if len(got.AskedOfMe) != 1 || got.AskedOfMe[0].Comment != "c-1" {
		t.Errorf("asked_of_me is %+v, want the question put to the credential",
			got.AskedOfMe)
	}
	if len(got.ChecklistItems) != 1 || got.ChecklistItems[0].Item != "i-1" {
		t.Errorf("checklist_items is %+v, want the claim under the credential",
			got.ChecklistItems)
	}
	// AND THE ANSWER IS STILL ABOUT THE SEAT. The alias is matched against
	// and never rendered: a screen that put a token where a person's
	// handle goes would be showing them a credential as themselves.
	if got.Handle != "jane-founder" {
		t.Errorf("the answer names %q, want the seat", got.Handle)
	}
}

// THE EXCLUSION COVERS BOTH NAMES TOO.
//
// `collaborating` is "brought on WITHOUT owning", and it excludes what this
// person holds. With two identities the exclusion has to cover both or a task
// assigned to the seat and collaborated on under the credential is one row
// spending two of the seven blocks — the exact double-counting the block's
// own predicate exists to prevent.
//
// BOTH ARRANGEMENTS, because the two names are not interchangeable in the
// predicate: the seat leads the identity list, so an exclusion that kept only
// its first member is correct for the task assigned to the SEAT and wrong for
// the one assigned to the credential.
func TestCollaboratingExcludesWhatEitherIdentityOwns(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	seatHolds := newTask("t-seat")
	seatHolds.Assignee = "jane-founder"
	seatHolds.Collaborators = []string{"founder"}
	create(t, r, "seat", seatHolds, nil)

	tokenHolds := newTask("t-token")
	tokenHolds.Assignee = "founder"
	tokenHolds.Collaborators = []string{"jane-founder"}
	create(t, r, "token", tokenHolds, nil)

	got := r.myWorkAs(founderParty)
	if len(got.Assigned) != 2 {
		t.Fatalf("assigned is %+v, want both tasks this person holds",
			got.Assigned)
	}
	if len(got.Collaborating) != 0 {
		t.Errorf("collaborating is %+v — this person owns those tasks under "+
			"one of their names, so both are in `assigned` and belong in no "+
			"second block", got.Collaborating)
	}
}

// AN UNASSIGNED TASK IS STILL SOMEBODY'S TO COLLABORATE ON.
//
// The exclusion is now `assignee NOT IN (…)`, and that column is NOT NULL
// with the empty string as its default — so an identity list carrying an
// empty member would exclude every unassigned task from the block.
// [tracker.Party.Handles] is what stops that, and this is what would catch it
// stopping.
func TestAnUnassignedTaskStaysInTheCollaboratingBlock(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-loose")
	task.Assignee = ""
	task.Collaborators = []string{"jane-founder"}
	create(t, r, "loose", task, nil)

	for name, who := range map[string]tracker.Party{
		"one identity": tracker.PartyOf("jane-founder"),
		"two":          founderParty,
	} {
		t.Run(name, func(t *testing.T) {
			got := r.myWorkAs(who)
			if len(got.Collaborating) != 1 {
				t.Fatalf("collaborating is %+v, want the unassigned task "+
					"this person was brought onto", got.Collaborating)
			}
		})
	}
}

// A ONE-IDENTITY PARTY IS THE READ THIS ALWAYS WAS.
//
// Every agent seat has exactly one name, so the ordinary case is a party of
// one — and it must be the answer the single-handle predicate gave, including
// the negative half: nobody else's work leaks in.
func TestAPartyOfOneAnswersExactlyItsOwn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "hers", "ana")
	assign(t, r, "his", "bob")
	askOn(t, r, "op-ask", "hers", tracker.Comment{
		ID: "c-1", Task: "hers", Author: "bob", AuthorKind: tracker.AuthorAgent,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	})

	got := r.myWorkAs(tracker.PartyOf("ana"))
	if len(got.Assigned) != 1 || got.Assigned[0].ID != "hers" {
		t.Fatalf("assigned is %+v, want ana's one task", got.Assigned)
	}
	if len(got.AskedOfMe) != 1 || got.AskedOfMe[0].Comment != "c-1" {
		t.Fatalf("asked_of_me is %+v, want the one open ask", got.AskedOfMe)
	}
	other := r.myWorkAs(tracker.PartyOf("bob"))
	if len(other.AskedOfMe) != 0 {
		t.Fatalf("bob holds %d of ana's asks — the predicate matches more "+
			"than the party it was given", len(other.AskedOfMe))
	}
	if len(other.Assigned) != 1 || other.Assigned[0].ID != "his" {
		t.Fatalf("bob's assigned is %+v, want his own one task", other.Assigned)
	}
}

// THE INBOX IS ONE PERSON'S, WHICHEVER NAME THE COMPANY USED.
//
// `tracker_notifications` is keyed on the recipient the RECORD named, and the
// applier writes one row per candidate — so a change that concerned this
// person under their credential is a row nothing else will ever show them: a
// human seat is never woken (internal/notify/selfaction.go: "a human seat is
// addressable but never woken — a person reads the surface the event arrived
// on"), and for the native tracker this read IS that surface.
func TestTheInboxAnswersForEitherOfAPersonsIdentities(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "founder")

	if blind := r.inbox(tracker.InboxQuery{
		Who: tracker.PartyOf("jane-founder"),
	}); len(blind.Notices) != 0 {

		t.Fatalf("the seat alone already answers %d notices, so this case "+
			"proves nothing", len(blind.Notices))
	}

	got := r.inbox(tracker.InboxQuery{Who: founderParty})
	if len(got.Notices) != 1 {
		t.Fatalf("the inbox holds %d notices, want the one filed under the "+
			"credential", len(got.Notices))
	}
	if got.Notices[0].Reason != tracker.ReasonAssignee {
		t.Errorf("the reason is %q, want assignee", got.Notices[0].Reason)
	}
	if got.Handle != "jane-founder" {
		t.Errorf("the answer names %q, want the seat", got.Handle)
	}
}

// ONE CHANGE IS ONE NOTICE, EVEN WHEN IT NAMED BOTH NAMES.
//
// [tracker.Candidates] already gives each HANDLE one reason, the strongest
// that names them. A party of two gets one row per identity, so without the
// same tie-break one level up a single comment arrives twice — once as
// `assignee` and once as `reporter` — which is exactly the noise the
// one-reason rule exists to prevent. The survivor is the stronger reason,
// because that is the fact this person will act on.
func TestOneChangeNamingBothIdentitiesIsOneNotice(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-1")
	task.Assignee = "jane-founder"
	create(t, r, "1", task, nil)
	commentNamingBoth(t, r, "op-c1", "t-1", "c-1")

	// BOTH ROWS EXIST, which is the applier doing its job: the record is a
	// complete account of everyone the change concerned.
	if got := r.strings(
		`SELECT recipient FROM tracker_notifications ORDER BY recipient`,
	); len(got) != 2 {
		t.Fatalf("the applier wrote %v, want one row per identity — this "+
			"case is about collapsing two, and with one row it asserts "+
			"nothing", got)
	}

	got := r.inbox(tracker.InboxQuery{Who: founderParty})
	if len(got.Notices) != 1 {
		t.Fatalf("the inbox holds %d notices for one change: %+v",
			len(got.Notices), got.Notices)
	}
	if got.Notices[0].Reason != tracker.ReasonAssignee {
		t.Errorf("the surviving reason is %q, want assignee — it outranks "+
			"reporter in tracker.Reasons, which IS the precedence",
			got.Notices[0].Reason)
	}
}

// commentNamingBoth writes one change that concerns ONE person under each of
// their two names: assigned to the seat, reported by the credential they
// filed it with. A comment is the gesture that reaches both, which is
// [tracker.Candidates]' own rule — `reporter` hears about a comment and not
// about a create.
func commentNamingBoth(t *testing.T, r *roundTrip, opID, task, comment string) {
	t.Helper()
	body := tracker.Comment{
		ID: comment, Task: task, Author: "agent-swe",
		AuthorKind: tracker.AuthorAgent, Body: "said something",
		CreatedAt: wednesday,
	}
	if _, err := r.writer.UpdateTask(t.Context(), opID, task, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &body},
		tracker.ChangeComment, &tracker.Notify{
			Kind: tracker.ChangeComment,
			Snapshot: tracker.Snapshot{
				Key: "ENG-1", Project: "ENG", Title: "a task",
				Assignee: "jane-founder", Reporter: "founder",
			},
			Excerpt: "said something",
		}); err != nil {

		t.Fatalf("comment on %s: %v", task, err)
	}
	r.drain()
}

// A PAGE IS COUNTED IN CHANGES, NOT IN ROWS.
//
// The table is keyed on (change, recipient), so a party of two can hold two
// rows for one change. A read that took `limit+1` rows would return half a
// page and call it a full one — and a caller paging through would see the
// cursor advance while the screen stayed short.
func TestTheInboxPageIsCountedInChanges(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, id := range []string{"t-1", "t-2", "t-3"} {
		task := newTask(id)
		task.Assignee = "jane-founder"
		create(t, r, id, task, nil)
		commentNamingBoth(t, r, "op-c-"+id, id, "c-"+id)
	}

	first := r.inbox(tracker.InboxQuery{Who: founderParty, Limit: 2})
	if len(first.Notices) != 2 {
		t.Fatalf("the first page holds %d notices, want the 2 asked for",
			len(first.Notices))
	}
	if first.NextCursor == "" {
		t.Fatal("a page with a change behind it carries no cursor")
	}
	second := r.inbox(tracker.InboxQuery{
		Who: founderParty, Limit: 2, Cursor: first.NextCursor,
	})
	if len(second.Notices) != 1 {
		t.Fatalf("the second page holds %d, want the remaining 1 — a cursor "+
			"that cut inside a change would lose it or repeat it",
			len(second.Notices))
	}
	if second.Notices[0].RecordID == first.Notices[1].RecordID {
		t.Error("the second page repeats the first page's last change")
	}
}

// THE PERSON'S OWN RECORD IS THE FIRST IDENTITY THAT HAS ONE.
//
// A person's record is what THEY decided — the order they mean to work in,
// the views they pinned, which notices they have read — so two of them merged
// is an arrangement nobody made. The seat comes first and the credential is
// the fallback, which is what makes a founder whose assistant has been
// marking their inbox read see those marks instead of an inbox where nothing
// has ever been read.
func TestThePersonRecordIsTheFirstIdentityThatHasOne(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "t-1", "jane-founder")

	// ONLY THE CREDENTIAL HAS A RECORD, which is the state the person
	// tools leave: they write on behalf of `actor.Handle`, and through the
	// operator MCP that is the token's own id.
	token := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder",
	})
	if _, err := token.WritePriorities(t.Context(), "op-prio", "founder",
		[]string{"t-1"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	if blind := r.myWorkAs(tracker.PartyOf("jane-founder")); len(
		blind.Priorities) != 0 {

		t.Fatalf("the seat alone already answers %d priorities, so this case "+
			"proves nothing", len(blind.Priorities))
	}
	got := r.myWorkAs(founderParty)
	if len(got.Priorities) != 1 || got.Priorities[0].ID != "t-1" {
		t.Fatalf("priorities is %+v, want the list the credential wrote",
			got.Priorities)
	}

	state, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Who: founderParty, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	switch {
	case !state.Held:
		t.Error("the person reads as unwritten while their credential's " +
			"record exists")
	case len(state.Priorities) != 1:
		t.Errorf("the record carries %v", state.Priorities)
	case state.Handle != "jane-founder":
		t.Errorf("the record answers as %q, want the seat", state.Handle)
	}

	// AND THE SEAT'S OWN RECORD WINS THE MOMENT IT EXISTS, because from
	// then on it is the one this person is writing.
	seat := r.writer.As("jane-founder", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := seat.WritePriorities(t.Context(), "op-prio-2", "jane-founder",
		nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities as the seat: %v", err)
	}
	r.drain()
	if after := r.myWorkAs(founderParty); len(after.Priorities) != 0 {
		t.Errorf("priorities is %+v after the seat cleared its own list, "+
			"want the seat's record to be the one that answers",
			after.Priorities)
	}
}

// THE VIEW STRIP IS PERSONALISED FOR THE SAME PERSON.
//
// A saved view is OWNED by whoever wrote it and a pin lives on their own
// record — and both are written through the operator tool server, which
// attributes to the credential. So a founder's own view was owned by `founder`
// while the strip was asked for under `jane-founder`, and the sidebar showed
// them the shared views and nothing of their own.
func TestTheViewStripCarriesEitherOfAPersonsIdentities(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// SAVED AND PINNED THROUGH THE CREDENTIAL, which is the only surface
	// that holds either verb.
	token := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder",
	})
	if _, err := token.WriteView(t.Context(), "op-view",
		aView("v-mine", func(v *tracker.View) {
			v.Owner, v.Name = "founder", "What I am on"
		})); err != nil {
		t.Fatalf("save the view: %v", err)
	}
	r.drain()
	if _, err := token.WritePins(t.Context(), "op-pins", "founder",
		[]string{"v-mine"}, nil); err != nil {
		t.Fatalf("WritePins: %v", err)
	}
	r.drain()
	// A SHARED VIEW BESIDE IT, so the pin has something to come before:
	// within the saved half the pinned ones lead, and a strip with one
	// saved view could not tell a working pin from a pin that did nothing.
	if _, err := r.writer.WriteView(t.Context(), "op-shared",
		aView("v-shared", func(v *tracker.View) {
			v.Name = "Everything"
		})); err != nil {
		t.Fatalf("save the shared view: %v", err)
	}
	r.drain()

	container := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}
	if blind := stripKeys(r.strip(container, "jane-founder")); slices.Contains(
		blind, "v-mine") {

		t.Fatal("the seat alone already sees the view, so this case proves " +
			"nothing")
	}

	got, err := r.reader.Views(t.Context(), tracker.ViewQuery{
		Container: container, Viewer: founderParty, Level: statelog.ReadStale,
	})
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	var mine *tracker.ViewRow
	for i, row := range got.Views {
		if row.Key == "v-mine" {
			mine = &got.Views[i]
		}
	}
	if mine == nil {
		t.Fatalf("the strip is %v and holds none of this person's own views",
			stripKeys(got))
	}
	if !mine.Pinned {
		t.Error("the view is on the strip and not pinned — the pins live on " +
			"the person's own record, which is the other half of the same " +
			"read")
	}
	keys := stripKeys(got)
	if slices.Index(keys, "v-mine") > slices.Index(keys, "v-shared") {
		t.Errorf("the strip is %v — a pin puts a view FIRST within the saved "+
			"half, and a pin that does not reorder is a pin that did nothing",
			keys)
	}
}

// AND AN UNNAMED PARTY IS STILL THE SHARED STRIP, which is what the sidebar
// and the board ask for on every poll before anybody is known. It is a real
// answer rather than a refusal, so the predicate must degrade to `owner = ”`
// rather than to an empty `IN` list.
func TestAnUnnamedPartyGetsTheSharedStrip(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteView(t.Context(), "op-shared",
		aView("v-shared", nil)); err != nil {
		t.Fatalf("save the shared view: %v", err)
	}
	r.drain()

	got, err := r.reader.Views(t.Context(), tracker.ViewQuery{
		Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
		Level:     statelog.ReadStale,
	})
	if err != nil {
		t.Fatalf("Views with no viewer: %v", err)
	}
	if !slices.Contains(stripKeys(got), "v-shared") {
		t.Fatalf("the shared strip is %v and holds no shared view",
			stripKeys(got))
	}
}

// AND THE WRITE PUTS THE STATE UNDER THE PERSON, WHICH IS WHAT ENDS IT.
//
// The read above is the recovery; this is the cause. A person write used to be
// keyed on the ACTOR, and through the operator tool server the actor is the
// token — so a bound founder's marks and pins grew a second record named after
// their credential. Keyed on [tracker.Provenance.Seat] there is one record per
// person, and the seat's own read finds it with no alias to fall back on.
//
// THE AUTHOR IS ASSERTED IN THE SAME CASE, because the one wrong fix is to let
// the seat take the author field over: that is a write attributed to a person
// the caller named, and a tracker whose author field is chosen by the writer
// is not an audit trail.
func TestABoundCredentialWritesThePersonsOwnRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "t-1", "jane-founder")

	// THE WRITER THE OPERATOR SURFACE BUILDS for a bound token: the
	// credential in the author field, the seat in the provenance.
	token := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder", Seat: "jane-founder",
	})
	if _, err := token.WritePins(t.Context(), "op-pins", "jane-founder",
		[]string{"v-mine"}, nil); err != nil {
		t.Fatalf("WritePins: %v", err)
	}
	r.drain()

	// ASKED ABOUT THE SEAT ALONE, so the fallback loop cannot be what
	// answers: there is no alias on this party to fall back to.
	state, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Who: tracker.PartyOf("jane-founder"), Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if !state.Held || len(state.PinnedViews) != 1 {
		t.Fatalf("the seat's own record is %+v, want the pin this person's "+
			"assistant set", state)
	}

	// AND THERE IS EXACTLY ONE RECORD. A second under the credential is
	// invisible to every screen that asks under the seat, which is the
	// whole defect — and it reads as a working write because the call
	// answers `applied` either way.
	if got := r.strings(
		`SELECT handle FROM tracker_persons ORDER BY handle`,
	); len(got) != 1 || got[0] != "jane-founder" {
		t.Fatalf("the person rows are %v, want one under the seat", got)
	}

	// AND THE HISTORY STILL NAMES THE TOKEN.
	if got := r.strings(
		`SELECT actor || '/' || actor_kind FROM tracker_history ORDER BY rowid DESC LIMIT 1`,
	); len(got) != 1 || got[0] != "founder/operator" {
		t.Fatalf("the record is authored %v, want the credential and the "+
			"kind that says it is not a seat", got)
	}
}

// AND IT IS STILL ONLY THAT PERSON'S RECORD.
//
// The rule an inbox rests on is that nobody else's hand is ever in it, and
// widening the subject is exactly the move that could lose it: a writer that
// matched on either of its two names, or on none, would let a bound founder
// write a colleague's marks. The gate moved from the actor to the PERSON, and
// it is still one name.
func TestABoundCredentialStillWritesNobodyElsesRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	token := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder", Seat: "jane-founder",
	})

	for name, write := range map[string]func(handle string) error{
		"the inbox": func(handle string) error {
			_, err := token.WriteInbox(t.Context(), "op-inbox-"+handle, handle,
				nil, nil, nil, nil, tracker.Position{})
			return err
		},
		"the pins": func(handle string) error {
			_, err := token.WritePins(t.Context(), "op-pins-"+handle, handle,
				[]string{"v-1"}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := write("ana"); err == nil {
				t.Fatal("a bound credential wrote somebody else's record")
			}
			// AND THE CREDENTIAL'S OWN NAME IS NOT THE PERSON EITHER,
			// which is the other direction of the same gate: once the
			// token names a seat, the seat is who it is.
			if err := write("founder"); err == nil {
				t.Error("a bound credential wrote a record under its own " +
					"name — that is the second record this change removes")
			}
			if err := write("jane-founder"); err != nil {
				t.Errorf("a bound credential was refused its own person's "+
					"record: %v", err)
			}
			// DRAINED, because both subtests write the one person
			// subject and the second decides from a snapshot that has
			// to carry the first.
			r.drain()
		})
	}
}

// AND AN UNBOUND CREDENTIAL IS UNCHANGED: it writes its own record, under its
// own id, which is an operator outside the org chart and an ordinary state.
func TestAnUnboundCredentialWritesItsOwnRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	token := r.writer.As("ci", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "ci",
	})
	if _, err := token.WritePins(t.Context(), "op-pins", "ci",
		[]string{"v-1"}, nil); err != nil {
		t.Fatalf("WritePins: %v", err)
	}
	r.drain()
	if got := r.strings(
		`SELECT handle FROM tracker_persons`,
	); len(got) != 1 || got[0] != "ci" {
		t.Fatalf("the person rows are %v, want the token's own", got)
	}
}

// THE PRIORITIES FILTER READS THE SAME RECORD EVERY OTHER PERSONAL READ DOES.
//
// `priorities=` and the `preset=priorities` it expands into went through the
// handle alone while `my_work` went through the party — so the one person who
// can hold two records saw their list on My work's own block and an empty
// board on the tab beside it, from one read of one company.
func TestThePrioritiesFilterAnswersForEitherIdentity(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "t-1", "jane-founder")

	// A LIST WRITTEN BEFORE THE BINDING KEYED THE WRITE ON THE SEAT, which
	// is the state every company that ran an earlier build is in and the
	// only thing the alias is still for.
	token := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder",
	})
	if _, err := token.WritePriorities(t.Context(), "op-prio", "founder",
		[]string{"t-1"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	// THE SEAT ALONE SEES NOTHING, which is the defect and the proof that
	// the assertion below is not passing for another reason.
	if got := r.priorityBoard(tracker.Viewer{Handle: "jane-founder"}); len(got.Rows) != 0 {
		t.Fatalf("the seat alone already answers %d rows, so this case proves "+
			"nothing", len(got.Rows))
	}
	got := r.priorityBoard(tracker.Viewer{
		Handle: "jane-founder", OperatorID: "founder",
	})
	if len(got.Rows) != 1 || got.Rows[0].ID != "t-1" {
		t.Fatalf("preset=priorities answers %+v, want the list this person's "+
			"own credential wrote", got.Rows)
	}
}

// priorityBoard is `preset=priorities` as a surface asks it: expanded from the
// viewer, then parsed, which is the one entry point that can add the alias a
// parameter cannot carry.
func (r *roundTrip) priorityBoard(viewer tracker.Viewer) tracker.Answer {
	r.t.Helper()
	q, err := r.reader.ExpandedQuery(r.t.Context(),
		map[string]any{"preset": tracker.PresetPriorities}, viewer,
		wednesday, berlin)
	if err != nil {
		r.t.Fatalf("ExpandedQuery: %v", err)
	}
	q.Level = statelog.ReadStale
	answer, err := r.reader.Tasks(r.t.Context(), q, wednesday)
	if err != nil {
		r.t.Fatalf("Tasks: %v", err)
	}
	return answer
}

// A PARTY NAMES SOMEBODY, always — the same rule as before, over the new
// shape. A party whose seat is blank is a question about everybody, and the
// alias alone is not a name this answer may be reported under.
func TestAnUnnamedPartyIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	alias := tracker.Party{OperatorID: "founder"}

	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Who: alias, Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("my_work with an alias and no seat answered")
	}
	if _, err := r.reader.Inbox(t.Context(), tracker.InboxQuery{
		Who: alias, Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("an inbox read with an alias and no seat answered")
	}
	if _, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Who: alias, Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("a person read with an alias and no seat answered")
	}
}

// THE IDENTITY LIST IS THE SEAT FIRST, DE-DUPLICATED, AND CARRIES NO BLANK.
//
// Every predicate in this change binds this list, and each of the three rules
// costs something different when it breaks: the order decides which record a
// person's own state comes from, a duplicate doubles the rows a keyset page
// over-reads for, and a blank member turns `assignee NOT IN (…)` into a
// filter that hides every unassigned task.
func TestAPartysIdentitiesAreOrderedUniqueAndNonEmpty(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		party tracker.Party
		want  []string
	}{
		"the seat leads": {founderParty, []string{"jane-founder", "founder"}},
		"no alias":       {tracker.PartyOf("ana"), []string{"ana"}},
		"the same name":  {tracker.Party{Handle: "ci", OperatorID: "ci"}, []string{"ci"}},
		"a blank alias":  {tracker.Party{Handle: "ana", OperatorID: "  "}, []string{"ana"}},
		"nobody at all":  {tracker.Party{}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tc.party.Handles()
			if len(got) != len(tc.want) {
				t.Fatalf("Handles() = %v, want %v", got, tc.want)
			}
			for i, handle := range tc.want {
				if got[i] != handle {
					t.Fatalf("Handles() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
