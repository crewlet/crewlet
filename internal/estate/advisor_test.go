package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/signedask/signedasktest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FACADES ASK EXACTLY THE ADVISOR LOG'S OPERATIONS, each method one: every
// read through AdvisorReads, every write through AdvisorWrites, nothing else.
//
// Mutation: point AdvisorReads.Run at opAdvisorLatestRun, or
// AdvisorWrites.Release at opAdvisorMove, and a facade's operations lack one.
func TestTheAdvisorFacadesAskExactlyTheAdvisorOperations(t *testing.T) {
	t.Parallel()
	r, asker := atlasRouter(t)
	for _, tc := range []struct {
		facade any
		want   func(*opSpec) bool
	}{
		{r.AdvisorReads(), func(s *opSpec) bool { return s.domain == advisorDomain && s.class == opRead }},
		{r.AdvisorWrites(), func(s *opSpec) bool { return s.domain == advisorDomain && s.class != opRead }},
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
			got[opened(t.Context(), sent[0].payload).Op] = true
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

// advisorDesk is a data node's advisor log as the cases reach it: every write
// records what crossed, and a key decided from version 7 or held answers the
// domain's refusals.
type advisorDesk struct {
	advisorDomainReads
	mu    sync.Mutex
	calls []string
}

func (d *advisorDesk) For(context.Context, string) (Backend, bool) {
	return Backend{Advisor: AdvisorHalf{Reader: d, Writer: d}}, true
}

func (d *advisorDesk) wrote(what string, c advisordomain.Call) (statelog.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, what+"|"+c.By.Name+"|"+string(c.By.Kind)+"|"+c.OpID)
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID}, nil
}

func (d *advisorDesk) Open(_ context.Context, c advisordomain.Call, o advisordomain.Opening,
	ifMatch uint64) (statelog.Result, error) {
	switch ifMatch {
	case 7:
		return statelog.Result{}, fmt.Errorf("%w: at 9, decided from 7", advisordomain.ErrStaleVersion)
	case 8:
		return statelog.Result{}, fmt.Errorf("%w: %s is held", advisordomain.ErrHeld, o.Key)
	}
	return d.wrote(fmt.Sprintf("open:%s:%s:%s:%d:%d:%s", o.Key, o.Episode, o.Severity, *o.ImpactMinutes,
		ifMatch, o.Body), c)
}

func (d *advisorDesk) Revise(_ context.Context, c advisordomain.Call, key string, r advisordomain.Revision,
	ifMatch uint64) (statelog.Result, error) {
	return d.wrote(fmt.Sprintf("revise:%s:%d:%d", key, r.Revision, ifMatch), c)
}

func (d *advisorDesk) Refresh(_ context.Context, c advisordomain.Call, key string, r advisordomain.Refresh,
	ifMatch uint64) (statelog.Result, error) {
	return d.wrote(fmt.Sprintf("refresh:%s:%d:%d", key, r.Revision, ifMatch), c)
}

func (d *advisorDesk) Move(_ context.Context, c advisordomain.Call, key string, m advisordomain.Move,
	ifMatch uint64) (statelog.Result, error) {
	return d.wrote(fmt.Sprintf("move:%s:%s:%s:%s:%s:%s:%d", key, m.Event, m.State, m.Reason,
		m.Hold.Until.Format(time.RFC3339), m.Hold.Severity, ifMatch), c)
}

func (d *advisorDesk) Explain(_ context.Context, c advisordomain.Call, key string,
	e advisordomain.Explanation) (statelog.Result, error) {
	return d.wrote("explain:"+key+":"+e.Text, c)
}

func (d *advisorDesk) Release(_ context.Context, c advisordomain.Call, key string,
	ifMatch uint64) (statelog.Result, error) {
	return d.wrote(fmt.Sprintf("release:%s:%d", key, ifMatch), c)
}

func (d *advisorDesk) RequestRun(_ context.Context, c advisordomain.Call,
	r advisordomain.RunRequest) (statelog.Result, error) {
	return d.wrote("request:"+r.Run+":"+r.By, c)
}

func (d *advisorDesk) RecordRun(_ context.Context, c advisordomain.Call,
	r advisordomain.RunRecord) (statelog.Result, error) {
	return d.wrote("record:"+r.Run+":"+string(r.Classes), c)
}

func (d *advisorDesk) Progress(_ context.Context, c advisordomain.Call,
	p advisordomain.RunProgress) (statelog.Result, error) {
	return d.wrote("progress:"+p.Run+":"+p.Text, c)
}

func (d *advisorDesk) WriteReport(_ context.Context, c advisordomain.Call,
	r advisordomain.Report) (statelog.Result, error) {
	return d.wrote("report:"+r.Week+":"+r.From.Format(time.RFC3339), c)
}

func (d *advisorDesk) RaiseAlarm(_ context.Context, c advisordomain.Call,
	a advisordomain.AlarmRaise) (statelog.Result, error) {
	return d.wrote("raise:"+a.Node+":"+a.Kind+":"+a.Instance, c)
}

func (d *advisorDesk) ClearAlarm(_ context.Context, c advisordomain.Call, node, kind, instance string,
	a advisordomain.AlarmClear) (statelog.Result, error) {
	return d.wrote("clear:"+node+":"+kind+":"+instance+":"+a.ClearedAt.Format(time.RFC3339), c)
}

func (d *advisorDesk) Expire(_ context.Context, c advisordomain.Call, day time.Time,
	e advisordomain.Expiry) (statelog.Result, error) {
	return d.wrote("expire:"+advisordomain.Day(day)+":"+e.ClosedBefore.Format(time.RFC3339), c)
}

// advisorDomainReads answers every read with one row of each shape.
type advisorDomainReads struct{}

// advisorRow is the one finding the desk holds.
var advisorRow = advisordomain.FindingRow{ID: "0b6e3c52-7d1a-4f8e-9c2b-5a4d3e2f1a0b",
	Key: "seats.idle_with_queue:seat:sre", Severity: advisordomain.SeverityCosts,
	State: advisordomain.StateApplied, ImpactTokens: new(int64(120_000)),
	Body:     json.RawMessage(`{"title":"An idle seat"}`),
	OpenedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), KeyVersion: 42}

func (advisorDomainReads) Findings(context.Context, advisordomain.Filter,
	statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return []advisordomain.FindingRow{advisorRow}, "next", nil
}
func (advisorDomainReads) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return []advisordomain.FindingRow{advisorRow}, nil
}
func (advisorDomainReads) Finding(_ context.Context, id string,
	_ statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	if id != advisorRow.ID {
		return advisordomain.FindingRow{}, nil, fmt.Errorf("%w: %s", advisordomain.ErrNotFound, id)
	}
	return advisorRow, []advisordomain.HistoryRow{{Event: advisordomain.EventApplied, ByKind: iam.ActorHuman,
		ByName: "ana", Moved: json.RawMessage(`[]`)}}, nil
}
func (advisorDomainReads) Episodes(context.Context, string, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (advisorDomainReads) Key(context.Context, string, statelog.Freshness) (advisordomain.KeyRow, bool, error) {
	return advisordomain.KeyRow{Version: 42, HoldForGood: true, Ineffective: []string{"fix"}}, true, nil
}
func (advisorDomainReads) Counts(context.Context, statelog.Freshness) (map[advisordomain.Severity]int, error) {
	return map[advisordomain.Severity]int{advisordomain.SeverityCosts: 1}, nil
}
func (advisorDomainReads) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "", nil, false, nil
}
func (advisorDomainReads) RunProgress(context.Context, string, statelog.Freshness) ([]advisordomain.ProgressLine, error) {
	return nil, nil
}
func (advisorDomainReads) Checkpoint(context.Context, string, statelog.Freshness) (json.RawMessage, bool, error) {
	return json.RawMessage(`{"agenda":[]}`), true, nil
}
func (advisorDomainReads) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{}, false, nil
}
func (advisorDomainReads) Run(context.Context, string, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{}, false, nil
}
func (advisorDomainReads) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (advisorDomainReads) Unowned(context.Context, statelog.Freshness) ([]advisordomain.UnownedRun, error) {
	return nil, nil
}
func (advisorDomainReads) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return nil, nil
}
func (advisorDomainReads) Reports(context.Context, string, int,
	statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	return nil, "", nil
}
func (advisorDomainReads) Report(context.Context, string, statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	return advisordomain.ReportRow{}, false, nil
}
func (advisorDomainReads) Tally(context.Context, time.Time, time.Time, statelog.Freshness) (advisordomain.Tally, error) {
	return advisordomain.Tally{}, nil
}
func (advisorDomainReads) AlarmEpisodes(context.Context, time.Time,
	statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, error) {
	return nil, nil
}
func (advisorDomainReads) EpisodesPage(context.Context, string, string,
	statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (advisorDomainReads) ClassRunsPage(context.Context, string,
	statelog.Freshness) (map[string]time.Time, string, error) {
	return nil, "", nil
}
func (advisorDomainReads) AlarmEpisodesPage(context.Context, time.Time, string,
	statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, string, error) {
	return nil, "", nil
}

// THE ADVISOR LOG FROM A NODE HOLDING NO DATA IS A DATA NODE'S: an opening, a
// dismissal's hold and an alarm's series cross whole with the version they
// were decided from, the author and the operation; every refusal keeps its
// identity; and a finding read back keeps its optional counts and its body.
//
// Mutation: drop IfMatch from advisorOpenArgs, or Instance from
// advisorClearArgs, and the case goes red.
func TestTheAdvisorLogFromANodeWithoutDataCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	desk := &advisorDesk{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{Taken: signedasktest.NewLedger(),
		Self: "data-d", Local: desk, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx := t.Context()
	w := f.client.AdvisorWrites()
	engineCall := advisordomain.Call{By: iam.Actor{Name: "engine", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "advisor")}
	o := advisordomain.Opening{Key: "seats.idle_with_queue:seat:sre", Episode: advisorRow.ID,
		Severity: advisordomain.SeverityCosts, ImpactMinutes: new(int64(540)), Body: advisorRow.Body}
	if res, err := w.Open(ctx, engineCall, o, 4); err != nil || res.Outcome != statelog.OutcomeApplied ||
		res.OpID != engineCall.OpID {
		t.Errorf("open = (%+v, %v)", res, err)
	}
	if _, err := w.Open(ctx, engineCall, o, 7); !errors.Is(err, advisordomain.ErrStaleVersion) {
		t.Errorf("an opening decided from a moved key crossed as %v", err)
	}
	if _, err := w.Open(ctx, engineCall, o, 8); !errors.Is(err, advisordomain.ErrHeld) {
		t.Errorf("an opening of a held key crossed as %v", err)
	}
	until := time.Date(2027, 2, 16, 9, 0, 0, 0, time.UTC)
	ana := advisordomain.Call{By: iam.Actor{Name: "ana", Kind: iam.ActorHuman, OperatorID: "session"},
		OpID: statelog.NewOpID(time.Now(), "advisor")}
	if _, err := w.Move(ctx, ana, o.Key, advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventClosed,
		State: advisordomain.StateDismissed, Reason: advisordomain.ReasonDismissedWontFix,
		Hold: &advisordomain.Hold{Until: until, Severity: advisordomain.SeverityCosts}}, 5); err != nil {
		t.Errorf("move: %v", err)
	}
	if _, err := w.ClearAlarm(ctx, engineCall, "data-1", "pool_starved", "replicated",
		advisordomain.AlarmClear{ClearedAt: until}); err != nil {
		t.Errorf("clear: %v", err)
	}
	desk.mu.Lock()
	want := []string{
		"open:seats.idle_with_queue:seat:sre:" + advisorRow.ID + ":costs:540:4:" + string(advisorRow.Body) +
			"|engine|system|" + engineCall.OpID,
		"move:seats.idle_with_queue:seat:sre:closed:dismissed:dismissed_wont_fix:2027-02-16T09:00:00Z:costs:5" +
			"|ana|human|" + ana.OpID,
		"clear:data-1:pool_starved:replicated:2027-02-16T09:00:00Z|engine|system|" + engineCall.OpID,
	}
	if !slices.Equal(desk.calls, want) {
		t.Errorf("the data node ran %v, want %v", desk.calls, want)
	}
	desk.mu.Unlock()

	r := f.client.AdvisorReads()
	page, next, err := r.Findings(ctx, advisordomain.Filter{Open: true}, statelog.Freshness{})
	if err != nil || next != "next" || len(page) != 1 || page[0].ImpactTokens == nil ||
		*page[0].ImpactTokens != 120_000 || page[0].ImpactMinutes != nil ||
		string(page[0].Body) != string(advisorRow.Body) || page[0].KeyVersion != 42 {
		t.Errorf("a page of findings crossed as %+v next %q (%v)", page, next, err)
	}
	if _, _, err := r.Finding(ctx, "missing", statelog.Freshness{}); !errors.Is(err, advisordomain.ErrNotFound) {
		t.Errorf("a missing finding crossed as %v", err)
	}
	counts, err := r.Counts(ctx, statelog.Freshness{})
	if err != nil || counts[advisordomain.SeverityCosts] != 1 {
		t.Errorf("the counts crossed as %v (%v)", counts, err)
	}
}
