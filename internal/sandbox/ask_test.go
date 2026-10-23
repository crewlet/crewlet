package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/redact"
)

// choosing is a decision a run asks with, put in its own question's words.
func choosing(question string) *decision.Decision {
	return &decision.Decision{
		Question: question,
		Options: []decision.Option{
			{ID: "main", Label: "Target main", Detail: "ships with the next release"},
			{ID: "release-2", Label: "Target release/2"},
		},
		Recommended: "main", Rationale: "the flake reproduces on main only",
		Evidence: []decision.Evidence{{Kind: decision.EvidenceURL,
			Ref: "https://example.com/ci/42", Label: "the failing job"}},
		Role: decision.RoleApprover,
	}
}

// AN ASK IS REFUSED WITH EVERY FAULT AT ONCE, each naming its field (ADR-0059):
// a run whose ask is refused writes it again, and a refusal naming one fault of
// several spends that attempt on the next. Nothing is cut or defaulted, a
// branch included: a shortened branch, or one git would refuse, names nothing a
// fresh machine could check out, so it is refused to the run that named it
// rather than dropped where the run is collected.
//
// Mutation: return on the first fault, or drop any one check, and a sentence
// goes missing.
func TestAnAskIsRefusedWithEveryFaultAtOnce(t *testing.T) {
	t.Parallel()
	d := choosing("which one?")
	d.Options = d.Options[:1]
	d.Recommended = ""
	d.Inform = &decision.Inform{Surface: decision.InformSlack, Channel: "eng"}
	err := Ask{
		Question: strings.Repeat("q", decision.MaxQuestion+1),
		Context:  strings.Repeat("c", MaxAskContextBytes+1),
		Branch:   "wip t1",
		Decision: d,
	}.Validate()
	if !errors.Is(err, decision.ErrInvalid) {
		t.Fatalf("Validate = %v, want a refusal", err)
	}
	for _, want := range []string{
		"question is 301 bytes",
		"context is 16385 bytes",
		"to is empty",
		`branch is "wip t1"`,
		"decision.question differs",
		"decision.inform is set",
		"needs at least 2",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// AT ITS BOUNDS AN ASK IS ACCEPTED, and one byte past each it is refused: the
// bounds are where the ask travels, and an ask that fits them is never refused
// for being near one.
func TestAnAskIsHeldToItsBoundsAndNoTighter(t *testing.T) {
	t.Parallel()
	question := strings.Repeat("q", decision.MaxQuestion)
	atBounds := Ask{
		Question: question, Context: strings.Repeat("c", MaxAskContextBytes),
		Decision: choosing(question), To: strings.Repeat("t", MaxAskToBytes),
		Branch: "wip/" + strings.Repeat("b", MaxBranchBytes-len("wip/")),
	}
	if err := atBounds.Validate(); err != nil {
		t.Fatalf("an ask at every bound was refused: %v", err)
	}
	for name, past := range map[string]func(*Ask){
		"the context": func(a *Ask) { a.Context += "c" },
		"the to":      func(a *Ask) { a.To += "t" },
		"the branch":  func(a *Ask) { a.Branch += "b" },
	} {
		ask := atBounds
		past(&ask)
		if err := ask.Validate(); !errors.Is(err, decision.ErrInvalid) {
			t.Errorf("%s a byte past its bound was accepted: %v", name, err)
		}
	}
	for _, branch := range []string{"", "wip/t1", "feature/ask-structured"} {
		if err := (Ask{Question: "which?", To: AskToRequester, Branch: branch}).Validate(); err != nil {
			t.Errorf("branch %q was refused: %v", branch, err)
		}
	}
	for _, branch := range []string{"HEAD", "-wip", "wip..t1", "wip/", "wip:t1"} {
		if err := (Ask{Question: "which?", To: AskToRequester, Branch: branch}).Validate(); err == nil {
			t.Errorf("branch %q, which git refuses, was accepted", branch)
		}
	}
}

// AN ASK IS REDACTED IN EVERY TEXT A PERSON OR A MODEL READS, and a branch a
// redaction would change is dropped rather than recorded as a marker that names
// no branch. The ask redacted is a copy: the caller's decision is untouched.
//
// Mutation: skip any one field in Redacted, and its credential survives.
func TestAnAskIsRedactedInEveryTextItCarries(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("e", 36)
	question := "Is " + secret + " right?"
	d := choosing(question)
	d.Options[0].Label = "Use " + secret
	d.Options[1].Detail = "not " + secret
	d.Rationale = "found " + secret
	d.Evidence[0].Label = "the " + secret + " log"
	d.Evidence[0].Ref = "https://ci.example.com/run/1?token=" + secret
	ask := Ask{Question: question, Context: "the env held " + secret, Decision: d,
		To: AskToRequester, Branch: "wip/" + secret}

	got := ask.Redacted()
	if got.Decision == nil || len(got.Decision.Options) != 2 || len(got.Decision.Evidence) != 1 {
		t.Fatalf("the redacted decision is %+v, want the ask's", got.Decision)
	}
	for where, text := range map[string]string{
		"question": got.Question, "context": got.Context,
		"decision.question":    got.Decision.Question,
		"decision.options[0]":  got.Decision.Options[0].Label,
		"decision.options[1]":  got.Decision.Options[1].Detail,
		"decision.rationale":   got.Decision.Rationale,
		"decision.evidence[0]": got.Decision.Evidence[0].Label,
		// A URL ITS RUN CITES can carry a token in its query.
		"decision.evidence[0].ref": got.Decision.Evidence[0].Ref,
	} {
		if strings.Contains(text, secret) || !strings.Contains(text, redact.Marker) {
			t.Errorf("the %s reads %q; want the credential replaced by its marker", where, text)
		}
	}
	if got.Branch != "" {
		t.Errorf("the branch is %q; a branch a redaction changed names nothing, so want none", got.Branch)
	}
	if ask.Decision.Options[0].Label != "Use "+secret || ask.Decision.Evidence[0].Label != "the "+secret+" log" {
		t.Error("redacting the ask rewrote the caller's decision in place")
	}

	plain := Ask{Question: "which branch?", To: AskToRequester, Branch: "wip/t1"}
	if got := plain.Redacted(); got != plain {
		t.Errorf("an ask with nothing to redact came back as %+v", got)
	}
}

// AN ANSWER'S CHOICE IS READ BY THE RUN AS IT ASKED IT: the option's label, as
// the run wrote it, and its id, which the run named it by, then the person's
// words. A choice the run did not offer is refused, because the run would be
// told it chose something nobody offered, and so is any choice of an open
// question.
func TestAnAnswerNamesTheOptionItChoseAsTheRunAskedIt(t *testing.T) {
	t.Parallel()
	d := choosing("which branch?")
	for name, tc := range map[string]struct {
		decision      *decision.Decision
		choice, words string
		want          string
	}{
		"a choice and words": {d, "main", "  ship it today  ",
			"Chosen option: Target main (`main`).\n\nship it today"},
		"a choice alone":            {d, "release-2", "", "Chosen option: Target release/2 (`release-2`)."},
		"words on a decision":       {d, "", " none of these ", "none of these"},
		"words on an open question": {nil, "", "use main", "use main"},
	} {
		got, err := AnswerText(tc.decision, tc.choice, tc.words)
		if err != nil || got != tc.want {
			t.Errorf("%s: AnswerText = %q, %v; want %q", name, got, err, tc.want)
		}
	}

	if _, err := AnswerText(d, "develop", "use develop"); !errors.Is(err, decision.ErrInvalid) ||
		!strings.Contains(err.Error(), `it offers "main" or "release-2"`) {
		t.Errorf("a choice the decision does not offer = %v; want a refusal listing what it offers", err)
	}
	if _, err := AnswerText(nil, "main", ""); !errors.Is(err, decision.ErrInvalid) {
		t.Errorf("a choice of an open question = %v; want a refusal", err)
	}
}
