package turn_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// REGISTERED AS REACHING SOMEBODY. An MCP-served tool counts unless it is
// positively annotated read-only — "not a known read" is POSITIVE, so an
// unannotated tool counts, which is the fail-closed direction — and a
// first-party tool counts only where it was declared to deliver.
func TestDeliverableIsServerBackedAndNotAKnownRead(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{"slack_post", "slack_history", "reflect_and_persist", "tracker_do"},
		// The registry computed this: slack_history is annotated read-only
		// so it is absent, and reflect_and_persist is a first-party tool
		// registered without tools.DeliversTo().
		Deliveries: map[string]string{"slack_post": "test", "tracker_do": "test"},
		KnownReads: []string{"slack_history"},
	}
	for name, want := range map[string]bool{
		"slack_post":          true,  // server-backed write
		"tracker_do":          true,  // server-backed, unannotated — fail closed
		"slack_history":       false, // positively read-only
		"reflect_and_persist": false, // a builtin, however much it writes
		"never_registered":    false,
	} {
		if got := turn.Deliverable(name, s); got != want {
			t.Errorf("Deliverable(%q) = %v, want %v", name, got, want)
		}
	}
}

// SUCCESSFUL CALLS ONLY. A failed post did not post, and counting it would
// close the check on exactly the turn that needs to iterate.
func TestDeliveredIgnoresFailedCalls(t *testing.T) {
	t.Parallel()
	s := turn.Surface{Deliveries: map[string]string{"slack_post": "test"}}
	if turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}}, s) {
		t.Error("a failed call counted as a delivery")
	}
	if !turn.Delivered([]ledger.Call{{Name: "slack_post", Failed: true}, {Name: "slack_post"}}, s) {
		t.Error("a retry that succeeded did not count")
	}
}

// PROOF, NOT SUSPICION. Acted decides whether a broken turn's trigger is
// given up rather than redelivered, so a true answer SPENDS the work — and the
// tools that must not trip it are the ones a turn calls on every single round.
//
// This is the case that kills the obvious implementation. `submit_work`, the
// discovery pair and the sub-agent spawner are all registered with NO
// annotations at all (internal/agent/runner/phases.go), so any predicate of
// the form "not proven read-only" reads true for the executor's own
// terminator — and every turn that closed a round would then be abandoned.
func TestActedCountsOnlyWhatIsPROVENToHaveLeftTheEngine(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{
			"slack_post", "slack_history", "tracker_do", "submit_work",
			"activate_tool", "reflect_and_persist", "a2a_ask", "run_sandbox",
		},
		Deliveries:     map[string]string{"slack_post": "test", "tracker_do": "test"},
		KnownReads:     []string{"slack_history"},
		KnownOpenWorld: []string{"a2a_ask", "run_sandbox"},
	}
	for name, want := range map[string]bool{
		"slack_post":    true,  // a declared deliverable reached a person
		"tracker_do":    true,  // declared a deliverable at registration
		"a2a_ask":       true,  // a builtin, but it woke a colleague
		"run_sandbox":   true,  // a builtin, but it started a billed box
		"slack_history": false, // positively read-only
		// The three that make this predicate different from
		// !ReadOnlyProven, and the reason it is not written that way:
		// every one of them is unannotated in-engine plumbing that a
		// healthy turn calls constantly.
		"submit_work":         false,
		"activate_tool":       false,
		"reflect_and_persist": false, // an agent's own diary is not the world
		"never_registered":    false,
	} {
		if got := turn.Acted([]ledger.Call{{Name: name}}, s); got != want {
			t.Errorf("Acted(%q) = %v, want %v", name, got, want)
		}
	}
}

// SUCCESSFUL CALLS ONLY, for the same reason Delivered ignores them — and here
// the consequence is sharper: abandoning a turn over a post that did not post
// discards work for a write that never happened.
func TestActedIgnoresFailedCalls(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Deliveries:     map[string]string{"slack_post": "test"},
		KnownOpenWorld: []string{"a2a_ask"},
	}
	if turn.Acted([]ledger.Call{{Name: "slack_post", Failed: true}}, s) {
		t.Error("a failed post counted as having reached the world")
	}
	if turn.Acted([]ledger.Call{{Name: "a2a_ask", Failed: true}}, s) {
		t.Error("a failed ask counted as having woken a colleague")
	}
	if !turn.Acted([]ledger.Call{{Name: "slack_post", Failed: true}, {Name: "slack_post"}}, s) {
		t.Error("a retry that succeeded did not count")
	}
}

// Acted and Delivered answer DIFFERENT questions and neither contains the
// other. Kept as its own case because merging them is the tempting
// simplification and it is wrong in both directions.
func TestActedAndDeliveredDisagreeOnPurpose(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue:      []string{"a2a_ask", "tracker_do"},
		Deliveries:     map[string]string{"tracker_do": "test"},
		KnownOpenWorld: []string{"a2a_ask"},
	}
	ask := []ledger.Call{{Name: "a2a_ask"}}
	// An ask is not a DELIVERY: nobody waiting on this turn got an answer,
	// which is what the delivery gate is for.
	if turn.Delivered(ask, s) {
		t.Error("a colleague ask counted as a delivery to the person waiting")
	}
	// It IS an act: a redelivery would wake that colleague a second time
	// and spend another of their turns.
	if !turn.Acted(ask, s) {
		t.Error("a colleague ask counted as nothing having happened")
	}
}

func TestAwaitedIsEveryReplyButNone(t *testing.T) {
	t.Parallel()
	for r, want := range map[turn.Reply]bool{
		turn.NoReply(): false, turn.ToolReply(""): true, turn.EngineReply(): true,
	} {
		if got := r.Awaited(); got != want {
			t.Errorf("%s.Awaited() = %v, want %v", r, got, want)
		}
	}
}

// THE ENGINE'S CORRECTION GOES LAST because it is the one the next round must
// act on: on the override path the reviewer said done and wrote no correction
// of its own, so there it is the only instruction there is.
func TestAppendCorrectionPutsTheEnginesWordLast(t *testing.T) {
	t.Parallel()
	got := turn.AppendCorrection("the tone is off", "nothing was delivered")
	if !strings.HasSuffix(got, "nothing was delivered") {
		t.Errorf("joined = %q, want the engine's correction last", got)
	}
	if !strings.Contains(got, "the tone is off") {
		t.Errorf("joined = %q, want the reviewer's notes kept", got)
	}
	// Either side missing leaves the other alone rather than a blank line.
	if got := turn.AppendCorrection("notes", ""); got != "notes" {
		t.Errorf("with no correction = %q", got)
	}
	if got := turn.AppendCorrection("   ", "fix it"); got != "fix it" {
		t.Errorf("with blank notes = %q", got)
	}
}

// THE INCIDENT, AS A TEST.
//
// A founder asked the CEO seat a question in a Mattermost DM. The seat created
// a work item and told nobody. Every layer passed: a tracker write is a real
// delivery, so the flat check saw one and the reviewer's `done` stood. The
// founder was left holding a clarification question they had already been
// answered out from under.
func TestATrackerWriteDoesNotAnswerSomebodyWaitingInChat(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue: []string{"create_work_item", "mattermost_post_message"},
		Deliveries: map[string]string{
			"create_work_item":        "tracker",
			"mattermost_post_message": "mattermost",
		},
	}
	asked := turn.ToolReply("mattermost")
	filedATask := []ledger.Call{{Name: "create_work_item"}}

	if turn.DeliveredTo(filedATask, s, asked) {
		t.Error("a tracker write answered a founder waiting in a chat thread")
	}
	// The flat question still says yes, and that is exactly the gap: the
	// turn DID reach somebody, just not the person who asked.
	if !turn.Delivered(filedATask, s) {
		t.Error("the flat check no longer sees the tracker write at all — " +
			"this narrowed the wrong question")
	}
	// The post on the awaited surface is what counts.
	if !turn.DeliveredTo([]ledger.Call{{Name: "mattermost_post_message"}}, s, asked) {
		t.Error("a post on the surface the ask arrived from did not count")
	}
	// And the correction has to NAME the surface, because "no tool was
	// called" reads as false to a model looking at its own successful write.
	w := turn.Work{Outcome: turn.OutcomeDelivered, Calls: filedATask}
	override, correction := turn.OverrideDone(w, nil, asked, s)
	if !override {
		t.Fatal("the reviewer's done stood over a turn that answered nobody waiting")
	}
	if !strings.Contains(correction, "mattermost") {
		t.Errorf("correction = %q, want it to name the surface", correction)
	}
}

// NARROWED ONLY WHERE IT CAN BE — the two fallbacks, which are what keep this
// from being stricter than a seat can satisfy.
func TestTheSurfaceCheckFallsBackWhereItCannotJudge(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Catalogue:  []string{"create_work_item"},
		Deliveries: map[string]string{"create_work_item": "tracker"},
	}
	filedATask := []ledger.Call{{Name: "create_work_item"}}

	// An obligation the engine could not place. An assignment is the honest
	// example: the answer may land on the item, on the ticket, or in the
	// thread the work came from, and nothing here can pick.
	if !turn.DeliveredTo(filedATask, s, turn.ToolReply("")) {
		t.Error("an obligation with no named surface was judged against one anyway")
	}
	// A surface this seat holds no tool for. It could not have answered
	// there however many rounds it spent, so there is nothing to enforce —
	// and insisting would turn an operator's missing integration into a
	// seat that fails every turn.
	if !turn.DeliveredTo(filedATask, s, turn.ToolReply("mattermost")) {
		t.Error("a seat was held to a surface it holds no tool for, which it " +
			"can only satisfy by never finishing")
	}
}

// The refusal a model reads has to list the tools that would ACTUALLY have
// counted. Listing every deliverable is how it was told to cite
// create_work_item at a founder waiting in a chat thread — a citation that
// would have produced a clean `delivered`, a clean `done`, and the same
// silence.
func TestDeliverersForNamesOnlyTheAwaitedSurface(t *testing.T) {
	t.Parallel()
	s := turn.Surface{
		Deliveries: map[string]string{
			"create_work_item":        "tracker",
			"write_page":              "pages",
			"mattermost_post_message": "mattermost",
		},
	}
	got := turn.DeliverersFor(s, turn.ToolReply("mattermost"))
	if len(got) != 1 || got[0] != "mattermost_post_message" {
		t.Errorf("DeliverersFor = %v, want only the tool that reaches the asker", got)
	}
	// Where the engine cannot judge, everything is citable — the same
	// fallback the check itself takes, because the two must agree or a model
	// is told one thing at submission and judged by another.
	if got := turn.DeliverersFor(s, turn.ToolReply("")); len(got) != 3 {
		t.Errorf("DeliverersFor with no surface = %v, want every deliverable", got)
	}
	if got := turn.DeliverersFor(s, turn.ToolReply("teams")); len(got) != 3 {
		t.Errorf("DeliverersFor on an unreachable surface = %v, want every deliverable", got)
	}
}

// A Reply is stored as a string on a suspended coding run's row and read back
// by whatever build resumes it, which may be days and a restart later.
func TestAReplyRoundTripsThroughItsStoredForm(t *testing.T) {
	t.Parallel()
	for _, want := range []turn.Reply{
		turn.NoReply(), turn.EngineReply(), turn.ToolReply(""), turn.ToolReply("mattermost"),
	} {
		if got := turn.ParseReply(want.String()); got != want {
			t.Errorf("ParseReply(%q) = %+v, want %+v", want.String(), got, want)
		}
	}
	// A value this build does not know comes back as unset rather than as a
	// guess, so Run refuses it by the same rule as any other invalid input.
	if got := turn.ParseReply("carrier-pigeon:aviary"); got.Valid() {
		t.Errorf("an unknown kind parsed as valid: %+v", got)
	}
}

// ONLY A TOOL OBLIGATION IS A QUESTION ABOUT TOOL CALLS. The ledger asks
// whether the asker has this turn's answer, and for two of the three
// obligations the tool record cannot say: an A2A ask is answered by the engine
// on the channel it opened, and an unprompted turn has nobody to answer at
// all. Reading the record on all three filed both as "this never reached
// anyone" — the same false record the Unsent field exists to prevent, pointed
// the other way, and it broke a resumed coding turn's history.
func TestAnsweredOnlyConsultsTheRecordWhenAToolOwesTheAnswer(t *testing.T) {
	t.Parallel()
	s := turn.Surface{Deliveries: map[string]string{"slack_post": "slack"}}
	nothing := []ledger.Call{{Name: "run_sandbox"}}

	if !turn.Answered(nothing, s, turn.NoReply()) {
		t.Error("a turn nobody asked for was recorded as having failed to reply")
	}
	if !turn.Answered(nothing, s, turn.EngineReply()) {
		t.Error("an A2A answer the engine itself carries back was recorded as unsent")
	}
	// The one case the record CAN settle, in both directions.
	if turn.Answered(nothing, s, turn.ToolReply("slack")) {
		t.Error("a turn that owed a post and made none counted as answered")
	}
	if !turn.Answered([]ledger.Call{{Name: "slack_post"}}, s, turn.ToolReply("slack")) {
		t.Error("a post on the awaited surface did not count as answered")
	}
}

// trackerSurface is a seat woken on the engine's own tracker: first-party
// tools that deliver on `work`, which the executor holds from its first round,
// beside an MCP chat server it would have to discover.
func trackerSurface() turn.Surface {
	return turn.Surface{
		Catalogue: []string{"get_work_item", "comment_on_work_item", "update_work_item",
			"mattermost_post_message"},
		Deliveries: map[string]string{
			"comment_on_work_item":    "work",
			"update_work_item":        "work",
			"mattermost_post_message": "mattermost",
		},
		KnownReads:   []string{"get_work_item"},
		Discoverable: []string{"mattermost_post_message"},
	}
}

// commented is a closed round whose one write was the comment that answered
// the person waiting on the item.
func commented() []ledger.Iteration {
	return []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{{Name: "comment_on_work_item"}}}}
}

// THE OBLIGATION IS THE TURN'S, AS A TEST.
//
// A founder answered a seat's question on a work item. The seat's first round
// commented on the item — which IS the delivery — and its reviewer sent the
// round back to re-read the item first. The second round re-read it and cited
// the comment; every check read only that round's calls, so the citation was
// refused eleven times and the reviewer's `done` was overturned, and the third
// round posted the same acknowledgement in a chat channel instead. The
// prior-work ledger had told the round, all along, not to comment again.
func TestAnEarlierRoundsDeliveryStillAnswersTheTurn(t *testing.T) {
	t.Parallel()
	s := trackerSurface()
	asked := turn.ToolReply("work")
	reread := turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "re-read the item",
		Deliveries: []string{"comment_on_work_item"},
		Calls:      []ledger.Call{{Name: "get_work_item"}},
	}

	if v := turn.Check(reread, commented(), asked, s); v.Correction != "" || v.Skip {
		t.Errorf("Check = %+v, want the round passed to the reviewer: an earlier round "+
			"of this turn already answered on `work`", v)
	}
	if override, correction := turn.OverrideDone(reread, commented(), asked, s); override {
		t.Errorf("the reviewer's done was overturned on a turn that answered in its "+
			"first round: %q", correction)
	}

	// The counterfactuals that keep this from passing on a check that
	// stopped checking: the same round with nothing before it delivered
	// nothing, and a FAILED earlier comment did not deliver either.
	if v := turn.Check(reread, nil, asked, s); v.Correction == "" {
		t.Error("a turn that never delivered passed the check")
	}
	if override, _ := turn.OverrideDone(reread, nil, asked, s); !override {
		t.Error("a done stood over a turn that never delivered")
	}
	failed := []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "comment_on_work_item", Failed: true}}}}
	if v := turn.Check(reread, failed, asked, s); v.Correction == "" {
		t.Error("an earlier round's FAILED comment counted as the delivery")
	}
	// And the surface still matters across rounds: an earlier chat post does
	// not answer somebody waiting on the item.
	chat := []ledger.Iteration{{Iteration: 1,
		Calls: []ledger.Call{{Name: "mattermost_post_message"}}}}
	if override, _ := turn.OverrideDone(reread, chat, asked, s); !override {
		t.Error("an earlier round's chat post answered somebody waiting on the tracker")
	}
}

// "NOTHING EXTERNAL HAPPENED" IS A FACT ABOUT THE TURN. A round that reports
// no_action after an earlier one posted would otherwise end the turn as a skip,
// recorded as one that touched nothing.
func TestNoActionIsNotASkipOnceAnEarlierRoundActed(t *testing.T) {
	t.Parallel()
	s := trackerSurface()
	quiet := turn.Work{Outcome: turn.OutcomeNoAction, Summary: "nothing to do"}

	if v := turn.Check(quiet, commented(), turn.NoReply(), s); v.Skip || v.Correction == "" {
		t.Errorf("Check = %+v, want a correction: an earlier round of this turn "+
			"already wrote on the tracker", v)
	}
	if v := turn.Check(quiet, nil, turn.NoReply(), s); !v.Skip {
		t.Errorf("Check = %+v, want a skip for a turn that touched nothing", v)
	}
}

// THE REMEDY NAMES THE TOOLS. It used to name a mechanism — "list_mcp_server_
// tools and activate_tool will find it" — which is right for a vendor's
// surface and wrong for the tracker's, whose tools no discovery call lists: a
// seat sent to discovery for `work` found the one MCP server it held and posted
// in chat, where nothing could count it.
func TestTheRemedyNamesTheToolsThatDeliverWhereTheAskerIs(t *testing.T) {
	t.Parallel()
	s := trackerSurface()

	onTracker := turn.Remedy(s, turn.ToolReply("work"))
	for _, want := range []string{"`work`", "comment_on_work_item", "update_work_item"} {
		if !strings.Contains(onTracker, want) {
			t.Errorf("tracker remedy = %q, want it to name %q", onTracker, want)
		}
	}
	for _, wrong := range []string{"list_mcp_server_tools", "activate_tool", "mattermost"} {
		if strings.Contains(onTracker, wrong) {
			t.Errorf("tracker remedy = %q, sends the seat to %q", onTracker, wrong)
		}
	}

	inChat := turn.Remedy(s, turn.ToolReply("mattermost"))
	for _, want := range []string{"`mattermost` MCP server", "list_mcp_server_tools", "activate_tool"} {
		if !strings.Contains(inChat, want) {
			t.Errorf("chat remedy = %q, want it to name %q", inChat, want)
		}
	}
	if strings.Contains(inChat, "comment_on_work_item") {
		t.Errorf("chat remedy = %q, offers a tool that does not reach the asker", inChat)
	}

	// Both kinds on one surface: name what is held, and say where the rest is.
	mixed := trackerSurface()
	mixed.Deliveries["vendor_comment"] = "work"
	mixed.Discoverable = append(mixed.Discoverable, "vendor_comment")
	both := turn.Remedy(mixed, turn.ToolReply("work"))
	if !strings.Contains(both, "comment_on_work_item") || !strings.Contains(both, "list_mcp_server_tools") {
		t.Errorf("mixed remedy = %q, want the held tools named and discovery pointed at", both)
	}
	if strings.Contains(both, "vendor_comment") {
		t.Errorf("mixed remedy = %q, names a tool the seat does not hold yet", both)
	}

	// Where the asker's surface cannot be named, any delivery counts, and the
	// general instruction is the honest one.
	for _, r := range []turn.Reply{turn.ToolReply(""), turn.ToolReply("slack")} {
		if got := turn.Remedy(s, r); !strings.Contains(got, "list_mcp_server_tools") {
			t.Errorf("Remedy(%s) = %q, want the general instruction", r, got)
		}
	}
}

// THE CORRECTION SAYS THE TURN REACHED SOMEBODY ONLY WHEN IT DID. It said so
// to every turn waiting on a named surface — including one whose only calls
// were reads — and a model told it has written somewhere goes looking for the
// write.
func TestOverrideDoneClaimsAWriteElsewhereOnlyWhenThereWasOne(t *testing.T) {
	t.Parallel()
	s := trackerSurface()
	asked := turn.ToolReply("work")

	_, read := turn.OverrideDone(turn.Work{Outcome: turn.OutcomeDelivered,
		Calls: []ledger.Call{{Name: "get_work_item"}}}, nil, asked, s)
	if strings.Contains(read, "reached somebody") {
		t.Errorf("correction = %q, claims a write a read-only turn never made", read)
	}
	_, chat := turn.OverrideDone(turn.Work{Outcome: turn.OutcomeDelivered,
		Calls: []ledger.Call{{Name: "mattermost_post_message"}}}, nil, asked, s)
	if !strings.Contains(chat, "reached somebody") {
		t.Errorf("correction = %q, want it to say the turn wrote somewhere else", chat)
	}
	for _, c := range []string{read, chat} {
		if !strings.Contains(c, "comment_on_work_item") {
			t.Errorf("correction = %q, want it to name the tool that delivers on `work`", c)
		}
	}
}
