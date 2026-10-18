package builtin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The operator catalogue's WRITES are what the dashboard's act transport and
// an operator's assistant both call, and both read the same two things off a
// successful answer: the call's `outcome` and its `position`, at the TOP of
// the answer. A tool that stated them only per record handed the act
// transport nothing, and it reported `applied` at no position over records
// whose outcome may have been `unknown`. And both retry an `unknown` write
// under the same identity — the `Idempotency-Key` the dashboard sends again,
// carried on the actor as the request's key, the `op_id` an assistant brings
// back — which is one write only if every operation the call derives is
// derived from it.
//
// So these walk EVERY write the catalogue serves rather than the ones
// somebody remembered: a tool added without an entry in [everyWriteCall]
// fails here until it has one.

// everyWriteCall is one successful call of every operator write, against the
// fakes [writeSurface] builds.
var everyWriteCall = map[string]map[string]any{
	worklet.CreateWorkItemTool: {"title": "Rotate the key", "project": "ENG",
		"labels": []any{"ops"}, "labels_create_missing": true, "waiting_on": []any{"ENG-2"}},
	worklet.UpdateWorkItemTool: {"item": "ENG-1", "status": "in_progress",
		"waiting_on": map[string]any{"add": []any{"ENG-3"}}},
	worklet.CommentOnWorkTool:      {"item": "ENG-1", "body": "on it"},
	worklet.MergeWorkItemTool:      {"item": "ENG-2", "into": "ENG-1"},
	worklet.SaveWorkViewTool:       {"container": "project:ENG", "name": "Mine", "type": "list"},
	worklet.WriteWorkCatalogueTool: {"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}, "fields": []any{}},
	worklet.SetPrioritiesTool:      {"items": []any{"ENG-1"}},
	worklet.SetPinsTool:            {"views": map[string]any{"add": []any{"v-1"}}},
	worklet.MarkInboxTool:          {"read": []any{"r-1"}},
	worklet.WriteProjectTool: {"project": "ENG",
		"tags_add": []any{map[string]any{"slug": "ops", "label": "Ops"}}, "default_assignee": ""},
	worklet.RemoveWorkItemTool:  {"item": "ENG-1"},
	worklet.RestoreWorkItemTool: {"item": "ENG-5"},
	worklet.MoveWorkItemTool:    {"item": "ENG-1", "project": "OPS"},
	worklet.PlaceWorkItemTool: {"item": "ENG-1", "before": "ENG-2",
		"status": "in_progress", "if_match": 7},
	builtin.WritePageTool:     {"title": "Runbook", "body": "step one", "container": "ENG"},
	builtin.SavePageTool:      {"page": "p1", "base_version": 4, "body": "step two", "title": "Deploy Book"},
	builtin.CommentOnPageTool: {"page": "p1", "body": "is this current?"},
	// THE CHAT GESTURES, every one the dashboard makes — the read-state
	// flush included, which appends nothing ([appendsNoRecord]).
	builtin.PostMessageTool:   {"channel": chatRoomID, "body": "standup moved"},
	builtin.ReplyInThreadTool: {"channel": chatRoomID, "reply_to": "m1", "body": "ok"},
	builtin.SendDMTool:        {"to": []any{"agent-ceo"}, "body": "a word"},
	builtin.ReactToMessageTool: {"channel": chatRoomID, "message_id": "m1",
		"emoji": ":+1:"},
	builtin.JoinChannelTool:  {"channel": chatRoomID},
	builtin.LeaveChannelTool: {"channel": chatRoomID},
	chatlet.EditMessageTool: {"channel": chatRoomID, "message_id": "m1",
		"body": "on it, by noon"},
	chatlet.DeleteMessageTool: {"channel": chatRoomID, "message_id": "m1"},
	chatlet.CreateChannelTool: {"channel": "launch", "kind": "private",
		"members": []any{"agent-ceo"}},
	chatlet.OpenDMTool:        {"to": []any{"agent-ceo"}},
	chatlet.UpdateChannelTool: {"channel": chatRoomID, "topic": "the launch"},
	chatlet.SetChannelMembersTool: {"channel": chatRoomID, "members": []any{"jane",
		map[string]any{"handle": "agent-ceo", "follow_all": true}}},
	chatlet.MarkChatReadTool: {"cursors": map[string]any{chatRoomID: 12.0},
		"mute": []any{chatRoomID}},
}

// appendsNoRecord are the operator writes that append nothing to any log, each
// with why — and every gate below holds them to THAT, explicitly, rather than
// leaving them out: an answer at the top is still what the act transport lifts,
// so it must say `applied` at no position, and no record may reach a writer.
//
// `mark_chat_read` writes a person's read state to the coordination store, a
// compare-and-set that has landed when it answers: there is no record whose
// outcome could be pending, no position a read could wait for, and no
// operation to derive — a cursor only advances and a mute states one room's,
// so the same flush twice is the same state. Its one `unknown` is a put whose
// answer was lost, which is the store's to report rather than a record's
// (TestAFlushWhoseAnswerWasLostIsUnknown).
var appendsNoRecord = map[string]string{
	chatlet.MarkChatReadTool: "a read-state flush is a compare-and-set in the " +
		"coordination store, on no log",
}

// answersAppliedAtNoPosition is the receipt a write that appends no record
// must answer with, and that it appended none.
func answersAppliedAtNoPosition(t *testing.T, name string, answer map[string]any,
	w *ledgerFake, callsBefore int) {

	t.Helper()
	if answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Errorf("%s answered outcome %v, want applied — it landed when it "+
			"answered: %v", name, answer["outcome"], answer)
	}
	if position, stated := answer["position"]; !stated || position != nil {
		t.Errorf("%s answered position %v, want null at the top — there is no "+
			"record a read could wait for: %v", name, answer["position"], answer)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls != callsBefore {
		t.Errorf("%s (%s) appended %d records", name, appendsNoRecord[name],
			w.calls-callsBefore)
	}
}

// EVERY WRITE ANSWERS FOR EVERY RECORD IT APPENDED, AT THE TOP. The writers
// here answer `pending`, each at a later position than the last, so a tool
// that lifted its first record's position — or answered per record only — is
// caught: the top of its answer must be `pending` at the LAST position the
// call was handed.
func TestEveryOperatorWriteAnswersForEveryRecordItAppended(t *testing.T) {
	t.Parallel()
	w := newLedgerFake(statelog.OutcomePending)
	catalogue := writeSurface(w)
	for name, tool := range catalogue {
		t.Run(name, func(t *testing.T) {
			w.mu.Lock()
			before := w.calls
			w.mu.Unlock()
			answer, last := w.call(t, tool, request(1), everyWriteCall[name])
			if _, none := appendsNoRecord[name]; none {
				answersAppliedAtNoPosition(t, name, answer, w, before)
				return
			}
			if answer["outcome"] != string(statelog.OutcomePending) {
				t.Errorf("%s answered outcome %v at the top, want pending — its "+
					"records were all pending, and the transport lifts this key "+
					"as the call's answer: %v", name, answer["outcome"], answer)
			}
			if answer["position"] != last.String() {
				t.Errorf("%s answered position %v at the top, want %s — the "+
					"LAST record it appended, which is what a caller barriers "+
					"on before its next read: %v", name, answer["position"], last, answer)
			}
		})
	}
}

// AND A RETRY IS THE SAME OPERATIONS. Every write sent twice under one request
// derives exactly the operations (and, through the knowledge base, the call
// keys) it derived the first time — so the broker collapses the second into
// the first — and under another request derives none of them.
func TestARetriedOperatorWriteDerivesTheSameOperations(t *testing.T) {
	t.Parallel()
	w := newLedgerFake(statelog.OutcomeApplied)
	catalogue := writeSurface(w)
	for name, tool := range catalogue {
		t.Run(name, func(t *testing.T) {
			first := w.opsOf(t, tool, request(11), everyWriteCall[name])
			retry := w.opsOf(t, tool, request(11), everyWriteCall[name])
			other := w.opsOf(t, tool, request(22), everyWriteCall[name])
			if why, none := appendsNoRecord[name]; none {
				// NOTHING TO DERIVE, AND NOTHING DERIVED: a repeat is
				// the same state rather than the same operation.
				if len(first)+len(retry)+len(other) != 0 {
					t.Errorf("%s (%s) derived operations %v, %v, %v", name,
						why, first, retry, other)
				}
				return
			}
			if len(first) == 0 {
				t.Fatalf("%s reached no writer at all, so this case certifies nothing", name)
			}
			if !slices.Equal(first, retry) {
				t.Errorf("%s derived %v and then, retried under the same "+
					"Idempotency-Key, %v — every operation that differs is written twice",
					name, first, retry)
			}
			for _, op := range other {
				if slices.Contains(first, op) {
					t.Errorf("%s wrote %q under two different Idempotency-Keys — the "+
						"second gesture is collapsed into the first and answered "+
						"`applied` without landing", name, op)
				}
			}
		})
	}
}

// AND ITS ANSWER NAMES THE REQUEST, NEVER AN `op_id`. A caller whose
// transport keyed the request makes the call again by repeating the request
// under its key, and an `op_id` beside one is refused — so a receipt carrying
// one hands it an instruction it cannot follow, and an `unknown` that says
// "make the same call with `op_id` …" sends it to a refusal.
func TestARequestsAnswerNamesTheRequestNotAnOpID(t *testing.T) {
	t.Parallel()
	for name, args := range everyWriteCall {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer, _ := newLedgerFake(statelog.OutcomeApplied).call(t,
				writeSurface(newLedgerFake(statelog.OutcomeApplied))[name],
				request(66), args)
			if op, carried := answer["op_id"]; carried {
				t.Errorf("%s's receipt under a request carries op_id %v: %v",
					name, op, answer)
			}

			w := newLedgerFake(statelog.OutcomeUnknown)
			tool := writeSurface(w)[name]
			if _, none := appendsNoRecord[name]; none {
				// NO LEDGER TO LOSE AN ACKNOWLEDGEMENT: a log whose every
				// write is unknown leaves this one applied at no position.
				answer, _ := w.call(t, tool, request(67), args)
				answersAppliedAtNoPosition(t, name, answer, w, 0)
				return
			}
			got, err := tool.Call(w.asked(t, tool, request(67), args), args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable ||
				!tools.UnknownOf(got) {
				t.Fatalf("%s with its write unknown answered %+v, want the "+
					"failed unknown", name, got)
			}
			// NEITHER OTHER CALLER'S REPEAT: the MCP one names an op_id, and
			// a seat's is a call made "before calling it with any others".
			for _, foreign := range []string{"op_id", "before calling it with any others"} {
				if strings.Contains(got.Output, foreign) {
					t.Errorf("%s tells a request's caller how another caller "+
						"repeats a call (%q): %s", name, foreign, got.Output)
				}
			}
		})
	}
	// AND WHERE THE REPEAT IS THE SAME OPERATION, it is the request's.
	w := newLedgerFake(statelog.OutcomeUnknown)
	tool := writeSurface(w)[worklet.UpdateWorkItemTool]
	args := everyWriteCall[worklet.UpdateWorkItemTool]
	got, err := tool.Call(w.asked(t, tool, request(68), args), args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if want := "send the same update_work_item request again — under the " +
		"same idempotency-key"; !strings.Contains(strings.ToLower(got.Output), want) {
		t.Errorf("an unknown update under a request does not say %q: %s",
			want, got.Output)
	}
}

// THE LESS CERTAIN OUTCOME WINS, over both of write_project's records and
// both of write_work_catalogue's. The first record is `pending` and the second
// applied: the call is `pending` at the second's position, never `applied`.
func TestATwoRecordWriteAnswersItsLessCertainOutcome(t *testing.T) {
	t.Parallel()
	for _, name := range []string{worklet.WriteProjectTool, worklet.WriteWorkCatalogueTool} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			w.script = []statelog.Outcome{statelog.OutcomePending, statelog.OutcomeApplied}
			answer, last := w.call(t, writeSurface(w)[name], request(33),
				everyWriteCall[name])
			if answer["outcome"] != string(statelog.OutcomePending) {
				t.Errorf("%s answered %v over a pending record and an applied one, "+
					"want pending — a write nobody has applied here yet is never "+
					"reported applied: %v", name, answer["outcome"], answer)
			}
			if answer["position"] != last.String() {
				t.Errorf("%s answered position %v, want the applied record's %s",
					name, answer["position"], last)
			}
		})
	}
}

// AN UNKNOWN RECORD IS NEVER A RECEIPT, first or second. A first record whose
// outcome is unknown stops the call before the second — whose write waits for
// it — and a second one after a first that landed says the first did; either
// way the answer is the failed unknown every tracker write gives, which the act
// transport tells a person is unknown rather than done.
func TestATwoRecordWriteWithAnUnknownRecordIsUnknown(t *testing.T) {
	t.Parallel()
	for _, name := range []string{worklet.WriteProjectTool, worklet.WriteWorkCatalogueTool} {
		for which, script := range map[string][]statelog.Outcome{
			"the first":  {statelog.OutcomeUnknown, statelog.OutcomeApplied},
			"the second": {statelog.OutcomeApplied, statelog.OutcomeUnknown},
		} {
			t.Run(name+"/"+which, func(t *testing.T) {
				t.Parallel()
				w := newLedgerFake(statelog.OutcomeApplied)
				w.script = script
				got, err := writeSurface(w)[name].Call(
					w.asked(t, writeSurface(w)[name], request(34), everyWriteCall[name]),
					everyWriteCall[name])
				if err != nil {
					t.Fatalf("call: %v", err)
				}
				if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable ||
					!tools.UnknownOf(got) || !strings.Contains(got.Output, "is unknown") {
					t.Fatalf("%s with %s record unknown answered %+v, want the "+
						"failed unknown", name, which, got)
				}
				w.mu.Lock()
				calls := w.calls
				w.mu.Unlock()
				if which == "the first" && calls != 1 {
					t.Errorf("%s wrote its second record over a first whose outcome "+
						"is unknown (%d writes)", name, calls)
				}
				if which == "the second" && !strings.Contains(got.Output, " written") {
					t.Errorf("%s does not say its first record landed: %s", name, got.Output)
				}
			})
		}
	}
}

// A SECOND RECORD REFUSED AFTER A FIRST THAT LANDED says so. The first list is
// replaced on every node, so "the change was NOT made" would send a caller to
// redo — or to report undone — a change that happened.
func TestARefusedSecondRecordReportsTheFirstLanded(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]string{
		worklet.WriteProjectTool:       "The tag change was written",
		worklet.WriteWorkCatalogueTool: "The types were replaced",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			w.failAt = 2
			tool := writeSurface(w)[name]
			got, err := tool.Call(w.asked(t, tool, request(44), everyWriteCall[name]),
				everyWriteCall[name])
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict {
				t.Fatalf("%s answered %+v, want the second record's conflict", name, got)
			}
			if !strings.Contains(got.Output, landed) || !strings.Contains(got.Output, "CREWLET_TRACKER_LOG@1:1") {
				t.Errorf("%s's refusal does not say the first record landed, and where: %q",
					name, got.Output)
			}
		})
	}
}

// ---- the surface ------------------------------------------------------- //

// request is the nth request key a test sends: an operation id in the
// engine's grammar, as internal/api/opkey holds every `Idempotency-Key` to,
// since every id derived from it carries its instant.
func request(n int) string {
	return fmt.Sprintf("0192a1b2-c3d4-7e5f-8a6b-%012d", n)
}

type requestKeyCtx struct{}

// withRequestKey is ctx carrying the key a transport named one request by —
// what a keyed surface puts on the actors it hands the tools.
func withRequestKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, requestKeyCtx{}, key)
}

// requestKeyOf reads the key [withRequestKey] stored, or "".
func requestKeyOf(ctx context.Context) string {
	key, _ := ctx.Value(requestKeyCtx{}).(string)
	return key
}

// writeSurface is the operator catalogue's writes over one recording fake,
// with the actors carrying the request's key the context names — the shape a
// keyed surface builds: the tracker's as [builtin.Actor.WorkKey] with its
// instant, the knowledge base's as [doclet.Actor.OpKey].
func writeSurface(w *ledgerFake) map[string]tools.Callable {
	trk := newFakeTracker()
	trk.tasks["ENG-5"] = worklet.TaskDetail{
		Task: worklet.Task{ID: "id-5", Key: "ENG-5", Project: "ENG",
			Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
			Removed: &worklet.Tombstone{By: "ops", At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}},
		Complete: true,
	}
	kb := newFakeKB()
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: func(builtin.Actor) builtin.WorkWriter { return w },
			Merges:          func(builtin.Actor) builtin.WorkMerger { return w },
			Dependencies:    func(builtin.Actor) builtin.WorkDepender { return w },
			Search:          trk,
			ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return w },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return w },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return w },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return w },
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return w },
			Moves:           func(builtin.Actor) builtin.WorkMover { return w },
			Placer:          func(builtin.Actor) builtin.WorkPlacer { return w },
			Inbox:           trk,
			Actor: func(ctx context.Context, _ *turnctx.Turn) (builtin.Actor, error) {
				key := requestKeyOf(ctx)
				since, _ := statelog.OpMintedAt(key)
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator,
					OperatorID: "ops", WorkKey: key, WorkSince: since}, nil
			},
		},
		Pages: builtin.PageDeps{
			Reader: kb, Writer: w,
			Actor: func(ctx context.Context, _ *turnctx.Turn) (doclet.Actor, error) {
				return doclet.Actor{Handle: "ops", Kind: doclet.AuthorOperator,
					OperatorID: "ops", OpKey: requestKeyOf(ctx)}, nil
			},
		},
		// THE BOUND SEAT AS AUTHOR, KIND HUMAN AND THE CREDENTIAL BESIDE
		// IT, with the request's key on the actor — the attribution the
		// operator surface's chat actor builds. A reader, which the
		// gestures decided after a read are served with; every call here
		// names its room by id, and the caller holds admin, so none reads.
		// And a read-state store, so the one write that appends no record
		// is served and held to that ([appendsNoRecord]).
		Chat: builtin.ChatDeps{
			Reader: newFakeChat(), Writer: w, Cursors: fakeCursors{},
			Actor: func(ctx context.Context, _ *turnctx.Turn) (chatlet.Actor, error) {
				return chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorHuman,
					OperatorID: "ops", OpKey: requestKeyOf(ctx)}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if !mcp.ReadOnlyProven(builtin.AnnotationsFor(tool.Name())) {
			out[tool.Name()] = tool
		}
	}
	return out
}

// TestTheWriteTableCoversEveryOperatorWrite keeps [everyWriteCall] honest in
// both directions: a write the catalogue serves with no entry is a write the
// two cases above never call, and an entry the catalogue does not serve is a
// case certifying a tool nobody can reach.
func TestTheWriteTableCoversEveryOperatorWrite(t *testing.T) {
	t.Parallel()
	served := writeSurface(newLedgerFake(statelog.OutcomeApplied))
	var missing, extra []string
	for name := range served {
		if _, ok := everyWriteCall[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range everyWriteCall {
		if _, ok := served[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("the operator writes and this table disagree: served with no "+
			"call %v, a call for nothing served %v", missing, extra)
	}
}

// ---- the recording writer ---------------------------------------------- //

// ledgerFake is every write seam the operator catalogue has, recording the
// operation (or, for a page, the actor's key and the verb) each write arrives
// with and answering each at the next position on its log.
type ledgerFake struct {
	mu      sync.Mutex
	outcome statelog.Outcome
	// script, when set, is the outcome of the Nth write in order.
	script []statelog.Outcome
	// failAt refuses the Nth write with a conflict, 1-based; 0 never.
	failAt int
	// stop, when set, is what a move, a removal or a restore answers beside
	// the record its root landed: the walk over the rest stopped.
	stop  error
	seq   uint64
	calls int
	ops   []string
}

func newLedgerFake(outcome statelog.Outcome) *ledgerFake {
	return &ledgerFake{outcome: outcome}
}

// asked is the context one call of one tool under a request carries: the
// request's key, and the principal every gated tool is decided for.
func (f *ledgerFake) asked(t *testing.T, _ tools.Callable, request string,
	_ map[string]any) context.Context {

	t.Helper()
	if err := statelog.CheckCallerOpID(request); err != nil {
		t.Fatalf("request key %q: %v", request, err)
	}
	// A PERSON, because the operator surface's callers are people and
	// machines and never a seat — and place_work_item is human-only —
	// BOUND TO A HUMAN SEAT, because every chat verb is made as one.
	return withRequestKey(personBoundTo("jane", iam.AllGrants...), request)
}

// call runs one tool under a request and answers its decoded result and the
// last position any write in the call was handed.
func (f *ledgerFake) call(t *testing.T, tool tools.Callable, request string,
	args map[string]any) (map[string]any, statelog.Position) {

	t.Helper()
	got, err := tool.Call(f.asked(t, tool, request, args), args)
	if err != nil || got.Failed {
		t.Fatalf("%s failed: %v %q", tool.Name(), err, got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered something that is not JSON: %q", tool.Name(), got.Output)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return answer, f.position(f.seq)
}

// opsOf runs one tool under a request and answers the operations it derived.
func (f *ledgerFake) opsOf(t *testing.T, tool tools.Callable, request string,
	args map[string]any) []string {

	t.Helper()
	f.mu.Lock()
	from := len(f.ops)
	f.mu.Unlock()
	if got, err := tool.Call(f.asked(t, tool, request, args), args); err != nil || got.Failed {
		t.Fatalf("%s failed: %v %q", tool.Name(), err, got.Output)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops[from:])
}

func (f *ledgerFake) position(seq uint64) statelog.Position {
	return statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: seq}
}

// record takes one write and answers it.
func (f *ledgerFake) record(op string) (statelog.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		return statelog.Result{}, fmt.Errorf("%w: ENG kept changing", statelog.ErrConflict)
	}
	f.ops = append(f.ops, op)
	outcome := f.outcome
	if f.calls <= len(f.script) {
		outcome = f.script[f.calls-1]
	}
	if outcome == statelog.OutcomeUnknown {
		// AN UNKNOWN HAS NO POSITION, which is the whole content of it.
		return statelog.Result{Outcome: outcome, OpID: op}, nil
	}
	f.seq++
	at := f.position(f.seq)
	return statelog.Result{Outcome: outcome, Position: at, OpID: op, Version: at.Packed()}, nil
}

func (f *ledgerFake) written(op string) (worklet.WriteResult, error) {
	r, err := f.record(op)
	return worklet.WriteResult{Result: r}, err
}

func (f *ledgerFake) CreateTask(_ context.Context, opID string, _ worklet.Task,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	r, err := f.written(opID)
	r.Key = "ENG-9"
	return r, err
}

func (f *ledgerFake) CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
	_ worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error) {
	return f.CreateTask(ctx, opID, task, notify)
}

func (f *ledgerFake) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	_ worklet.TaskPatch, _ worklet.ChangeKind, _ *worklet.Notify) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) Depend(_ context.Context, opID string, _ worklet.DependencyChange,
	_ worklet.Leads) (worklet.DependencyResult, error) {
	r, err := f.written(opID)
	return worklet.DependencyResult{WriteResult: r}, err
}

func (f *ledgerFake) MergeDuplicates(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return f.stopped(f.written(opID))
}

func (f *ledgerFake) RestoreTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return f.stopped(f.written(opID))
}

// stopped is a walk's answer: its root's record, and [ledgerFake.stop] where
// one is set.
func (f *ledgerFake) stopped(r worklet.WriteResult, err error) (worklet.WriteResult, error) {
	if err == nil && f.stop != nil {
		return r, f.stop
	}
	return r, err
}

// PlaceTask appends the two records a drag across lanes is, each at its own
// derived step — the way the tracker's own sequence derives them.
func (f *ledgerFake) PlaceTask(_ context.Context, opID string, place worklet.Place,
	_ *worklet.Notify) (worklet.PlaceResult, error) {
	out := worklet.PlaceResult{Version: int64(place.IfMatch)}
	if place.Status != nil {
		lane, err := f.written(statelog.StepOpID(opID, "lane"))
		if err != nil {
			return worklet.PlaceResult{}, err
		}
		out.Lane, out.Version = lane, lane.Version
	}
	order, err := f.written(statelog.StepOpID(opID, "order"))
	if err != nil {
		return worklet.PlaceResult{}, err
	}
	out.Order, out.Rank = order, "a0V"
	return out, nil
}

// MoveTaskToProject is the cross-project move: one record here, at the move's
// own operation.
func (f *ledgerFake) MoveTaskToProject(_ context.Context, opID, _, target string,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	r, err := f.written(opID)
	r.Key = target + "-3"
	return f.stopped(r, err)
}

func (f *ledgerFake) WriteView(_ context.Context, opID string, _ worklet.View,
	_ worklet.ViewPrior) (worklet.WriteResult, error) {
	return f.written(opID)
}

// ViewPrior holds every view a case names, as a shared tab on ENG: a save
// naming an id no view holds is refused before it writes anything.
func (f *ledgerFake) ViewPrior(context.Context, string) (worklet.ViewPrior, error) {
	return worklet.ViewPrior{Held: true,
		Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}}, nil
}

func (f *ledgerFake) WriteTypes(_ context.Context, opID string, _ []worklet.TaskType) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteFields(_ context.Context, opID string, _ []worklet.FieldDef) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) MarkInbox(_ context.Context, opID, _ string,
	_ worklet.InboxGesture, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WritePins(_ context.Context, opID, _ string,
	_ worklet.PinGesture, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ *uint64, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteProject(_ context.Context, opID, _ string, _ worklet.ProjectEdit,
	_ worklet.ProjectAuthority) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) WriteTags(_ context.Context, opID, _ string, _ worklet.TagEdit,
	_ worklet.TagAuthority) (worklet.WriteResult, error) {
	return f.written(opID)
}

func (f *ledgerFake) EnsureTags(_ context.Context, opID, _ string, tags []string) ([]string, []caveat.Caveat, error) {
	if _, err := f.record(opID); err != nil {
		return nil, nil, err
	}
	return tags, nil, nil
}

// The knowledge base's writes. Their operation is derived INSIDE the store
// from the actor's key ([doclet.Actor.OpKey]), so the key is what is recorded,
// keyed with the verb, so a save and its rename under one key are still two
// entries.

// The chat writes. Their operation is derived INSIDE the store from the
// actor's key, as a page write's is, so the key is what is recorded — with the
// verb, so a direct message's open and post under one key are two entries.

func (f *ledgerFake) chatWrite(verb string, actor chatlet.Actor) (chatlet.Written, error) {
	r, err := f.record(verb + ":" + actor.OpKey)
	return chatlet.Written{
		Channel:  chatlet.Channel{ID: chatRoomID, Name: chatRoomName, Kind: chatlet.KindPublic},
		Revision: uint64(r.Position.Packed()), ChangeID: r.OpID, Outcome: r,
	}, err
}

func (f *ledgerFake) Post(_ context.Context, actor chatlet.Actor, _ string,
	_ chatlet.NewMessage) (chatlet.Written, error) {
	return f.chatWrite("post", actor)
}

func (f *ledgerFake) Reply(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.NewMessage) (chatlet.Written, error) {
	return f.chatWrite("reply", actor)
}

func (f *ledgerFake) OpenDirect(_ context.Context, actor chatlet.Actor, _ []string) (
	chatlet.Written, bool, error) {
	w, err := f.chatWrite("open", actor)
	return w, err == nil, err
}

func (f *ledgerFake) React(_ context.Context, actor chatlet.Actor, _, _, _ string) (
	chatlet.Written, error) {
	return f.chatWrite("react", actor)
}

func (f *ledgerFake) Unreact(_ context.Context, actor chatlet.Actor, _, _, _ string) (
	chatlet.Written, error) {
	return f.chatWrite("unreact", actor)
}

func (f *ledgerFake) Join(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return f.chatWrite("join", actor)
}

func (f *ledgerFake) Leave(_ context.Context, actor chatlet.Actor, _ string) (chatlet.Written, error) {
	return f.chatWrite("leave", actor)
}

func (f *ledgerFake) CreateChannel(_ context.Context, actor chatlet.Actor,
	_ chatlet.NewChannel) (chatlet.Written, error) {
	return f.chatWrite("create", actor)
}

func (f *ledgerFake) PatchChannel(_ context.Context, actor chatlet.Actor, _ string,
	_ chatlet.ChannelPatch, _ chatlet.ChannelAuthority) (chatlet.Written, error) {
	return f.chatWrite("patch", actor)
}

func (f *ledgerFake) SetMembers(_ context.Context, actor chatlet.Actor, _ string,
	_ []chatlet.Member) (chatlet.Written, error) {
	return f.chatWrite("members", actor)
}

func (f *ledgerFake) Edit(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.EditMessage) (chatlet.Written, error) {
	return f.chatWrite("edit", actor)
}

func (f *ledgerFake) Delete(_ context.Context, actor chatlet.Actor, _, _ string,
	_ chatlet.MessageAuthority) (chatlet.Written, error) {
	return f.chatWrite("delete", actor)
}

func (f *ledgerFake) pageWrite(verb, key string) (doclet.Written, error) {
	r, err := f.record(verb + ":" + key)
	return doclet.Written{Page: doclet.Page{ID: "p1", Version: 5}, Revision: uint64(r.Position.Packed()),
		ChangeID: r.OpID, Outcome: r}, err
}

func (f *ledgerFake) Create(_ context.Context, actor doclet.Actor, _ doclet.NewPage) (doclet.Written, error) {
	return f.pageWrite("create", actor.OpKey)
}

func (f *ledgerFake) SavePage(_ context.Context, actor doclet.Actor, _ string, _ doclet.Save) (doclet.Written, error) {
	return f.pageWrite("save", actor.OpKey)
}

func (f *ledgerFake) Rename(_ context.Context, actor doclet.Actor, _, _ string,
	_ bool) (doclet.Written, error) {
	return f.pageWrite("rename", actor.OpKey)
}

func (f *ledgerFake) EditComment(_ context.Context, actor doclet.Actor, _, commentID,
	_ string) (doclet.Comment, doclet.Written, error) {
	w, err := f.pageWrite("edit", actor.OpKey)
	return doclet.Comment{ID: commentID}, w, err
}

func (f *ledgerFake) Comment(_ context.Context, actor doclet.Actor, _ string,
	_ doclet.NewComment) (doclet.Comment, doclet.Written, error) {
	w, err := f.pageWrite("comment", actor.OpKey)
	return doclet.Comment{ID: "m1"}, w, err
}

// TWO DIFFERENT CALLS UNDER ONE REQUEST ARE TWO OPERATIONS. A turn's calls
// share its work key, and a verb that writes one shared object — a view, a
// project's tags or policy, the catalogue's lists — derived its operation
// from the object alone, so inside the broker's duplicate window the second
// change was collapsed into the first and answered `applied` without landing.
// save_work_view did it for EVERY caller: `view-<id>` was one fixed operation
// for the life of the view. The same call sent again is still one operation —
// a request's key is a unit of work as a turn's trigger is, and every id is
// derived from it with a digest of the call's own arguments.
func TestTwoDifferentCallsUnderOneKeyAreTwoOperations(t *testing.T) {
	t.Parallel()
	cases := map[string][2]map[string]any{
		worklet.SaveWorkViewTool: {
			{"id": "v-1", "container": "project:ENG", "name": "Mine", "type": "list"},
			{"id": "v-1", "container": "project:ENG", "name": "Mine, this week", "type": "list"},
		},
		worklet.WriteProjectTool: {
			{"project": "ENG", "tags_add": []any{map[string]any{"slug": "ops", "label": "Ops"}}},
			{"project": "ENG", "tags_archive": []any{"ops"}},
		},
		worklet.WriteWorkCatalogueTool: {
			{"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}},
			{"types": []any{map[string]any{"slug": "bug", "name": "Defect"}}},
		},
		worklet.CreateWorkItemTool: {
			{"title": "One", "project": "ENG", "labels": []any{"ops"}, "labels_create_missing": true},
			{"title": "Two", "project": "ENG", "labels": []any{"sre"}, "labels_create_missing": true},
		},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newLedgerFake(statelog.OutcomeApplied)
			tool := writeSurface(w)[name]
			key := request(55)
			first := w.opsOf(t, tool, key, calls[0])
			second := w.opsOf(t, tool, key, calls[1])
			again := w.opsOf(t, tool, key, calls[1])
			for _, op := range second {
				if slices.Contains(first, op) {
					t.Errorf("two different %s calls under one key both wrote %q — "+
						"the broker collapses the second and it never lands", name, op)
				}
			}
			if !slices.Equal(second, again) {
				t.Errorf("one %s call sent twice under one key wrote under %v and %v",
					name, second, again)
			}
		})
	}
}
