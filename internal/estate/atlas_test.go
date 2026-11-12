package estate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// atlasRouter is a router that records what it asks of a data node that
// serves nothing.
func atlasRouter(t *testing.T) (*Router, *recordingAsker) {
	t.Helper()
	asker := &recordingAsker{answer: reply{Node: "data-a", Unserved: unservedNoBackend,
		Detail: "data-a runs no native backend for it"}}
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: asker,
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Git: &fakePlacement{}, Session: NewSession(),
		Timing: Production(), Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	return r, asker
}

// THE FACADES ASK EXACTLY THE ATLAS LOG'S OPERATIONS, each method one: every
// atlas read through the reader facade and every atlas write through the writer
// facade, and nothing else through either.
//
// Mutation: point AtlasReads.Log at opAtlasEntry, or AtlasWrites.PurgeEntry
// at opAtlasPurgeValue, and a facade's operations lack one.
func TestTheAtlasFacadesAskExactlyTheAtlasOperations(t *testing.T) {
	t.Parallel()
	r, asker := atlasRouter(t)
	for _, tc := range []struct {
		facade any
		want   func(*opSpec) bool
	}{
		{r.AtlasReads(), func(s *opSpec) bool { return s.domain == atlasDomain && s.class == opRead }},
		{r.AtlasWrites(), func(s *opSpec) bool { return s.domain == atlasDomain && s.class != opRead }},
	} {
		v := reflect.ValueOf(tc.facade)
		got := map[string]bool{}
		for i := range v.NumMethod() {
			method, m := v.Type().Method(i).Name, v.Method(i)
			in := make([]reflect.Value, m.Type().NumIn())
			in[0] = reflect.ValueOf(t.Context())
			for j := 1; j < len(in); j++ {
				in[j] = reflect.Zero(m.Type().In(j))
			}
			before := len(asker.requests())
			m.Call(in)
			sent := asker.requests()[before:]
			if len(sent) != 1 {
				t.Errorf("%s.%s asked %d operations, want one", v.Type().Name(), method, len(sent))
				continue
			}
			got[opened(sent[0].payload).Op] = true
		}
		want := map[string]bool{}
		for name, spec := range registry {
			if tc.want(spec) {
				want[name] = true
			}
		}
		if len(want) == 0 || !maps.Equal(got, want) {
			t.Errorf("%s asks %v, want exactly %v", v.Type().Name(),
				slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	}
}

// atlasDesk is a data node's atlas log as the cases reach it: every write
// records its call and answers at a position, and an entry whose version moved
// answers each refusal the domain has.
type atlasDesk struct {
	mu    sync.Mutex
	calls []string
	asked []string
}

// atlasLineAt is when the desk's one log line was recorded.
var atlasLineAt = time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)

func (d *atlasDesk) For(context.Context, string) (Backend, bool) {
	return Backend{Atlas: AtlasHalf{Reader: d, Writer: d}}, true
}

func (d *atlasDesk) wrote(what string, c atlasdomain.Call) (statelog.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, what+"|"+c.By.Name+"|"+string(c.By.Kind)+"|"+c.OpID)
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID}, nil
}

func (d *atlasDesk) Record(_ context.Context, c atlasdomain.Call, claim atlas.Claim,
	ifMatch uint64) (statelog.Result, error) {
	switch ifMatch {
	case 7:
		return statelog.Result{}, fmt.Errorf("%w: at 9, decided from 7", atlasdomain.ErrStaleVersion)
	case 8:
		return statelog.Result{}, fmt.Errorf("%w: %s was purged", atlasdomain.ErrPurged, claim.Subject)
	}
	return d.wrote(fmt.Sprintf("record:%s:%s:%s=%s:%d", claim.Subject, claim.Facet.Kind, claim.Facet.Fact,
		claim.Value.Text, ifMatch), c)
}

func (d *atlasDesk) Observe(_ context.Context, c atlasdomain.Call, claim atlas.Claim,
	seen atlasdomain.Observation) (statelog.Result, error) {
	if seen.Turn == "" {
		return statelog.Result{}, fmt.Errorf("%w: no turn", atlasdomain.ErrInvalid)
	}
	return d.wrote(fmt.Sprintf("observe:%s:%s:%s=%s:%s:%s", claim.Subject, claim.Facet.Kind, claim.Facet.Fact,
		claim.Value.Text, seen.Turn, seen.Evidence), c)
}

func (d *atlasDesk) PurgeEntry(_ context.Context, c atlasdomain.Call, ref atlas.Ref) (statelog.Result, error) {
	return d.wrote("purge_entry:"+ref.String(), c)
}

func (d *atlasDesk) PurgeDigest(_ context.Context, c atlasdomain.Call, digest string) (statelog.Result, error) {
	if !atlasdomain.IsDigest(digest) {
		return statelog.Result{}, fmt.Errorf("%w: not a digest", atlasdomain.ErrInvalid)
	}
	return d.wrote("purge_value:"+digest, c)
}

func (d *atlasDesk) Live(context.Context, statelog.Freshness) (atlasdomain.Live, error) {
	return atlasdomain.Live{Complete: true}, nil
}

func (d *atlasDesk) Entry(context.Context, string, statelog.Freshness) (atlasdomain.EntryRow, bool, error) {
	return atlasdomain.EntryRow{}, false, nil
}

func (d *atlasDesk) Log(_ context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, fmt.Sprintf("log:%v:%s:%d:%s", q.Entries, q.Cursor, q.Limit, q.Fresh.Level))
	return atlasdomain.LogPage{Lines: []atlasdomain.LogLine{{ID: "line", Entry: "entry", At: atlasLineAt,
		Withdrawn: true}}, Next: "below", Complete: true}, nil
}

func (d *atlasDesk) Activity(_ context.Context, since time.Time,
	fresh statelog.Freshness) (atlasdomain.Activity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, fmt.Sprintf("activity:%s:%s", since.Format(time.RFC3339), fresh.Level))
	return atlasdomain.Activity{Days: []atlasdomain.ActivityDay{{Entry: "entry", Target: "other", Day: since,
		Lines: 3}}, Complete: true}, nil
}

func (d *atlasDesk) Purges(context.Context, statelog.Freshness) ([]atlasdomain.PurgeRow, error) {
	return nil, nil
}

// A PERSON'S ATLAS WRITE FROM A NODE HOLDING NO DATA IS A DATA NODE'S WRITE:
// the claim, the version it was decided from, the author and the operation
// cross whole, a purge of a value crosses as its digest alone, and every
// refusal keeps its identity, so the surface on the asking node answers it as
// the writer decided it.
//
// Mutation: drop IfMatch from atlasRecordArgs, or Writer from the half the
// serving node answers with, and the case goes red.
func TestAPersonsAtlasWriteFromANodeWithoutDataCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	desk := &atlasDesk{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: desk, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx := t.Context()
	w := f.client.AtlasWrites()
	ana := atlasdomain.Call{By: iam.Actor{Name: "ana", Kind: iam.ActorHuman, OperatorID: "session"},
		OpID: statelog.NewOpID(time.Now(), "atlas")}
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	service := atlas.MustRef(atlas.KindService, "checkout-api")
	claim, err := atlas.Says(service, atlas.FactEnvironment, "production", atlas.PersonSource("ana"), at)
	if err != nil {
		t.Fatal(err)
	}

	if res, err := w.Record(ctx, ana, claim, 4); err != nil || res.Outcome != statelog.OutcomeApplied ||
		res.OpID != ana.OpID {
		t.Errorf("record = (%+v, %v)", res, err)
	}
	if _, err := w.Record(ctx, ana, claim, 7); !errors.Is(err, atlasdomain.ErrStaleVersion) {
		t.Errorf("an edit decided from a moved version crossed as %v", err)
	}
	if _, err := w.Record(ctx, ana, claim, 8); !errors.Is(err, atlasdomain.ErrPurged) {
		t.Errorf("a claim on a purged entry crossed as %v", err)
	}
	if _, err := w.PurgeEntry(ctx, ana, service); err != nil {
		t.Errorf("purge entry: %v", err)
	}
	const value = "orders-db.internal.acme.example"
	if _, err := w.PurgeDigest(ctx, ana, atlasdomain.Digest(value)); err != nil {
		t.Errorf("purge value: %v", err)
	}
	if _, err := w.PurgeDigest(ctx, ana, value); !errors.Is(err, atlasdomain.ErrInvalid) {
		t.Errorf("a purge naming no digest crossed as %v", err)
	}

	desk.mu.Lock()
	defer desk.mu.Unlock()
	by := "|ana|human|" + ana.OpID
	want := []string{
		"record:service:checkout-api:fact:environment=production:4" + by,
		"purge_entry:service:checkout-api" + by,
		"purge_value:" + atlasdomain.Digest(value) + by,
	}
	if !slices.Equal(desk.calls, want) {
		t.Errorf("the data node ran %v, want %v", desk.calls, want)
	}
}

// AN AGENT'S ATLAS RECORD FROM A NODE HOLDING NO DATA IS A DATA NODE'S WRITE
// (ADR-0132): the claim, the turn it was made in, the evidence the asking node
// derived, the seat and the operation cross whole, and a refusal keeps its
// identity. A seat is held wherever its lease lands, a leaf without data
// included, and only that node holds what its turn's calls answered.
//
// Mutation: drop Seen from atlasObserveArgs and the record arrives naming no
// turn, which the data node refuses.
func TestAnAgentsAtlasRecordFromANodeWithoutDataCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	desk := &atlasDesk{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: desk, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	w := f.client.AtlasWrites()
	sre := atlasdomain.Call{By: iam.Actor{Name: "sre", Kind: iam.ActorAgent},
		OpID: statelog.NewOpID(time.Now(), "atlas")}
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	db := atlas.MustRef(atlas.KindDatastore, "web-1/orders-db")
	claim, err := atlas.Says(db, atlas.FactEngine, "postgres", atlas.AgentSource("sre", "run-1", atlas.EvidenceNone), at)
	if err != nil {
		t.Fatal(err)
	}
	seen := atlasdomain.Observation{Turn: "run-1", Evidence: atlas.EvidenceCall}
	if res, err := w.Observe(t.Context(), sre, claim, seen); err != nil || res.Outcome != statelog.OutcomeApplied ||
		res.OpID != sre.OpID {
		t.Errorf("observe = (%+v, %v)", res, err)
	}
	if _, err := w.Observe(t.Context(), sre, claim, atlasdomain.Observation{}); !errors.Is(err, atlasdomain.ErrInvalid) {
		t.Errorf("a record naming no turn crossed as %v", err)
	}
	desk.mu.Lock()
	defer desk.mu.Unlock()
	want := []string{"observe:datastore:web-1/orders-db:fact:engine=postgres:run-1:call|sre|agent|" + sre.OpID}
	if !slices.Equal(desk.calls, want) {
		t.Errorf("the data node ran %v, want %v", desk.calls, want)
	}
}

// THE ATLAS LOG READ FROM A NODE HOLDING NO DATA IS A DATA NODE'S READ: the
// page asked for (which entries, from which line, how many, how fresh) and the
// window counted cross whole, and the lines, the next page and the counts
// cross back as the data node answered them.
//
// Mutation: drop Cursor from the query the operation carries, or Reader from
// the half the serving node answers with, and the case goes red.
func TestTheAtlasLogReadFromANodeWithoutDataCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	desk := &atlasDesk{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: desk, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx := t.Context()
	reads := f.client.AtlasReads()
	stale := statelog.Freshness{Level: statelog.ReadStale}

	page, err := reads.Log(ctx, atlasdomain.LogQuery{Entries: []string{"a", "b"}, Cursor: "from", Limit: 25,
		Fresh: stale})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Next != "below" || !page.Complete {
		t.Fatalf("the page crossed back as %+v", page)
	}
	if l := page.Lines[0]; l.ID != "line" || l.Entry != "entry" || !l.At.Equal(atlasLineAt) || !l.Withdrawn {
		t.Errorf("the line crossed back as %+v", l)
	}
	since := atlasLineAt.Add(-14 * 24 * time.Hour)
	activity, err := reads.Activity(ctx, since, stale)
	if err != nil {
		t.Fatal(err)
	}
	if want := (atlasdomain.ActivityDay{Entry: "entry", Target: "other", Day: since, Lines: 3}); !activity.Complete ||
		len(activity.Days) != 1 || activity.Days[0] != want {
		t.Errorf("the counts crossed back as %+v", activity)
	}

	desk.mu.Lock()
	defer desk.mu.Unlock()
	want := []string{"log:[a b]:from:25:stale", "activity:" + since.Format(time.RFC3339) + ":stale"}
	if !slices.Equal(desk.asked, want) {
		t.Errorf("the data node was asked %v, want %v", desk.asked, want)
	}
}
