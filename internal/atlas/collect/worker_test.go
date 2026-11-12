package collect_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeSource is a source whose world the test sets.
type fakeSource struct {
	mu     sync.Mutex
	claims []atlas.Claim
	err    error
	reads  int
}

func (*fakeSource) Kind() integration.Kind { return integration.KindCloudflare }

func (s *fakeSource) Collect(_ context.Context, in collect.Input) (collect.Read, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return collect.Read{}, s.err
	}
	var out []atlas.Claim
	for _, c := range s.claims {
		c.ObservedAt = in.Now
		out = append(out, c)
	}
	return collect.Read{Claims: out, Calls: 2}, nil
}

func (s *fakeSource) set(claims []atlas.Claim, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims, s.err = claims, err
}

// fakeLog is the atlas log as the duty sees it: one voice's live claims, kept
// by entry and slot, and the reads that moved them.
type fakeLog struct {
	mu      sync.Mutex
	live    map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim
	reads   []atlasdomain.ReadInput
	purged  map[atlas.Ref]bool
	refuse  error
	callers []iam.Actor
}

func newFakeLog() *fakeLog {
	return &fakeLog{live: map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim{}, purged: map[atlas.Ref]bool{}}
}

func (l *fakeLog) LiveOf(_ context.Context, src atlas.Source, _ statelog.Freshness) (atlasdomain.Live, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out atlasdomain.Live
	for _, slots := range l.live[src] {
		for _, c := range slots {
			out.Claims = append(out.Claims, c)
		}
	}
	out.Complete = true
	return out, nil
}

func (l *fakeLog) Read(_ context.Context, c atlasdomain.Call, in atlasdomain.ReadInput) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.callers = append(l.callers, c.By)
	if l.refuse != nil {
		return statelog.Result{}, l.refuse
	}
	if l.purged[in.Entry] {
		return statelog.Result{}, atlasdomain.ErrPurged
	}
	l.reads = append(l.reads, in)
	voice := l.live[in.Source]
	if voice == nil {
		voice = map[atlas.Ref]map[string]atlas.Claim{}
		l.live[in.Source] = voice
	}
	slots := voice[in.Entry]
	if slots == nil {
		slots = map[string]atlas.Claim{}
		voice[in.Entry] = slots
	}
	for _, claim := range in.Claims {
		claim.Source, claim.ObservedAt = in.Source, in.ObservedAt
		slots[claim.Slot()] = claim
	}
	for _, slot := range in.Withdrawn {
		delete(slots, slot)
	}
	return statelog.Result{}, nil
}

func (l *fakeLog) recorded() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reads)
}

// harness is one worker over the fakes and the memory coordination store.
type harness struct {
	t      *testing.T
	source *fakeSource
	log    *fakeLog
	store  *collect.CoordStore
	worker *collect.Worker
	clock  time.Time
	held   bool
	claims int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := collect.NewCoordStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, source: &fakeSource{}, log: newFakeLog(), store: store, clock: t0, held: true}
	company := &config.Company{Name: "acme"}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", APIToken: "${CF_TOKEN}"}}
	h.worker, err = collect.New(collect.Options{
		Sources:  []collect.Source{h.source},
		Store:    store,
		Log:      h.log,
		Input:    func(context.Context) (collect.Input, bool) { return collect.Input{Config: company}, true },
		Interval: func() time.Duration { return time.Hour },
		ClaimDuty: func(context.Context) (bool, error) {
			h.claims++
			return h.held, nil
		},
		Now: func() time.Time { return h.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) tick(after time.Duration) {
	h.t.Helper()
	h.clock = h.clock.Add(after)
	h.worker.Tick(h.t.Context())
}

func (h *harness) standing() collect.Status {
	h.t.Helper()
	all, err := h.store.Load(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	return all[integration.KindCloudflare]
}

// A SOURCE IS READ WHEN DUE, RECORDED ONCE, AND AN UNCHANGED READ WRITES
// NOTHING: the first tick records every entry, a tick before the interval
// reads nothing, and a tick after it reads again and records nothing.
//
// Mutation: skip the diff and record every read, and the third tick writes
// four records again.
func TestASourceIsReadWhenDueAndAnUnchangedReadWritesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	if got := h.log.recorded(); got != 4 {
		t.Fatalf("the first tick recorded %d entries, want 4", got)
	}
	st := h.standing()
	if !st.Completed || !st.LastCompleteAt.Equal(t0) || st.Recorded != 4 || st.Calls != 2 {
		t.Errorf("the standing after the first read: %+v", st)
	}
	h.tick(30 * time.Minute)
	if h.source.reads != 1 {
		t.Errorf("a tick half an hour later read the source again")
	}
	h.tick(31 * time.Minute)
	if h.source.reads != 2 || h.log.recorded() != 4 {
		t.Errorf("a tick an hour later read %d times and recorded %d entries, want 2 reads and no new record",
			h.source.reads, h.log.recorded())
	}
	for _, by := range h.log.callers {
		if by.Kind != iam.ActorSystem || by.Name != collect.DutyName {
			t.Errorf("a read was recorded as %+v, want the duty's own actor", by)
		}
	}
}

// A FAILED READ RECORDS AND RETIRES NOTHING AND FORGETS NO MISS: the standing
// says the fault, the source is retried after the retry wait, and the entry
// the failed reads never listed is retired only by complete ones.
func TestAFailedReadRecordsAndRetiresNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	h.source.set(nil, errors.New("cloudflare: read the zones: 503"))
	h.tick(time.Hour)
	st := h.standing()
	if st.Completed || st.LastError == "" || !st.LastCompleteAt.Equal(t0) || h.log.recorded() != 4 {
		t.Fatalf("after a failed read: %+v, %d records", st, h.log.recorded())
	}
	h.tick(collect.RetryAfter)
	if h.source.reads != 3 {
		t.Errorf("a failed source was not retried after %s: %d reads", collect.RetryAfter, h.source.reads)
	}
	h.tick(2 * collect.RetryAfter)
	if len(h.standing().Missing) != 0 {
		t.Errorf("failed reads marked entries missing: %+v", h.standing().Missing)
	}
	h.source.set(nil, nil)
	h.tick(collect.RetryAfter)
	if h.log.recorded() != 4 || len(h.standing().Missing) != 1 {
		t.Fatalf("an empty complete read retired at once or missed nothing: %d records, %+v", h.log.recorded(),
			h.standing().Missing)
	}
	h.tick(25 * time.Hour)
	if h.log.recorded() != 8 || h.standing().Retired != 4 {
		t.Errorf("an empty complete read a day later retired %d entries over %d records",
			h.standing().Retired, h.log.recorded()-4)
	}
}

// A READ THE LOG REFUSES AS PURGED IS COUNTED, NEVER RETRIED AS A FAULT, and
// the rest of the read is recorded.
func TestAPurgedEntryIsCountedAndTheRestRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.log.purged[www] = true
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	st := h.standing()
	if !st.Completed || st.Purged != 1 || st.Recorded != 3 || h.log.recorded() != 3 {
		t.Errorf("a purged entry: %+v, %d records", st, h.log.recorded())
	}
}

// A NODE THAT DOES NOT HOLD THE DUTY READS NOTHING, and a source the company
// does not connect loses its standing.
func TestOnlyTheDutysHolderReadsAndADisconnectedSourceIsForgotten(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.held = false
	h.tick(0)
	if h.source.reads != 0 || h.log.recorded() != 0 {
		t.Fatal("a node without the duty read a source")
	}
	h.held = true
	h.tick(0)
	if h.source.reads != 1 {
		t.Fatal("the duty's holder did not read")
	}
	h.source.set(nil, integration.ErrNotConfigured)
	h.tick(time.Hour)
	if _, kept := mustLoad(t, h)[integration.KindCloudflare]; kept {
		t.Error("a source the company no longer connects keeps its standing")
	}
	if h.log.recorded() != 4 {
		t.Error("a disconnect touched the log")
	}
}

func mustLoad(t *testing.T, h *harness) map[integration.Kind]collect.Status {
	t.Helper()
	all, err := h.store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// THE STANDING ROUND-TRIPS THROUGH THE COORDINATION STORE, misses included,
// and one this build cannot decode is skipped rather than raised.
func TestAStandingRoundTripsThroughTheStore(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store, err := collect.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	want := collect.Status{Kind: integration.KindCloudflare, LastAttemptAt: t0, Completed: true, LastCompleteAt: t0,
		Digest: "ab", Entries: 3, Missing: []collect.Missing{{Since: t0, Entries: []string{"a", "b"}}}}
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if err := fleet.PutAtlasSourceStatus(t.Context(), "datadog", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[want.Kind].LastCompleteAt.Equal(t0) || got[want.Kind].Digest != "ab" ||
		!slices.Equal(got[want.Kind].Missing[0].Entries, []string{"a", "b"}) {
		t.Errorf("Load = %+v, want the saved standing alone", got)
	}
	if _, err := collect.NewCoordStore(nil); err == nil {
		t.Error("a nil backend was wrapped")
	}
}
