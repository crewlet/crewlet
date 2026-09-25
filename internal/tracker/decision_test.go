package tracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A DECISION IS REFUSED AT WRITE FOR EVERY WAY IT CAN BE MALFORMED, naming the
// field, and a well-formed one passes untouched.
//
// Each row is one clause of [tracker.Decision.Validate]; a clause that stopped
// refusing would let a decision through that a person cannot answer — one
// option, two options with one id, a recommendation that is not on offer, a
// link that is not https — and nothing downstream checks again.
func TestADecisionIsValidatedField(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, c := range []struct {
		name  string
		edit  func(*tracker.Decision)
		field string // "" when the decision must pass
	}{
		{"well formed", func(*tracker.Decision) {}, ""},
		{"a contributor", func(d *tracker.Decision) { d.Role = tracker.RoleContributor }, ""},
		{"six options", func(d *tracker.Decision) {
			for i := len(d.Options); i < tracker.MaxDecisionOptions; i++ {
				d.Options = append(d.Options, tracker.DecisionOption{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, ""},
		{"no recommendation", func(d *tracker.Decision) { d.Recommended = "" }, ""},
		{"every evidence kind", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{
				{Kind: tracker.EvidenceTask, Ref: "ENG-1"},
				{Kind: tracker.EvidencePage, Ref: "p-1"},
				{Kind: tracker.EvidenceTurn, Ref: "turn-1"},
				{Kind: tracker.EvidenceRun, Ref: "run-1"},
				{Kind: tracker.EvidenceURL, Ref: "https://example.com/a", Label: "a"},
			}
		}, ""},
		{"an inform", func(d *tracker.Decision) {
			d.Inform = &tracker.Inform{Surface: tracker.InformSlack, Channel: "eng"}
		}, ""},

		{"no question", func(d *tracker.Decision) { d.Question = "  " }, "decision.question"},
		{"a long question", func(d *tracker.Decision) {
			d.Question = long(tracker.MaxDecisionQuestion + 1)
		}, "decision.question"},
		{"one option", func(d *tracker.Decision) { d.Options = d.Options[:1] }, "at least 2"},
		{"seven options", func(d *tracker.Decision) {
			for i := len(d.Options); i <= tracker.MaxDecisionOptions; i++ {
				d.Options = append(d.Options, tracker.DecisionOption{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, "maximum is 6"},
		{"an id with a capital", func(d *tracker.Decision) { d.Options[0].ID = "Ship" }, "options[0].id"},
		{"an id with a space", func(d *tracker.Decision) { d.Options[1].ID = "hold on" }, "options[1].id"},
		{"a 33-byte id", func(d *tracker.Decision) { d.Options[0].ID = long(33) }, "options[0].id"},
		{"two options one id", func(d *tracker.Decision) { d.Options[1].ID = "ship" }, "names two options"},
		{"no label", func(d *tracker.Decision) { d.Options[0].Label = "" }, "options[0].label"},
		{"a long label", func(d *tracker.Decision) {
			d.Options[0].Label = long(tracker.MaxOptionLabel + 1)
		}, "options[0].label"},
		{"a long detail", func(d *tracker.Decision) {
			d.Options[1].Detail = long(tracker.MaxOptionDetail + 1)
		}, "options[1].detail"},
		{"a recommendation not on offer", func(d *tracker.Decision) { d.Recommended = "wait" }, "decision.recommended"},
		{"a long rationale", func(d *tracker.Decision) {
			d.Rationale = long(tracker.MaxDecisionRationale + 1)
		}, "decision.rationale"},
		{"nine pieces of evidence", func(d *tracker.Decision) {
			for range tracker.MaxDecisionEvidence + 1 {
				d.Evidence = append(d.Evidence, tracker.Evidence{
					Kind: tracker.EvidenceTurn, Ref: "turn"})
			}
		}, "maximum is 8"},
		{"an unknown evidence kind", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{{Kind: "doc", Ref: "x"}}
		}, "evidence[0].kind"},
		{"evidence with no ref", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{{Kind: tracker.EvidenceTask}}
		}, "evidence[0].ref"},
		{"an http url", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{{Kind: tracker.EvidenceURL, Ref: "http://example.com"}}
		}, "https://"},
		{"a script url", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{{Kind: tracker.EvidenceURL, Ref: "javascript:alert(1)"}}
		}, "https://"},
		{"a relative url", func(d *tracker.Decision) {
			d.Evidence = []tracker.Evidence{{Kind: tracker.EvidenceURL, Ref: "/pages/1"}}
		}, "https://"},
		{"no role", func(d *tracker.Decision) { d.Role = "" }, "decision.role"},
		{"an unknown role", func(d *tracker.Decision) { d.Role = "owner" }, "decision.role"},
		{"an unknown surface", func(d *tracker.Decision) {
			d.Inform = &tracker.Inform{Surface: "teams", Channel: "eng"}
		}, "inform.surface"},
		{"an inform with no channel", func(d *tracker.Decision) {
			d.Inform = &tracker.Inform{Surface: tracker.InformMattermost}
		}, "inform.channel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := *decisionFixture()
			d.Options = append([]tracker.DecisionOption(nil), d.Options...)
			c.edit(&d)
			err := d.Validate()
			switch {
			case c.field == "" && err != nil:
				t.Fatalf("a well-formed decision was refused: %v", err)
			case c.field == "":
				return
			case err == nil:
				t.Fatalf("the decision was accepted; want a refusal naming %q", c.field)
			case !errors.Is(err, tracker.ErrInvalid):
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
		tasks: map[string]string{"ENG-7": "task-uuid-7"},
		pages: map[string]string{"KB/Audit plan": "page-uuid-1"},
	}
	d := *decisionFixture()
	d.Evidence = []tracker.Evidence{
		{Kind: tracker.EvidenceTask, Ref: "ENG-7", Label: "the audit"},
		{Kind: tracker.EvidencePage, Ref: "KB/Audit plan"},
	}
	got, err := tracker.ValidateDecision(t.Context(), d, lookup)
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
		evidence tracker.Evidence
		invalid  bool
	}{
		"an unknown task": {tracker.Evidence{Kind: tracker.EvidenceTask, Ref: "ENG-404"}, true},
		"a missing page":  {tracker.Evidence{Kind: tracker.EvidencePage, Ref: "page-2"}, true},
		"a lookup down":   {tracker.Evidence{Kind: tracker.EvidencePage, Ref: "boom"}, false},
	} {
		d := *decisionFixture()
		d.Evidence = []tracker.Evidence{c.evidence}
		_, err := tracker.ValidateDecision(t.Context(), d, lookup)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if errors.Is(err, tracker.ErrInvalid) != c.invalid {
			t.Errorf("%s: %v marked invalid %v, want %v", name, err,
				errors.Is(err, tracker.ErrInvalid), c.invalid)
		}
	}
}

// A DECISION RECORD IS RETAINED BY A BUILD THAT CANNOT READ IT.
//
// A build reading version 3 has no field for a decision or a choice: applied
// there, its copy of the ask would offer no options and its copy of the answer
// would name none, for good. So a comment carrying either is stamped at 4, and
// a comment carrying neither still at 1 — an old node holds back only what it
// would apply lossily.
func TestADecisionRecordIsRetainedByABuildThatCannotReadIt(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	for name, c := range map[string]struct {
		comment tracker.Comment
		want    int
	}{
		"an ask with a decision": {tracker.Comment{
			ID: "c-ask", Ask: "pm", Body: "?", Decision: decisionFixture()}, 4},
		"an answer with a choice": {tracker.Comment{
			ID: "c-ans", Answers: &answers, Body: "go", Choice: "ship"}, 4},
		"a plain ask": {tracker.Comment{ID: "c-q", Ask: "pm", Body: "?"}, 1},
	} {
		mutation, err := json.Marshal(tracker.TaskPatch{Comment: &c.comment})
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		body, err := tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				OpID: "op-" + name, Subject: tracker.TaskSubject("t-1"),
				Op: tracker.OpPatch, CreatedAt: wednesday,
				Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
			},
			Kind: tracker.ChangeComment, Mutation: mutation,
			Actor: "dev", ActorKind: tracker.AuthorAgent,
		}.Encode()
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		env, err := tracker.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("%s: envelope: %v", name, err)
		}
		if env.V != c.want {
			t.Errorf("%s: stamped %d, want %d", name, env.V, c.want)
		}
		if c.want > 3 && env.ReadableBy(3) {
			t.Errorf("%s: a build reading version 3 would apply it and drop "+
				"the field", name)
		}
		if !env.ReadableBy(tracker.RecordVersion) {
			t.Errorf("%s: this build cannot read what it writes", name)
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
	askOn(t, r, "op-ask", "decide", tracker.Comment{
		ID: "c-ask", Task: "decide", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	})
	askOn(t, r, "op-plain", "decide", tracker.Comment{
		ID: "c-plain", Task: "decide", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	})

	answer := func(opID, id, answers, choice string) error {
		_, err := r.writer.UpdateTask(t.Context(), opID, "decide", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: id, Task: "decide", Author: "ana", AuthorKind: tracker.AuthorHuman,
				Body: "because", Answers: &answers, Choice: choice, CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
		return err
	}
	err := answer("op-bad", "c-bad", "c-ask", "wait")
	if !errors.Is(err, tracker.ErrInvalid) || !strings.Contains(err.Error(), "ship, hold") {
		t.Fatalf("a choice the ask did not offer: %v — want invalid, listing "+
			"the options", err)
	}
	err = answer("op-none", "c-none", "c-plain", "ship")
	if !errors.Is(err, tracker.ErrInvalid) || !strings.Contains(err.Error(), "carries no decision") {
		t.Fatalf("a choice on an ask that offered none: %v", err)
	}
	// AND THE THREAD READ REFUSES IT TOO, before anything is published.
	if _, err := r.reader.Thread(t.Context(), tracker.ThreadQuery{
		Task: "decide", Answers: "c-ask", Author: "ana", Choice: "wait",
	}, statelog.Freshness{Level: statelog.ReadStale}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("the thread read passed a choice the ask did not offer: %v", err)
	}
	// A choice on a comment that answers nothing is refused before the
	// publish: a choice is an answer.
	if _, err := r.writer.UpdateTask(t.Context(), "op-loose", "decide", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "c-loose", Task: "decide", Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "ship", Choice: "ship",
			CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("a choice answering nothing: %v", err)
	}

	thread, err := r.reader.Thread(t.Context(), tracker.ThreadQuery{
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
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
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
	write := func(opID string, c tracker.Comment) error {
		c.Task, c.Author, c.AuthorKind, c.CreatedAt = "fixed", "dev", tracker.AuthorAgent, wednesday
		_, err := r.writer.UpdateTask(t.Context(), opID, "fixed", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &c}, tracker.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := write("op-remark", tracker.Comment{ID: "c-r", Body: "fyi",
		Decision: decisionFixture()}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("a decision on a comment that asks nobody: %v", err)
	}
	if err := write("op-ask", tracker.Comment{ID: "c-a", Body: "Friday?", Ask: "ana",
		Decision: decisionFixture()}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// AN EDIT THAT KEEPS IT LANDS — a body can be corrected —
	if err := write("op-edit", tracker.Comment{ID: "c-a", Body: "Friday, then?",
		Ask: "ana", Decision: decisionFixture()}); err != nil {
		t.Fatalf("an edit keeping the decision was refused: %v", err)
	}
	// — and one that changes it, or drops it, does not.
	changed := decisionFixture()
	changed.Options[1].Label = "Ship Monday"
	for name, d := range map[string]*tracker.Decision{"changed": changed, "dropped": nil} {
		if err := write("op-"+name, tracker.Comment{ID: "c-a", Body: "Friday?",
			Ask: "ana", Decision: d}); !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("an edit that %s the decision: %v", name, err)
		}
	}
	// Nor can a remark acquire one after the fact.
	if err := write("op-plain", tracker.Comment{ID: "c-p", Body: "which?", Ask: "ana"}); err != nil {
		t.Fatalf("plain ask: %v", err)
	}
	if err := write("op-late", tracker.Comment{ID: "c-p", Body: "which?", Ask: "ana",
		Decision: decisionFixture()}); !errors.Is(err, tracker.ErrInvalid) {
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
	askOn(t, r, "op-ask", "once", tracker.Comment{
		ID: "c-ask", Task: "once", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	})
	answered := wednesday.Add(time.Hour)
	answer := func(opID, id string, at time.Time) error {
		answers := "c-ask"
		_, err := r.writer.UpdateTask(t.Context(), opID, "once", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: id, Task: "once", Author: "ana", AuthorKind: tracker.AuthorHuman,
				Body: "yes", Answers: &answers, Choice: "ship", CreatedAt: at,
			}}, tracker.ChangeComment, nil)
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
	var already *tracker.AlreadyAnsweredError
	if !errors.As(err, &already) || !errors.Is(err, tracker.ErrAlreadyAnswered) {
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
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	for _, c := range detail.Comments {
		if c.ID == "c-second" {
			t.Error("the second answer landed beside the first")
		}
	}

	// AND THE THREAD READ SAYS THE SAME, so a tool refuses before publishing.
	_, err = r.reader.Thread(t.Context(), tracker.ThreadQuery{
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
	wake := func(body, choice string, decision *tracker.Decision) string {
		n := tracker.Wake{
			Kind: tracker.ChangeComment,
			Comment: &tracker.Comment{ID: "c-ans", Body: body, Answers: &answers,
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
		len(got) > tracker.MaxExcerpt {
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
	askOn(t, r, "op-ask", "asked", tracker.Comment{
		ID: "c-1", Task: "asked", Author: "dev", AuthorKind: tracker.AuthorAgent,
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
func decisionFixture() *tracker.Decision {
	return &tracker.Decision{
		Question: "Ship on Friday or hold for the audit?",
		Options: []tracker.DecisionOption{
			{ID: "ship", Label: "Ship Friday"},
			{ID: "hold", Label: "Hold for the audit", Detail: "about a week"},
		},
		Recommended: "ship",
		Rationale:   "The audit reviews last quarter's code.",
		Role:        tracker.RoleApprover,
	}
}

type fakeEvidence struct {
	tasks map[string]string
	pages map[string]string
}

func (f fakeEvidence) TaskID(_ context.Context, ref string) (string, error) {
	if id, ok := f.tasks[ref]; ok {
		return id, nil
	}
	return "", fmt.Errorf("resolve %s: %w", ref, tracker.ErrNoTask)
}

func (f fakeEvidence) PageID(_ context.Context, ref string) (string, bool, error) {
	if ref == "boom" {
		return "", false, errors.New("pages: read: database is locked")
	}
	id, ok := f.pages[ref]
	return id, ok, nil
}
