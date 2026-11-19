package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fakeDepartedWork is the tracker's writer as a retirement re-homes through
// it: the departed holders it answers, and every departure it was handed.
type fakeDepartedWork struct {
	holders    []string
	holdersErr error
	moved      []worklet.Rehomed
	moveErr    error
	departures []worklet.Departure
}

func (f *fakeDepartedWork) DepartedHolders(context.Context) ([]string, error) {
	return f.holders, f.holdersErr
}

func (f *fakeDepartedWork) RehomeDeparted(_ context.Context, d worklet.Departure) ([]worklet.Rehomed, error) {
	f.departures = append(f.departures, d)
	return f.moved, f.moveErr
}

// A RETIRED SEAT'S WORK IS RE-HOMED BY ITS HANDLE, under the retirement's own
// instant and with the items its runs are on: the handle the record names, or,
// for a record that lost it, the departed holder whose id under the company's
// name is the seat's. A handle the chart still gives a seat (an agent seat
// made a person's) keeps its work, and a write that does not land fails the
// retirement so the next tick retries it.
//
// Mutations: drop the chart check and a person loses the work of the seat they
// now hold; match a nameless record's holders by anything but the derived id
// and another departed seat's work is re-homed with it; swallow the writer's
// error and the mailbox goes with the work still on the seat.
func TestARetiredSeatsWorkIsRehomedByItsHandle(t *testing.T) {
	t.Parallel()
	retiring := time.Date(2031, 4, 17, 9, 0, 0, 0, time.UTC)
	chart := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Ben", DeclaredHandle: "ben", Kind: org.KindHuman},
	}}
	ana, _ := org.DeriveAgentID("Acme", "ana")
	ben, _ := org.DeriveAgentID("Acme", "ben")
	runs := []string{"t-1"}

	named := &fakeDepartedWork{}
	if err := rehomeDepartedWork(t.Context(), named, chart, runs, ana, "ana", retiring); err != nil {
		t.Fatal(err)
	}
	if len(named.departures) != 1 || named.departures[0].Handle != "ana" ||
		!named.departures[0].At.Equal(retiring) || !slices.Equal(named.departures[0].Runs, runs) {
		t.Errorf("re-homed %+v, want ana's work under the retirement's instant with its runs' items",
			named.departures)
	}

	person := &fakeDepartedWork{}
	if err := rehomeDepartedWork(t.Context(), person, chart, nil, ben, "ben", retiring); err != nil ||
		len(person.departures) != 0 {
		t.Errorf("a handle the chart still gives a person's seat: re-homed %+v (%v)", person.departures, err)
	}

	nameless := &fakeDepartedWork{holders: []string{"ana", "cat", "dan"}}
	if err := rehomeDepartedWork(t.Context(), nameless, chart, nil, ana, "", retiring); err != nil {
		t.Fatal(err)
	}
	if len(nameless.departures) != 1 || nameless.departures[0].Handle != "ana" {
		t.Errorf("a record with no handle re-homed %+v, want ana's work alone", nameless.departures)
	}

	unread := &fakeDepartedWork{holdersErr: errors.New("the estate could not be read")}
	if err := rehomeDepartedWork(t.Context(), unread, chart, nil, ana, "", retiring); err == nil ||
		len(unread.departures) != 0 {
		t.Errorf("holders that could not be read: re-homed %+v (%v), want nothing and the error",
			unread.departures, err)
	}

	stale := &fakeDepartedWork{moveErr: worklet.ErrStaleVersion}
	if err := rehomeDepartedWork(t.Context(), stale, chart, nil, ana, "ana", retiring); !errors.Is(err,
		worklet.ErrStaleVersion) {
		t.Errorf("a write that did not land answered %v, want the retirement failed with it", err)
	}
}

// THE RUNS' ITEMS ARE WORKLET'S, by id, each once: a run charged to another
// tracker's item, or to none, names no item the re-homing could hold.
func TestARetiredSeatsRunsNameTheirWorkletItems(t *testing.T) {
	t.Parallel()
	got := runItems([]sandbox.PendingRun{
		{WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "t-1"}},
		{WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "t-1"}},
		{WorkItem: &types.WorkItem{Backend: types.WorkJira, ID: "10001"}},
		{},
		{WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "t-2"}},
	})
	if !slices.Equal(got, []string{"t-1", "t-2"}) {
		t.Errorf("the runs name %v, want t-1 and t-2", got)
	}
}
