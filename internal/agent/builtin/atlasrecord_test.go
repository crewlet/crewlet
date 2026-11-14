package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// Observe records an agent's claim as the log's writer does: the seat as its
// source, in the turn and with the evidence the call names, judged by the
// vocabulary, and every operation answered once.
func (l *atlasLedger) Observe(_ context.Context, c atlasdomain.Call, claim atlas.Claim,
	seen atlasdomain.Observation) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the operation id %w", atlasdomain.ErrInvalid, err)
	}
	if res, ok := l.ops[c.OpID]; ok {
		return res, nil
	}
	if c.By.Kind != iam.ActorAgent {
		return statelog.Result{}, fmt.Errorf("%w: not an agent's", atlasdomain.ErrInvalid)
	}
	claim.Source = atlas.AgentSource(c.By.Name, seen.Turn, seen.Evidence).Citing(seen.Tool)
	claim.Position = uint64(len(l.claims) + 1)
	if err := errors.Join(claim.Recordable(), claim.Validate()); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", atlasdomain.ErrInvalid, err)
	}
	l.claims = append(l.claims, claim)
	l.calls = append(l.calls, c)
	l.versions[claim.Subject.ID().String()] = claim.Position
	res := statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID,
		Position: statelog.Unpack(atlasStream, int64(claim.Position)), Version: int64(claim.Position)}
	l.ops[c.OpID] = res
	return res, nil
}

// by is every claim the ledger holds from the agent seat, in order.
func (l *atlasLedger) by(seat string) []atlas.Claim {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []atlas.Claim
	for _, c := range l.claims {
		if c.Source.Kind == atlas.SourceAgent && c.Source.Name == seat {
			out = append(out, c)
		}
	}
	return out
}

// dayCounter is a seat's allowance for the day, as the fleet's counter keeps
// it: every call counted, room while the count stays within the limit, and
// down where the store cannot say.
type dayCounter struct {
	mu    sync.Mutex
	spent map[string]int
	down  bool
}

func (d *dayCounter) allow(_ context.Context, agentID string, limit int, _ time.Time) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return false, errors.New("the counter could not be read")
	}
	if d.spent == nil {
		d.spent = map[string]int{}
	}
	d.spent[agentID]++
	return d.spent[agentID] <= limit, nil
}

// recordRig is the agent's atlas tools over acme: sre reaches web-1 over SSH,
// the atlas's log is a ledger, and the people who may wake a turn are the
// founder, who holds Atlas, and mallory, who does not.
type recordRig struct {
	t        *testing.T
	reg      *tools.Registry
	ledger   *atlasLedger
	org      *org.Organization
	settings *builtin.AtlasSettings
	counter  *dayCounter
	at       time.Time
}

func newRecordRig(t *testing.T) *recordRig {
	t.Helper()
	settings, o := reachCompany(t)
	r := &recordRig{t: t, ledger: newAtlasLedger(), org: o, settings: &settings, counter: &dayCounter{},
		at: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
	r.reg = tools.NewRegistry()
	_, err := builtin.Register(r.reg, gated(builtin.Deps{
		Atlas: builtin.AtlasDeps{
			Settings: func(context.Context) (builtin.AtlasSettings, bool) { return *r.settings, true },
			Records:  atlasLogOnly(r.ledger),
		},
		AtlasRecord: builtin.AtlasRecordDeps{Log: r.ledger, Allow: r.counter.allow,
			Now: func() time.Time { return r.at }},
		Requesters: requesters{
			"founder": founderPrincipal(iam.GrantAtlas),
			"mallory": {Kind: iam.KindPerson, Seat: "mallory", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}},
		},
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r
}

// turn is a turn of sre's under run, which has called nothing yet.
func (r *recordRig) turn(run string) *turnctx.Turn {
	return &turnctx.Turn{RunID: run, WorkKey: "wk-" + run, Seat: r.org.AgentSeatByHandle("sre"), Org: r.org,
		Calls: turnctx.NewCallLog(), Lookups: turnctx.NewLookups(), Seen: turnctx.NewSeen()}
}

// call makes one call of the tool name as sre in turn, through the gate.
func (r *recordRig) call(name string, turn *turnctx.Turn, args map[string]any) tools.Result {
	r.t.Helper()
	entry, ok := r.reg.Snapshot().Lookup(name)
	if !ok {
		r.t.Fatalf("%s is not registered", name)
	}
	ctx := iam.WithPrincipal(context.Background(), turnctx.SeatPrincipal(r.org, r.org.AgentSeatByHandle("sre")))
	res, err := entry.Tool.(tools.SeatCallable).CallForTurn(ctx, turn, args)
	if err != nil {
		r.t.Fatalf("%s: %v", name, err)
	}
	return res
}

// recordAnswer is a tool's answer as the JSON object it is.
func recordAnswer(t *testing.T, res tools.Result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("the tool answered no JSON object: %v\n%s", err, res.Output)
	}
	return out
}

// dockerPS is what an SSH read of web-1 printed in the cases: one container
// running a database.
const dockerPS = "CONTAINER ID   IMAGE           PORTS                    NAMES\n" +
	"3f9a1c2b7d10   postgres:16.2   0.0.0.0:5432->5432/tcp   orders-db\n"

// sawOnWeb1 notes on turn an SSH read of web-1 that answered output.
func sawOnWeb1(turn *turnctx.Turn, output string) {
	turn.Seen.Note(turnctx.Answer{Tool: builtin.SSHReadTool, Args: map[string]any{"host": "web-1",
		"command": "docker ps"}, Output: output})
}

// ordersDB is the record the cases make of what dockerPS shows.
func ordersDB() map[string]any {
	return map[string]any{
		"atlas_entry": "datastore:web-1/orders-db",
		"facts": []any{
			map[string]any{"fact": "engine", "value": "postgres"},
			map[string]any{"fact": "engine_version", "value": "16.2"},
			map[string]any{"fact": "port", "value": "5432"},
		},
		"relation": "runs_on", "to": "host:ssh/web-1",
		"seen_in": builtin.SSHReadTool,
	}
}

// AN AGENT RECORDS WHAT A CALL OF ITS OWN TURN SHOWED IT (ADR-0132): the
// entry, its facts and where it runs, each a claim in the seat's voice naming
// the turn, at the confidence the engine reads off the cited call. An SSH
// read of a server makes the entry likely, and the agent passed no confidence.
// Each claim names the tool of the call it cited, as the turn recorded it, so
// a person reviewing it is told which call it rests on.
//
// Mutation: make grounded.shownBy answer true for every claim (the grounding
// check, broken on purpose) and TestAnUncitedValueIsRefused goes red; make
// evidenceOf answer EvidenceCall for every tool and
// TestAFactFromAChatMessageStaysUnconfirmed goes red.
func TestAnAgentRecordsWhatACallOfItsTurnShowed(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	turn := r.turn("run-1")
	sawOnWeb1(turn, dockerPS)
	got := r.call(builtin.AtlasRecordTool, turn, ordersDB())
	if got.Failed {
		t.Fatalf("a record a call of the turn shows was refused: %s", got.Output)
	}
	claims := r.ledger.by("sre")
	if len(claims) != 5 {
		t.Fatalf("the record made %d claims, want the entry, three facts and a relation: %+v", len(claims), claims)
	}
	for _, c := range claims {
		if c.Source != atlas.AgentSource("sre", "run-1", atlas.EvidenceCall).Citing(builtin.SSHReadTool) {
			t.Errorf("the %s claim is %+v, want sre's in run-1 on a call's evidence, citing its tool",
				c.Facet.Kind, c.Source)
		}
		if c.Denied {
			t.Errorf("a record denied something: %+v", c)
		}
	}
	live, _ := r.ledger.Live(t.Context(), statelog.Freshness{})
	e, ok := atlas.Fold(live.Claims, r.at).Find(atlas.MustRef(atlas.KindDatastore, "web-1/orders-db").ID())
	if !ok || e.Word != atlas.WordLikely || e.State != atlas.StateDiscovered {
		t.Errorf("the entry reads %+v (found %v), want a likely, discovered one", e, ok)
	}
	if a := recordAnswer(t, got); a["evidence"] != "call" || a["shown_as"] != "likely" || a["recorded"] != float64(5) {
		t.Errorf("the answer reads %v, want five claims recorded as likely on a call's evidence", a)
	}
}

// AN UNCITED VALUE IS REFUSED, AND NOTHING IS RECORDED: a record with no call
// behind it, one citing a tool the turn never called, one citing a call of
// another turn, and one stating a value the cited answer does not show, whole.
// The refusal names the argument and never the value.
//
// Mutation: make grounded.shownBy answer true for every claim, and the
// invented version, the invented entry and the longer host name are recorded.
func TestAnUncitedValueIsRefused(t *testing.T) {
	t.Parallel()
	with := func(change func(map[string]any)) map[string]any {
		args := ordersDB()
		change(args)
		return args
	}
	for name, c := range map[string]struct {
		args  map[string]any
		saw   bool
		names string
	}{
		"no call cited": {args: with(func(a map[string]any) { delete(a, "seen_in") }), saw: true, names: "seen_in"},
		"a tool the turn never called": {args: with(func(a map[string]any) { a["seen_in"] = builtin.CodeReadTool }),
			saw: true, names: "no call of that tool"},
		"a call of another turn": {args: ordersDB(), saw: false, names: "no call of that tool"},
		"a version the answer does not show": {args: with(func(a map[string]any) {
			a["facts"] = []any{map[string]any{"fact": "engine_version", "value": "17.1"}}
		}), saw: true, names: "`facts[0]` (engine_version)"},
		"a value shown only inside a longer one": {args: with(func(a map[string]any) {
			a["facts"] = []any{map[string]any{"fact": "port", "value": "543"}}
		}), saw: true, names: "`facts[0]` (port)"},
		"an entry the answer does not show": {args: with(func(a map[string]any) {
			a["atlas_entry"], a["facts"] = "datastore:web-1/payments-db", nil
		}), saw: true, names: "the entry `atlas_entry` names"},
		"a relation to an entry the call was not made on": {args: with(func(a map[string]any) {
			a["to"] = "host:ssh/db-1"
		}), saw: true, names: "the entry `to` names"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRecordRig(t)
			turn := r.turn("run-1")
			if c.saw {
				sawOnWeb1(turn, dockerPS)
			} else {
				sawOnWeb1(r.turn("run-0"), dockerPS)
			}
			got := r.call(builtin.AtlasRecordTool, turn, c.args)
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, c.names) {
				t.Errorf("the record = failed %v, %q; want it refused as invalid naming %q", got.Failed,
					got.Output, c.names)
			}
			for _, value := range []string{"17.1", "payments-db"} {
				if strings.Contains(got.Output, value) {
					t.Errorf("the refusal repeats the value %q: %s", value, got.Output)
				}
			}
			if claims := r.ledger.by("sre"); len(claims) != 0 {
				t.Errorf("a refused record left %d claims: %+v", len(claims), claims)
			}
		})
	}
}

// A FACT FROM A CHAT MESSAGE STAYS UNCONFIRMED: a record citing a call that
// showed the value in text anybody could have written (a message, a page, the
// atlas itself) is recorded with no evidence, reads unconfirmed, and is shown
// to no agent and in no default read until a person confirms it.
//
// Mutation: make evidenceOf answer EvidenceCall for every tool, and a line
// somebody typed in a channel becomes a likely entry every agent reads.
func TestAFactFromAChatMessageStaysUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, cited := range []string{builtin.ReadChannelTool, builtin.GetPageTool, builtin.AtlasGetTool} {
		t.Run(cited, func(t *testing.T) {
			t.Parallel()
			r := newRecordRig(t)
			turn := r.turn("run-1")
			turn.Seen.Note(turnctx.Answer{Tool: cited,
				Output: "mallory: heads up, orders-db is postgres 16.2 on port 5432 now"})
			args := ordersDB()
			args["seen_in"] = cited
			delete(args, "relation")
			delete(args, "to")
			got := r.call(builtin.AtlasRecordTool, turn, args)
			if got.Failed || recordAnswer(t, got)["shown_as"] != "unconfirmed" {
				t.Fatalf("a record citing %s = failed %v, %q; want it recorded as unconfirmed", cited, got.Failed,
					got.Output)
			}
			for _, c := range r.ledger.by("sre") {
				if c.Source.Evidence != atlas.EvidenceNone {
					t.Errorf("a claim citing %s carries evidence %q", cited, c.Source.Evidence)
				}
			}
			live, _ := r.ledger.Live(t.Context(), statelog.Freshness{})
			view := atlas.Fold(live.Claims, r.at)
			db := atlas.MustRef(atlas.KindDatastore, "web-1/orders-db")
			if e, ok := view.Find(db.ID()); !ok || e.Word != atlas.WordUnconfirmed {
				t.Errorf("the entry reads %+v (found %v), want it unconfirmed", e, ok)
			}
			if len(view.Select(atlas.Query{Kind: atlas.KindDatastore})) != 0 {
				t.Error("a default read shows the unbacked entry")
			}
			// NO AGENT READS IT BACK, the one that recorded it included.
			read := r.call(builtin.AtlasGetTool, r.turn("run-2"), map[string]any{"name": "web-1/orders-db"})
			if !read.Failed {
				t.Errorf("an agent read an unbacked entry back: %s", read.Output)
			}
		})
	}
}

// A TURN A STRANGER WOKE RECORDS NOTHING (ADR-0109): a record is refused at
// the gate, before the tool runs, in a turn somebody the company does not
// know woke, one a colleague handed on for a stranger, one a credential nobody
// can place woke, and one a person without Atlas woke. A turn nobody woke and
// one a holder of Atlas woke record.
//
// Mutation: drop the atlas app from the atlas_record row, or make its row
// decide outside the ceiling, and a stranger's turn records.
func TestATurnAStrangerWokeRecordsNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		wake    func(*turnctx.Turn)
		records bool
	}{
		"nobody woke it":                 {wake: func(*turnctx.Turn) {}, records: true},
		"a holder of Atlas woke it":      {wake: func(t *turnctx.Turn) { t.Requester = "founder" }, records: true},
		"a person without Atlas woke it": {wake: func(t *turnctx.Turn) { t.Requester = "mallory" }},
		"a sender nobody knows": {wake: func(t *turnctx.Turn) {
			t.Senders = []types.CanonicalIdentity{{}}
		}},
		"a stranger handed on by a colleague": {wake: func(t *turnctx.Turn) {
			t.WorkingFor = &types.WorkingFor{Kind: types.WorkingForStranger}
		}},
		"a credential nobody can place": {wake: func(t *turnctx.Turn) {
			t.RequesterLogin, t.RequesterApps = "token:ci", []iam.Grant{iam.GrantAtlas}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRecordRig(t)
			turn := r.turn("run-1")
			c.wake(turn)
			sawOnWeb1(turn, dockerPS)
			got := r.call(builtin.AtlasRecordTool, turn, ordersDB())
			if recorded := !got.Failed && len(r.ledger.by("sre")) > 0; recorded != c.records {
				t.Fatalf("the turn recorded %v (%s), want %v", recorded, got.Output, c.records)
			}
			if c.records {
				return
			}
			refused, ok := errors.AsType[*builtin.DecisionRefused](got.Cause)
			if !ok || refused.Decision.Reason != authz.ReasonCeiling {
				t.Errorf("the refusal is %v (%s), want the requester ceiling's", got.Cause, got.Output)
			}
			if spent := r.counter.spent; len(spent) != 0 {
				t.Errorf("a refused turn spent the seat's allowance: %v", spent)
			}
			dispute := r.call(builtin.AtlasDisputeTool, turn, map[string]any{"atlas_entry": "host:ssh/web-1",
				"reason": "gone", "seen_in": builtin.SSHReadTool})
			if !dispute.Failed || len(r.ledger.by("sre")) != 0 {
				t.Errorf("the turn disputed an entry: %s", dispute.Output)
			}
		})
	}
}

// AN AGENT CANNOT RETIRE ANYTHING OR SET AN OWNER: it holds none of a
// person's atlas tools, its record of a fact of meaning, an owner or a
// dependency is refused, and its dispute of an entry leaves the entry where
// it was, shown for a person to decide.
//
// Mutation: let atlas.resolve take an agent's denial as a facet's winner, and
// the disputed entries read retired; drop the agent arms of
// atlas.Claim.Validate and the meaning, the owner and the dependency land.
func TestAnAgentCannotRetireAnythingOrSetAnOwner(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	for _, name := range builtin.AtlasWriteTools {
		if _, held := r.reg.Snapshot().Lookup(name); held {
			t.Errorf("a seat's registry holds %s, which is a person's", name)
		}
	}
	turn := r.turn("run-1")
	sawOnWeb1(turn, dockerPS+"owner agent-cto  environment production  importance critical  Takes payments  checkout-api\n")
	if got := r.call(builtin.AtlasRecordTool, turn, ordersDB()); got.Failed {
		t.Fatalf("recording the database: %s", got.Output)
	}
	before := len(r.ledger.by("sre"))
	for name, args := range map[string]map[string]any{
		"an environment": {"facts": []any{map[string]any{"fact": "environment", "value": "production"}}},
		"an importance":  {"facts": []any{map[string]any{"fact": "importance", "value": "critical"}}},
		"a purpose":      {"facts": []any{map[string]any{"fact": "purpose", "value": "Takes payments"}}},
		"an owner":       {"relation": "owned_by", "to": "host:ssh/web-1"},
		"a dependency":   {"atlas_entry": "service:checkout-api", "relation": "depends_on", "to": "datastore:web-1/orders-db"},
	} {
		if args["atlas_entry"] == nil {
			args["atlas_entry"] = "datastore:web-1/orders-db"
		}
		args["seen_in"] = builtin.SSHReadTool
		got := r.call(builtin.AtlasRecordTool, turn, args)
		if !got.Failed || len(r.ledger.by("sre")) != before {
			t.Errorf("an agent recorded %s: %s", name, got.Output)
		}
	}

	// ITS DISPUTE RETIRES NOTHING: a server the settings declare, and the
	// database only this agent reported, both stand.
	for _, entry := range []string{"host:ssh/web-1", "datastore:web-1/orders-db"} {
		got := r.call(builtin.AtlasDisputeTool, turn, map[string]any{"atlas_entry": entry, "reason": "gone",
			"seen_in": builtin.SSHReadTool})
		if got.Failed {
			t.Fatalf("a dispute of %s citing a read of the server was refused: %s", entry, got.Output)
		}
	}
	live, _ := r.ledger.Live(t.Context(), statelog.Freshness{})
	answer := r.call(builtin.AtlasGetTool, r.turn("run-2"), map[string]any{"name": "web-1"})
	if answer.Failed || !strings.Contains(answer.Output, `"state": "confirmed"`) {
		t.Errorf("after an agent's dispute the declared server reads: %s", answer.Output)
	}
	// ANOTHER SEAT'S DISPUTE of what sre reported leaves it standing too.
	gone, err := atlas.Dispute(atlas.Claim{Subject: atlas.MustRef(atlas.KindDatastore, "web-1/orders-db"),
		Facet: atlas.Facet{Kind: atlas.FacetExists}}, atlas.AgentSource("dev", "run-9", atlas.EvidenceCall), r.at)
	if err != nil {
		t.Fatal(err)
	}
	withDev := append(live.Claims, gone)
	e, ok := atlas.Fold(withDev, r.at).Find(gone.Subject.ID())
	if !ok || e.State == atlas.StateRetired || len(e.Facts) != 3 {
		t.Errorf("after two agents' disputes the database reads %+v (found %v), want it standing with its facts",
			e, ok)
	}
}

// THE CAPS HOLD: a turn records at most twenty times and a seat two hundred
// times a day across its turns; past either the record is refused and nothing
// is written, and where the day's count cannot be read the record is refused
// rather than made uncounted.
//
// Mutation: drop the spendTurn check and the turn's twenty-first record
// lands; ignore Allow's answer and the day's two hundred and first lands.
func TestTheAtlasRecordCapsHold(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	// One read of the server lists more databases than a day's allowance.
	var listing strings.Builder
	for i := range builtin.AtlasRecordsPerDay + 5 {
		fmt.Fprintf(&listing, "postgres:16.2  db-%03d\n", i)
	}
	record := func(turn *turnctx.Turn, i int) tools.Result {
		return r.call(builtin.AtlasRecordTool, turn, map[string]any{
			"atlas_entry": fmt.Sprintf("datastore:web-1/db-%03d", i), "seen_in": builtin.SSHReadTool})
	}
	turn := r.turn("run-0")
	sawOnWeb1(turn, listing.String())
	for i := range builtin.AtlasRecordsPerTurn {
		if got := record(turn, i); got.Failed {
			t.Fatalf("record %d of the turn was refused: %s", i+1, got.Output)
		}
	}
	over := record(turn, builtin.AtlasRecordsPerTurn)
	if !over.Failed || tools.RefusalOf(over) != tools.RefusalQuotaExceeded ||
		len(r.ledger.by("sre")) != builtin.AtlasRecordsPerTurn {
		t.Fatalf("the turn's record past its cap = failed %v, %q, with %d claims; want it refused as over quota "+
			"and %d claims", over.Failed, over.Output, len(r.ledger.by("sre")), builtin.AtlasRecordsPerTurn)
	}

	// THE DAY, across the seat's turns.
	made := builtin.AtlasRecordsPerTurn
	for n := 1; made < builtin.AtlasRecordsPerDay; n++ {
		next := r.turn(fmt.Sprintf("run-%d", n))
		sawOnWeb1(next, listing.String())
		for range builtin.AtlasRecordsPerTurn {
			if got := record(next, made); got.Failed {
				t.Fatalf("record %d of the day was refused: %s", made+1, got.Output)
			}
			made++
		}
	}
	last := r.turn("run-late")
	sawOnWeb1(last, listing.String())
	spent := record(last, made)
	if !spent.Failed || tools.RefusalOf(spent) != tools.RefusalQuotaExceeded ||
		len(r.ledger.by("sre")) != builtin.AtlasRecordsPerDay {
		t.Fatalf("the day's record past its cap = failed %v, %q, with %d claims; want it refused as over quota "+
			"and %d claims", spent.Failed, spent.Output, len(r.ledger.by("sre")), builtin.AtlasRecordsPerDay)
	}

	// A COUNT NOBODY CAN READ REFUSES, never records uncounted.
	down := newRecordRig(t)
	down.counter.down = true
	blind := down.turn("run-1")
	sawOnWeb1(blind, dockerPS)
	got := down.call(builtin.AtlasRecordTool, blind, ordersDB())
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable || len(down.ledger.by("sre")) != 0 {
		t.Errorf("a record whose day could not be counted = failed %v, %q; want it refused as unavailable",
			got.Failed, got.Output)
	}
}

// THE COMPANY SWITCH TURNS AGENTS' RECORDS OFF: with `atlas.agent_records`
// false neither tool records, and the refusal says the company turned it off.
// Unset is on.
//
// Mutation: drop the AgentsRecord check and a company that turned it off
// still collects records.
func TestTheCompanySwitchTurnsAgentRecordsOff(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	off := false
	r.settings.Company.Atlas.AgentRecords = &off
	turn := r.turn("run-1")
	sawOnWeb1(turn, dockerPS)
	for name, args := range map[string]map[string]any{
		builtin.AtlasRecordTool: ordersDB(),
		builtin.AtlasDisputeTool: {"atlas_entry": "host:ssh/web-1", "reason": "gone",
			"seen_in": builtin.SSHReadTool},
	} {
		got := r.call(name, turn, args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
			!strings.Contains(got.Output, "atlas.agent_records") {
			t.Errorf("%s with the switch off = failed %v, %q; want it refused naming the setting", name,
				got.Failed, got.Output)
		}
	}
	if claims := r.ledger.by("sre"); len(claims) != 0 || len(r.counter.spent) != 0 {
		t.Errorf("with the switch off the tools left %d claims and spent %v", len(claims), r.counter.spent)
	}
}

// A RECORD THE ATLAS ALREADY HOLDS WRITES NOTHING: the same record made again
// within a week is answered as already known, spends no allowance and adds no
// row; after a week it is written again, so a fact an agent keeps meeting
// never goes stale. A fact the settings already declare is never recorded.
//
// Mutation: make observing.holds answer false, and every repeat adds a row to
// the entry's history.
func TestARecordTheAtlasAlreadyHoldsWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	first := r.turn("run-1")
	sawOnWeb1(first, dockerPS)
	if got := r.call(builtin.AtlasRecordTool, first, ordersDB()); got.Failed {
		t.Fatalf("the first record: %s", got.Output)
	}
	held, spent := len(r.ledger.by("sre")), r.counter.spent[first.AgentID()]

	r.at = r.at.Add(24 * time.Hour)
	again := r.turn("run-2")
	sawOnWeb1(again, dockerPS)
	got := r.call(builtin.AtlasRecordTool, again, ordersDB())
	if got.Failed || recordAnswer(t, got)["recorded"] != float64(0) || len(r.ledger.by("sre")) != held ||
		r.counter.spent[first.AgentID()] != spent {
		t.Errorf("the same record a day later = failed %v, %q, %d claims, %d spent; want nothing recorded or spent",
			got.Failed, got.Output, len(r.ledger.by("sre")), r.counter.spent[first.AgentID()])
	}

	// WHAT THE SETTINGS DECLARE is known already: web-1's address is theirs.
	declared := r.call(builtin.AtlasRecordTool, again, map[string]any{"atlas_entry": "host:ssh/web-1",
		"facts": []any{map[string]any{"fact": "address", "value": "9.9.9.9"}}, "seen_in": builtin.SSHReadTool})
	if declared.Failed || recordAnswer(t, declared)["recorded"] != float64(0) || len(r.ledger.by("sre")) != held {
		t.Errorf("a record of what the settings declare = failed %v, %q", declared.Failed, declared.Output)
	}

	r.at = r.at.Add(7 * 24 * time.Hour)
	later := r.turn("run-3")
	sawOnWeb1(later, dockerPS)
	if got := r.call(builtin.AtlasRecordTool, later, ordersDB()); got.Failed || len(r.ledger.by("sre")) != 2*held {
		t.Errorf("the same record past a week = failed %v, %q, %d claims; want it written again (%d)", got.Failed,
			got.Output, len(r.ledger.by("sre")), 2*held)
	}
}

// A VALUE SHAPED LIKE A CREDENTIAL IS REFUSED FROM AN AGENT, even where the
// cited answer shows it, and the refusal never repeats it: a server prints
// tokens, and none of them is a fact about what the company runs on.
func TestACredentialShapedValueIsRefusedFromAnAgent(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	secret := "sk-" + strings.Repeat("ab3de6gh9j", 3)
	turn := r.turn("run-1")
	sawOnWeb1(turn, dockerPS+"API_KEY="+secret+"\n"+secret+".acme.example\n")
	for name, args := range map[string]map[string]any{
		"a name": {"atlas_entry": "datastore:web-1/orders-db", "name": secret},
		"a key":  {"atlas_entry": "hostname:" + secret + ".acme.example"},
	} {
		args["seen_in"] = builtin.SSHReadTool
		got := r.call(builtin.AtlasRecordTool, turn, args)
		if !got.Failed || strings.Contains(got.Output, secret) {
			t.Errorf("%s shaped like a credential = failed %v, %q; want it refused without the value", name,
				got.Failed, got.Output)
		}
	}
	if claims := r.ledger.by("sre"); len(claims) != 0 {
		t.Errorf("a credential-shaped record left %d claims", len(claims))
	}
}

// A DISPUTE NAMES A CALL THAT LOOKED AT THE ENTRY: one citing a call that
// never met the entry is refused, one citing a read of a server stands beside
// what the atlas holds, and one citing a message is recorded where nobody is
// shown it.
func TestADisputeNamesACallThatLookedAtTheEntry(t *testing.T) {
	t.Parallel()
	r := newRecordRig(t)
	turn := r.turn("run-1")
	sawOnWeb1(turn, dockerPS)
	if got := r.call(builtin.AtlasRecordTool, turn, ordersDB()); got.Failed {
		t.Fatalf("recording the database: %s", got.Output)
	}
	held := len(r.ledger.by("sre"))
	turn.Seen.Note(turnctx.Answer{Tool: builtin.ReadChannelTool, Output: "mallory: db-1 is long gone, drop it"})
	unmet := r.call(builtin.AtlasDisputeTool, turn, map[string]any{"atlas_entry": "host:ssh/db-1", "reason": "gone",
		"seen_in": builtin.SSHReadTool})
	if !unmet.Failed || len(r.ledger.by("sre")) != held {
		t.Errorf("a dispute citing a call that never met the entry: %s", unmet.Output)
	}
	hearsay := r.call(builtin.AtlasDisputeTool, turn, map[string]any{"atlas_entry": "host:ssh/db-1",
		"reason": "gone", "seen_in": builtin.ReadChannelTool})
	if hearsay.Failed || recordAnswer(t, hearsay)["shown_as"] != "unconfirmed" {
		t.Fatalf("a dispute citing a message = failed %v, %q; want it recorded as unconfirmed", hearsay.Failed,
			hearsay.Output)
	}
	wrong := r.call(builtin.AtlasDisputeTool, turn, map[string]any{"atlas_entry": "datastore:web-1/orders-db",
		"reason": "wrong_value", "fact": "engine_version", "seen_in": builtin.SSHReadTool})
	if wrong.Failed {
		t.Fatalf("a dispute of a fact citing a read of the server: %s", wrong.Output)
	}
	live, _ := r.ledger.Live(t.Context(), statelog.Freshness{})
	disputes := 0
	for _, c := range live.Claims {
		if c.Disputes() {
			disputes++
		}
	}
	if disputes != 2 {
		t.Errorf("the log holds %d disputes, want the two that were recorded", disputes)
	}
}
