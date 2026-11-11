package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// atlasHoursFake answers the hours it holds, at most limit of them.
type atlasHoursFake struct {
	hours  []time.Time
	cutoff time.Time
}

func (f *atlasHoursFake) ExpirableHours(_ context.Context, cutoff time.Time, limit int) ([]time.Time, error) {
	f.cutoff = cutoff
	return f.hours[:min(limit, len(f.hours))], nil
}

// atlasExpiriesFake records every expiry it is asked for, and answers err.
type atlasExpiriesFake struct {
	calls []atlasdomain.Call
	hours []time.Time
	err   error
}

func (f *atlasExpiriesFake) Expire(_ context.Context, c atlasdomain.Call, hour, _ time.Time) (statelog.Result, error) {
	f.calls, f.hours = append(f.calls, c), append(f.hours, hour)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, f.err
}

// THE REAPER EXPIRES THE ATLAS'S HISTORY HOUR BY HOUR, on the company's own
// retention: it nominates the hours the log says hold history past the cutoff,
// expires each as the reaper itself, and takes an hour with nothing left as one
// no longer eligible rather than a failure.
//
// Mutation: nominate the hours in another spelling than atlasdomain.Bucket and
// the purge of each refuses it.
func TestTheReaperExpiresTheAtlasHistoryHourByHour(t *testing.T) {
	t.Parallel()
	first := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)
	cutoff := first.Add(48 * time.Hour)
	reader := &atlasHoursFake{hours: []time.Time{first, first.Add(time.Hour)}}
	writer := &atlasExpiriesFake{}
	trash := atlasTrash{reader: reader, writer: writer, term: func(context.Context) (time.Duration, error) {
		return atlasRetentionOf(nil), nil
	}}
	if term, _ := trash.Term(t.Context()); term != 400*24*time.Hour {
		t.Errorf("a company that states no retention keeps the atlas's history %s, want 400 days", term)
	}
	ids, err := trash.TrashedBefore(t.Context(), cutoff, 10)
	if err != nil || !slices.Equal(ids, []string{"2030-01-01T09", "2030-01-01T10"}) || !reader.cutoff.Equal(cutoff) {
		t.Fatalf("the reaper nominates %v (%v) at %s, want the two hours at %s", ids, err, reader.cutoff, cutoff)
	}
	for _, id := range ids {
		if err := trash.PurgeTrashed(t.Context(), id, cutoff, "op-"+id); err != nil {
			t.Fatalf("expire %s: %v", id, err)
		}
	}
	if len(writer.calls) != 2 || writer.calls[0].By != reaperActor || writer.calls[1].OpID != "op-2030-01-01T10" ||
		!writer.hours[1].Equal(first.Add(time.Hour)) {
		t.Errorf("the reaper expired %+v at %v, want each hour as itself under its operation", writer.calls, writer.hours)
	}
	writer.err = atlasdomain.ErrNotDue
	if err := trash.PurgeTrashed(t.Context(), ids[0], cutoff, "op-again"); !errors.Is(err, trashreap.ErrNotEligible) {
		t.Errorf("an hour with nothing left: %v, want not eligible", err)
	}
	if err := trash.PurgeTrashed(t.Context(), "yesterday", cutoff, "op-bad"); !errors.Is(err, trashreap.ErrNotEligible) {
		t.Errorf("an hour the log never spells: %v, want not eligible", err)
	}
}

// THE COMPANY'S OWN RETENTION is what the reaper reads, and the block's default
// where the company states none.
func TestTheAtlasRetentionIsTheCompanys(t *testing.T) {
	t.Parallel()
	set := &Company{Config: &config.Company{Atlas: config.Atlas{RetentionDays: 120}}}
	if got := atlasRetentionOf(set); got != 120*24*time.Hour {
		t.Errorf("a company that keeps 120 days is read as %s", got)
	}
	if got := atlasRetentionOf(&Company{}); got != (config.Atlas{}).Retention() {
		t.Errorf("a company built in code is read as %s, want the default", got)
	}
}
