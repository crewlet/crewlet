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

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A DECISION IS REFUSED AT WRITE FOR EVERY WAY IT CAN BE MALFORMED, naming the
// field, and a well-formed one passes untouched.
//
// Each row is one clause of [worklet.Decision.Validate]; a clause that stopped
// refusing would let a decision through that a person cannot answer — one
// option, two options with one id, a recommendation that is not on offer, a
// link that is not https — and nothing downstream checks again.
func TestADecisionIsValidatedField(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, c := range []struct {
		name  string
		edit  func(*worklet.Decision)
		field string // "" when the decision must pass
	}{
		{"well formed", func(*worklet.Decision) {}, ""},
		{"a contributor", func(d *worklet.Decision) { d.Role = worklet.RoleContributor }, ""},
		{"six options", func(d *worklet.Decision) {
			for i := len(d.Options); i < worklet.MaxDecisionOptions; i++ {
				d.Options = append(d.Options, worklet.DecisionOption{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, ""},
		{"no recommendation", func(d *worklet.Decision) { d.Recommended = "" }, ""},
		{"every evidence kind", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{
				{Kind: worklet.EvidenceTask, Ref: "ENG-1"},
				{Kind: worklet.EvidencePage, Ref: "p-1"},
				{Kind: worklet.EvidenceTurn, Ref: "turn-1"},
				{Kind: worklet.EvidenceRun, Ref: "run-1"},
				{Kind: worklet.EvidenceURL, Ref: "https://example.com/a", Label: "a"},
			}
		}, ""},
		{"an inform", func(d *worklet.Decision) {
			d.Inform = &worklet.Inform{Surface: worklet.InformSlack, Channel: "eng"}
		}, ""},
		{"an inform on the engine's own chat", func(d *worklet.Decision) {
			d.Inform = &worklet.Inform{Surface: worklet.InformChat, Channel: "eng"}
		}, ""},

		{"no question", func(d *worklet.Decision) { d.Question = "  " }, "decision.question"},
		{"a long question", func(d *worklet.Decision) {
			d.Question = long(worklet.MaxDecisionQuestion + 1)
		}, "decision.question"},
		{"one option", func(d *worklet.Decision) { d.Options = d.Options[:1] }, "at least 2"},
		{"seven options", func(d *worklet.Decision) {
			for i := len(d.Options); i <= worklet.MaxDecisionOptions; i++ {
				d.Options = append(d.Options, worklet.DecisionOption{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, "maximum is 6"},
		{"an id with a capital", func(d *worklet.Decision) { d.Options[0].ID = "Ship" }, "options[0].id"},
		{"an id with a space", func(d *worklet.Decision) { d.Options[1].ID = "hold on" }, "options[1].id"},
		{"a 33-byte id", func(d *worklet.Decision) { d.Options[0].ID = long(33) }, "options[0].id"},
		{"two options one id", func(d *worklet.Decision) { d.Options[1].ID = "ship" }, "names two options"},
		{"no label", func(d *worklet.Decision) { d.Options[0].Label = "" }, "options[0].label"},
		{"a long label", func(d *worklet.Decision) {
			d.Options[0].Label = long(worklet.MaxOptionLabel + 1)
		}, "options[0].label"},
		{"a long detail", func(d *worklet.Decision) {
			d.Options[1].Detail = long(worklet.MaxOptionDetail + 1)
		}, "options[1].detail"},
		{"a recommendation not on offer", func(d *worklet.Decision) { d.Recommended = "wait" }, "decision.recommended"},
		{"a long rationale", func(d *worklet.Decision) {
			d.Rationale = long(worklet.MaxDecisionRationale + 1)
		}, "decision.rationale"},
		{"nine pieces of evidence", func(d *worklet.Decision) {
			for range worklet.MaxDecisionEvidence + 1 {
				d.Evidence = append(d.Evidence, worklet.Evidence{
					Kind: worklet.EvidenceTurn, Ref: "turn"})
			}
		}, "maximum is 8"},
		{"an unknown evidence kind", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{{Kind: "doc", Ref: "x"}}
		}, "evidence[0].kind"},
		{"evidence with no ref", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{{Kind: worklet.EvidenceTask}}
		}, "evidence[0].ref"},
		{"an http url", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{{Kind: worklet.EvidenceURL, Ref: "http://example.com"}}
		}, "https://"},
		{"a script url", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{{Kind: worklet.EvidenceURL, Ref: "javascript:alert(1)"}}
		}, "https://"},
		{"a relative url", func(d *worklet.Decision) {
			d.Evidence = []worklet.Evidence{{Kind: worklet.EvidenceURL, Ref: "/doclet/pages/1"}}
		}, "https://"},
		{"no role", func(d *worklet.Decision) { d.Role = "" }, "decision.role"},
		{"an unknown role", func(d *worklet.Decision) { d.Role = "owner" }, "decision.role"},
		{"an unknown surface", func(d *worklet.Decision) {
			d.Inform = &worklet.Inform{Surface: "teams", Channel: "eng"}
		}, "inform.surface"},
		{"an inform with no channel", func(d *worklet.Decision) {
			d.Inform = &worklet.Inform{Surface: worklet.InformMattermost}
		}, "inform.channel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := *decisionFixture()
			d.Options = append([]worklet.DecisionOption(nil), d.Options...)
			c.edit(&d)
			err := d.Validate()
			switch {
			case c.field == "" && err != nil:
				t.Fatalf("a well-formed decision was refused: %v", err)
			case c.field == "":
				return
			case err == nil:
				t.Fatalf("the decision was accepted; want a refusal naming %q", c.field)
			case !errors.Is(err, worklet.ErrInvalid):
				t.Fatalf("the refusal %v is not marked invalid, so a surface "+
					"would read it as the node's failure", err)
			case !strings.Contains(err.Error(), c.field):
				t.Fatalf("the refusal %q does not name %q", err, c.field)
			}
		})
	}
}

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
	d.Evidence = []worklet.Evidence{
		{Kind: worklet.EvidenceTask, Ref: "ENG-7", Label: "the audit"},
		{Kind: worklet.EvidencePage, Ref: "KB/Audit plan"},
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
		evidence worklet.Evidence
		invalid  bool
	}{
		"an unknown task": {worklet.Evidence{Kind: worklet.EvidenceTask, Ref: "ENG-404"}, true},
		"a missing page":  {worklet.Evidence{Kind: worklet.EvidencePage, Ref: "page-2"}, true},
		"a lookup down":   {worklet.Evidence{Kind: worklet.EvidencePage, Ref: "boom"}, false},
	} {
		d := *decisionFixture()
		d.Evidence = []worklet.Evidence{c.evidence}
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
		d.Evidence = []worklet.Evidence{{Kind: worklet.EvidenceTask, Ref: "GP-20", Label: c.Label}}
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
	for name, d := range map[string]*worklet.Decision{"changed": changed, "dropped": nil} {
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
	wake := func(body, choice string, decision *worklet.Decision) string {
		n := worklet.Wake{
			Kind: worklet.ChangeComment,
			Comment: &worklet.Comment{ID: "c-ans", Body: body, Answers: &answers,
				Choice: choice},
			AnswersDecision: decision,
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
func decisionFixture() *worklet.Decision {
	return &worklet.Decision{
		Question: "Ship on Friday or hold for the audit?",
		Options: []worklet.DecisionOption{
			{ID: "ship", Label: "Ship Friday"},
			{ID: "hold", Label: "Hold for the audit", Detail: "about a week"},
		},
		Recommended: "ship",
		Rationale:   "The audit reviews last quarter's code.",
		Role:        worklet.RoleApprover,
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

// EVERY FAULT OF A DECISION IN ONE REFUSAL (ADR-0059): an asker writes a
// refused decision again once, and a refusal naming one fault of several
// spent that attempt on the next.
//
// Mutation: return from Validate at its first fault and only the question is
// named.
func TestEveryFaultOfADecisionIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	d := decisionFixture()
	d.Question = ""
	d.Options[0].ID = "Ship"
	d.Options[1].Label = ""
	d.Recommended = "nope"
	d.Role = "boss"
	err := d.Validate()
	if !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("Validate = %v, want an invalid refusal", err)
	}
	for _, want := range []string{"decision.question is empty", "decision.options[0].id",
		"decision.options[1].label is empty", "decision.recommended is \"nope\"", "decision.role is \"boss\""} {
		if !strings.Contains(worklet.Sentence(err), want) {
			t.Errorf("the refusal %q does not name %q", worklet.Sentence(err), want)
		}
	}
}
