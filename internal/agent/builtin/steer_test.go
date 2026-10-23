package builtin_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// steerRing is the keyring every node of the tool's test fleet holds, and
// steerWire the steer channel they sign on.
var (
	steerRing = statelog.OneKey("k1", "the steer_turn cases' fleet key material")
	steerWire = mustSteerWire(steerRing)
)

func mustSteerWire(ring statelog.Keyring) *signedask.Channel[steer.Request, steer.Reply] {
	wire, err := signedask.New[steer.Request, steer.Reply](steer.Channel, ring)
	if err != nil {
		panic(err)
	}
	return wire
}

// signedReply is rep as a node of the fleet signs it, bound to whatever
// request it names.
func signedReply(t *testing.T, wire *signedask.Channel[steer.Request, steer.Reply], rep steer.Reply) []byte {
	t.Helper()
	out, err := wire.SealReply(rep)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fleetAsker answers a scattered note the way the node running its turn does —
// replies signed by the fleet and bound to the request they answer — plus any
// raw bytes a case adds, and records what it was asked. A request the fleet
// did not sign is refused, so a tool that sent one fails every case.
type fleetAsker struct {
	replies []steer.Reply
	raw     [][]byte
	err     error
	asked   []steer.Request
	subject string
	want    int
}

func (a *fleetAsker) Ask(_ context.Context, subject string, request []byte, want int) ([][]byte, error) {
	a.subject, a.want = subject, want
	admitted := steerWire.Admit(request, signedask.NewGuard(), time.Now())
	if admitted.Disposition != signedask.Admitted {
		return nil, fmt.Errorf("the tool sent a request the fleet cannot take: %s", admitted.Why)
	}
	req := admitted.Request
	a.asked = append(a.asked, req)
	if a.err != nil {
		return nil, a.err
	}
	out := append([][]byte(nil), a.raw...)
	for _, r := range a.replies {
		r.Request = req.ID
		b, err := steerWire.SealReply(r)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// steerRig is steer_turn on the operator catalogue, called as one principal
// and decided by the real authority table over a chart.
type steerRig struct {
	tool   tools.Callable
	caller context.Context
	cto    uuid.UUID
}

// operatorCaller is an admin — admitted on any turn without the fleet being
// asked whose it is.
var operatorCaller = boundTo("ops", iam.GrantAdmin)

// newSteerRig builds the tool with the actor the act route hands it
// (internal/api/operator's workActor): the caller's principal, carrying the
// request's operation key as its WORK KEY, at the instant the key was minted.
// The note's id is taken from that and from nothing else, so a rig that put
// the key anywhere the act route does not would certify a path no request
// takes. An empty key is a call whose transport named no operation.
func newSteerRig(t *testing.T, asker builtin.FleetAsker, key string,
	caller iam.Principal, chart authz.Chart) steerRig {

	t.Helper()
	o := organization(t)
	cto := ctoSeat(t)
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Org: func() *org.Organization { return o },
		Steer: builtin.SteerDeps{Asker: asker, Wire: steerWire, Actor: func(ctx context.Context,
			turn *turnctx.Turn) (builtin.Actor, error) {
			actor, err := builtin.PrincipalActor(ctx, turn)
			if key != "" {
				actor.WorkKey = key
				actor.WorkSince, _ = statelog.OpMintedAt(key)
			}
			return actor, err
		}},
		Authorize: builtin.Decide(chart),
	}) {
		if tool.Name() == builtin.SteerTurnTool {
			return steerRig{tool: tool, caller: iam.WithPrincipal(t.Context(), caller), cto: cto}
		}
	}
	t.Fatal("the operator catalogue serves no steer_turn with an asker wired")
	return steerRig{}
}

// ctoSeat is the fixture's CTO seat by the id a turn's reply names it by.
func ctoSeat(t *testing.T) uuid.UUID {
	t.Helper()
	o := organization(t)
	id, ok := o.AgentIDFor(o.Role("agent-cto"))
	if !ok {
		t.Fatal("the fixture's CTO seat has no agent id")
	}
	return id
}

// steerTool is [newSteerRig] for the deployment's own operator, which every
// case not about authority acts as.
func steerTool(t *testing.T, asker builtin.FleetAsker, key string) steerRig {
	t.Helper()
	return newSteerRig(t, asker, key, operatorCaller, seatLeads)
}

// requestKey is an operation key as the act route hands one to a tool: an
// operation id in the state log's grammar, which carries the instant it was
// minted.
func requestKey() string { return statelog.NewOpID(time.Now(), "") }

func steerCall(t *testing.T, rig steerRig, turnID, note string) (tools.Result, map[string]any) {
	t.Helper()
	res, err := rig.tool.Call(rig.caller, map[string]any{"turn_id": turnID, "note": note})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("the answer is not JSON: %s", res.Output)
		}
	}
	return res, out
}

// A TAKEN NOTE IS PENDING, sent as the person, under the request's own id.
// What became of it is the running node's to record — this one cannot see that
// far.
func TestSteerTurnSendsTheNoteAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
		AgentHandle: "swe", Status: steer.StatusAccepted}}}
	key := requestKey()
	res, out := steerCall(t, steerTool(t, asker, key), "run-1", "  use staging  ")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	if out["outcome"] != "pending" || out["agent_handle"] != "swe" || out["note_id"] != key {
		t.Errorf("answered %v, want the note pending under the request's key %q", out, key)
	}
	if asker.subject != topics.SeatSteer || asker.want != 1 {
		t.Errorf("asked %q for %d replies, want the steer subject and the one node running it",
			asker.subject, asker.want)
	}
	// AS THE PERSON, in iam.ActorFor's three halves — and with no probe
	// first, because admin is admitted on any turn.
	if len(asker.asked) != 1 {
		t.Fatalf("asked the fleet %d times, want the offer alone: %+v",
			len(asker.asked), asker.asked)
	}
	got := asker.asked[0]
	if got.TurnID != "run-1" || got.Note != "use staging" || got.By != "ops" ||
		got.ByKind != iam.ActorHuman || got.OperatorID != "session:ops-1" ||
		got.NoteID != key || got.Probe {
		t.Errorf("sent %+v", got)
	}
}

// A RETRY OF ONE REQUEST IS ONE NOTE. The dashboard mints one key per gesture
// and sends it again, unchanged, when an answer never came — and the answer
// that never came is exactly the `unknown` a retry exists for: the note may
// have been taken. So both offers must carry ONE note id, the request's own,
// and the box that took the first answers the second `accepted` without
// taking it twice. A note id minted per call is a note the turn reads, and
// acts on, twice.
//
// AND ONLY THEN: two requests are two notes, however alike their text, and a
// call whose transport named no operation is a new note every time — a stable
// id there would answer a person's second, different gesture as the first.
func TestARetriedRequestIsOneNote(t *testing.T) {
	t.Parallel()
	// sent calls steer_turn once per key, each through a catalogue built
	// for its own call — which is how the act route serves a retry — and
	// answers the note id every offer carried, holding each answer's
	// `note_id` to the id that was offered.
	sent := func(t *testing.T, keys ...string) []string {
		t.Helper()
		asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
			AgentHandle: "swe", Status: steer.StatusAccepted}}}
		for i, key := range keys {
			res, out := steerCall(t, steerTool(t, asker, key),
				"run-1", "use staging")
			if res.Failed {
				t.Fatalf("call %d refused: %s", i, res.Output)
			}
			if len(asker.asked) != i+1 {
				t.Fatalf("call %d: the fleet was asked %d times in all, want one offer per call",
					i, len(asker.asked))
			}
			if offered := asker.asked[i].NoteID; out["note_id"] != offered || offered == "" {
				t.Fatalf("call %d answered note_id %v and offered %q", i, out["note_id"], offered)
			}
		}
		ids := make([]string, 0, len(asker.asked))
		for _, req := range asker.asked {
			ids = append(ids, req.NoteID)
		}
		return ids
	}

	t.Run("one request sent twice", func(t *testing.T) {
		t.Parallel()
		key := requestKey()
		if ids := sent(t, key, key); ids[0] != key || ids[1] != key {
			t.Errorf("a retry of request %q offered notes %q, want the request's own id both times",
				key, ids)
		}
	})
	t.Run("two requests", func(t *testing.T) {
		t.Parallel()
		first, second := requestKey(), requestKey()
		if ids := sent(t, first, second); ids[0] != first || ids[1] != second {
			t.Errorf("requests %q and %q offered notes %q, want each request's own id",
				first, second, ids)
		}
	})
	t.Run("a call naming no operation", func(t *testing.T) {
		t.Parallel()
		if ids := sent(t, "", ""); ids[0] == ids[1] {
			t.Errorf("two calls naming no operation offered one note %q twice: the "+
				"second gesture would be answered as the first", ids[0])
		}
	})
}

// NO REPLY IS UNKNOWN, NOT NOT RUNNING. A reply lost on its way back is
// indistinguishable from none, so the note may well have been taken: the
// person is told nobody confirmed it — which a retry settles, since the retry
// is the same note — and never that the turn is over, which nobody said.
func TestNoReplyIsUnknownNotNotRunning(t *testing.T) {
	t.Parallel()
	accepted := steer.Reply{Version: 1, TurnID: "run-1", AgentHandle: "swe",
		Status: steer.StatusAccepted}
	elsewhere := accepted
	elsewhere.Request = "somebody-elses"
	for name, asker := range map[string]*fleetAsker{
		"silence":             {},
		"an unreadable reply": {raw: [][]byte{[]byte("{not json")}},
		// A REPLY NOBODY CAN VOUCH FOR IS NO REPLY, however much it
		// promises: one not signed by the fleet, and the fleet's own
		// answer to another request.
		"a reply the fleet did not sign": {raw: [][]byte{signedReply(t,
			mustSteerWire(statelog.OneKey("k1", "not the fleet's material")), accepted)}},
		"the fleet's reply to another request": {raw: [][]byte{signedReply(t, steerWire, elsewhere)}},
		"another turn's reply": {replies: []steer.Reply{{TurnID: "run-2",
			Status: steer.StatusClosed}}},
		"a status this build does not know": {replies: []steer.Reply{{TurnID: "run-1",
			Status: "deferred"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, out := steerCall(t, steerTool(t, asker, requestKey()), "run-1", "use staging")
			if res.Failed {
				t.Fatalf("refused %s: %s", tools.RefusalOf(res), res.Output)
			}
			if out["outcome"] != "unknown" {
				t.Errorf("answered %v, want unknown", out)
			}
		})
	}
}

// WHAT THE RUNNING NODE SAID is what the person is told, each in the class
// their surface acts on.
func TestSteerTurnRefusesWhatTheRunningNodeRefused(t *testing.T) {
	t.Parallel()
	for status, want := range map[steer.Status]tools.Refusal{
		steer.StatusClosed:      tools.RefusalNotRunning,
		steer.StatusFull:        tools.RefusalConflict,
		steer.StatusUnsupported: tools.RefusalSteerUnsupported,
	} {
		asker := &fleetAsker{replies: []steer.Reply{{TurnID: "run-1", AgentHandle: "swe", Status: status}}}
		res, _ := steerCall(t, steerTool(t, asker, requestKey()), "run-1", "use staging")
		if !res.Failed || tools.RefusalOf(res) != want {
			t.Errorf("%s: answered failed=%v %s (%s), want %s", status, res.Failed,
				tools.RefusalOf(res), res.Output, want)
		}
	}
}

func TestSteerTurnRefusesABadNote(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{}
	tool := steerTool(t, asker, requestKey())
	for name, args := range map[string][2]string{
		"no turn":  {"", "use staging"},
		"no note":  {"run-1", "   "},
		"too long": {"run-1", strings.Repeat("a", steer.MaxNoteRunes+1)},
	} {
		res, _ := steerCall(t, tool, args[0], args[1])
		if !res.Failed || tools.RefusalOf(res) != tools.RefusalInvalid {
			t.Errorf("%s: answered %s (%s), want invalid", name, tools.RefusalOf(res),
				res.Output)
		}
	}
	if len(asker.asked) != 0 {
		t.Error("a note that was refused was scattered anyway")
	}
	// The ask itself failing is nothing sent — unavailable, not unknown —
	// when the queue is not live; and an error nothing marked is a fault of
	// this node, whose words are the log's, never the sentence's.
	failing := &fleetAsker{err: fmt.Errorf("no broker: %w", queue.ErrNotLive)}
	res, _ := steerCall(t, steerTool(t, failing, requestKey()), "run-1", "x")
	if tools.RefusalOf(res) != tools.RefusalUnavailable {
		t.Errorf("an ask on a queue that is not live answered %s", tools.RefusalOf(res))
	}
	broken := &fleetAsker{err: errors.New("ask crewlet.seat.steer: open a reply mailbox: nats://10.0.0.7:4222 refused")}
	res, _ = steerCall(t, steerTool(t, broken, requestKey()), "run-1", "x")
	if tools.RefusalOf(res) != tools.RefusalInternalError || strings.Contains(res.Output, "10.0.0.7") ||
		!strings.Contains(res.Output, "Nothing was sent") {
		t.Errorf("an ask that broke answered %s: %q", tools.RefusalOf(res), res.Output)
	}
}

// A LEAD STEERS THEIR REPORT'S TURN, and the fleet is asked whose turn it is
// FIRST: a probe that offers nothing, then the decision on the seat it names,
// then the note. Asked in the other order the note would be taken before
// anybody decided whether the caller may send it.
func TestALeadSteersTheirReportsTurnAfterAProbe(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
		Agent: ctoSeat(t).String(), AgentHandle: "agent-cto", Status: steer.StatusAccepted}}}
	rig := newSteerRig(t, asker, requestKey(), boundTo("jane"), seatLeads)

	res, out := steerCall(t, rig, "run-1", "use staging")
	if res.Failed || out["outcome"] != "pending" {
		t.Fatalf("the CTO's lead was answered %+v %v, want the note pending", res, out)
	}
	if len(asker.asked) != 2 || !asker.asked[0].Probe || asker.asked[0].Note != "" ||
		asker.asked[1].Probe || asker.asked[1].Note != "use staging" {
		t.Errorf("asked the fleet %+v, want a probe carrying no note, then the note", asker.asked)
	}
}

// AND NOBODY ELSE'S: a caller who leads ANOTHER seat is asked about — the
// probe runs, since leading somebody could have been leading this one — and
// refused on the turn's own seat with nothing offered; a caller who leads
// nobody, or a seat, is refused before the fleet hears anything at all.
func TestSteeringSomebodyElsesTurnIsRefused(t *testing.T) {
	t.Parallel()
	ceoLead := leadPairs{{"jane", "agent-ceo"}: true}
	for _, tc := range []struct {
		name   string
		caller iam.Principal
		chart  authz.Chart
		asks   int
	}{
		{"the lead of another seat", boundTo("jane"), ceoLead, 1},
		{"a colleague who leads nobody", boundTo("sam", iam.Apps...), ceoLead, 0},
		{"a seat holding every grant", iam.Principal{ID: uuid.New(), Kind: iam.KindSeat,
			Login: "agent-ceo", Seat: "agent-ceo", Stage: iam.StageActive,
			Grants: iam.AllGrants}, ceoLead, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asker := &fleetAsker{replies: []steer.Reply{{TurnID: "run-1",
				Agent: ctoSeat(t).String(), AgentHandle: "agent-cto",
				Status: steer.StatusAccepted}}}
			rig := newSteerRig(t, asker, requestKey(), tc.caller, tc.chart)
			res, _ := steerCall(t, rig, "run-1", "use staging")
			if !res.Failed || !errors.Is(res.Cause, builtin.ErrRefused) {
				t.Fatalf("answered %+v, want the authority's refusal", res)
			}
			if len(asker.asked) != tc.asks {
				t.Errorf("asked the fleet %d times, want %d: %+v", len(asker.asked),
					tc.asks, asker.asked)
			}
			for _, req := range asker.asked {
				if !req.Probe {
					t.Errorf("a refused caller's note was offered: %+v", req)
				}
			}
		})
	}
}

// A PROBE NOBODY ANSWERS, OR ONE NAMING NO SEAT, DECIDES NOTHING, and nothing
// is offered: `unavailable` rather than the offer's `unknown`, because no note
// is in flight to be uncertain about.
func TestAProbeThatSaysNothingSendsNothing(t *testing.T) {
	t.Parallel()
	for name, replies := range map[string][]steer.Reply{
		"no node answered": nil,
		"the node could not name its seat": {{TurnID: "run-1", AgentHandle: "agent-cto",
			Status: steer.StatusAccepted}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			asker := &fleetAsker{replies: replies}
			rig := newSteerRig(t, asker, requestKey(), boundTo("jane"), seatLeads)
			res, _ := steerCall(t, rig, "run-1", "use staging")
			if !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable {
				t.Fatalf("answered %+v, want unavailable", res)
			}
			if len(asker.asked) != 1 || !asker.asked[0].Probe {
				t.Errorf("asked the fleet %+v, want the probe alone", asker.asked)
			}
		})
	}
}

// A NOTE IS SIGNED AND BOUND, every time: the running node offers it as the
// person it names on this node's decision, so what crosses the broker is the
// fleet's — a fresh id per ask, so a probe and the offer after it are two
// requests, and a deadline the running node remembers the id until.
func TestEveryNoteIsSignedAndBound(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
		Agent: ctoSeat(t).String(), AgentHandle: "agent-cto", Status: steer.StatusAccepted}}}
	res, _ := steerCall(t, newSteerRig(t, asker, requestKey(), boundTo("jane"), seatLeads),
		"run-1", "use staging")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	if len(asker.asked) != 2 {
		t.Fatalf("asked the fleet %d times, want a probe and an offer", len(asker.asked))
	}
	probe, offer := asker.asked[0], asker.asked[1]
	if probe.ID == "" || offer.ID == "" || probe.ID == offer.ID {
		t.Errorf("the probe and the offer were bound to %q and %q, want an id each", probe.ID, offer.ID)
	}
	if probe.Deadline.IsZero() || offer.Deadline.After(time.Now().Add(builtin.SteerAskBudget)) {
		t.Errorf("deadlines %s and %s, want each within the ask's budget", probe.Deadline,
			offer.Deadline)
	}
}

// AN UNVERIFIED NOTE IS LOGGED ON THE ASKING NODE NAMING THE NODE THAT RUNS THE
// TURN AND THE KEYS IT HOLDS, as fields of their own: the running node used to
// write them as a sentence about "this node", which the asking node's line read
// as itself, beside advice to add the key "on the node running the turn".
// Mutation: drop either field from the line and this fails. Not parallel: it
// adds a destination to the process's logger.
func TestAnUnverifiedNoteIsLoggedWithTheNodeToAddTheKeyOn(t *testing.T) {
	var lines lockedLines
	debug := slog.LevelDebug
	logging.SetFile(logging.FileSink{Writer: &lines, Format: logging.FormatJSON, Level: &debug})
	t.Cleanup(func() { logging.SetFile(logging.FileSink{}) })
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-logged",
		Unverified: &steer.Unverified{Node: "node-2", HeldKeys: []string{"k0", "k9"}}}}}
	steerCall(t, newSteerRig(t, asker, requestKey(), operatorCaller, seatLeads),
		"run-logged", "use staging")
	var found bool
	for _, rec := range lines.records(t) {
		if rec["msg"] != "steer_turn_unverified" || rec["turn_id"] != "run-logged" {
			continue
		}
		found = true
		held, _ := rec["held_keys"].([]any)
		if rec["running_node"] != "node-2" ||
			!slices.Equal(held, []any{"k0", "k9"}) {
			t.Errorf("the line %v doesn't name the running node and the keys it holds", rec)
		}
	}
	if !found {
		t.Fatal("no steer_turn_unverified line was logged")
	}
}

// lockedLines collects what the process's logger writes, one JSON record a
// line, safe for the logger's writes and the test's read.
type lockedLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLines) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	scan := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for scan.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(scan.Bytes(), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}

// THE RUNNING NODE COULD NOT VERIFY THE NOTE: it took nothing, and the refusal
// says so rather than `unknown`, which a retry would never change, and under
// no wait, since only an operator bringing the keys into agreement does. It is
// a CONDITION ([steer.ErrUnverified]), so each reader has words of their own:
// the person's sentence says who fixes it, and the assistant that called the
// tool is told the same and that sending it again won't help. The keys and the
// turn go to the log: said to the person steering, they were a turn id and key
// ids nobody at a dashboard chose. On the probe as on the offer, and nothing is
// offered after a probe that says so.
// Mutation: put reply.Unverified back into the sentence, refuse on another
// cause, or let the condition clear on a wait, and this fails.
func TestANoteTheRunningNodeCouldNotVerifyIsRefusedAsUnavailable(t *testing.T) {
	t.Parallel()
	unverified := steer.Reply{Version: 1, TurnID: "run-1",
		Unverified: &steer.Unverified{Node: "node-2", HeldKeys: []string{"k0"}}}
	for name, caller := range map[string]iam.Principal{
		"the offer, by the deployment's operator": operatorCaller,
		"the probe, by a lead":                    boundTo("jane"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			asker := &fleetAsker{replies: []steer.Reply{unverified}}
			res, _ := steerCall(t, newSteerRig(t, asker, requestKey(), caller, seatLeads),
				"run-1", "use staging")
			if !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable ||
				!strings.Contains(res.Output, "an administrator") {
				t.Fatalf("answered %s: %q, want unavailable saying who fixes it",
					tools.RefusalOf(res), res.Output)
			}
			person, ok := builtin.ConditionSentence(res.Cause)
			if !ok || !strings.Contains(person, "your note") ||
				!strings.Contains(person, "An administrator") {
				t.Errorf("a person is told %q (%v), want who fixes it", person, ok)
			}
			for _, said := range []string{res.Output, person} {
				for _, leak := range []string{"run-1", "node-2", "k0", "secrets.keys"} {
					if strings.Contains(said, leak) {
						t.Errorf("the refusal %q names %q, which only the log should", said, leak)
					}
				}
			}
			if builtin.RetryAfter(res.Cause, time.Second) != 0 {
				t.Errorf("the refusal carries a wait, and no wait changes the keys")
			}
			if len(asker.asked) != 1 {
				t.Errorf("asked the fleet %d times, want once and nothing offered after", len(asker.asked))
			}
		})
	}
}
