package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
		body, ok := answers[r.Path]
		if !ok {
			return exec.Response{Status: 404,
				Body: []byte(`{"error":"not_found","message":"No such record."}`)}, nil
		}
		b, err := json.Marshal(body)
		return exec.Response{Status: 200, Body: b}, err
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
		want, err := tool.Request(args)
		if err != nil {
			t.Fatal(err)
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
	if p.Note != "Backend engineer (engineer) starts on the new task once this is approved." {
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
