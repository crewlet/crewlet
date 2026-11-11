package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A PERSON IS NAMED BY `@handle` OR BY THE NAME THE CHART GIVES THEM, NEVER BY
// A COMMON WORD (ADR-0122): a handle or a one-word name that is also an
// ordinary word (`will`, `may`, `max`) is not matched where the text uses the
// word, so a date wait whose reason says "the release will be cut Friday" waits
// on nobody; `@will` and "Will Turner" name the person.
//
// Mutation: match a bare handle as a whole word, and "the release will be cut
// Friday" waits on will.
func TestAPersonIsNamedByHandleOrNameNeverByACommonWord(t *testing.T) {
	t.Parallel()
	people := []worklet.PersonSeat{
		{Handle: "will", Name: "Will Turner"},
		{Handle: "may", Name: "May"},
		{Handle: "max"},
		{Handle: "jane-founder", Name: "Jane Founder"},
	}
	for _, row := range []struct {
		text string
		want []string
	}{
		{"the release will be cut Friday", nil},
		{"may slip to max 3 retries", nil},
		{"until @will confirms", []string{"will"}},
		{"until @Will confirms", []string{"will"}},
		{"until Will Turner confirms", []string{"will"}},
		{"until WILL TURNER confirms", nil},
		{"@willow has the keys", nil},
		{"May signs off then", []string{"may"}},
		{"@max and Jane Founder", []string{"max", "jane-founder"}},
		{"jane-founder signs", nil},
		{"Jane Founders' meeting", nil},
	} {
		got := worklet.PersonsNamed(row.text, people)
		if !slices.Equal(got, row.want) {
			t.Errorf("PersonsNamed(%q) = %v, want %v", row.text, got, row.want)
		}
	}
}

// readyNaming is handle's ready work with people as the persons a wait can name.
func (r *roundTrip) readyNaming(handle string, people []worklet.PersonSeat) worklet.ReadyWork {
	r.t.Helper()
	out, err := r.reader.ReadyWork(r.t.Context(), worklet.ReadyWorkQuery{
		Handle: handle, Level: statelog.ReadStale, People: people,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("ReadyWork(%s): %v", handle, err)
	}
	return out
}

// AN AGENT'S WAIT ON A PERSON IT HAS NOT ASKED IS READY BEFORE ITS DATE
// (ADR-0122): a wait until a later date whose reason names a person comes back
// to its holder, naming them, however long ago it was written; it does not
// when its reason names nobody, when no person is declared, when a person
// wrote the wait, when the holder already asked on the item (it is held on
// that question), or when the agent that set the wait asked that person; and a
// wait whose writer asked one person it names still comes back for another.
//
// Mutation: drop the wait block from the ready read, and the item sits out its
// date with nobody asked; read the writer's asks from the holder alone, and a
// colleague's wait it asked about comes back to the holder.
func TestAnAgentsWaitOnAPersonItHasNotAskedIsReady(t *testing.T) {
	t.Parallel()
	people := []worklet.PersonSeat{{Handle: "jane", Name: "Jane Founder"}, {Handle: "will", Name: "Will Turner"}}
	until := wednesday.Add(72 * time.Hour)
	wait := func(reason string) worklet.TaskPatch {
		return worklet.TaskPatch{StartAt: &until, WaitReason: &reason}
	}
	ask := func(t *testing.T, r *roundTrip, by, of string) {
		t.Helper()
		c := worklet.Comment{ID: "c-" + by + "-" + of, Task: "w-1", Author: by,
			AuthorKind: worklet.AuthorAgent, Body: "can you confirm?", Ask: of, CreatedAt: r.at}
		probeBy(t, r, "op-ask-"+by, by, worklet.AuthorAgent, "w-1", worklet.TaskPatch{Comment: &c},
			worklet.ChangeComment)
	}
	for _, row := range []struct {
		name   string
		set    func(t *testing.T, r *roundTrip)
		people []worklet.PersonSeat
		want   []string
	}{
		{"an agent's wait naming a person", func(t *testing.T, r *roundTrip) {
			probeUpdate(wait("until Jane Founder signs off"), worklet.ChangeFields)(t, r, "turn-1", "w-1")
		}, people, []string{"jane"}},
		{"by handle", func(t *testing.T, r *roundTrip) {
			probeUpdate(wait("until @jane signs off"), worklet.ChangeFields)(t, r, "turn-1", "w-1")
		}, people, []string{"jane"}},
		{"a wait naming nobody", func(t *testing.T, r *roundTrip) {
			probeUpdate(wait("the release will be cut then"), worklet.ChangeFields)(t, r, "turn-1", "w-1")
		}, people, nil},
		{"with no person declared", func(t *testing.T, r *roundTrip) {
			probeUpdate(wait("until @jane signs off"), worklet.ChangeFields)(t, r, "turn-1", "w-1")
		}, nil, nil},
		{"a person's own wait", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-wait", "pm", worklet.AuthorHuman, "w-1", wait("until @jane signs off"), worklet.ChangeFields)
		}, people, nil},
		{"a wait its holder asked about", func(t *testing.T, r *roundTrip) {
			probeUpdate(wait("until @jane signs off"), worklet.ChangeFields)(t, r, "turn-1", "w-1")
			ask(t, r, "ana", "jane")
		}, people, nil},
		{"a colleague's wait it asked about", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-wait", "cto", worklet.AuthorAgent, "w-1", wait("until @jane signs off"), worklet.ChangeFields)
			ask(t, r, "cto", "jane")
		}, people, nil},
		{"a colleague's wait it did not ask about", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-wait", "cto", worklet.AuthorAgent, "w-1", wait("until @jane signs off"), worklet.ChangeFields)
		}, people, []string{"jane"}},
		{"a wait naming two, one of them asked", func(t *testing.T, r *roundTrip) {
			probeBy(t, r, "op-wait", "cto", worklet.AuthorAgent, "w-1", wait("until @jane and @will agree"),
				worklet.ChangeFields)
			ask(t, r, "cto", "jane")
		}, people, []string{"will"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			fileFor(t, r, "w-1", "ana", worklet.StatusTodo, worklet.PriorityNormal)
			row.set(t, r)
			got := r.readyNaming("ana", row.people)
			if row.want == nil {
				if len(got.Items) != 0 {
					t.Errorf("%s left ready work %v, want none before its date", row.name, readyIDs(got))
				}
				return
			}
			if len(got.Items) != 1 {
				t.Fatalf("%s left ready work %v, want w-1", row.name, readyIDs(got))
			}
			item := got.Items[0]
			if item.Why != types.PickupWaitsOnPerson || !slices.Equal(item.WaitsOn, row.want) {
				t.Errorf("%s came back as %q naming %v, want %q naming %v", row.name, item.Why,
					item.WaitsOn, types.PickupWaitsOnPerson, row.want)
			}
			// QUIET SINCE ITS LAST WRITE, never since the date ahead of it.
			if !item.Quiet.Equal(wednesday) {
				t.Errorf("%s is quiet since %s, want %s, its last write", row.name, item.Quiet, wednesday)
			}
		})
	}
}
