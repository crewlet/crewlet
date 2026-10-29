package worklet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE CHECKS THAT NEED THE WORLD: a task reference is stored as the task's ID —
// a key is retired by a move and the record is kept for ever — a page must
// exist, and a lookup that fails is the node's failure rather than a refusal
// of the decision.
func TestValidateDecisionResolvesTasksAndRequiresPages(t *testing.T) {
	t.Parallel()
	lookup := fakeEvidence{
		tasks: map[string]worklet.CitedTask{
			"ENG-7": {ID: "task-uuid-7", Keys: []string{"ENG-7"}},
		},
		pages: map[string]string{"KB/Audit plan": "page-uuid-1"},
	}
	d := *decisionFixture()
	d.Evidence = []decision.Evidence{
		{Kind: decision.EvidenceTask, Ref: "ENG-7", Label: "the audit"},
		{Kind: decision.EvidencePage, Ref: "KB/Audit plan"},
	}
	got, err := worklet.ValidateDecision(t.Context(), d, lookup)
	if err != nil {
		t.Fatalf("a decision citing a real task and page was refused: %v", err)
	}
	if got.Evidence[0].Ref != "task-uuid-7" {
		t.Errorf("the task reference is stored as %q, want the id", got.Evidence[0].Ref)
	}
	// A PAGE BY ITS ID TOO: its address is its title, and a rename
	// retires the title while the decision is kept for ever.
	if got.Evidence[1].Ref != "page-uuid-1" {
		t.Errorf("the page reference is stored as %q, want the id", got.Evidence[1].Ref)
	}
	if d.Evidence[0].Ref != "ENG-7" {
		t.Error("ValidateDecision rewrote its caller's evidence in place")
	}

	for name, c := range map[string]struct {
		evidence decision.Evidence
		invalid  bool
	}{
		"an unknown task": {decision.Evidence{Kind: decision.EvidenceTask, Ref: "ENG-404"}, true},
		"a missing page":  {decision.Evidence{Kind: decision.EvidencePage, Ref: "page-2"}, true},
		"a lookup down":   {decision.Evidence{Kind: decision.EvidencePage, Ref: "boom"}, false},
	} {
		d := *decisionFixture()
		d.Evidence = []decision.Evidence{c.evidence}
		_, err := worklet.ValidateDecision(t.Context(), d, lookup)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if errors.Is(err, worklet.ErrInvalid) != c.invalid {
			t.Errorf("%s: %v marked invalid %v, want %v", name, err,
				errors.Is(err, worklet.ErrInvalid), c.invalid)
		}
	}
}

// A TASK'S LABEL IS WHAT FOLLOWS ITS KEY: the Inbox draws a cited task as its
// current key and then its label, so a label that repeats the key read it
// twice ("GP-20 GP-20 sitemap fix"). A former key is the task's too; a key
// that only starts like it (GP-2 of GP-20, GP-20 of GP-20-b) is the label's
// own words. The cases are the dashboard's too (`lib/work.ts`'s
// labelAfterKey draws an evidence label by the same rule), read from one
// file so the two cannot drift.
//
// Mutation: store the label as written and every stripping case fails.
func TestATaskEvidenceLabelIsStoredWithoutTheKeyTheLinkSays(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "evidence_labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Keys  []string `json:"keys"`
		Cases []struct {
			Label string `json:"label"`
			Want  string `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) < 10 {
		t.Fatalf("the shared table holds %d cases, so this certifies nothing", len(table.Cases))
	}
	lookup := fakeEvidence{tasks: map[string]worklet.CitedTask{
		"GP-20": {ID: "task-20", Keys: table.Keys},
	}}
	for _, c := range table.Cases {
		d := *decisionFixture()
		d.Evidence = []decision.Evidence{{Kind: decision.EvidenceTask, Ref: "GP-20", Label: c.Label}}
		got, err := worklet.ValidateDecision(t.Context(), d, lookup)
		if err != nil {
			t.Fatalf("%q: refused: %v", c.Label, err)
		}
		if got.Evidence[0].Label != c.Want {
			t.Errorf("the label %q is stored as %q, want %q", c.Label, got.Evidence[0].Label, c.Want)
		}
	}
}

// A CHOICE MUST NAME AN OPTION OF THE ASK IT ANSWERS — checked in the write's
// own snapshot against the ask's row, so no caller can land an answer choosing
// something that was never offered, and one answering a question that offered
// nothing is told to answer in the body.
func TestAChoiceMustNameAnOptionOfTheAskItAnswers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "decide", "ana")
	askOn(t, r, "op-ask", "decide", worklet.Comment{
		ID: "c-ask", Task: "decide", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	})
	askOn(t, r, "op-plain", "decide", worklet.Comment{
		ID: "c-plain", Task: "decide", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	})

	answer := func(opID, id, answers, choice string) error {
		_, err := r.writer.UpdateTask(t.Context(), opID, "decide", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: id, Task: "decide", Author: "ana", AuthorKind: worklet.AuthorHuman,
				Body: "because", Answers: &answers, Choice: choice, CreatedAt: wednesday,
			}}, worklet.ChangeComment, nil)
		return err
	}
	err := answer("op-bad", "c-bad", "c-ask", "wait")
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), "ship, hold") {
		t.Fatalf("a choice the ask did not offer: %v — want invalid, listing "+
			"the options", err)
	}
	err = answer("op-none", "c-none", "c-plain", "ship")
	if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), "carries no decision") {
		t.Fatalf("a choice on an ask that offered none: %v", err)
	}
	// AND THE THREAD READ REFUSES IT TOO, before anything is published.
	if _, err := r.reader.Thread(t.Context(), worklet.ThreadQuery{
		Task: "decide", Answers: "c-ask", Author: "ana", Choice: "wait",
	}, statelog.Freshness{Level: statelog.ReadStale}); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("the thread read passed a choice the ask did not offer: %v", err)
	}
	// A choice on a comment that answers nothing is refused before the
	// publish: a choice is an answer.
	if _, err := r.writer.UpdateTask(t.Context(), "op-loose", "decide", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "c-loose", Task: "decide", Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "ship", Choice: "ship",
			CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a choice answering nothing: %v", err)
	}

	thread, err := r.reader.Thread(t.Context(), worklet.ThreadQuery{
		Task: "decide", Answers: "c-ask", Author: "ana", Choice: "hold",
	}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("the thread read refused a choice the ask offers: %v", err)
	}
	if thread.AnswersDecision == nil || thread.AnswersDecision.Question != decisionFixture().Question {
		t.Fatalf("the thread read carries %+v, want the ask's decision", thread.AnswersDecision)
	}
	if err := answer("op-good", "c-good", "c-ask", "hold"); err != nil {
		t.Fatalf("a choice the ask offers was refused: %v", err)
	}
	r.drain()
	detail, err := r.reader.Task(t.Context(), "decide",
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	var sawAsk, sawChoice bool
	for _, c := range detail.Comments {
		switch c.ID {
		case "c-ask":
			sawAsk = c.Decision != nil && len(c.Decision.Options) == 2
		case "c-good":
			sawChoice = c.Choice == "hold"
		case "c-bad", "c-none", "c-loose":
			t.Errorf("the refused comment %s landed", c.ID)
		}
	}
	if !sawAsk || !sawChoice {
		t.Errorf("the thread does not carry the decision (%v) and the choice "+
			"(%v) it was written with: %+v", sawAsk, sawChoice, detail.Comments)
	}
}

// A DECISION IS SET ONLY WHEN THE QUESTION IS ASKED — never on a remark, and
// never changed by an edit, because an answer names an option by id and
// options edited under it would make that answer mean something else.
func TestADecisionRidesOnlyTheAskAndNeverChanges(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "fixed", "ana")
	write := func(opID string, c worklet.Comment) error {
		c.Task, c.Author, c.AuthorKind, c.CreatedAt = "fixed", "dev", worklet.AuthorAgent, wednesday
		_, err := r.writer.UpdateTask(t.Context(), opID, "fixed", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := write("op-remark", worklet.Comment{ID: "c-r", Body: "fyi",
		Decision: decisionFixture()}); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("a decision on a comment that asks nobody: %v", err)
	}
	if err := write("op-ask", worklet.Comment{ID: "c-a", Body: "Friday?", Ask: "ana",
		Decision: decisionFixture()}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// AN EDIT THAT KEEPS IT LANDS — a body can be corrected —
	if err := write("op-edit", worklet.Comment{ID: "c-a", Body: "Friday, then?",
		Ask: "ana", Decision: decisionFixture()}); err != nil {
		t.Fatalf("an edit keeping the decision was refused: %v", err)
	}
	// — and one that changes it, or drops it, does not.
	changed := decisionFixture()
	changed.Options[1].Label = "Ship Monday"
	for name, d := range map[string]*decision.Decision{"changed": changed, "dropped": nil} {
		if err := write("op-"+name, worklet.Comment{ID: "c-a", Body: "Friday?",
			Ask: "ana", Decision: d}); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("an edit that %s the decision: %v", name, err)
		}
	}
	// Nor can a remark acquire one after the fact.
	if err := write("op-plain", worklet.Comment{ID: "c-p", Body: "which?", Ask: "ana"}); err != nil {
		t.Fatalf("plain ask: %v", err)
	}
	if err := write("op-late", worklet.Comment{ID: "c-p", Body: "which?", Ask: "ana",
		Decision: decisionFixture()}); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a decision added to an existing ask: %v", err)
	}
}

// A SECOND ANSWER TO ONE ASK IS REFUSED, naming who answered and when.
//
// The thread read that routes a wake runs BEFORE the publish, so two answers
// decided at once both passed it; the applier's `answered_by IS NULL` then
// kept the first on the ask while the second landed anyway, claiming to answer
// a question it did not close. The check is in the write's own snapshot now —
// this writes the second answer straight at the writer, past any thread read,
// which is exactly the racing answer's position.
func TestASecondAnswerToOneAskIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "once", "ana")
	askOn(t, r, "op-ask", "once", worklet.Comment{
		ID: "c-ask", Task: "once", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	})
	answered := wednesday.Add(time.Hour)
	answer := func(opID, id string, at time.Time) error {
		answers := "c-ask"
		_, err := r.writer.UpdateTask(t.Context(), opID, "once", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: id, Task: "once", Author: "ana", AuthorKind: worklet.AuthorHuman,
				Body: "yes", Answers: &answers, Choice: "ship", CreatedAt: at,
			}}, worklet.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := answer("op-first", "c-first", answered); err != nil {
		t.Fatalf("the first answer: %v", err)
	}
	// A RETRY OF THE SAME ANSWER is not a second one: its first attempt
	// landed, and refusing it would report a success as a failure.
	if err := answer("op-first-again", "c-first", answered); err != nil {
		t.Fatalf("a retried answer was refused as a second one: %v", err)
	}

	err := answer("op-second", "c-second", answered.Add(time.Minute))
	var already *worklet.AlreadyAnsweredError
	if !errors.As(err, &already) || !errors.Is(err, worklet.ErrAlreadyAnswered) {
		t.Fatalf("the second answer: %v — want already answered", err)
	}
	if already.By != "ana" || !already.At.Equal(answered) {
		t.Errorf("the refusal names %q at %v, want ana at %v", already.By,
			already.At, answered)
	}
	if !strings.Contains(err.Error(), "ana") ||
		!strings.Contains(err.Error(), answered.Format(time.RFC3339)) {
		t.Errorf("the sentence %q does not say who answered and when", err)
	}
	detail, err := r.reader.Task(t.Context(), "once",
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	for _, c := range detail.Comments {
		if c.ID == "c-second" {
			t.Error("the second answer landed beside the first")
		}
	}

	// AND THE THREAD READ SAYS THE SAME, so a tool refuses before publishing.
	_, err = r.reader.Thread(t.Context(), worklet.ThreadQuery{
		Task: "once", Answers: "c-ask", Author: "ana", Comment: "c-third",
	}, statelog.Freshness{Level: statelog.ReadStale})
	if !errors.As(err, &already) || already.By != "ana" {
		t.Fatalf("the thread read: %v — want already answered by ana", err)
	}
}

// THE ANSWER'S CARD LEADS WITH THE CHOICE, by its label: the id alone tells
// the asker nothing, and cut at the cap a card that led with the body could
// lose the one thing the asker was waiting for.
func TestTheAnswerExcerptNamesTheChoice(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	wake := func(body, choice string, asked *decision.Decision) string {
		n := worklet.Wake{
			Kind: worklet.ChangeComment,
			Comment: &worklet.Comment{ID: "c-ans", Body: body, Answers: &answers,
				Choice: choice},
			AnswersDecision: asked,
		}.Notify(nil)
		if n == nil {
			t.Fatal("an answer woke nobody")
		}
		return n.Excerpt
	}
	if got := wake("the audit can wait", "ship", decisionFixture()); got != "Chose “Ship Friday”: the audit can wait" {
		t.Errorf("the excerpt is %q", got)
	}
	if got := wake("", "hold", decisionFixture()); got != "Chose “Hold for the audit”" {
		t.Errorf("a choice with no body reads %q", got)
	}
	if got := wake(strings.Repeat("because ", 200), "ship", decisionFixture()); !strings.HasPrefix(got, "Chose “Ship Friday”: because") ||
		len(got) > worklet.MaxExcerpt {
		t.Errorf("a long answer's excerpt is %d bytes and reads %.40q…", len(got), got)
	}
	if got := wake("eu-west-1", "", nil); got != "eu-west-1" {
		t.Errorf("an answer without a choice reads %q", got)
	}
}

// AN ASK ROW CARRIES ITS DECISION, and the call that answers it carries the
// recommendation as the choice — a reader who agrees sends it as written.
func TestAnAskRowCarriesItsDecision(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "asked", "ana")
	askOn(t, r, "op-ask", "asked", worklet.Comment{
		ID: "c-1", Task: "asked", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	})
	got := r.myWork("ana")
	if len(got.AskedOfMe) != 1 {
		t.Fatalf("asked_of_me is %+v, want the one ask", got.AskedOfMe)
	}
	row := got.AskedOfMe[0]
	if row.Decision == nil || row.Decision.Recommended != "ship" || len(row.Decision.Options) != 2 {
		t.Fatalf("the ask row carries %+v, want its decision", row.Decision)
	}
	if !row.Open {
		t.Error("an ask waiting on an answer is not open")
	}
	if !strings.Contains(row.Answer, `choice: "ship"`) || !strings.Contains(row.Answer, `answers: "c-1"`) {
		t.Errorf("the answering call %q does not carry the recommendation", row.Answer)
	}
}

// decisionFixture is a fresh well-formed decision the caller may change.
func decisionFixture() *decision.Decision {
	return &decision.Decision{
		Question: "Ship on Friday or hold for the audit?",
		Options: []decision.Option{
			{ID: "ship", Label: "Ship Friday"},
			{ID: "hold", Label: "Hold for the audit", Detail: "about a week"},
		},
		Recommended: "ship",
		Rationale:   "The audit reviews last quarter's code.",
		Role:        decision.RoleApprover,
	}
}

type fakeEvidence struct {
	tasks map[string]worklet.CitedTask
	pages map[string]string
}

func (f fakeEvidence) Task(_ context.Context, ref string) (worklet.CitedTask, error) {
	if task, ok := f.tasks[ref]; ok {
		return task, nil
	}
	return worklet.CitedTask{}, fmt.Errorf("resolve %s: %w", ref, worklet.ErrNoTask)
}

func (f fakeEvidence) PageID(_ context.Context, ref string) (string, bool, error) {
	if ref == "boom" {
		return "", false, errors.New("doclet: read: database is locked")
	}
	id, ok := f.pages[ref]
	return id, ok, nil
}
