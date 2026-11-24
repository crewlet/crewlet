package collect_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
)

// faultyMisses is the coordination store whose read or write of a source's
// misses fails, as one across a link that outlasts its crossing does.
type faultyMisses struct {
	*collect.CoordStore
	readErr, writeErr error
}

func (s *faultyMisses) Misses(ctx context.Context, st collect.Status) (collect.Status, error) {
	if s.readErr != nil {
		return st, s.readErr
	}
	return s.CoordStore.Misses(ctx, st)
}

func (s *faultyMisses) SaveMisses(ctx context.Context, st collect.Status) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.CoordStore.SaveMisses(ctx, st)
}

// faultyHarness is newHarness's worker over a store whose misses can fail.
func faultyHarness(t *testing.T) (*harness, *faultyMisses) {
	t.Helper()
	h := newHarness(t)
	store := &faultyMisses{CoordStore: h.store}
	company := &config.Company{Name: "acme"}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", APIToken: "${CF_TOKEN}"}}
	var err error
	h.worker, err = collect.New(collect.Options{Sources: []collect.Source{h.source}, Store: store, Log: h.log,
		Input:    func(context.Context) (collect.Input, bool) { return collect.Input{Config: company}, true },
		Interval: func() time.Duration { return time.Hour }, Now: func() time.Time { return h.clock }})
	if err != nil {
		t.Fatal(err)
	}
	return h, store
}

// A VISIT WHOSE MISSES CANNOT BE READ ASKS THE VENDOR NOTHING AND IS A FAULT:
// deciding without them would miss every entry afresh and push each
// retirement back a day, so the source is not read, nothing is recorded, and
// the standing says why while the misses stay as they were.
//
// Mutation: ignore the misses' read error, and the vendor is read and the
// visit completes.
func TestAVisitWhoseMissesCannotBeReadReadsNothing(t *testing.T) {
	t.Parallel()
	h, store := faultyHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	h.source.set(nil, nil)
	h.tick(time.Hour)
	before := h.misses()
	if len(before.Missing) != 1 {
		t.Fatalf("premise: an empty complete read missed %+v, want the four in one group", before.Missing)
	}
	store.readErr = errors.New("the misses crossed no link in time")
	h.tick(25 * time.Hour)
	st := h.standing()
	if h.source.reads != 2 || h.log.recorded() != 4 || st.Completed || !strings.Contains(st.LastError, "no link") {
		t.Fatalf("a visit that could not read its misses read the source %d times, recorded %d entries and "+
			"left %+v; want no read, no record and the fault said", h.source.reads, h.log.recorded(), st)
	}
	if after := h.misses(); st.Missed != 4 || len(after.Missing) != 1 ||
		!slices.Equal(after.Missing[0].Entries, before.Missing[0].Entries) {
		t.Errorf("the misses after the fault = %+v counted %d, want the four recorded before it", after.Missing,
			st.Missed)
	}
}

// A VISIT WHOSE MISSES CANNOT BE WRITTEN IS A FAULT, AND ITS STANDING COUNTS
// ONLY THE MISSES STILL RECORDED: a standing counting misses that never
// landed would tell every reader of the sources what no record holds.
//
// Mutation: drop the misses' write error, and the visit completes; keep the
// plan's count, and the standing counts four misses no record holds.
func TestAVisitWhoseMissesCannotBeWrittenCountsOnlyThoseRecorded(t *testing.T) {
	t.Parallel()
	h, store := faultyHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	store.writeErr = errors.New("the misses crossed no link in time")
	h.source.set(nil, nil)
	h.tick(time.Hour)
	st := h.standing()
	if st.Completed || !strings.Contains(st.LastError, "no link") {
		t.Fatalf("a visit whose misses were not written left %+v, want it failed and saying why", st)
	}
	if missed := h.misses(); st.Missed != 0 || len(missed.Missing) != 0 {
		t.Errorf("the standing counts %d misses beside %+v recorded, want none of either", st.Missed,
			missed.Missing)
	}
}
