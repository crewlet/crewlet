package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
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

func (f everyNode) SeatFeature(context.Context, uuid.UUID, coord.Feature) (bool, error) {
	return false, errors.New("the pause tools ask every node, never one")
}

func (f everyNode) AllLiveHave(_ context.Context, feature coord.Feature) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.has && feature == coord.FeatureSeatPause, nil
}

// leadPairs is a chart whose one relation is "this handle leads that one", by
// the exact strings, and nothing else: which is what makes a rule that asked
// about the wrong name — a typed alias, a role — visible.
type leadPairs map[[2]string]bool

func (c leadPairs) Leads(_ context.Context, actor, subject string) (bool, error) {
	return c[[2]string{actor, subject}], nil
}

func (leadPairs) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadPairs) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadPairs) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c leadPairs) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	for pair, led := range c {
		if led && pair[0] == actor {
			return true, nil
		}
	}
	return false, nil
}

// seatLeads is the chart the seat gestures are decided against: jane and omar
// each lead the CTO's seat, and nobody leads anybody else.
var seatLeads = leadPairs{{"jane", "agent-cto"}: true, {"omar", "agent-cto"}: true}

// boundTo is a person the directory binds to seat — what a pause, a resume
// and a note are made by — signed in through a session, holding grants.
func boundTo(seat string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Kind: iam.KindPerson,
		Login: seat + ".doe", Seat: seat, Via: "session:" + seat + "-1",
		Stage: iam.StageActive, Grants: grants}
}

type pauseRig struct {
	store  coord.SeatPauses
	pub    *announced
	tools  map[string]tools.Callable
	caller context.Context
	cto    uuid.UUID
}

// newPauseRig is the operator catalogue's pause tools over one store, called
// as caller and decided by the real authority table over [seatLeads].
func newPauseRig(t *testing.T, store builtin.SeatPauseStore, fleet builtin.Fleet,
	caller iam.Principal) *pauseRig {

	t.Helper()
	o := organization(t)
	cto, ok := o.AgentIDFor(o.Role("agent-cto"))
	if !ok {
		t.Fatal("the fixture's CTO seat has no agent id")
	}
	pub := &announced{}
	rig := &pauseRig{pub: pub, tools: map[string]tools.Callable{},
		caller: iam.WithPrincipal(context.Background(), caller), cto: cto}
	if s, ok := store.(coord.SeatPauses); ok {
		rig.store = s
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Fleet: fleet,
		Pauses: builtin.SeatPauseDeps{
			Pauses: store, Announce: pub,
			Org:   func() *org.Organization { return o },
			Actor: builtin.PrincipalActor,
			Now:   func() time.Time { return time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC) },
		},
		Authorize: builtin.Decide(seatLeads),
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
	res, err := r.tools[name].Call(r.caller, args)
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
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, boundTo("jane"))

	_, got := rig.call(t, builtin.PauseSeatTool, map[string]any{
		"handle": "agent-cto", "reason": "looping on one ticket",
	})
	// BY THE SEAT SHE IS BOUND TO, kind human, with the credential she
	// acted through beside it — iam.ActorFor's three halves, as every
	// other write a person makes records them.
	if got["outcome"] != "applied" || got["changed"] != true || got["paused_by"] != "jane" ||
		got["paused_by_kind"] != "human" || got["operator_id"] != "session:jane-1" {
		t.Fatalf("first pause = %v, want an applied change by jane", got)
	}
	p, found, err := rig.store.SeatPause(context.Background(), rig.cto)
	if err != nil || !found || p.By != "jane" || p.ByKind != iam.ActorHuman ||
		p.OperatorID != "session:jane-1" || p.Reason != "looping on one ticket" || p.StopRunning {
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
		paused.PausedBy != "jane" || paused.PausedByKind != "human" ||
		paused.OperatorID != "session:jane-1" || paused.Agent != rig.cto.String() {
		t.Errorf("seat_paused = %+v, want the seat named as its events name it", paused)
	}
}

// ASKING TO STOP A PAUSED SEAT'S TURN IS A REAL CHANGE: the pause in place is
// amended under the version read, names who asked for the stop, and says so.
func TestAPauseAddsAStopToThePauseInPlace(t *testing.T) {
	t.Parallel()
	store := coordmem.NewFleet()
	rig := newPauseRig(t, store, everyNode{has: true}, boundTo("omar"))
	if _, _, err := store.CreateSeatPause(context.Background(), coord.SeatPause{
		Seat: rig.cto, By: "jane", ByKind: iam.ActorHuman, Reason: "looping", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, got := rig.call(t, builtin.PauseSeatTool, map[string]any{
		"handle": "agent-cto", "stop_running": true,
	})
	if got["changed"] != true || got["stop_running"] != true || got["paused_by"] != "omar" {
		t.Fatalf("pause with a stop = %v, want the stop added by omar", got)
	}
	p, _, _ := store.SeatPause(context.Background(), rig.cto)
	if !p.StopRunning || p.By != "omar" || p.OperatorID != "session:omar-1" || p.Reason != "looping" {
		t.Errorf("record = %+v, want the stop added, omar named and the reason kept", p)
	}
}

// A RESUME LIFTS THE PAUSE AND SAYS WHOSE IT WAS; resuming a free seat is a
// no-op, announced by nobody.
func TestAResumeLiftsThePauseAndAnnouncesOnce(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, boundTo("jane"))
	rig.call(t, builtin.PauseSeatTool, map[string]any{"handle": "agent-cto"})
	_, got := rig.call(t, builtin.ResumeSeatTool, map[string]any{"handle": "agent-cto"})
	if got["outcome"] != "applied" || got["changed"] != true {
		t.Fatalf("resume = %v, want an applied change", got)
	}
	if _, found, _ := rig.store.SeatPause(context.Background(), rig.cto); found {
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
	if resumed.PausedBy != "jane" || resumed.PausedByKind != "human" ||
		resumed.ResumedBy != "jane" || resumed.ResumedByKind != "human" ||
		resumed.OperatorID != "session:jane-1" || resumed.PausedAt.IsZero() {
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
			rig := newPauseRig(t, coordmem.NewFleet(), tc.fleet, boundTo("jane"))
			for _, tool := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
				res, _ := rig.call(t, tool, map[string]any{"handle": "agent-cto"})
				if !res.Failed || tools.RefusalOf(res) != tc.want {
					t.Errorf("%s = %+v, want refused %s", tool, res, tc.want)
				}
			}
			if _, found, _ := rig.store.SeatPause(context.Background(), rig.cto); found {
				t.Error("a refused pause was recorded anyway")
			}
		})
	}
}

// ONLY AN AGENT SEAT IS PAUSED, and a reason is one line. Asked by the
// deployment's grant, which is admitted on any name — so what refuses each of
// these is the tool, not the authority.
func TestAPauseNamesAnAgentSeatAndALine(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true},
		boundTo("ops", iam.GrantFleetOperate))
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
		if !res.Failed || tools.RefusalOf(res) != tc.want {
			t.Errorf("%s: %+v, want refused %s", name, res, tc.want)
		}
	}
	if got := rig.pub.types(); len(got) != 0 {
		t.Errorf("a refused pause announced %v", got)
	}
}

// unreadablePauses is a pause record that cannot be read.
type unreadablePauses struct{ builtin.SeatPauseStore }

func (unreadablePauses) SeatPause(context.Context, uuid.UUID) (coord.SeatPause, bool, error) {
	return coord.SeatPause{}, false, coord.ErrUnavailable
}

// A RECORD THAT CANNOT BE READ IS UNAVAILABLE, never "not paused": a pause
// written over a read that failed could overwrite one somebody just took.
func TestAnUnreadablePauseRecordIsUnavailable(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, unreadablePauses{coordmem.NewFleet()}, everyNode{has: true},
		boundTo("jane"))
	for _, tool := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
		res, _ := rig.call(t, tool, map[string]any{"handle": "agent-cto"})
		if !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable {
			t.Errorf("%s over an unreadable record = %+v, want unavailable", tool, res)
		}
	}
}

// WHETHER A SEAT WORKS IS ITS LEAD'S TO DECIDE, or the deployment's — and no
// reader's. Everybody signed in holds a credential, so a rule that asked for
// one would let any reader of the board stop any seat. And it is decided on
// the SEAT the handle resolves to, so a lead typing `@agent-cto` is admitted
// exactly as one typing the handle is.
func TestAPauseIsTheSeatsLeadsOrTheDeployments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		caller  iam.Principal
		handle  string
		allowed bool
	}{
		{"the seat's lead", boundTo("jane"), "agent-cto", true},
		{"the lead, typing a mention", boundTo("jane"), "@agent-cto", true},
		{"the deployment's grant", boundTo("ops", iam.GrantFleetOperate), "agent-cto", true},
		{"a reader who leads nobody", boundTo("sam", iam.GrantStateRead,
			iam.GrantWorkWrite), "agent-cto", false},
		// AND NEVER A SEAT, however it is granted: whether a seat works is
		// a person's decision about it.
		{"a seat holding the deployment's grant", iam.Principal{ID: uuid.New(),
			Kind: iam.KindSeat, Login: "agent-ceo", Seat: "agent-ceo",
			Stage: iam.StageActive, Grants: iam.AllGrants}, "agent-cto", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, tc.caller)
			for _, tool := range []string{builtin.PauseSeatTool, builtin.ResumeSeatTool} {
				res, _ := rig.call(t, tool, map[string]any{"handle": tc.handle})
				switch {
				case tc.allowed && res.Failed:
					t.Errorf("%s refused %s: %s", tool, tc.name, res.Output)
				case !tc.allowed && (!res.Failed || !errors.Is(res.Cause, builtin.ErrRefused) ||
					tools.RefusalOf(res) != tools.RefusalForbidden):
					t.Errorf("%s answered %s with %+v, want the authority's refusal",
						tool, tc.name, res)
				}
			}
			if !tc.allowed {
				if _, found, _ := rig.store.SeatPause(context.Background(), rig.cto); found {
					t.Error("a refused caller's pause was recorded")
				}
				if got := rig.pub.types(); len(got) != 0 {
					t.Errorf("a refused caller's gesture announced %v", got)
				}
			}
		})
	}
}

// A HANDLE NO SEAT HOLDS IS DECIDED BEFORE IT IS LOOKED FOR, so the answer a
// caller who leads somebody gets never says whether the seat exists: the
// authority's refusal, the same as on a seat they do not lead.
func TestAPauseOfANameNobodyHoldsIsRefusedAsTheAuthorityRefuses(t *testing.T) {
	t.Parallel()
	rig := newPauseRig(t, coordmem.NewFleet(), everyNode{has: true}, boundTo("jane"))
	res, _ := rig.call(t, builtin.PauseSeatTool, map[string]any{"handle": "nobody"})
	if !res.Failed || !errors.Is(res.Cause, builtin.ErrRefused) {
		t.Errorf("a lead pausing a handle nobody holds was answered %+v, want the "+
			"authority's refusal", res)
	}
}
