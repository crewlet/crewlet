package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// fakeAssistant is the assistant's reader and writer as the reaper's source
// reaches them.
type fakeAssistant struct {
	owners   []uuid.UUID
	inactive []uuid.UUID
	erased   []uuid.UUID
	purged   []uuid.UUID
	notDue   map[uuid.UUID]bool
}

func (f *fakeAssistant) Owners(context.Context) ([]uuid.UUID, error) { return f.owners, nil }

func (f *fakeAssistant) InactiveBefore(_ context.Context, _ time.Time, limit int) ([]uuid.UUID, error) {
	return f.inactive[:min(limit, len(f.inactive))], nil
}

func (f *fakeAssistant) EraseOwner(_ context.Context, c assistantdomain.Call) (statelog.Result, error) {
	f.erased = append(f.erased, c.Owner)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (f *fakeAssistant) PurgeInactive(_ context.Context, _ iam.Actor, _ string, id uuid.UUID,
	_ time.Time) (statelog.Result, error) {

	if f.notDue[id] {
		return statelog.Result{}, assistantdomain.ErrNotDue
	}
	f.purged = append(f.purged, id)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// THE ASSISTANT'S REAPER SOURCE NOMINATES AN ERASURE BEFORE ANY RETENTION PURGE,
// and only for a person the identity estate removed: a removed person's
// conversations are erased in one record, a conversation past its retention is
// purged, and one used since it was listed is skipped rather than failed.
// Mutation: nominate every owner rather than the removed ones and a person
// still here is erased.
func TestTheAssistantsReaperSourceErasesTheRemovedFirst(t *testing.T) {
	t.Parallel()
	here, gone := uuid.New(), uuid.New()
	idle, busy := uuid.New(), uuid.New()
	fake := &fakeAssistant{owners: []uuid.UUID{here, gone}, inactive: []uuid.UUID{idle, busy},
		notDue: map[uuid.UUID]bool{busy: true}}
	src := assistantTrash{reader: fake, writer: fake,
		term: func(context.Context) (time.Duration, error) { return 30 * 24 * time.Hour, nil },
		removed: func(_ context.Context, ids []string) ([]string, error) {
			return slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
				return id != gone.String()
			}), nil
		}}
	if term, err := src.Term(t.Context()); err != nil || term != 30*24*time.Hour {
		t.Errorf("the source's term is %v (%v)", term, err)
	}
	got, err := src.TrashedBefore(t.Context(), time.Now(), 10)
	if err != nil {
		t.Fatalf("TrashedBefore: %v", err)
	}
	want := []string{ownerItem + gone.String(), idle.String(), busy.String()}
	if !slices.Equal(got, want) {
		t.Fatalf("the source nominates %v, want %v", got, want)
	}
	if got, _ := src.TrashedBefore(t.Context(), time.Now(), 1); !slices.Equal(got, want[:1]) {
		t.Errorf("a listing of one nominates %v, want the erasure alone", got)
	}
	for _, id := range want[:2] {
		if err := src.PurgeTrashed(t.Context(), id, time.Now(), "op"); err != nil {
			t.Errorf("purge %s: %v", id, err)
		}
	}
	if err := src.PurgeTrashed(t.Context(), busy.String(), time.Now(), "op"); !errors.Is(err,
		trashreap.ErrNotEligible) {
		t.Errorf("a conversation used since it was listed answers %v, want not eligible", err)
	}
	if !slices.Equal(fake.erased, []uuid.UUID{gone}) || !slices.Equal(fake.purged, []uuid.UUID{idle}) {
		t.Errorf("erased %v and purged %v", fake.erased, fake.purged)
	}
	if err := src.PurgeTrashed(t.Context(), "owner:not-a-person", time.Now(), "op"); !errors.Is(err,
		trashreap.ErrNotEligible) {
		t.Errorf("an unreadable item answers %v", err)
	}
}

// THE RETENTION IS THE DOCUMENT'S, as its block states it: unset (or no
// document at all) is the block's thirty days, a set value is kept, and a
// value past the bounds is clamped into them by the same accessor validation
// reads. Which document that is, is the fleet's pointer's
// (TestTheAssistantsRetentionIsKnownOnlyOnANodeServingTheFleetsRevision).
// Mutation: answer a constant and the set and clamped cases go red.
func TestTheAssistantsRetentionIsTheDocumentsOwn(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	company := func(days int) *Company {
		return &Company{Config: &config.Company{Assistant: config.Assistant{RetentionDays: days}}}
	}
	for _, c := range []struct {
		name    string
		company *Company
		want    time.Duration
	}{
		{"a company with no config", &Company{}, 30 * day},
		{"a company that sets nothing", company(0), 30 * day},
		{"a company that keeps a week", company(7), 7 * day},
		{"a company past the bound", company(900), 365 * day},
	} {
		if got := assistantRetentionOf(c.company); got != c.want {
			t.Errorf("%s keeps conversations %v, want %v", c.name, got, c.want)
		}
	}
}

// THE RETENTION IS THE FLEET REVISION'S, KNOWN ONLY ON A NODE THAT RUNS IT, and
// a pass that cannot know it purges no conversation.
//
// The reaper's purge is a record every data node applies, and a purged
// conversation is gone for the person who owned it, so the term is the
// `assistant.retention_days` of the revision the fleet's activation pointer
// names, read on a node whose company was applied from that very activation
// ([Engine.fleetCompany]). A node still serving an activation the fleet has
// replaced (one that refused the fleet's revision), a node booted on an
// offline offer no activation named, a pointer that cannot be read and a fleet
// that has activated nothing each leave it unknown, and the pass over the
// source purges nothing; a node serving the fleet's activation purges at its
// document's week. Mutation: read the term off the node's own company again
// and every case but the first reads a term it cannot know, and purges by it.
func TestTheAssistantsRetentionIsKnownOnlyOnANodeServingTheFleetsRevision(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	theFleets := func(_, current coord.Activation) time.Time { return current.At }
	for _, tc := range []struct {
		name string
		// stamp is the activation the node's company was applied from,
		// given the fleet's earlier and current ones; nil activates
		// nothing at all.
		stamp func(earlier, current coord.Activation) time.Time
		// down makes the pointer unreadable.
		down bool
		// known is whether this node may read the term.
		known bool
	}{
		{name: "a node serving the fleet's activation", stamp: theFleets, known: true},
		{name: "a node serving an activation the fleet has replaced",
			stamp: func(earlier, _ coord.Activation) time.Time { return earlier.At }},
		{name: "a node booted on an offer no activation named",
			stamp: func(coord.Activation, coord.Activation) time.Time { return time.Time{} }},
		{name: "a node that cannot read the pointer", stamp: theFleets, down: true},
		{name: "a fleet that has activated nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
			company := &Company{Config: &config.Company{
				Assistant: config.Assistant{RetentionDays: 7}}}
			if tc.stamp != nil {
				earlier := activate(t, fleet, "rev-1")
				company.ActivatedAt = tc.stamp(earlier, activate(t, fleet, "rev-2"))
			}
			fleet.down = tc.down
			e := &Engine{backends: &Backends{Fleet: fleet}}
			e.epoch.current.Store(company)

			term, err := e.assistantRetention(t.Context())
			if tc.known != (err == nil) || (tc.known && term != week) {
				t.Fatalf("the term reads %v (%v), want it known (%v) at a week only "+
					"on a node serving the fleet's activation", term, err, tc.known)
			}

			// AND THE PASS over the source the engine builds with it.
			idle := uuid.New()
			fake := &fakeAssistant{inactive: []uuid.UUID{idle}}
			src := assistantTrash{reader: fake, writer: fake, term: e.assistantRetention,
				removed: func(context.Context, []string) ([]string, error) { return nil, nil }}
			reaper, err := trashreap.New(trashreap.Deps{
				Sources: []trashreap.Source{src}, Term: 30 * 24 * time.Hour,
				Reports: coordmem.NewFleet(), Node: "node-a",
			})
			if err != nil {
				t.Fatalf("build the reaper: %v", err)
			}
			report, passErr := reaper.Pass(t.Context())
			if tc.known {
				if passErr != nil || !slices.Equal(fake.purged, []uuid.UUID{idle}) {
					t.Errorf("a node serving the fleet's activation purged %v (%v), "+
						"want the idle conversation", fake.purged, passErr)
				}
				return
			}
			if len(fake.purged) != 0 {
				t.Errorf("a node that cannot know the fleet's term purged %v", fake.purged)
			}
			if passErr == nil || report.Sources[0].Error == "" {
				t.Errorf("a pass that purged nothing for want of a term says nothing "+
					"of why (%v, %+v)", passErr, report.Sources)
			}
		})
	}
}
