package worklet_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// readyFor is whom each of handle's ready items is for.
func (r *roundTrip) readyFor(handle string) map[string]*types.WorkingFor {
	r.t.Helper()
	out := map[string]*types.WorkingFor{}
	for _, item := range r.readyWork(handle).Items {
		out[item.ID] = item.For
	}
	return out
}

// writing is a writer for an agent seat in a turn that works for whom; nil
// is a turn nobody woke.
func (r *roundTrip) writing(handle string, whom *types.WorkingFor) *worklet.Writer {
	return r.writer.As(handle, worklet.AuthorAgent, worklet.Provenance{WrittenFor: types.ForTurn(whom)})
}

// fileAs files item id, assigned to assignee ("" for nobody), as w.
func (r *roundTrip) fileAs(w *worklet.Writer, id, assignee string) {
	r.t.Helper()
	task := newTask(id)
	task.Key, task.Assignee = "ENG-"+id, assignee
	if _, err := w.CreateTask(r.t.Context(), "op-file-"+id, task, nil); err != nil {
		r.t.Fatalf("file %s: %v", id, err)
	}
	r.drain()
}

// move writes patch to item id as w, under its own op.
func (r *roundTrip) move(w *worklet.Writer, op, id string, patch worklet.TaskPatch, kind worklet.ChangeKind) {
	r.t.Helper()
	if _, err := w.UpdateTask(r.t.Context(), op, id, "ENG", worklet.NoIfMatch, patch, kind,
		nil); err != nil {
		r.t.Fatalf("%s on %s: %v", op, id, err)
	}
	r.drain()
}

func seatOf(handle string) *types.WorkingFor {
	return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
}

// A PICKUP WORKS FOR WHOM ITS ASSIGNMENT WAS WRITTEN FOR (ADR-0109, ADR-0112).
// Agent b hands c an item with a status-and-assignee update in a turn somebody
// woke: c's pickup works for that somebody — a seat, a credential with the apps
// the writing turn used, a stranger — never for b, which holds every app. A
// hand-off made in a turn nobody woke works for b, as before.
//
// Mutation: read the history row's writer and never its carrier, and the R, L
// and stranger rows read b.
func TestAPickupWorksForWhomItsAssignmentWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	credential := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	stranger := &types.WorkingFor{Kind: types.WorkingForStranger}
	c, started := "c", worklet.StatusInProgress
	hands := map[string]*types.WorkingFor{
		"for-r": seatOf("r"), "for-token": credential, "for-a-stranger": stranger, "for-nobody": nil,
	}
	for id, whom := range hands {
		r.fileAs(pm, id, "")
		r.move(r.writing("b", whom), "op-hand-"+id, id,
			worklet.TaskPatch{Assignee: &c, Status: &started}, worklet.ChangeStatus)
	}
	want := map[string]*types.WorkingFor{
		"for-r": seatOf("r"), "for-token": credential, "for-a-stranger": stranger,
		"for-nobody": seatOf("b"),
	}
	if got := r.readyFor("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("c's pickups work for %+v, want %+v", got, want)
	}
}

// WORK A SEAT FILED FOR SOMEBODY IS THEIRS. A creation c made in a turn for r
// carries r, and c's pickup of it works for r: filed unassigned and taken in a
// turn nobody woke (only the creation can answer, since that take carries
// nothing), or filed already assigned to itself. Work c filed for itself is its
// own. And a later hand-off made for somebody else outranks the creation.
//
// Mutation: keep the seat's own creation out of its arm, and the first row
// reads nobody; prefer the creation to the assignment, and the last reads r1.
func TestWorkASeatFiledForSomebodyIsTheirs(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	c := "c"
	r.fileAs(r.writing("c", seatOf("r")), "taken-later", "")
	r.move(r.writing("c", nil), "op-take-taken-later", "taken-later",
		worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
	r.fileAs(r.writing("c", seatOf("r")), "filed-assigned-for-r", "c")
	r.fileAs(r.writing("c", nil), "filed-for-itself", "c")
	r.fileAs(r.writing("c", seatOf("r1")), "handed-on-for-r2", "d")
	r.move(r.writing("b", seatOf("r2")), "op-hand-handed-on-for-r2", "handed-on-for-r2",
		worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
	want := map[string]*types.WorkingFor{
		"taken-later": seatOf("r"), "filed-assigned-for-r": seatOf("r"), "filed-for-itself": nil,
		"handed-on-for-r2": seatOf("r2"),
	}
	if got := r.readyFor("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("c's pickups work for %+v, want %+v", got, want)
	}
}

// A TAKE MADE FOR SOMEBODY IS WORKED FOR THEM. c takes an item pm filed in a
// turn for r: its pickup works for r. A take in a turn nobody woke reads the
// filer, as before. And the seat's own moves away from itself are never read:
// c, in a turn for w, unassigns the item or hands it to d, then takes it back
// in a turn nobody woke — the pickup reads pm, who assigned it, never w, for
// whom c only let it go.
//
// Mutation: keep the seat's own take out of the assignment arm, and the first
// row reads pm; admit every carried row of the seat's, and the last two read w.
func TestATakeMadeForSomebodyIsWorkedForThem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	c, d, nobody := "c", "d", ""
	take := func(id string, whom *types.WorkingFor) {
		t.Helper()
		r.move(r.writing("c", whom), "op-take-"+id, id,
			worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
	}
	r.fileAs(pm, "taken-for-r", "")
	take("taken-for-r", seatOf("r"))
	r.fileAs(pm, "taken-for-nobody", "")
	take("taken-for-nobody", nil)
	for id, away := range map[string]*string{"unassigned-then-taken": &nobody, "handed-away-then-taken": &d} {
		r.fileAs(pm, id, "")
		r.move(pm, "op-assign-"+id, id, worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
		r.move(r.writing("c", seatOf("w")), "op-away-"+id, id, worklet.TaskPatch{Assignee: away},
			worklet.ChangeAssignee)
		take(id, nil)
	}
	want := map[string]*types.WorkingFor{
		"taken-for-r": seatOf("r"), "taken-for-nobody": seatOf("pm"),
		"unassigned-then-taken": seatOf("pm"), "handed-away-then-taken": seatOf("pm"),
	}
	if got := r.readyFor("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("c's pickups work for %+v, want %+v", got, want)
	}
}

// WORK HANDED BACK TO THE SEAT IT WAS DONE FOR IS ITS OWN. Agent b, in a turn
// for c, assigns the item to c: the carrier names the reading seat, which the
// ceiling reads as the seat's own work, and so does the pickup.
//
// Mutation: hand the carrier on unread, and the row reads {seat c}.
func TestWorkHandedBackToTheSeatItWasDoneForIsItsOwn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	c := "c"
	r.fileAs(pm, "done-for-c", "")
	r.move(r.writing("b", seatOf("c")), "op-hand-done-for-c", "done-for-c",
		worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
	want := map[string]*types.WorkingFor{"done-for-c": nil}
	if got := r.readyFor("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("c's pickup works for %+v, want its own work", got)
	}
}

// A REVIEW IS WORKED FOR WHOM ITS ASSIGNMENT WAS MADE FOR (#891). x, in a
// turn for r1, assigns the item to c; d, in a turn for r2, moves it into
// review. The move into review says who asked for the review, never whom the
// work is for: c's review pickup works for r1.
//
// Mutation: read the review request's row for an item in review, and it reads
// r2.
func TestAReviewIsWorkedForWhomItsAssignmentWasMadeFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	pm := r.writer.As("pm", worklet.AuthorHuman, worklet.Provenance{})
	c, review := "c", worklet.StatusInReview
	r.fileAs(pm, "in-review", "")
	r.move(r.writing("x", seatOf("r1")), "op-assign-in-review", "in-review",
		worklet.TaskPatch{Assignee: &c}, worklet.ChangeAssignee)
	r.move(r.writing("d", seatOf("r2")), "op-review-in-review", "in-review",
		worklet.TaskPatch{Status: &review}, worklet.ChangeStatus)
	want := map[string]*types.WorkingFor{"in-review": seatOf("r1")}
	if got := r.readyFor("c"); !reflect.DeepEqual(got, want) {
		t.Errorf("c's review pickup works for %+v, want r1, whom its assignment was made for", got)
	}
}
