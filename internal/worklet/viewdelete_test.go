package worklet_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// remove deletes a view the way every caller does: read what it deletes,
// decide on that, and delete against it.
func remove(t *testing.T, w *worklet.Writer, opID, id string) (worklet.WriteResult, error) {
	t.Helper()
	prior, err := w.ViewPrior(t.Context(), id)
	if err != nil {
		t.Fatalf("read what deleting %s takes away: %v", id, err)
	}
	return w.DeleteView(t.Context(), opID, id, prior)
}

// savedIDs is the views somebody saved in a listing, builtins left out.
func savedIDs(l worklet.ViewListing) []string {
	var out []string
	for _, row := range l.Views {
		if !row.Builtin {
			out = append(out, row.ID)
		}
	}
	return out
}

// A DELETED VIEW IS GONE FROM EVERY READ OF THE VIEWS.
//
// A view nobody could take away stayed in everybody's strip for good: the
// engine could only create or replace one. Deleted, it is in no strip, in no
// inventory, runs as nothing, and is no view a save can replace or a second
// delete can take away again.
func TestADeletedViewIsGoneFromEveryReadOfTheViews(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}
	for _, id := range []string{"v-keep", "v-gone"} {
		if _, err := save(t, r.writer, "op-"+id,
			aView(id, func(v *worklet.View) { v.Name = id })); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}

	if _, err := remove(t, r.writer, "op-delete", "v-gone"); err != nil {
		t.Fatalf("delete v-gone: %v", err)
	}
	r.drain()

	if got := savedIDs(r.strip(eng, "ana")); !slices.Equal(got, []string{"v-keep"}) {
		t.Fatalf("the strip holds %v after the delete, want [v-keep]", got)
	}
	every, err := r.reader.EveryView(t.Context(), worklet.EveryViewQuery{
		Viewer: "ana", Level: statelog.ReadStale,
	})
	if err != nil {
		t.Fatalf("EveryView: %v", err)
	}
	if got := savedIDs(every); !slices.Equal(got, []string{"v-keep"}) {
		t.Fatalf("the inventory holds %v after the delete, want [v-keep]", got)
	}

	// RUN BY ITS ID, refused for good: a read to try again would be a lie
	// about a view that is not coming back.
	_, err = r.reader.Expand(t.Context(), map[string]any{"view": "v-gone"},
		worklet.Viewer{Handle: "ana"})
	if !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("running the deleted view answered %v, want an ErrInvalid refusal", err)
	}

	prior, err := r.writer.ViewPrior(t.Context(), "v-gone")
	if err != nil {
		t.Fatalf("ViewPrior: %v", err)
	}
	if prior.Held {
		t.Fatal("the prior of a deleted view says it is held, so a save would replace it")
	}
	if _, err := r.writer.DeleteView(t.Context(), "op-delete-again", "v-gone",
		prior); !errors.Is(err, worklet.ErrNoView) {
		t.Fatalf("deleting it again answered %v, want ErrNoView", err)
	}
}

// A SAVE DECIDED BEFORE THE DELETE DOES NOT BRING THE VIEW BACK.
//
// The save's prior was read while the view was live; replacing the deleted row
// would publish the view again under the authority decided on a view that is
// gone. It is refused as a conflict, and the re-read says there is no view.
func TestASaveDecidedBeforeADeleteDoesNotBringTheViewBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}
	if _, err := save(t, r.writer, "op-v", aView("v-1", nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	stale, err := r.writer.ViewPrior(t.Context(), "v-1")
	if err != nil {
		t.Fatalf("ViewPrior: %v", err)
	}
	if _, err := remove(t, r.writer, "op-delete", "v-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	r.drain()

	_, err = r.writer.WriteView(t.Context(), "op-rename",
		aView("v-1", func(v *worklet.View) { v.Name = "renamed" }), stale)
	if !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("the save decided before the delete answered %v, want a conflict", err)
	}
	r.drain()
	if got := savedIDs(r.strip(eng, "ana")); len(got) != 0 {
		t.Fatalf("the strip holds %v, want the deleted view still gone", got)
	}
}

// A PROTECTED VIEW IS ITS OWNER'S TO DELETE, as it is theirs to change.
func TestAProtectedViewIsDeletedOnlyByItsOwner(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// Written by ana, whom the harness's own writer is.
	if _, err := save(t, r.writer, "op-guarded", aView("v-guarded", func(v *worklet.View) {
		v.Owner, v.Protected = "ana", true
	})); err != nil {
		t.Fatalf("save the protected view: %v", err)
	}
	r.drain()

	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	_, err := remove(t, bob, "op-bob", "v-guarded")
	if !errors.Is(err, worklet.ErrForbidden) {
		t.Fatalf("bob deleting ana's protected view answered %v, want ErrForbidden", err)
	}
	if _, err := remove(t, r.writer, "op-ana", "v-guarded"); err != nil {
		t.Fatalf("ana could not delete her own protected view: %v", err)
	}
}

// A DELETE MADE AGAIN IS ANSWERED BY THE DELETED ROW.
//
// A rebased re-run of the delete is new to the ledger and carries the prior
// its first attempt read; the view is gone, which is what it asked for, so it
// is answered with nothing published rather than refused.
func TestADeleteMadeAgainIsAnsweredByTheDeletedRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := save(t, r.writer, "op-v", aView("v-1", nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	prior, err := r.writer.ViewPrior(t.Context(), "v-1")
	if err != nil {
		t.Fatalf("ViewPrior: %v", err)
	}
	if _, err := r.writer.DeleteView(t.Context(), "op-delete", "v-1", prior); err != nil {
		t.Fatalf("delete: %v", err)
	}
	r.drain()
	if _, err := r.writer.DeleteView(t.Context(), "op-delete-rebased", "v-1", prior); err != nil {
		t.Fatalf("the re-run of the delete was refused: %v", err)
	}
}

// A PIN OF A DELETED VIEW GOES WHEN ITS PERSON'S PINS NEXT CHANGE.
//
// A delete cannot write every pinner's record, so the pin outlives the view,
// drawn nowhere. Left there for good it would hold one of the person's
// [worklet.MaxPinnedViews] places with nothing on screen to unpin.
func TestAPinOfADeletedViewGoesWhenThePinsNextChange(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"v-1", "v-2"} {
		if _, err := save(t, r.writer, "op-"+id, aView(id, nil)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.WritePins(t.Context(), "op-pin-1", "ana",
		worklet.PinGesture{Views: worklet.SetChange[string]{Add: []string{"v-1"}}},
		worklet.PersonAuthority{}); err != nil {
		t.Fatalf("pin v-1: %v", err)
	}
	r.drain()
	if _, err := remove(t, r.writer, "op-delete", "v-1"); err != nil {
		t.Fatalf("delete v-1: %v", err)
	}
	r.drain()
	if _, err := r.writer.WritePins(t.Context(), "op-pin-2", "ana",
		worklet.PinGesture{Views: worklet.SetChange[string]{Add: []string{"v-2"}}},
		worklet.PersonAuthority{}); err != nil {
		t.Fatalf("pin v-2: %v", err)
	}
	r.drain()
	person := r.person("ana")
	if !slices.Equal(person.PinnedViews, []string{"v-2"}) {
		t.Fatalf("ana's pins are %v, want [v-2] with the deleted view dropped", person.PinnedViews)
	}
}
