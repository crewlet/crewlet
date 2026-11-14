package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// deliverySurface is a plain one-write-tool surface, which the citation check
// judges against.
func deliverySurface() turn.Surface {
	return turn.Surface{
		Catalogue:  []string{"slack_post", "slack_history", "lookup_colleague"},
		Deliveries: map[string]string{"slack_post": "test"},
		KnownReads: []string{"slack_history"},
	}
}

// workTool builds a submit_work tool over a scripted record of the turn: what
// was called, and who is waiting. Both are facts the model does not control,
// which is the whole point — the account a phase gives of itself is exactly
// what a phase can get wrong.
func workTool(reply turn.Reply, calls ...ledger.Call) *structured.Tool[workPayload] {
	return structured.New(SubmitWorkTool, submitWorkDescription, workSchema,
		decodeWork(reply,
			func() []ledger.Call { return calls },
			func() turn.Surface { return deliverySurface() }))
}

func args(t *testing.T, blob string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("test fixture is not JSON: %v", err)
	}
	return m
}

func TestASubmissionCapturesThePhasesAnswer(t *testing.T) {
	t.Parallel()
	// The tool does not DO anything: the phase's answer IS the arguments it
	// was called with, so the loop's ordinary tool machinery carries it out
	// and nothing needs a side channel.
	tool := workTool(turn.ToolReply(""), ledger.Call{Name: "slack_post"})
	if _, called := tool.Value(); called {
		t.Fatal("a fresh tool reports a submission")
	}
	res, err := tool.Call(context.Background(), args(t, `{
		"outcome": "delivered",
		"summary": "posted the weekly summary",
		"deliveries": ["slack_post"]
	}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("a valid submission failed: %s", res.Output)
	}
	got, called := tool.Value()
	if !called {
		t.Fatal("a valid submission was not captured")
	}
	if got.Outcome != "delivered" || len(got.Deliveries) != 1 {
		t.Errorf("captured %+v", got)
	}
}

func TestAnInvalidSubmissionGoesBackToTheModel(t *testing.T) {
	t.Parallel()
	// It is the one tool failure a model can reliably fix, and refusing the
	// turn over a malformed submission throws away everything the phase
	// already did.
	tool := workTool(turn.NoReply())
	res, err := tool.Call(context.Background(), args(t, `{"outcome":"wat","summary":"s"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an invalid outcome was accepted")
	}
	if !strings.Contains(res.Output, "delivered, no_action or blocked") {
		t.Errorf("the failure does not say what is allowed: %s", res.Output)
	}
	if _, called := tool.Value(); called {
		t.Error("an invalid submission was captured anyway")
	}
}

func TestTheLastSubmissionWins(t *testing.T) {
	t.Parallel()
	// A model that submits twice has corrected itself. Rejecting the second
	// leaves the engine acting on the draft the model just replaced.
	tool := workTool(turn.NoReply())
	ctx := context.Background()
	if _, err := tool.Call(ctx, args(t, `{"outcome":"blocked","summary":"a","evidence":"e"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	res, err := tool.Call(ctx, args(t, `{"outcome":"blocked","summary":"b","evidence":"e"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Failed {
		t.Fatalf("the second submission was refused: %s", res.Output)
	}
	got, _ := tool.Value()
	if got.Summary != "b" {
		t.Errorf("captured %q, want the correction", got.Summary)
	}
}

func TestAnAbsentOutcomeTakesTheCommonCase(t *testing.T) {
	t.Parallel()
	// `delivered` is what every other field is written for, and `done` is
	// the unremarkable review. Refusing the most predictable omission would
	// throw away a round of real work — and the engine checks the delivery
	// claim against the record either way.
	decode := decodeWork(turn.ToolReply(""),
		func() []ledger.Call { return []ledger.Call{{Name: "slack_post"}} },
		deliverySurface)
	w, err := decode(args(t, `{"summary":"posted it","deliveries":["slack_post"]}`))
	if err != nil {
		t.Fatalf("decodeWork: %v", err)
	}
	if turn.Outcome(w.Outcome) != turn.OutcomeDelivered {
		t.Errorf("outcome = %q, want delivered", w.Outcome)
	}
	r, err := decodeReview(args(t, `{"final_artifact":"done here"}`))
	if err != nil {
		t.Fatalf("decodeReview: %v", err)
	}
	if r.Decision != "done" {
		t.Errorf("decision = %q, want done", r.Decision)
	}
}

// A submission with no summary tells the reviewer and the next turn nothing at
// all, and it is the one field every outcome needs.
func TestASubmissionWithNoSummaryIsRefused(t *testing.T) {
	t.Parallel()
	tool := workTool(turn.NoReply())
	for _, blob := range []string{`{"outcome":"no_action"}`, `{"outcome":"no_action","summary":"  "}`} {
		res, _ := tool.Call(context.Background(), args(t, blob))
		if !res.Failed {
			t.Errorf("%s was accepted", blob)
		}
	}
}

// SILENCE IS NOT A DECLINE. The requester cannot tell it from a message that
// was lost, and the engine knows one arrived — so this is refused where the
// model can still act on it rather than corrected a phase later.
func TestNoActionIsRefusedOnAnAwaitedTurn(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.ToolReply(""), turn.EngineReply()} {
		tool := workTool(reply)
		res, _ := tool.Call(context.Background(),
			args(t, `{"outcome":"no_action","summary":"not for me"}`))
		if !res.Failed {
			t.Errorf("%s: no_action was accepted on a turn somebody asked for", reply)
		}
		if !strings.Contains(res.Output, "even to decline") {
			t.Errorf("%s: the refusal does not say what to do instead: %s", reply, res.Output)
		}
	}
	// The counterfactual: nobody asking makes it the right answer, and
	// without this the assertion above passes for a decoder that refuses
	// no_action outright.
	tool := workTool(turn.NoReply())
	if res, _ := tool.Call(context.Background(),
		args(t, `{"outcome":"no_action","summary":"a broadcast"}`)); res.Failed {
		t.Errorf("no_action was refused on an unaddressed turn: %s", res.Output)
	}
}

// "Blocked" with no account of what was tried is a round the reviewer can only
// send back blind.
func TestBlockedNeedsEvidence(t *testing.T) {
	t.Parallel()
	tool := workTool(turn.NoReply())
	res, _ := tool.Call(context.Background(), args(t, `{"outcome":"blocked","summary":"stuck"}`))
	if !res.Failed {
		t.Fatal("blocked was accepted with no evidence")
	}
	if !strings.Contains(res.Output, "what did you try") {
		t.Errorf("the refusal does not say what is missing: %s", res.Output)
	}
	if res, _ := tool.Call(context.Background(),
		args(t, `{"outcome":"blocked","summary":"stuck","evidence":"the channel 404s"}`)); res.Failed {
		t.Errorf("blocked with evidence was refused: %s", res.Output)
	}
}

// CHECKED INSIDE THE LOOP, where a wrong claim costs one bounced tool call the
// model can fix — instead of a whole review round or a silently accepted
// no-op.
func TestADeliveryClaimMustNameACallThatHappened(t *testing.T) {
	t.Parallel()
	called := []ledger.Call{{Name: "slack_post"}, {Name: "slack_history"}}
	decode := decodeWork(turn.ToolReply(""), func() []ledger.Call { return called }, deliverySurface)

	if _, err := decode(args(t, `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`)); err != nil {
		t.Errorf("a real delivery was refused: %v", err)
	}
	// A name the model MEANT to call. The refusal lists what is citable,
	// because a bare "no" sends it round the same loop.
	err := mustErr(t, decode, `{"outcome":"delivered","summary":"s","deliveries":["slack_postMessage"]}`)
	if !strings.Contains(err.Error(), "slack_post") {
		t.Errorf("the refusal does not list what is citable: %v", err)
	}
	// A read is not a delivery, however successfully it ran.
	mustErr(t, decode, `{"outcome":"delivered","summary":"s","deliveries":["slack_history"]}`)
	// And a failed call did not deliver.
	failed := decodeWork(turn.ToolReply(""),
		func() []ledger.Call { return []ledger.Call{{Name: "slack_post", Failed: true}} },
		deliverySurface)
	mustErr(t, failed, `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`)
}

// Nothing citable at all is a different message: the model has not called an
// outward tool yet, and telling it to pick from an empty list is useless.
//
// HOW TO DELIVER, AND HOW TO FIND A TOOL ONLY WHERE ONE CAN BE FOUND: an
// executor holds every tool it can reach, so its refusal says to call the tool
// that delivers and names no discovery tool, which is on no executor's surface;
// a phase that reaches its MCP tools by discovery is told how to find one.
//
// Mutation: name the discovery pair whatever the surface, and the executor is
// sent to two tools it does not hold.
func TestNothingDeliveredYetSaysSoRatherThanListingNothing(t *testing.T) {
	t.Parallel()
	read := func() []ledger.Call { return []ledger.Call{{Name: "slack_history"}} }
	claim := `{"outcome":"delivered","summary":"s","deliveries":["slack_post"]}`
	err := mustErr(t, decodeWork(turn.ToolReply(""), read, deliverySurface), claim)
	if !strings.Contains(err.Error(), "nothing has been delivered yet") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "Call the tool that delivers") {
		t.Errorf("the refusal does not say how to deliver: %v", err)
	}
	if strings.Contains(err.Error(), "list_mcp_server_tools") {
		t.Errorf("a surface with nothing to discover was sent to discovery: %v", err)
	}

	discovering := func() turn.Surface {
		s := deliverySurface()
		s.Discoverable = []string{"slack_post"}
		return s
	}
	err = mustErr(t, decodeWork(turn.ToolReply(""), read, discovering), claim)
	if !strings.Contains(err.Error(), "list_mcp_server_tools") {
		t.Errorf("a phase that discovers its MCP tools is not told how to find one: %v", err)
	}
}

// A REFUSAL ON THE TRACKER NAMES THE TRACKER'S OWN TOOLS, and claims a write
// elsewhere only when there was one.
//
// The refusal used to say "a call that reached somewhere else does not answer
// the person waiting … `list_mcp_server_tools` and `activate_tool` will find
// it" to every turn waiting on a named surface. A seat woken by a founder's
// comment, whose only calls were reads, was told both: that it had written
// somewhere, and to find the tracker's tool through discovery — which lists
// MCP servers' tools and never the tracker's. It found the one server it held
// and posted in chat.
func TestARefusalOnTheTrackerNamesTheTrackersOwnTools(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{"get_work_item", "comment_on_work_item", "mattermost_post_message"},
		Deliveries: map[string]string{
			"comment_on_work_item":    "work",
			"mattermost_post_message": "mattermost",
		},
		KnownReads:   []string{"get_work_item"},
		Discoverable: []string{"mattermost_post_message"},
	}
	surface := func() turn.Surface { return s }
	claim := `{"outcome":"delivered","summary":"s","deliveries":["comment_on_work_item"]}`

	readOnly := decodeWork(turn.ToolReply("work"),
		func() []ledger.Call { return []ledger.Call{{Name: "get_work_item"}} }, surface)
	err := mustErr(t, readOnly, claim)
	if !strings.Contains(err.Error(), "comment_on_work_item") {
		t.Errorf("the refusal does not name the tool that delivers on the tracker: %v", err)
	}
	for _, wrong := range []string{"list_mcp_server_tools", "somewhere else"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("the refusal says %q to a turn that only read: %v", wrong, err)
		}
	}

	chatted := decodeWork(turn.ToolReply("work"),
		func() []ledger.Call { return []ledger.Call{{Name: "mattermost_post_message"}} }, surface)
	elsewhere := mustErr(t, chatted, claim)
	if !strings.Contains(elsewhere.Error(), "somewhere else") {
		t.Errorf("the refusal does not say the turn's write landed elsewhere: %v", elsewhere)
	}

	// BOTH OPEN WITH THE ONE EXPORTED CLAUSE, which is what a scripted model
	// recognises the refusal by: a copy of the sentence that drifted from
	// this one never sees it, and answers the same claim until the rounds
	// run out.
	for _, refused := range []error{err, elsewhere} {
		if !strings.HasPrefix(refused.Error(), NotDeliveredOn("work")) {
			t.Errorf("a refusal naming the surface does not open with %q: %v",
				NotDeliveredOn("work"), refused)
		}
	}
}

// A REFUSAL CLAIMS A WRITE ELSEWHERE ONLY WHERE THE ENGINE KNOWS OF ONE. A
// turn waiting on the tracker that only searched through an MCP tool that no
// hint marks as a read or as a write, which the delivery check counts as a
// delivery to that server, is not told "a call that reached somewhere else":
// it is told what it called and that none of it reached the person waiting.
//
// Mutation: key the elsewhere case on turn.Delivered again, and the search is
// told it reached somewhere else.
func TestACitationRefusalDoesNotCallAPresumedWriteOne(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue:      []string{"comment_on_work_item", "jira_search_issues"},
		Deliveries:     map[string]string{"comment_on_work_item": "work", "jira_search_issues": "jira"},
		PresumedWrites: []string{"jira_search_issues"},
	}
	claim := `{"outcome":"delivered","summary":"s","deliveries":["comment_on_work_item"]}`
	refused := mustErr(t, decodeWork(turn.ToolReply("work"),
		func() []ledger.Call { return []ledger.Call{{Name: "jira_search_issues"}} },
		func() turn.Surface { return s }), claim)
	if !strings.HasPrefix(refused.Error(), NotDeliveredOn("work")) ||
		!strings.Contains(refused.Error(), `called "jira_search_issues"`) ||
		strings.Contains(refused.Error(), "somewhere else") {
		t.Errorf("refusal = %v, want the search named and no write elsewhere claimed", refused)
	}
}

// A FAILED POST ELSEWHERE IS NO WRITE ELSEWHERE AT SUBMISSION EITHER. A turn
// waiting on the tracker whose one call was a chat post that failed reached
// nobody, so its refusal says no call in this turn has reached anybody, never
// that a call reached somewhere else.
//
// Mutation: count a failed call in turn.KnownDelivered, and the failed post is
// told it reached somewhere else.
func TestACitationRefusalDoesNotCallAFailedPostAWriteElsewhere(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{"comment_on_work_item", "mattermost_post_message"},
		Deliveries: map[string]string{"comment_on_work_item": "work",
			"mattermost_post_message": "mattermost"},
	}
	claim := `{"outcome":"delivered","summary":"s","deliveries":["comment_on_work_item"]}`
	refused := mustErr(t, decodeWork(turn.ToolReply("work"),
		func() []ledger.Call { return []ledger.Call{{Name: "mattermost_post_message", Failed: true}} },
		func() turn.Surface { return s }), claim)
	if !strings.HasPrefix(refused.Error(), NotDeliveredOn("work")) ||
		!strings.Contains(refused.Error(), "no call in this turn has reached anybody") ||
		strings.Contains(refused.Error(), "somewhere else") {
		t.Errorf("refusal = %v, want it to say no call reached anybody and claim no write elsewhere",
			refused)
	}
}

// A REFUSAL THAT FINDS NO DELIVERY SAYS WHAT THE TURN DID CALL. A turn that
// asked a colleague or reacted, and then claimed the work delivered, was told
// "no call in this turn has reached anybody", or that no tool acting outside
// the engine had been called — false against its own successful call, and a
// refusal a model can see is false is one it argues with. It is told what it
// called and that none of it reached the person waiting, still opening with
// the clause a scripted model recognises; a turn that only read is told what
// it was told before.
//
// Mutation: drop either acted case, and the colleague ask is told it reached
// nobody or called nothing.
func TestANothingDeliveredRefusalSaysWhatTheTurnCalled(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue:      []string{"get_work_item", "comment_on_work_item", "ask"},
		Deliveries:     map[string]string{"comment_on_work_item": "work"},
		KnownReads:     []string{"get_work_item"},
		KnownOpenWorld: []string{"comment_on_work_item", "ask"},
	}
	surface := func() turn.Surface { return s }
	claim := `{"outcome":"delivered","summary":"s","deliveries":["comment_on_work_item"]}`
	record := func(name string) func() []ledger.Call {
		return func() []ledger.Call { return []ledger.Call{{Name: name}} }
	}

	asked := mustErr(t, decodeWork(turn.ToolReply("work"), record("ask"), surface), claim)
	if !strings.HasPrefix(asked.Error(), NotDeliveredOn("work")) ||
		!strings.Contains(asked.Error(), `called "ask"`) ||
		strings.Contains(asked.Error(), "no call in this turn has reached anybody") {
		t.Errorf("on `work`: refusal = %v; want it to open with %q and name the ask", asked,
			NotDeliveredOn("work"))
	}
	unnamed := mustErr(t, decodeWork(turn.ToolReply(""), record("ask"), surface), claim)
	if !strings.HasPrefix(unnamed.Error(), "nothing has been delivered yet") ||
		!strings.Contains(unnamed.Error(), `called "ask"`) ||
		strings.Contains(unnamed.Error(), "no tool that acts outside the engine") {
		t.Errorf("on no surface: refusal = %v; want it to name the ask", unnamed)
	}

	for reply, want := range map[string]string{
		"work": "no call in this turn has reached anybody",
		"":     "no tool that acts outside the engine has been called",
	} {
		read := mustErr(t, decodeWork(turn.ToolReply(reply), record("get_work_item"), surface), claim)
		if !strings.Contains(read.Error(), want) {
			t.Errorf("on %q: refusal = %v; want a turn that only read told %q", reply, read, want)
		}
	}
}

// A NO_ACTION IS REFUSED AT SUBMISSION ONCE THE TURN ACTED. A turn nobody
// asked for that asked a colleague, or posted, and then said nobody was asking
// would leave what it did unreported; the run is told so while it can still
// say what it did, naming the calls, as one bounced call rather than a round
// relaunched by the engine's check. A read of the world, a failed call and no
// call at all leave no_action open. A closed round's act counts as much as
// this round's. And no_action alone is refused: the refusal tells the run to
// report what the turn did as delivered or as blocked, so after any act both
// stay open to it.
//
// LIVE ONLY: decodeWork, which the replay of a made submission decodes with,
// accepts the same claim, since a no_action the run made before it acted is
// collected as made and corrected by turn.Check. And on a turn somebody is
// waiting on, the decline rule speaks first.
//
// Mutation: drop the check, and every refusal is accepted; move it into
// decodeWork, and the replay refuses what the run had made; refuse any outcome
// but delivered after an act, and the blocked turn that asked is refused.
func TestANoActionIsRefusedAtSubmissionOnceTheTurnActed(t *testing.T) {
	t.Parallel()
	s := deliverySurface()
	s.Catalogue = append(s.Catalogue, "ask", "search")
	s.KnownOpenWorld = []string{"ask", "search"}
	s.KnownReads = append(s.KnownReads, "search")
	surface := func() turn.Surface { return s }
	quiet := `{"outcome":"no_action","summary":"nothing for me here"}`
	reported := `{"outcome":"delivered","summary":"asked the lead whether it is theirs"}`
	stuck := `{"outcome":"blocked","summary":"asked the lead","evidence":"the lead has not answered"}`
	making := func(reply turn.Reply, calls []ledger.Call) func(map[string]any) (workPayload, error) {
		return makingWork(reply, func() []ledger.Call { return calls }, surface)
	}

	for name, tc := range map[string]struct {
		calls []ledger.Call
		named string
	}{
		"after an ask": {calls: []ledger.Call{{Name: "ask"}}, named: "ask"},
		"after a post": {calls: []ledger.Call{{Name: "slack_post"}}, named: "slack_post"},
		"after a closed round posted": {
			calls: turn.Record([]ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{{Name: "slack_post"}}}}, nil),
			named: "slack_post",
		},
	} {
		err := mustErr(t, making(turn.NoReply(), tc.calls), quiet)
		if !strings.Contains(err.Error(), "no_action is not available") ||
			!strings.Contains(err.Error(), `called "`+tc.named+`"`) {
			t.Errorf("%s: refusal = %v, want no_action refused naming %q", name, err, tc.named)
		}
		for _, told := range []string{reported, stuck} {
			if _, refused := making(turn.NoReply(), tc.calls)(args(t, told)); refused != nil {
				t.Errorf("%s: %s was refused: %v", name, told, refused)
			}
		}
	}
	for name, calls := range map[string][]ledger.Call{
		"after a read of the world": {{Name: "search"}},
		"after a failed post":       {{Name: "slack_post", Failed: true}},
		"after nothing":             nil,
	} {
		if _, err := making(turn.NoReply(), calls)(args(t, quiet)); err != nil {
			t.Errorf("%s: no_action was refused: %v", name, err)
		}
	}

	asked := []ledger.Call{{Name: "ask"}}
	replay := decodeWork(turn.NoReply(), func() []ledger.Call { return asked }, surface)
	if _, err := replay(args(t, quiet)); err != nil {
		t.Errorf("the replay's decoder refused a no_action the run had made: %v", err)
	}
	if err := mustErr(t, making(turn.ToolReply(""), asked), quiet); !strings.Contains(err.Error(), "even to decline") {
		t.Errorf("on an awaited turn: refusal = %v, want the decline rule's", err)
	}
}

// A NO_ACTION REFUSED FOR A CALL THE ENGINE ONLY PRESUMES WROTE SAYS SO. A
// turn nobody asked for that only searched through an MCP tool that no hint
// marks as a read or as a write has its no_action refused, since the delivery
// check counts that call whatever it did; told it "has already acted outside
// the engine", a model reads the refusal as false against its own search, and
// the outcome field it submitted under promised that a read changes nothing.
// So the refusal names the call as one the engine cannot tell from a write. A
// write its server marks as one is still refused as an act.
//
// Mutation: refuse by turn.ActedWith again, and the search is told it acted.
func TestANoActionAfterAnUnmarkedSearchIsRefusedSayingWhy(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for name, ann := range map[string]tools.Annotations{
		"jira_search_issues": {},
		"jira_add_comment":   {ReadOnly: mcp.No, OpenWorld: mcp.Yes},
	} {
		if err := reg.RegisterWith(stubTool(name), tools.Origin("jira"), ann); err != nil {
			t.Fatal(err)
		}
	}
	s := describe(tools.NewSurface("execute", reg.Snapshot(),
		[]string{"jira_search_issues", "jira_add_comment"}))
	quiet := `{"outcome":"no_action","summary":"nothing for me here"}`
	refusal := func(call string) string {
		calls := []ledger.Call{{Name: call}}
		return mustErr(t, makingWork(turn.NoReply(), func() []ledger.Call { return calls },
			func() turn.Surface { return s }), quiet).Error()
	}

	searched := refusal("jira_search_issues")
	if !strings.HasPrefix(searched, "no_action is not available: ") ||
		!strings.Contains(searched, `called "jira_search_issues", which its server does not `+
			`mark read-only, so the engine cannot tell it from a write`) ||
		strings.Contains(searched, "already acted") {
		t.Errorf("refusal = %q, want the search named as a call the engine cannot tell from a write",
			searched)
	}
	if commented := refusal("jira_add_comment"); !strings.Contains(commented,
		`already acted outside the engine — it called "jira_add_comment"`) {
		t.Errorf("refusal = %q, want the comment its server marks a write refused as an act", commented)
	}
}

// A REFUSED NO_ACTION SAYS THE WHOLE ACCOUNT, BOUNDED AS THE CHECK'S IS: what
// turn.ActedAccount says, each of its two lists five tools by name and then
// how many more, since the refusal points the run at calls its own transcript
// holds.
//
// Mutation: list either kind with words.And in turn.ActedAccount, and the
// refusal names all seven of that kind.
func TestANoActionRefusalNamesFiveOfEachKindAndCountsTheRest(t *testing.T) {
	t.Parallel()
	s := turn.Surface{Deliveries: map[string]string{}}
	var calls []ledger.Call
	for _, n := range []string{"7", "3", "1", "6", "2", "5", "4"} {
		act, presumed := "t"+n, "p"+n
		s.Catalogue = append(s.Catalogue, act, presumed)
		s.KnownOpenWorld = append(s.KnownOpenWorld, act)
		s.Deliveries[presumed] = "vendor"
		s.PresumedWrites = append(s.PresumedWrites, presumed)
		calls = append(calls, ledger.Call{Name: presumed}, ledger.Call{Name: act})
	}
	refused := mustErr(t, makingWork(turn.NoReply(), func() []ledger.Call { return calls },
		func() turn.Surface { return s }), `{"outcome":"no_action","summary":"nothing for me here"}`)
	want := `no_action is not available: this turn already acted outside the engine — it ` +
		`called "t1", "t2", "t3", "t4", "t5" and 2 more — and it called "p1", "p2", "p3", ` +
		`"p4", "p5" and 2 more, which their servers do not mark read-only, so the engine ` +
		`cannot tell them from writes. Report what this turn did`
	if !strings.HasPrefix(refused.Error(), want) {
		t.Errorf("refusal = %q\nwant it to open %q", refused, want)
	}
}

// A colleague's ask is answered by the engine on the channel it opened, and an
// unaddressed turn owes nobody a posted answer — so demanding a citation in
// either case would loop a turn that did exactly the right thing.
func TestOnlyAToolAwaitedTurnMustCiteACall(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.NoReply(), turn.EngineReply()} {
		decode := decodeWork(reply, func() []ledger.Call { return nil }, deliverySurface)
		if _, err := decode(args(t, `{"outcome":"delivered","summary":"answered in prose"}`)); err != nil {
			t.Errorf("%s: a delivery with no tool call was refused: %v", reply, err)
		}
	}
}

func mustErr(t *testing.T, decode func(map[string]any) (workPayload, error), blob string) error {
	t.Helper()
	_, err := decode(args(t, blob))
	if err == nil {
		t.Fatalf("%s was accepted", blob)
	}
	return err
}

func TestALoopBackNeedsSomethingToDoDifferently(t *testing.T) {
	t.Parallel()
	// A self_iterate with no correction sends the next round to do exactly
	// what the last one did. The stall guard catches it eventually — but
	// only after spending the rounds.
	if _, err := decodeReview(args(t, `{"decision":"self_iterate"}`)); err == nil {
		t.Error("a correction-free loop-back was accepted")
	}
	if _, err := decodeReview(args(t, `{"decision":"self_iterate","notes":"   "}`)); err == nil {
		t.Error("whitespace counted as a correction")
	}
	if _, err := decodeReview(args(t, `{"decision":"self_iterate","notes":"repost with the fixed link"}`)); err != nil {
		t.Errorf("a real correction was refused: %v", err)
	}
	// The counterfactual: done and failed need no notes.
	for _, d := range []string{"done", "failed"} {
		if _, err := decodeReview(args(t, `{"decision":"`+d+`"}`)); err != nil {
			t.Errorf("%s was refused: %v", d, err)
		}
	}
}

func TestASchemaShapedSubmissionDecodesIntoItsStruct(t *testing.T) {
	t.Parallel()
	// The schemas are hand-written so their DESCRIPTIONS can be real prose,
	// which means the tags and the schema can drift. This is what says they
	// have not: every property the schema publishes is a field the struct
	// accepts. It can say so only because a submission's decode REFUSES a
	// field its struct lacks — while the decode dropped one, a property
	// published with no field behind it decoded cleanly here and was
	// discarded from every real submission.
	decodeAWork := decodeWork(turn.NoReply(), func() []ledger.Call { return nil }, deliverySurface)
	for name, pair := range map[string]struct {
		schema map[string]any
		decode func(map[string]any) error
	}{
		"work":   {workSchema, func(m map[string]any) error { _, err := decodeAWork(m); return err }},
		"review": {reviewSchema, func(m map[string]any) error { _, err := decodeReview(m); return err }},
	} {
		props, ok := pair.schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema has no properties", name)
		}
		payload := map[string]any{}
		for key, spec := range props {
			payload[key] = sampleFor(t, name, key, spec)
		}
		if err := pair.decode(payload); err != nil {
			t.Errorf("%s: a submission shaped by the schema did not decode: %v", name, err)
		}
	}
}

// A SUBMISSION FIELD THE SCHEMA DOES NOT DECLARE GOES BACK TO THE MODEL,
// named, rather than being dropped from what the phase decided.
//
// A reviewer's `complete_work` for `completed_work` was accepted as a
// self_iterate that never told the next round what had already landed — the
// field exists so that round adds to a post rather than firing it a second
// time — and a work submission's `evidnce` as an answer saying nothing about
// what it tried. Each an answer the model did not give.
func TestASubmissionFieldTheSchemaDoesNotDeclareIsRefused(t *testing.T) {
	t.Parallel()
	decodeAWork := decodeWork(turn.NoReply(), func() []ledger.Call { return nil }, deliverySurface)
	if _, err := decodeReview(args(t, `{"decision":"self_iterate",`+
		`"notes":"link the runbook","complete_work":"posted the summary"}`)); err == nil ||
		!strings.Contains(err.Error(), "complete_work is not a key") ||
		!strings.Contains(err.Error(), "completed_work") {
		t.Errorf("a review's misspelt field answered %v, want it refused by name", err)
	}
	if _, err := decodeAWork(args(t,
		`{"outcome":"no_action","summary":"nothing asked","evidnce":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "evidnce is not a key") ||
		!strings.Contains(err.Error(), "evidence") {
		t.Errorf("a work submission's misspelt field answered %v, want it refused by name", err)
	}
}

// sampleFor builds a value matching a schema property, so the test above
// exercises the real mapping rather than a hand-written fixture that could
// drift alongside the schema it is meant to check.
func sampleFor(t *testing.T, phase, key string, spec any) any {
	t.Helper()
	obj, ok := spec.(map[string]any)
	if !ok {
		t.Fatalf("%s.%s: property is not an object", phase, key)
	}
	if enum, ok := obj["enum"].([]any); ok && len(enum) > 0 {
		// The LAST enum member for the work schema and the first for the
		// review's: both are the value with no extra requirement attached.
		// `delivered` needs a citation and `self_iterate` needs notes, so
		// picking either would fail for the right reason and hide a real
		// drift.
		if phase == "work" {
			return enum[len(enum)-1]
		}
		return enum[0]
	}
	switch obj["type"] {
	case "string":
		return "x"
	case "array":
		return []any{"x"}
	case "object":
		return map[string]any{}
	default:
		t.Fatalf("%s.%s: unhandled schema type %v", phase, key, obj["type"])
		return nil
	}
}

func TestArgumentsThatCannotBeReEncodedFailTheSubmissionNotTheTurn(t *testing.T) {
	t.Parallel()
	// A model cannot produce this through the wire, but a builtin caller
	// could. It must come back as a failed tool result the phase can retry,
	// not as a Go error that ends the turn.
	tool := workTool(turn.NoReply())
	res, err := tool.Call(context.Background(), map[string]any{"summary": make(chan int)})
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("an unencodable argument was accepted")
	}
	// The REASON matters. Swallowing the encode error leaves the payload
	// zero-valued, which then trips the missing-summary rule — so the
	// submission still fails, for a reason that has nothing to do with what
	// went wrong, and the model is told to add a summary it already wrote.
	// Found by mutation: asserting only that it failed passed either way.
	if !strings.Contains(res.Output, "re-encoded") {
		t.Errorf("the failure blames the wrong thing: %s", res.Output)
	}
}

func TestATypeMismatchIsReportedAsOne(t *testing.T) {
	t.Parallel()
	// A model that sends a bare string where the schema says array. The
	// reason has to be the mismatch: swallowing it leaves the payload part
	// filled, which trips a content rule instead, and the model is told to
	// fix something it got right.
	//
	// Found by mutation — asserting only that it failed passed either way.
	tool := workTool(turn.NoReply())
	res, err := tool.Call(context.Background(),
		args(t, `{"outcome":"delivered","summary":"s","deliveries":"slack_post"}`))
	if err != nil {
		t.Fatalf("Call returned a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("a type mismatch was accepted")
	}
	if !strings.Contains(res.Output, "do not match the schema") {
		t.Errorf("the failure blames the wrong thing: %s", res.Output)
	}
}

func TestALargeIDInASubmissionSurvivesTheStructRoundTrip(t *testing.T) {
	t.Parallel()
	// The submission's own arguments go through a marshal/unmarshal to
	// reach the struct. json.Number has to survive it, or the precision the
	// provider layer just protected is lost one layer higher.
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(
		`{"outcome":"blocked","summary":"issue 1234567890123456789","evidence":"e"}`))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	decode := decodeWork(turn.NoReply(), func() []ledger.Call { return nil }, deliverySurface)
	w, err := decode(m)
	if err != nil {
		t.Fatalf("decodeWork: %v", err)
	}
	if !strings.Contains(w.Summary, "1234567890123456789") {
		t.Errorf("summary = %q", w.Summary)
	}
}

// A PICKUP'S SUBMISSION IS REFUSED OVER WORDS ALONE (ADR-0122): a delivery, a
// block, or an outcome left out, reported while the engine's record shows no
// typed answer on the item and the item is still the seat's, is refused naming
// the five answers before any other check; a typed answer, or an item that left
// the seat meanwhile, submits; a no_action is refused as on every turn somebody
// waits on; and an assignment's turn is not asked.
//
// Mutation: read the typed answer only for `delivered`, and a block reported
// over a comment is accepted at submission and refused after it; accept only a
// typed answer, and a turn whose item was trashed under it cannot submit.
func TestAPickupsSubmissionIsRefusedOverWordsAlone(t *testing.T) {
	t.Parallel()
	pickup := turn.ToolReply("")
	pickup.Item = "w-1"
	posted := []ledger.Call{{Name: "slack_post"}}
	answer := types.PickupUnanswered
	decode := typedPickup(pickup, func(context.Context) types.PickupAnswer { return answer },
		decodeWork(pickup, func() []ledger.Call { return posted }, deliverySurface))
	for _, blob := range []string{
		`{"outcome":"delivered","summary":"commented","deliveries":["slack_post"]}`,
		`{"summary":"commented","deliveries":["slack_post"]}`,
		`{"outcome":"blocked","summary":"waits","evidence":"the vendor has not replied"}`,
	} {
		if _, err := decode(t.Context(), args(t, blob)); err == nil || err.Error() != turn.PickupUntyped {
			t.Errorf("%s over words alone: %v, want the pickup's refusal", blob, err)
		}
	}
	if _, err := decode(t.Context(), args(t, `{"outcome":"no_action","summary":"nothing"}`)); err == nil ||
		err.Error() == turn.PickupUntyped {
		t.Errorf("no_action on a pickup: %v, want the refusal every awaited turn gets", err)
	}
	for _, answer = range []types.PickupAnswer{types.PickupTyped, types.PickupRemoved,
		types.PickupFinished, types.PickupReassigned} {
		if _, err := decode(t.Context(), args(t,
			`{"outcome":"delivered","summary":"started it","deliveries":["slack_post"]}`)); err != nil {
			t.Errorf("a %s answer was refused: %v", answer, err)
		}
	}
	asked := 0
	assigned := typedPickup(turn.ToolReply(""), func(context.Context) types.PickupAnswer {
		asked++
		return types.PickupUnanswered
	}, decodeWork(turn.ToolReply(""), func() []ledger.Call { return posted }, deliverySurface))
	if _, err := assigned(t.Context(), args(t,
		`{"outcome":"delivered","summary":"posted","deliveries":["slack_post"]}`)); err != nil || asked != 0 {
		t.Errorf("an assignment's submission: %v after %d typed reads, want it accepted unread", err, asked)
	}
}
