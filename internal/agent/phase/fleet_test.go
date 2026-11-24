package phase_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

var fleetNow = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

// fleetModels is a company with entries a, b, c and default, the fleet
// opus of a and b, and default-fleet with members.
func fleetModels(defaultMembers ...string) phase.Models {
	return phase.Models{
		Order: []string{"a", "b", "c", "default"},
		Fleets: map[string]phase.Fleet{
			"opus":             {Strategy: fleet.Balanced, Threshold: 5, Members: []string{"a", "b"}},
			"empty":            {Strategy: fleet.Sequential, Threshold: 5},
			phase.DefaultFleet: {Strategy: fleet.Balanced, Threshold: 5, Members: defaultMembers},
		},
	}
}

// TestResolveNamesTheFleetAPhaseRunsOn holds resolution with fleets: a field
// naming a fleet is that field's level and names the fleet, a satellite that
// names nothing follows its seat's fleet, a seat naming nothing runs on
// default-fleet only once it has a member, and a fleet inside a list or one
// the company lacks names nothing.
func TestResolveNamesTheFleetAPhaseRunsOn(t *testing.T) {
	t.Parallel()
	onFleet := &org.Role{Name: "fleet seat", LLM: org.ProviderKeys{"fleet:opus"},
		LLMJudge: org.ProviderKeys{"c"}}
	for _, tc := range []struct {
		name   string
		role   *org.Role
		ph     phase.Phase
		models phase.Models
		want   phase.Resolution
	}{
		{"the executor on a fleet", onFleet, phase.Execute, fleetModels(),
			phase.Resolution{Keys: []string{"a", "b"}, Source: phase.SourceSeat, Fleet: "opus"}},
		{"a satellite naming nothing follows its seat's fleet", onFleet, phase.Review, fleetModels(),
			phase.Resolution{Keys: []string{"a", "b"}, Source: phase.SourceExecutor, Fleet: "opus"}},
		{"a satellite pinned beside a fleet stays pinned", onFleet, phase.Judge, fleetModels(),
			phase.Resolution{Keys: []string{"c"}, Source: phase.SourceSeat}},
		{"a satellite's own fleet", &org.Role{Name: "s", LLM: org.ProviderKeys{"c"},
			LLMReview: org.ProviderKeys{"fleet:opus"}}, phase.Review, fleetModels(),
			phase.Resolution{Keys: []string{"a", "b"}, Source: phase.SourceSeat, Fleet: "opus"}},
		{"a fleet with no member resolves to it, with no keys",
			&org.Role{Name: "s", LLM: org.ProviderKeys{"fleet:empty"}}, phase.Execute, fleetModels(),
			phase.Resolution{Source: phase.SourceSeat, Fleet: "empty"}},
		{"unset, while default-fleet is empty, keeps the default entry",
			&org.Role{Name: "none"}, phase.Execute, fleetModels(),
			phase.Resolution{Keys: []string{"default"}, Source: phase.SourceDefaultEntry}},
		{"unset, once default-fleet has a member, runs on it",
			&org.Role{Name: "none"}, phase.Review, fleetModels("c"),
			phase.Resolution{Keys: []string{"c"}, Source: phase.SourceFleet, Fleet: phase.DefaultFleet}},
		{"a fleet inside a list names nothing",
			&org.Role{Name: "s", LLM: org.ProviderKeys{"fleet:opus", "c"}}, phase.Execute, fleetModels(),
			phase.Resolution{Keys: []string{"c"}, Source: phase.SourceSeat}},
		{"a fleet the company lacks falls to the company's fallback",
			&org.Role{Name: "s", LLM: org.ProviderKeys{"fleet:gone"}}, phase.Execute, fleetModels(),
			phase.Resolution{Keys: []string{"default"}, Source: phase.SourceDefaultEntry}},
		{"a pinned seat routes as before",
			&org.Role{Name: "s", LLM: org.ProviderKeys{"b", "a"}}, phase.Execute, fleetModels("c"),
			phase.Resolution{Keys: []string{"b", "a"}, Source: phase.SourceSeat}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := phase.Resolve(tc.role, tc.ph, tc.models)
			if !slices.Equal(got.Keys, tc.want.Keys) || got.Source != tc.want.Source || got.Fleet != tc.want.Fleet {
				t.Errorf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// balanced is a fleet registry over entries a, b, c and default whose
// members read as signals says, on a ledger at fleetNow.
func balanced(t *testing.T, signals map[string]fleet.Signals, defaultMembers ...string) (*phase.Registry, *fleet.Ledger) {
	t.Helper()
	models := fleetModels(defaultMembers...)
	entries := make([]phase.Entry, 0, len(models.Order))
	for _, k := range models.Order {
		entries = append(entries, phase.Entry{Key: k, Provider: stub{model: k}})
	}
	ledger := fleet.NewLedger(func() time.Time { return fleetNow })
	r, err := phase.NewFleetRegistry(entries, models.Fleets, phase.Balance{Ledger: ledger,
		Signals: func(key string) fleet.Signals { return signals[key] }})
	if err != nil {
		t.Fatalf("NewFleetRegistry: %v", err)
	}
	return r, ledger
}

func planAt(used float64) fleet.Signals {
	return fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{{
		LimitReading: llm.LimitReading{Window: llm.LimitSession, State: llm.LimitOK, Used: &used},
		Seen:         fleetNow.Add(-time.Minute)}}}
}

// TestFleetWalkAsksTheBalancerAtEachPieceOfWork holds the registry's fleet
// chain: the balancer's walk with a pick per member, nothing recorded by a
// read (Chain), the home kept by the work that took it (Walk), and an empty
// fleet refused so its seat waits.
func TestFleetWalkAsksTheBalancerAtEachPieceOfWork(t *testing.T) {
	t.Parallel()
	signals := map[string]fleet.Signals{"a": planAt(0.40), "b": planAt(0.20)}
	r, ledger := balanced(t, signals)
	seat := &org.Role{Name: "Engineer", LLM: org.ProviderKeys{"fleet:opus"}}

	if got := chainKeys(t)(r.Chain(seat, phase.Execute)); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("Chain = %v, want b (least used) then a", got)
	}
	job := fleet.Job{Fleet: "opus", Subject: seat.Handle(), Job: "execute"}
	if home := ledger.Home(job); home != "" {
		t.Fatalf("a read recorded home %q; only the work that takes a member does", home)
	}
	w, err := r.Walk(seat, phase.Execute)
	if err != nil || len(w.Picks) != len(w.Members) {
		t.Fatalf("Walk = %v picks for %v members, %v", len(w.Picks), len(w.Members), err)
	}
	if p := w.Picks[0]; p.Key != "b" || p.Reason != fleet.ReasonLowestShare || p.Fleet != "opus" ||
		p.Share() == nil || *p.Share() != 20 {
		t.Fatalf("first pick = %+v, want b for the lowest share, at 20", p)
	}
	w.Picks[0].Take()

	// b now leads a by 4 points: within the threshold, the work stays home.
	signals["b"] = planAt(0.44)
	again, _ := r.Walk(seat, phase.Execute)
	if p := again.Picks[0]; p.Key != "b" || p.Reason != fleet.ReasonWithinThreshold || p.Moved() {
		t.Errorf("follow-up pick = %s (%s), want b kept within the threshold", p.Key, p.Reason)
	}

	empty := &org.Role{Name: "Writer", LLM: org.ProviderKeys{"fleet:empty"}}
	var emptyErr *phase.EmptyFleetError
	if _, err := r.Chain(empty, phase.Execute); !errors.As(err, &emptyErr) || emptyErr.Fleet != "empty" {
		t.Errorf("Chain on an empty fleet = %v, want an EmptyFleetError naming it", err)
	}
}

// TestUnsetSeatsJoinTheDefaultFleetOnceItHasAMember holds Q1 through the
// registry: a seat naming nothing keeps the default entry while default-fleet
// is empty, and runs on its members once it has one.
func TestUnsetSeatsJoinTheDefaultFleetOnceItHasAMember(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Unset"}
	empty, _ := balanced(t, nil)
	if got := chainKeys(t)(empty.Chain(seat, phase.Execute)); !slices.Equal(got, []string{"default"}) {
		t.Errorf("Chain with an empty default-fleet = %v, want the default entry", got)
	}
	joined, _ := balanced(t, map[string]fleet.Signals{"c": planAt(0.1)}, "c")
	w, err := joined.Walk(seat, phase.Execute)
	if err != nil || len(w.Picks) != 1 || w.Picks[0].Fleet != phase.DefaultFleet || w.Members[0].Key != "c" {
		t.Errorf("Walk once default-fleet has c = %+v, %v; want c picked by default-fleet", w.Members, err)
	}
}

// TestFleetRegistryNeedsItsBalancer holds the constructor: a fleet registry
// without a ledger or a reading of its members would pick blind.
func TestFleetRegistryNeedsItsBalancer(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "a", Provider: stub{model: "a"}}}
	if _, err := phase.NewFleetRegistry(entries, nil, phase.Balance{}); err == nil {
		t.Error("NewFleetRegistry with no balancer = nil error")
	}
}
