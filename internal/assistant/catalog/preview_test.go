package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// wednesday is the instant every preview here is made at.
var wednesday = time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)

// reader is a turn grant over answers, refusing anything it does not hold.
func reader(answers map[string]any) proposal.Read {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		return assistanttest.Answer(answers, r), nil
	}
}

// EVERY WRITE IS PREVIEWED FROM THE OBJECT AS IT IS NOW, and its preview sends
// exactly the request the tool builds.
//
// Each proposing tool's preview reads what it changes through the turn's
// grant, answers a card the proposal package accepts (a title, an approve
// label, at least one change, an audience exactly where the tool declares
// ChangesVisibility), binds the
// revision it read, and carries the request byte for byte. A share to the whole
// company asks a step-up (D09). Mutation: build the preview's request from
// other arguments than the tool's, or drop the share's audience, and this
// fails, and so does unsetting share_artifact's ChangesVisibility.
func TestEveryWriteIsPreviewedFromTheObjectAsItIsNow(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite && tool.Kind != catalog.KindPageAction {
			continue
		}
		args, ok := assistanttest.ProposalArgs[tool.Name]
		if !ok {
			t.Errorf("%s proposes and this suite does not preview it", tool.Name)
			continue
		}
		p, err := tool.Preview(t.Context(), read, wednesday, args)
		if err != nil {
			t.Errorf("%s: %v", tool.Name, err)
			continue
		}
		if err := p.Check(tool.ChangesVisibility); err != nil {
			t.Errorf("%s's preview: %v", tool.Name, err)
		}
		// A PREVIEW NAMING AN AUDIENCE IS A WRITE THAT DECLARES IT CHANGES
		// VISIBILITY, and the other way round, so the proposal is told to
		// hold the audience exactly where one is shown.
		if (len(p.Audience) > 0) != tool.ChangesVisibility {
			t.Errorf("%s previews the audience %v and declares ChangesVisibility %v",
				tool.Name, p.Audience, tool.ChangesVisibility)
		}
		// A CALL THE PREVIEW COMPLETES FROM THE OBJECT is the tool's own
		// request for the completed call: the principles' parts a change
		// leaves out are stated as they stand.
		built := args
		if whole, completes := completedArgs[tool.Name]; completes {
			built = whole
		}
		want, err := tool.Request(built)
		if err != nil {
			t.Fatal(err)
		}
		// A WRITE WHOSE BODY IS THE OBJECT AS READ (a seat's ceiling, sent
		// back as the seat) sends the tool's own request with that body.
		if body, reads := objectBodies[tool.Name]; reads {
			var got map[string]any
			if err := json.Unmarshal(p.Request.Body, &got); err != nil {
				t.Fatalf("%s previews a body that is not JSON: %v", tool.Name, err)
			}
			if !reflect.DeepEqual(got, body) {
				t.Errorf("%s previews the body %v, want %v", tool.Name, got, body)
			}
			want.Body = p.Request.Body
		}
		if !reflect.DeepEqual(p.Request, want) {
			t.Errorf("%s previews %+v and the tool sends %+v", tool.Name, p.Request, want)
		}
		if p.SourceRevision != "" {
			if _, err := proposal.ParseRevision(p.SourceRevision); err != nil {
				t.Errorf("%s binds %q: %v", tool.Name, p.SourceRevision, err)
			}
			now, err := catalog.Revision(t.Context(), read, p.SourceRevision)
			if err != nil || now != p.SourceRevision {
				t.Errorf("%s's revision %q re-reads as %q, %v", tool.Name, p.SourceRevision, now, err)
			}
		}
	}
	share, _ := catalog.Lookup("share_artifact")
	p, err := share.Preview(t.Context(), read, wednesday, assistanttest.ProposalArgs["share_artifact"])
	if err != nil || !p.StepUp || len(p.Audience) != 1 {
		t.Errorf("a company-wide share previews step-up %v, audience %v, %v", p.StepUp, p.Audience, err)
	}
	update, _ := catalog.Lookup("update_task")
	p, err = update.Preview(t.Context(), read, wednesday, assistanttest.ProposalArgs["update_task"])
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]wire.ChangeKind{}
	for _, c := range p.Changes {
		kinds[c.Field] = c.Kind
	}
	if kinds["status"] != wire.ChangeChanged || kinds["body"] != wire.ChangeChanged ||
		kinds["task"] != wire.ChangeFact || p.SourceRevision != "task:ENG-42@3" || p.Note == "" {
		t.Errorf("the update previews %+v at %q", p.Changes, p.SourceRevision)
	}
}

// completedArgs are the calls a preview completes from the object as it is,
// whole: [assistanttest.ProposalArgs]' principles change with the parts it
// leaves out as [assistanttest.World] states them.
var completedArgs = map[string]map[string]any{
	// A CREATE THAT NAMES NO PROJECT OR CONTAINER carries the person's own,
	// as GET /viewer answers it.
	"create_task": {"title": "File the report", "assignee": "pm", "body": "Details.",
		"project": "ENG"},
	"create_page": {"title": "Onboarding", "body": "Welcome.", "container": "ENG"},
	"update_company_principles": {"mission": "Help friends share costs fairly.",
		"vision": "Settling up takes seconds.", "policies": []any{"Miles is the final " +
			"authority.", "Money is an integer number of cents."}},
	// A MEMBERSHIP STATES EVERY MEMBER'S FOLLOWING: pm, named by handle
	// alone, keeps following the whole room.
	"set_chat_room_members": {"channel": assistanttest.RecordID, "members": []any{
		map[string]any{"handle": "pm", "follow_all": true},
		map[string]any{"handle": "engineer", "follow_all": false}},
		"removes": []any{"ada"}},
}

// objectBodies are the bodies of the writes that send the object as the
// preview read it, changed: set_token_budget's seat as [assistanttest.World]
// holds it, with only the one window's ceiling moved and the history line, and
// set_chat_room_members' call with the room's membership as read beside it.
var objectBodies = map[string]map[string]any{
	"set_token_budget": {"handle": "qa", "name": "QA Engineer", "goal": "Review the tests.",
		"token_budget": map[string]any{"day": float64(1500000)},
		catalog.SummaryKey: "Raise QA Engineer's daily token budget from 1,000,000 tokens " +
			"to 1,500,000 tokens"},
	"set_seat_access": {"seats": map[string]any{"qa": map[string]any{"datadog": map[string]any{
		"enabled": true, "level": "role-ro"}}}},
	// THE ROOM'S MEMBERSHIP AS THE CARD READ IT rides the call as its
	// expectation, beside the membership the call states.
	"set_chat_room_members": {"args": map[string]any{
		"channel": assistanttest.RecordID,
		"members": []any{map[string]any{"handle": "pm", "follow_all": true},
			map[string]any{"handle": "engineer", "follow_all": false}},
		"expected_members": []any{map[string]any{"handle": "pm", "follow_all": true},
			map[string]any{"handle": "ada", "follow_all": false}},
	}},
}

// A CHANGE PREPARED AGAINST AN OLD VERSION IS STALE, and a revision that moved
// re-reads as another, so an approval never applies what the person did not
// see. Mutation: drop the if_match comparison and this fails.
func TestAChangePreparedAgainstAnOldVersionIsStale(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	update, _ := catalog.Lookup("update_task")
	args := map[string]any{"item": "ENG-42", "if_match": float64(2), "status": "done"}
	if _, err := update.Preview(t.Context(), read, wednesday, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an update against version 2 of a task at 3 = %v, want ErrStale", err)
	}
	edit, _ := catalog.Lookup("edit_page")
	args = map[string]any{"page": assistanttest.RecordID, "base_version": float64(1), "body": "x"}
	if _, err := edit.Preview(t.Context(), read, wednesday, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an edit against version 1 of a page at 2 = %v, want ErrStale", err)
	}
	moved := assistanttest.World()
	moved["/worklet/ENG-42"].(map[string]any)["task"].(map[string]any)["version"] = 4
	if now, err := catalog.Revision(t.Context(), reader(moved), "task:ENG-42@3"); err != nil ||
		now != "task:ENG-42@4" {
		t.Errorf("a moved task re-reads as %q, %v", now, err)
	}
	if _, err := catalog.Revision(t.Context(), read, "task:ENG-404@1"); !errors.Is(err,
		catalog.ErrPreviewRead) {
		t.Errorf("a task the person may not read = %v, want ErrPreviewRead", err)
	}
}

// A MARK-INBOX CARD NAMES EVERY NOTICE IT CLEARS, read from the person's own
// inbox through the grant, so a notice the person did not mean to clear (a
// security notice a prompt talked the model into marking) is on the card they
// approve. A notice the inbox does not hold is said to be missing, and a long
// list names ten and counts the rest. Mutation: preview the counts alone and
// the named notices are missing.
func TestAMarkInboxCardNamesEveryNotice(t *testing.T) {
	t.Parallel()
	mark, _ := catalog.Lookup("mark_inbox")
	p, err := mark.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"read": []any{"r1", "r2", "r9"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range p.Changes {
		got = append(got, c.Label+": "+c.To)
	}
	want := []string{"Mark read: ENG-42: Ship it moved to review.",
		"Mark read: SEC-7: Rotate the signing key.",
		"Mark read: A notice not in your inbox (r9)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the card reads %q, want %q", got, want)
	}

	many := make([]any, 13)
	for i := range many {
		many[i] = fmt.Sprintf("x%d", i)
	}
	p, err = mark.Preview(t.Context(), reader(assistanttest.World()), wednesday, map[string]any{"unread": many})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 11 || p.Changes[10].To != "And 3 more" {
		t.Errorf("thirteen notices preview as %d rows ending %+v", len(p.Changes),
			p.Changes[len(p.Changes)-1])
	}
}

// A TASK'S CARD SHOWS WHAT THE WRITE CARRIES: a relative due date resolved on
// the company's clock, through the function the act tool resolves it with,
// and written into the request as that date, so an approval an hour or a day
// later writes the date the person saw; and a seat by its name, with a note in
// words. Mutation: preview the token as typed, or send it unresolved, and this
// fails.
func TestATaskCardShowsWhatTheWriteCarries(t *testing.T) {
	t.Parallel()
	create, _ := catalog.Lookup("create_task")
	p, err := create.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"title": "File the report", "assignee": "engineer", "due": "+7d"})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, c := range p.Changes {
		rows[c.Field] = c.To
	}
	if rows["due"] != "Wednesday 23 April 2031" ||
		rows["assignee"] != "Backend engineer (engineer)" {
		t.Errorf("the card reads due %q and assignee %q", rows["due"], rows["assignee"])
	}
	// THE WORLD HOLDS NO LIVE STATE FOR engineer, so the card promises only
	// the assignment (TestTheHandoverNoteSaysWhatTheSeatsStateSupports).
	if p.Note != "Backend engineer (engineer) is assigned the new task once this is approved." {
		t.Errorf("the note reads %q", p.Note)
	}
	var body struct {
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(p.Request.Body, &body); err != nil || body.Args["due"] != "2031-04-23" {
		t.Errorf("the request carries due %v (%v), want the resolved date", body.Args["due"], err)
	}

	p, err = create.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"title": "Review it", "assignee": "ada"})
	if err != nil || p.Note != "Ada Lovelace (ada) is notified of the new task once this is approved." {
		t.Errorf("a person's task notes %q, %v", p.Note, err)
	}
	if _, err := create.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"title": "Never", "due": "someday"}); !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Errorf("a due date that is no date previewed: %v", err)
	}
}

// A PAGE CHANGE'S ROWS READ AS WORDS once the screen is gone: a payload key is
// a label ("Due before") and a seat's handle its name, so a stored card does
// not say "assignee: engineer". Mutation: show the payload as it is and the
// rows read raw.
func TestAPageChangeReadsAsWords(t *testing.T) {
	t.Parallel()
	change, _ := catalog.Lookup("propose_page_change")
	p, err := change.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"action": "work.filter.apply",
			"args": map[string]any{"assignee": "engineer", "due_before": "2031-04-30"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, c := range p.Changes {
		rows[c.Label] = c.To
	}
	if rows["Assignee"] != "Backend engineer (engineer)" || rows["Due before"] != "2031-04-30" {
		t.Errorf("the page change reads %v", rows)
	}
}

// A PAGE CHANGE'S ROW IS NEVER LABELLED WITH A RAW PATH: a dotted key reads
// as its words, and a list as its items. The turn's open draft names a
// draft.set row with the screen's own label (internal/assistant's
// draftRows); this is what a row says where nothing does. Mutation: label a
// row with its key as written, or join a list into one line.
func TestAPageChangeIsNeverLabelledWithARawPath(t *testing.T) {
	t.Parallel()
	change, _ := catalog.Lookup("propose_page_change")
	p, err := change.Preview(t.Context(), reader(assistanttest.World()), wednesday,
		map[string]any{"action": catalog.DraftSet, "args": map[string]any{"values": map[string]any{
			"colors.dark_ground": "#090019", "voice.write_like": []any{"Short", "Plain"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Changes {
		if strings.Contains(c.Label, ".") || strings.Contains(c.Label, "_") {
			t.Errorf("row %s is labelled %q", c.Field, c.Label)
		}
		if c.Field == "voice.write_like" && !slices.Equal(c.Items, []string{"Short", "Plain"}) {
			t.Errorf("the list row carries the items %q", c.Items)
		}
	}
}

// A PRINCIPLES CHANGE IS A DIFF OF THE PRINCIPLES AS THEY ARE STORED.
//
// The preview reads the principles through the turn's grant and renders the
// mission's word diff from the stored mission (the stray "test" struck), and
// nothing for the vision and the policies the call left out, which the request
// states as they stand. A policy list is diffed policy by policy: one added,
// one removed and one reworded each read as one row, and a list that only
// gained a policy is not shown as every policy rewritten. The proposal is
// bound to the principles' entity tag, which re-reads as itself until the
// principles move, and the card says its approval asks a step-up. A call that
// changes nothing is refused. Mutation: diff against the call's own mission,
// or bind no revision, and this fails.
func TestAPrinciplesChangeIsADiffOfTheStoredPrinciples(t *testing.T) {
	t.Parallel()
	world := assistanttest.World()
	read := reader(world)
	tool, _ := catalog.Lookup("update_company_principles")
	p, err := tool.Preview(t.Context(), read, wednesday,
		assistanttest.ProposalArgs["update_company_principles"])
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Field != "mission" ||
		p.Changes[0].Kind != wire.ChangeText || p.Changes[0].From != assistanttest.WorldMission ||
		p.Changes[0].To != "Help friends share costs fairly." {
		t.Fatalf("the mission change previews %+v", p.Changes)
	}
	struck := false
	for _, w := range p.Changes[0].Words {
		struck = struck || (w.Op == wire.WordRemoved && strings.Contains(w.Text, "test"))
	}
	if !struck {
		t.Errorf("the word diff %+v does not strike the stray word", p.Changes[0].Words)
	}
	if !p.StepUp || !strings.HasPrefix(p.SourceRevision, "charter:company@charter-") {
		t.Errorf("the change asks step-up %v and binds %q", p.StepUp, p.SourceRevision)
	}
	if now, err := catalog.Revision(t.Context(), read, p.SourceRevision); err != nil ||
		now != p.SourceRevision {
		t.Errorf("the principles' revision re-reads as %q, %v", now, err)
	}
	moved := assistanttest.World()
	moved["/config/charter"] = assistanttest.Charter("Somebody else's mission.", "")
	if now, _ := catalog.Revision(t.Context(), reader(moved), p.SourceRevision); now == p.SourceRevision {
		t.Error("principles that moved re-read as the revision the proposal was bound to")
	}

	world["/config/charter"] = assistanttest.Charter("M.", "V.", "Keep it.", "Old wording.",
		"Gone soon.")
	p, err = tool.Preview(t.Context(), reader(world), wednesday, map[string]any{
		"policies": []any{"First.", "Keep it.", "New wording.", "Gone soon."}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[wire.ChangeKind]int{}
	for _, c := range p.Changes {
		kinds[c.Kind]++
	}
	if len(p.Changes) != 2 || kinds[wire.ChangeAdded] != 1 || kinds[wire.ChangeText] != 1 {
		t.Errorf("a policy added and one reworded previews %+v", p.Changes)
	}
	p, err = tool.Preview(t.Context(), reader(world), wednesday, map[string]any{
		"policies": []any{"Keep it.", "Old wording."}})
	if err != nil || len(p.Changes) != 1 || p.Changes[0].Kind != wire.ChangeRemoved ||
		p.Changes[0].From != "Gone soon." {
		t.Errorf("a policy removed previews %+v, %v", p.Changes, err)
	}
	if _, err := tool.Preview(t.Context(), reader(world), wednesday,
		map[string]any{"mission": "M."}); !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Errorf("a change that changes nothing answered %v", err)
	}
}

// THE HANDOVER NOTE PROMISES ONLY WHAT THE SEAT'S LIVE STATE SUPPORTS: an agent
// taking work picks it up, a stopped one is said to wait and for what, one
// whose last turn failed is flagged, and one whose state cannot be read is
// only said to be assigned. "Starts on the work once this is approved" was
// once printed for a seat that could not onboard. Mutation: drop the live read
// and every agent "picks the work up".
func TestTheHandoverNoteSaysWhatTheSeatsStateSupports(t *testing.T) {
	t.Parallel()
	create, _ := catalog.Lookup("create_task")
	const assigned = "Backend engineer (engineer) is assigned the new task once this is approved."
	for name, tc := range map[string]struct {
		live any
		want string
	}{
		"taking work": {map[string]any{"activity": "idle"},
			assigned + " It picks the work up on its next turn."},
		"last turn failed": {map[string]any{"activity": "idle",
			"last_turn": map[string]any{"outcome": "failed"}},
			assigned + " Its last turn failed, so check it before relying on it to start."},
		"paused": {map[string]any{"activity": "stopped", "stopped_reason": "paused"},
			assigned + " Backend engineer (engineer) is paused, so it does not start until " +
				"somebody resumes it."},
		"out of budget": {map[string]any{"activity": "stopped", "stopped_reason": "budget"},
			assigned + " Backend engineer (engineer) has used its token budget, so it does " +
				"not start until the budget resets or is raised."},
		"stopped for a reason this build does not know": {map[string]any{"activity": "stopped",
			"stopped_reason": "eclipse"}, assigned + " Backend engineer (engineer) cannot take " +
			"work right now, so it does not start until it can."},
		"unreadable": {nil, assigned},
	} {
		world := assistanttest.World()
		if tc.live != nil {
			world["/agents/engineer"] = map[string]any{"handle": "engineer", "live": tc.live}
		}
		p, err := create.Preview(t.Context(), reader(world), wednesday,
			map[string]any{"title": "File the report", "assignee": "engineer"})
		if err != nil || p.Note != tc.want {
			t.Errorf("%s: the note reads %q (%v), want %q", name, p.Note, err, tc.want)
		}
	}
}

// A PREVIEW READS THE SEAT ALONE: the seat route answers its call history by
// default (whole transcripts, megabytes for a busy seat, past what a preview
// reads), so every preview that reads a seat (the handover note of a task
// created or reassigned to it, a pause, a resume) asks with history=false,
// the only bound on that read. Mutation: drop seatAlone from handover or from
// seatPreview and this fails.
func TestAPreviewReadsTheSeatWithoutItsCalls(t *testing.T) {
	t.Parallel()
	for tool, args := range map[string]map[string]any{
		"create_task":  {"title": "File the report", "assignee": "engineer"},
		"update_task":  assistanttest.ProposalArgs["update_task"],
		"pause_agent":  assistanttest.ProposalArgs["pause_agent"],
		"resume_agent": assistanttest.ProposalArgs["resume_agent"],
	} {
		world := assistanttest.World()
		for _, handle := range []string{"engineer", "dev", "pm"} {
			world["/agents/"+handle] = map[string]any{"handle": handle, "role": handle,
				"live": map[string]any{"activity": "idle"}}
		}
		var asked []exec.Request
		read := func(ctx context.Context, r exec.Request) (exec.Response, error) {
			if strings.HasPrefix(r.Path, "/agents/") {
				asked = append(asked, r)
			}
			return reader(world)(ctx, r)
		}
		w, _ := catalog.Lookup(tool)
		if _, err := w.Preview(t.Context(), read, wednesday, args); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if len(asked) == 0 {
			t.Errorf("%s's preview read no seat", tool)
		}
		for _, r := range asked {
			if r.Query.Get("history") != "false" {
				t.Errorf("%s's preview asked %s?%s, with the seat's calls", tool, r.Path, r.Query.Encode())
			}
		}
	}
}

// AN ANSWER TO AN EXECUTOR IS PREVIEWED AGAINST THE QUESTION IT ANSWERS. A
// run that moved on to another question since the proposal was drafted is
// refused before anybody approves an answer to a question it no longer asks,
// and so is a choice its question does not offer; an offered choice is shown
// as the option's label, which is what the person approves (ADR-0068).
//
// Mutation: drop the launch check, and the stale proposal is previewed.
func TestARunsAnswerIsPreviewedAgainstItsQuestion(t *testing.T) {
	t.Parallel()
	read := reader(assistanttest.World())
	var tool catalog.Tool
	for _, one := range catalog.Tools() {
		if one.Name == "answer_executor" {
			tool = one
		}
	}
	p, err := tool.Preview(t.Context(), read, wednesday, map[string]any{
		"turn_id": assistanttest.RecordID, "launch_id": "launch-1", "choice": "main"})
	if err != nil {
		t.Fatalf("an answer to the question the run asks: %v", err)
	}
	shown := false
	for _, c := range p.Changes {
		raw, _ := json.Marshal(c)
		shown = shown || strings.Contains(string(raw), "Target main")
	}
	if !shown {
		t.Errorf("the preview %+v does not show the option chosen by its label", p.Changes)
	}
	for name, args := range map[string]map[string]any{
		"a question the run moved past": {"turn_id": assistanttest.RecordID,
			"launch_id": "launch-0", "choice": "main"},
		"a choice not offered": {"turn_id": assistanttest.RecordID,
			"launch_id": "launch-1", "choice": "rewrite"},
	} {
		if _, err := tool.Preview(t.Context(), read, wednesday, args); !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s previewed (%v), want it refused", name, err)
		}
	}
}
