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
		term: func() time.Duration { return 30 * 24 * time.Hour },
		removed: func(_ context.Context, ids []string) ([]string, error) {
			return slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
				return id != gone.String()
			}), nil
		}}
	if src.Term() != 30*24*time.Hour {
		t.Errorf("the source's term is %v", src.Term())
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

// THE RETENTION IS THE RUNNING COMPANY'S, as its block states it: unset (or no
// company at all) is the block's thirty days, a set value is kept, and a value
// past the bounds is clamped into them by the same accessor validation reads.
// Mutation: answer a constant and the set and clamped cases go red.
func TestTheAssistantsRetentionIsTheRunningCompanys(t *testing.T) {
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
		{"a node running no company", nil, 30 * day},
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
