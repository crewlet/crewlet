package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// announced is every record the pause tools published.
type announced struct {
	mu  sync.Mutex
	evs []*events.Event
}

func (a *announced) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return nil
}

func (a *announced) types() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.evs))
	for _, ev := range a.evs {
		out = append(out, ev.Type)
	}
	return out
}

// everyNode answers the all-nodes gate, and the seat gate never.
type everyNode struct {
	has bool
	err error
}

func (f everyNode) SeatFeature(context.Context, string, coord.Feature) (bool, error) {
	return false, errors.New("the pause tools ask every node, never one")
}

func (f everyNode) AllLiveHave(_ context.Context, feature coord.Feature) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.has && feature == coord.FeatureSeatPause, nil
}

type pauseRig struct {
	store coord.SeatPauses
	pub   *announced
	tools map[string]tools.Callable
}

func newPauseRig(t *testing.T, store builtin.SeatPauseStore, fleet builtin.Fleet, person string) *pauseRig {
	t.Helper()
	o := organization(t)
	pub := &announced{}
	rig := &pauseRig{pub: pub, tools: map[string]tools.Callable{}}
	if s, ok := store.(coord.SeatPauses); ok {
		rig.store = s
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Fleet: fleet,
		Pauses: builtin.SeatPauseDeps{
			Pauses: store, Announce: pub,
			Org: func() *org.Organization { return o },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: person + "-token", Kind: tracker.AuthorOperator,
					OperatorID: person + "-token", Seat: person}, nil
			},
			Now: func() time.Time { return time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC) },
		},
	}) {
		rig.tools[tool.Name()] = tool
	}
	for _, name := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
		if rig.tools[name] == nil {
			t.Fatalf("the operator catalogue serves no %s with a pause record wired", name)
		}
	}
	return rig
}

func (r *pauseRig) call(t *testing.T, name string, args map[string]any) (tools.Result, map[string]any) {
	t.Helper()
	res, err := r.tools[name].Call(context.Background(), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("%s answered %q, not JSON: %v", name, res.Output, err)
		}
	}
	return res, out
}

// A PAUSE IS ONE CHANGE, ANNOUNCED ONCE, AND A SECOND PAUSE IS NONE.
//
// The record is compare-and-set, so the caller whose write won is the one that
// announces — which is what keeps `seat_paused` once per change however many
// people press the button. A second pause of a paused seat answers `applied`
// (the seat is in the state asked for) and changes and announces nothing.
func TestAPauseIsOneChangeAndASecondPauseIsNone(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, "jane")

	_, got := rig.call(t, builtin.PauseSeatTool, map[string]any{
		"handle": "agent-cto", "reason": "looping on one ticket",
	})
	if got["outcome"] != "applied" || got["changed"] != true || got["paused_by_seat"] != "jane" {
		t.Fatalf("first pause = %v, want an applied change by jane", got)
	}
	p, found, err := rig.store.SeatPause(context.Background(), "agent-cto")
	if err != nil || !found || p.By != "jane-token" || p.Seat != "jane" ||
		p.Reason != "looping on one ticket" || p.StopRunning {
		t.Fatalf("record = %+v (found %v, %v), want jane's pause without a stop", p, found, err)
	}

	_, again := rig.call(t, builtin.PauseSeatTool, map[string]any{"handle": "@agent-cto"})
	if again["outcome"] != "applied" || again["changed"] != false {
		t.Errorf("second pause = %v, want an applied no-op", again)
	}
	if got := rig.pub.types(); len(got) != 1 || got[0] != "seat_paused" {
		t.Errorf("announced %v, want exactly one seat_paused", got)
	}
	ev := rig.pub.evs[0]
	paused, ok := events.DataAs[*types.SeatPaused](ev)
	if !ok || paused.AgentHandle != "agent-cto" || paused.RoleName != "Agent CTO" ||
		paused.PausedBySeat != "jane" || paused.Agent == "" {
		t.Errorf("seat_paused = %+v, want the seat named as its events name it", paused)
	}
}

// ASKING TO STOP A PAUSED SEAT'S TURN IS A REAL CHANGE: the pause in place is
// amended under the version read, names who asked for the stop, and says so.
func TestAPauseAddsAStopToThePauseInPlace(t *testing.T) {
	t.Parallel()
	store := coordmem.NewFleet()
	rig := newPauseRig(t, store, everyNode{has: true}, "omar")
	if _, _, err := store.CreateSeatPause(context.Background(), coord.SeatPause{
		Handle: "agent-cto", By: "jane-token", Seat: "jane", Reason: "looping", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, got := rig.call(t, builtin.PauseSeatTool, map[string]any{
		"handle": "agent-cto", "stop_running": true,
	})
	if got["changed"] != true || got["stop_running"] != true || got["paused_by_seat"] != "omar" {
		t.Fatalf("pause with a stop = %v, want the stop added by omar", got)
	}
	p, _, _ := store.SeatPause(context.Background(), "agent-cto")
	if !p.StopRunning || p.Seat != "omar" || p.Reason != "looping" {
		t.Errorf("record = %+v, want the stop added, omar named and the reason kept", p)
	}
}

// A RESUME LIFTS THE PAUSE AND SAYS WHOSE IT WAS; resuming a free seat is a
// no-op, announced by nobody.
func TestAResumeLiftsThePauseAndAnnouncesOnce(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, "jane")
	rig.call(t, builtin.PauseSeatTool, map[string]any{"handle": "agent-cto"})
	_, got := rig.call(t, builtin.ResumeSeatTool, map[string]any{"handle": "agent-cto"})
	if got["outcome"] != "applied" || got["changed"] != true {
		t.Fatalf("resume = %v, want an applied change", got)
	}
	if _, found, _ := rig.store.SeatPause(context.Background(), "agent-cto"); found {
		t.Fatal("the pause is still recorded after a resume")
	}
	_, again := rig.call(t, builtin.ResumeSeatTool, map[string]any{"handle": "agent-cto"})
	if again["changed"] != false {
		t.Errorf("second resume = %v, want a no-op", again)
	}
	if got := rig.pub.types(); len(got) != 2 || got[1] != "seat_resumed" {
		t.Fatalf("announced %v, want one seat_paused then one seat_resumed", got)
	}
	resumed, _ := events.DataAs[*types.SeatResumed](rig.pub.evs[1])
	if resumed.PausedBy != "jane-token" || resumed.ResumedBySeat != "jane" || resumed.PausedAt.IsZero() {
		t.Errorf("seat_resumed = %+v, want whose pause it lifted and who lifted it", resumed)
	}
}

// A FLEET THAT CANNOT CARRY A PAUSE REFUSES IT, AND WRITES NOTHING.
//
// Any live node may be the next to hold the seat, and one on an older build
// would run its mail as if it were not paused. `peer_upgrading` when a node
// definitively lacks the build, `unavailable` when the fleet could not be read.
func TestAPauseRefusesAFleetThatCannotCarryIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fleet builtin.Fleet
		want  tools.Refusal
	}{
		"a node on an older build": {everyNode{has: false}, tools.RefusalPeerUpgrading},
		"an unreadable fleet":      {everyNode{err: coord.ErrUnavailable}, tools.RefusalUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newPauseRig(t, coordmem.NewFleet(), tc.fleet, "jane")
			for _, tool := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
				res, _ := rig.call(t, tool, map[string]any{"handle": "agent-cto"})
				if !res.Failed || res.Refusal != tc.want {
					t.Errorf("%s = %+v, want refused %s", tool, res, tc.want)
				}
			}
			if _, found, _ := rig.store.SeatPause(context.Background(), "agent-cto"); found {
				t.Error("a refused pause was recorded anyway")
			}
		})
	}
}

// ONLY AN AGENT SEAT IS PAUSED, and a reason is one line.
func TestAPauseNamesAnAgentSeatAndALine(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, "jane")
	for name, tc := range map[string]struct {
		args map[string]any
		want tools.Refusal
	}{
		"no handle":       {map[string]any{}, tools.RefusalInvalid},
		"no such seat":    {map[string]any{"handle": "nobody"}, tools.RefusalNotFound},
		"a person's seat": {map[string]any{"handle": "founder"}, tools.RefusalNotFound},
		"a long reason": {map[string]any{"handle": "agent-cto",
			"reason": string(make([]rune, builtin.MaxPauseReasonRunes+1))}, tools.RefusalInvalid},
	} {
		res, _ := rig.call(t, builtin.PauseSeatTool, tc.args)
		if !res.Failed || res.Refusal != tc.want {
			t.Errorf("%s: %+v, want refused %s", name, res, tc.want)
		}
	}
	if got := rig.pub.types(); len(got) != 0 {
		t.Errorf("a refused pause announced %v", got)
	}
}

// unreadablePauses is a pause record that cannot be read.
type unreadablePauses struct{ builtin.SeatPauseStore }

func (unreadablePauses) SeatPause(context.Context, string) (coord.SeatPause, bool, error) {
	return coord.SeatPause{}, false, coord.ErrUnavailable
}

// A RECORD THAT CANNOT BE READ IS UNAVAILABLE, never "not paused": a pause
// written over a read that failed could overwrite one somebody just took.
func TestAnUnreadablePauseRecordIsUnavailable(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, unreadablePauses{coordmem.NewFleet()}, everyNode{has: true}, "jane")
	for _, tool := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
		res, _ := rig.call(t, tool, map[string]any{"handle": "agent-cto"})
		if !res.Failed || res.Refusal != tools.RefusalUnavailable {
			t.Errorf("%s over an unreadable record = %+v, want unavailable", tool, res)
		}
	}
}
