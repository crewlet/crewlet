package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// decisionRegistry is the seat surface with a tracker and a knowledge base, so
// a decision's task and page evidence both have something to be checked
// against.
func decisionRegistry(t *testing.T, trk *fakeTracker, kb builtin.PageReader) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{
		Work:  builtin.WorkDeps{Reader: trk, Writer: trk.as},
		Pages: builtin.PageDeps{Reader: kb},
	})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// decisionArg is a well-formed `decision` argument, as a model sends it.
func decisionArg() map[string]any {
	return map[string]any{
		"question": "Ship on Friday or hold for the audit?",
		"options": []any{
			map[string]any{"id": "ship", "label": "Ship Friday"},
			map[string]any{"id": "hold", "label": "Hold for the audit", "detail": "about a week"},
		},
		"recommended": "ship",
		"rationale":   "The audit reviews last quarter's code.",
		"evidence": []any{
			map[string]any{"kind": "task", "ref": "ENG-2", "label": "the audit"},
			map[string]any{"kind": "page", "ref": "ENG/Deploy Runbook"},
		},
		"role": "approver",
	}
}

// A COMMENT CAN ASK WITH A DECISION, and what is stored is what every node
// will hold for ever: the task and the page the evidence names are resolved
// to their IDS — a key is retired by a move and a title by a rename — and a
// decision whose keys a model misspelled is refused by name rather than
// stored without the part it misspelled.
func TestACommentCanAskWithADecision(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "body": "The audit starts Monday.", "ask": "pm",
		"decision": decisionArg(),
	})
	if got.Failed {
		t.Fatalf("the ask failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil {
		t.Fatalf("the ask did not ride the task's write: %+v", trk.patched)
	}
	comment := trk.patched[0].Comment
	if comment.Ask != "pm" || comment.Decision == nil {
		t.Fatalf("the comment is %+v, want an ask carrying its decision", comment)
	}
	asked := comment.Decision
	if len(asked.Options) != 2 || asked.Recommended != "ship" ||
		asked.Role != decision.RoleApprover {
		t.Errorf("the decision is %+v", asked)
	}
	if asked.Evidence[0].Ref != "id-2" || asked.Evidence[1].Ref != "p1" {
		t.Errorf("the evidence is stored as %+v, want the task's and the page's ids",
			asked.Evidence)
	}
	if !strings.Contains(got.Output, `"decision"`) {
		t.Errorf("the answer does not show the decision as stored: %s", got.Output)
	}

	misspelled := decisionArg()
	misspelled["recommendation"] = misspelled["recommended"]
	delete(misspelled, "recommended")
	refused := callWork(t, decisionRegistry(t, newFakeTracker(), newFakeKB()),
		builtin.CommentOnWorkTool, map[string]any{"asks": false,
			"item": "ENG-1", "body": "?", "ask": "pm", "decision": misspelled,
		})
	if !refused.Failed || !strings.Contains(refused.Output, "recommendation") {
		t.Errorf("a misspelled key was not refused by name: %s", refused.Output)
	}
}

// A DECISION WITHOUT AN ASK IS REFUSED, on both tools, before anything is
// read or written: a decision is a question put to somebody, and one asked of
// nobody is a set of options nobody is woken to weigh. And a page cited on a
// company with no knowledge base of its own is refused as the caller's to
// change — cite it as a url — rather than stored as a link nothing checked.
func TestADecisionWithoutAnAskIsRefused(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.CommentOnWorkTool, map[string]any{"asks": false,
			"item": "ENG-1", "body": "which?", "decision": decisionArg()}},
		{builtin.CreateWorkItemTool, map[string]any{
			"title": "which?", "project": "ENG", "decision": decisionArg()}},
	} {
		got := callWork(t, reg, c.tool, c.args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			!strings.Contains(got.Output, "`ask`") {
			t.Errorf("%s: a decision asked of nobody gave %+v", c.tool, got)
		}
	}
	if len(trk.patched) != 0 || len(trk.created) != 0 {
		t.Errorf("a refused decision was written: %d patches, %d creates",
			len(trk.patched), len(trk.created))
	}

	noKB := newFakeTracker()
	got := callWork(t, decisionRegistry(t, noKB, nil), builtin.CommentOnWorkTool,
		map[string]any{"asks": false, "item": "ENG-1", "body": "?", "ask": "pm", "decision": decisionArg()})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || !strings.Contains(got.Output, "url") {
		t.Errorf("a page cited with no knowledge base gave %+v", got)
	}
	if len(noKB.patched) != 0 {
		t.Error("a decision citing an unchecked page was written")
	}
}

// A CHOICE WITH NO BODY IS AN ANSWER: the option is what the asker was
// waiting for, and a reason is welcome but not owed. The choice rides the
// thread read — so the reader checks it against the ask's own options before
// anything is published — and the comment. A choice that answers nothing is
// refused, since there is no decision for it to be an option of.
func TestAChoiceWithNoBodyIsAnAnswer(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "answers": "c-ask", "choice": "ship",
	})
	if got.Failed {
		t.Fatalf("a bare choice was refused: %s", got.Output)
	}
	if trk.threadQuery.Choice != "ship" {
		t.Errorf("the thread read was asked about choice %q, want it checked there",
			trk.threadQuery.Choice)
	}
	comment := trk.patched[0].Comment
	if comment.Choice != "ship" || comment.Answers == nil || *comment.Answers != "c-ask" ||
		comment.Body != "" {
		t.Errorf("the answer is %+v, want the choice answering c-ask with no body", comment)
	}

	loose := newFakeTracker()
	got = callWork(t, decisionRegistry(t, loose, newFakeKB()), builtin.CommentOnWorkTool,
		map[string]any{"asks": false, "item": "ENG-1", "choice": "ship"})
	if !got.Failed || !strings.Contains(got.Output, "`answers`") {
		t.Errorf("a choice answering nothing gave %+v", got)
	}
	if len(loose.patched) != 0 {
		t.Error("a choice answering nothing was posted")
	}
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1",
	}); !got.Failed || !strings.Contains(got.Output, "`body`") {
		t.Errorf("a comment with neither a body nor a choice gave %+v", got)
	}
}

// A CREATE CAN CARRY ITS ASK IN ONE RECORD. "Ask" and "Message" file an item
// whose point is the question on it, so the question is the item's TITLE and
// it rides the create itself — the writer's one-record verb — and the wake it
// builds names the person asked, which is what routes them under `asked`.
func TestACreateCanCarryItsAskInOneRecord(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "Ship Friday or hold for the audit?", "project": "ENG",
		"body": "The audit starts Monday.", "assignee": "pm", "ask": "pm",
		"decision": decisionArg(),
	})
	if got.Failed {
		t.Fatalf("filing a question failed: %s", got.Output)
	}
	if len(trk.created) != 1 || len(trk.asks) != 1 {
		t.Fatalf("the create wrote %d tasks and %d asks, want one of each in "+
			"one call", len(trk.created), len(trk.asks))
	}
	ask := trk.asks[0]
	if ask.Ask != "pm" || ask.Body != trk.created[0].Title || ask.Decision == nil ||
		ask.Decision.Evidence[0].Ref != "id-2" {
		t.Errorf("the ask is %+v, want pm asked the title with the checked decision", ask)
	}
	notify := trk.notified[0]
	if notify == nil || notify.Snapshot.CommentAsk != "pm" || notify.CommentID != ask.ID {
		t.Errorf("the wake is %+v, want it to ask pm on comment %s", notify, ask.ID)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if answer["comment_id"] != ask.ID || answer["asked"] != "pm" {
		t.Errorf("the answer is %v, want the ask's comment id and who was asked", answer)
	}

	// AND WITHOUT `ask`, the plain create it always was.
	plain := newFakeTracker()
	if got := callWork(t, decisionRegistry(t, plain, newFakeKB()), builtin.CreateWorkItemTool,
		map[string]any{"title": "wire it", "project": "ENG"}); got.Failed {
		t.Fatalf("a plain create failed: %s", got.Output)
	}
	if len(plain.asks) != 0 || len(plain.created) != 1 {
		t.Errorf("a plain create carried an ask: %+v", plain.asks)
	}
}

// fakeChannels is a chart with chat surfaces per seat and declared unit
// channels.
type fakeChannels struct {
	surfaces map[string][]decision.InformSurface
	channels []string
}

func (f fakeChannels) ChatSurfaces(handle string) []decision.InformSurface {
	return f.surfaces[handle]
}

func (f fakeChannels) UnitChannels() []string { return f.channels }

// informRegistry is the seat surface with a chart to check an inform against,
// and an actor seam when the caller is not the turn's seat.
func informRegistry(t *testing.T, trk *fakeTracker,
	actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)) *tools.Registry {

	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Actor: actor,
			Channels: fakeChannels{
				surfaces: map[string][]decision.InformSurface{
					"eng": {decision.InformSlack},
				},
				channels: []string{"releases", "product"},
			},
		},
		Pages: builtin.PageDeps{Reader: newFakeKB()},
	})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// informedDecision is a decision promising to report its outcome.
func informedDecision(surface, channel string) map[string]any {
	d := decisionArg()
	delete(d, "evidence")
	d["inform"] = map[string]any{"surface": surface, "channel": channel}
	return d
}

// AN OPERATOR CANNOT ASK THE ENGINE TO INFORM.
//
// The engine keeps an inform by holding the ASKER'S answered turn open until a
// tool on the surface has posted, and a person acting through a token has no
// turn to hold — so their inform would be a line on the answering person's
// card that nothing keeps. Refused as FORBIDDEN, on both tools, before
// anything is written; the same decision without the inform is accepted.
func TestAnOperatorCannotAskTheEngineToInform(t *testing.T) {
	t.Parallel()
	operator := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator, OperatorID: "ops"}, nil
	}
	trk := newFakeTracker()
	reg := informRegistry(t, trk, operator)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.CommentOnWorkTool, map[string]any{"asks": false, "item": "ENG-1", "body": "?",
			"ask": "pm", "decision": informedDecision("slack", "releases")}},
		{builtin.CreateWorkItemTool, map[string]any{"title": "Ship Friday?",
			"project": "ENG", "ask": "pm", "decision": informedDecision("slack", "releases")}},
	} {
		got := callWork(t, reg, c.tool, c.args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
			!strings.Contains(got.Output, "agent seat") {
			t.Errorf("%s: an operator's inform gave %+v", c.tool, got)
		}
	}
	if len(trk.patched) != 0 || len(trk.created) != 0 {
		t.Errorf("a refused inform was written: %d patches, %d creates",
			len(trk.patched), len(trk.created))
	}
	plain := decisionArg()
	delete(plain, "evidence")
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "body": "?", "ask": "pm", "decision": plain,
	}); got.Failed {
		t.Errorf("an operator's decision without an inform was refused: %s", got.Output)
	}
}

// AN INFORM CHANNEL MUST BE ONE THE CHART DECLARES, on a surface the asking
// seat can post on — each half alone admits a promise nobody can keep. A
// leading `#` is accepted and the chart's spelling is stored.
func TestAnInformChannelMustBeOneTheChartDeclares(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		surface, channel string
		refusal          string
	}{
		"an undeclared channel": {"slack", "random", "releases, product"},
		"a surface not held":    {"mattermost", "releases", "slack"},
	} {
		trk := newFakeTracker()
		got := callWork(t, informRegistry(t, trk, nil), builtin.CommentOnWorkTool,
			map[string]any{"asks": false, "item": "ENG-1", "body": "?", "ask": "pm",
				"decision": informedDecision(c.surface, c.channel)})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			!strings.Contains(got.Output, c.refusal) {
			t.Errorf("%s: gave %+v — want invalid, listing %q", name, got, c.refusal)
		}
		if len(trk.patched) != 0 {
			t.Errorf("%s: the refused inform was written", name)
		}
	}

	trk := newFakeTracker()
	got := callWork(t, informRegistry(t, trk, nil), builtin.CommentOnWorkTool,
		map[string]any{"asks": false, "item": "ENG-1", "body": "?", "ask": "pm",
			"decision": informedDecision("slack", "#releases")})
	if got.Failed {
		t.Fatalf("a declared channel on a held surface was refused: %s", got.Output)
	}
	inform := trk.patched[0].Comment.Decision.Inform
	if inform == nil || inform.Surface != decision.InformSlack || inform.Channel != "releases" {
		t.Errorf("the inform is stored as %+v, want slack/releases as the chart spells it",
			inform)
	}

	// A SURFACE WITH NO CHART refuses rather than promising unchecked.
	bare := newFakeTracker()
	got = callWork(t, decisionRegistry(t, bare, newFakeKB()), builtin.CommentOnWorkTool,
		map[string]any{"asks": false, "item": "ENG-1", "body": "?", "ask": "pm",
			"decision": informedDecision("slack", "releases")})
	if !got.Failed || len(bare.patched) != 0 {
		t.Errorf("an inform nothing could check was accepted: %+v", got)
	}
}

// THE ENGINE'S OWN CHAT IS A SURFACE AN INFORM CAN NAME, under the one
// spelling that can be kept.
//
// An inform is kept by OWING its surface: the asker's answered turn is held
// until a tool that delivers there has run, and Chatlet's posting
// tools deliver to the chat's own feed source. So the surface a decision names
// must BE that source — any other spelling is a promise no tool discharges,
// and a company on Chatlet could promise nothing at all. The tool offers
// it, the value layer accepts it, and a seat that can post there promises it.
//
// Mutation: drop InformChat from decision.InformSurfaces and the schema offers
// no `chat`, and the inform is refused.
func TestAnInformCanNameTheEnginesOwnChat(t *testing.T) {
	t.Parallel()
	if string(decision.InformChat) != chatlet.Source {
		t.Fatalf("the Chatlet inform surface is %q and the chat tools deliver to "+
			"%q — the owed post could never be discharged", decision.InformChat,
			chatlet.Source)
	}
	for _, name := range []string{builtin.CreateWorkItemTool, builtin.CommentOnWorkTool} {
		schema := registered(t, builtin.Deps{Work: builtin.WorkDeps{
			Reader: newFakeTracker(), Writer: newFakeTracker().as,
		}}, name).Parameters()
		encoded, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"`+string(decision.InformChat)+`"`) {
			t.Errorf("%s does not offer %q as an inform surface: %s", name,
				decision.InformChat, encoded)
		}
	}

	trk := newFakeTracker()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			Channels: fakeChannels{
				surfaces: map[string][]decision.InformSurface{"eng": {decision.InformChat}},
				channels: []string{"releases"},
			},
		},
		Pages: builtin.PageDeps{Reader: newFakeKB()},
	})); err != nil {
		t.Fatalf("register: %v", err)
	}
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "body": "?", "ask": "pm",
		"decision": informedDecision(string(decision.InformChat), "#releases"),
	})
	if got.Failed {
		t.Fatalf("an inform on the engine's own chat was refused: %s", got.Output)
	}
	inform := trk.patched[0].Comment.Decision.Inform
	if inform == nil || inform.Surface != decision.InformChat || inform.Channel != "releases" {
		t.Errorf("the inform is stored as %+v, want chat/releases", inform)
	}
}

// AN AGENT SEAT'S QUESTION TO A PERSON IS A DECISION (ADR-0100), on both
// asking tools, refused before anything is written with a refusal that says
// how to turn it into one: before, over a third of the questions a person was
// asked were requests in prose ("the merge is yours") with nothing to press.
// A question between two agent seats may still be asked in words, and so may
// a person's own; a handle no roster can place is asked as of a person. And an
// agent's decision without the option it recommends, or without why, is
// refused, where a person's, who may have no view, is posted.
//
// Mutation: return nil from checkAskOfPerson, and the two prose questions to
// a person are posted; drop the roster lookup, and every agent-to-agent
// question in words is refused.
func TestAnAgentsQuestionToAPersonIsADecision(t *testing.T) {
	t.Parallel()
	roster := func() []colleague.Seat {
		return []colleague.Seat{
			{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
			{Handle: "morgan", Name: "Morgan", Kind: "human"},
		}
	}
	person := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "jane", Kind: worklet.AuthorHuman}, nil
	}
	unrecommended := decisionArg()
	delete(unrecommended, "recommended")
	delete(unrecommended, "rationale")
	comment := func(ask string, decided map[string]any) map[string]any {
		args := map[string]any{"item": "ENG-1", "body": "The merge is yours.", "ask": ask, "asks": true}
		if decided != nil {
			args["decision"] = decided
		}
		return args
	}
	create := func(ask string, decided map[string]any) map[string]any {
		args := map[string]any{"title": "Merge PR 24 now?", "project": "ENG", "ask": ask}
		if decided != nil {
			args["decision"] = decided
		}
		return args
	}
	for _, c := range []struct {
		name  string
		seats func() []colleague.Seat
		actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		tool  string
		args  map[string]any
		want  string // "" when the question must be posted
	}{
		{"a comment asking a person in words", roster, nil,
			builtin.CommentOnWorkTool, comment("morgan", nil), "carries a `decision`"},
		{"an item filed as a question to a person in words", roster, nil,
			builtin.CreateWorkItemTool, create("morgan", nil), "carries a `decision`"},
		{"a question no roster can place", nil, nil,
			builtin.CommentOnWorkTool, comment("pm", nil), "carries a `decision`"},
		{"a comment asking a person with no recommendation", roster, nil,
			builtin.CommentOnWorkTool, comment("morgan", unrecommended), "decision.recommended is empty"},
		{"an item asking a person with no rationale", roster, nil,
			builtin.CreateWorkItemTool, create("morgan", unrecommended), "decision.rationale is empty"},

		{"a comment asking a person with a decision", roster, nil,
			builtin.CommentOnWorkTool, comment("morgan", decisionArg()), ""},
		{"an item filed to a person with a decision", roster, nil,
			builtin.CreateWorkItemTool, create("morgan", decisionArg()), ""},
		{"a comment asking an agent seat in words", roster, nil,
			builtin.CommentOnWorkTool, comment("pm", nil), ""},
		{"an item filed to an agent seat in words", roster, nil,
			builtin.CreateWorkItemTool, create("pm", nil), ""},
		{"a person asking a person in words", roster, person,
			builtin.CommentOnWorkTool, comment("morgan", nil), ""},
		{"a person's decision with no recommendation", roster, person,
			builtin.CommentOnWorkTool, comment("morgan", unrecommended), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, gated(builtin.Deps{
				Work:  builtin.WorkDeps{Reader: trk, Writer: trk.as, Seats: c.seats, Actor: c.actor},
				Pages: builtin.PageDeps{Reader: newFakeKB()},
			})); err != nil {
				t.Fatalf("register: %v", err)
			}
			got := callWork(t, reg, c.tool, c.args)
			written := len(trk.patched) + len(trk.created)
			switch {
			case c.want == "" && got.Failed:
				t.Fatalf("the question was refused: %s", got.Output)
			case c.want == "" && written != 1:
				t.Fatalf("the question was not written once: %d writes", written)
			case c.want == "":
				return
			case !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid:
				t.Fatalf("the question was not refused as invalid: %+v", got)
			case !strings.Contains(got.Output, c.want):
				t.Errorf("the refusal %q does not say %q", got.Output, c.want)
			case written != 0:
				t.Errorf("a refused question was written: %d writes", written)
			}
		})
	}
}

// AN AGENT'S COMMENT THAT ASKS AND NAMES A PERSON PUTS THE QUESTION AS AN ASK
// (ADR-0100): otherwise it reaches the person only as a mention, never among the
// decisions waiting on them. Asking an agent, only mentioning a person, naming a
// team, or a person's own comment are posted as before.
//
// Mutation: return nil from checkAsksOfNamed, and the agent's request that
// names a person in words is posted.
func TestAnAgentsRequestThatNamesAPersonIsAnAsk(t *testing.T) {
	t.Parallel()
	roster := func() []colleague.Seat {
		return []colleague.Seat{
			{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
			{Handle: "ana", Name: "Ana", Kind: "human"},
		}
	}
	person := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "jane", Kind: worklet.AuthorHuman}, nil
	}
	comment := func(asks bool, ask string, decided map[string]any) map[string]any {
		args := map[string]any{"item": "ENG-1", "body": "Can you merge it?", "asks": asks}
		if ask != "" {
			args["ask"] = ask
		}
		if decided != nil {
			args["decision"] = decided
		}
		return args
	}
	for _, c := range []struct {
		name     string
		mentions fakeMentions
		actor    func(context.Context, *turnctx.Turn) (builtin.Actor, error)
		args     map[string]any
		refused  bool
	}{
		{"an agent asks a named person in words", fakeMentions{"ana"}, nil, comment(true, "", nil), true},
		{"an agent asks an agent and names a person", fakeMentions{"pm", "ana"}, nil, comment(true, "pm", nil), true},
		{"an agent asks the named person with a decision", fakeMentions{"ana"}, nil, comment(true, "ana", decisionArg()), false},
		{"an agent asks a named agent in words", fakeMentions{"pm"}, nil, comment(true, "", nil), false},
		{"an agent only mentions a person", fakeMentions{"ana"}, nil, comment(false, "", nil), false},
		{"an agent asks and names a team", fakeMentions{"eng"}, nil, comment(true, "", nil), false},
		{"a person asks a named person in words", fakeMentions{"ana"}, person, comment(true, "", nil), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, gated(builtin.Deps{
				Work: builtin.WorkDeps{Reader: trk, Writer: trk.as, Seats: roster, Actor: c.actor,
					Mentions: c.mentions},
				Pages: builtin.PageDeps{Reader: newFakeKB()},
			})); err != nil {
				t.Fatalf("register: %v", err)
			}
			got := callWork(t, reg, builtin.CommentOnWorkTool, c.args)
			switch {
			case !c.refused && got.Failed:
				t.Fatalf("the comment was refused: %s", got.Output)
			case !c.refused && len(trk.patched) != 1:
				t.Fatalf("the comment was not written once: %d writes", len(trk.patched))
			case !c.refused:
			case !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid:
				t.Fatalf("the comment was not refused as invalid: %+v", got)
			case !strings.Contains(got.Output, "set `ask` to their handle"):
				t.Errorf("the refusal %q does not say how to ask", got.Output)
			case len(trk.patched) != 0:
				t.Errorf("a refused comment was written: %d writes", len(trk.patched))
			}
		})
	}
}
