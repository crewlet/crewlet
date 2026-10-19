package decision_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/decision"
)

// A DECISION IS REFUSED AT WRITE FOR EVERY WAY IT CAN BE MALFORMED, naming the
// field, and a well-formed one passes untouched.
//
// Each row is one clause of [decision.Decision.Validate]; a clause that stopped
// refusing would let a decision through that a person cannot answer (one
// option, two options with one id, a recommendation that is not on offer, a
// link that is not https), and nothing downstream checks again.
func TestADecisionIsValidatedField(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, c := range []struct {
		name  string
		edit  func(*decision.Decision)
		field string // "" when the decision must pass
	}{
		{"well formed", func(*decision.Decision) {}, ""},
		{"a contributor", func(d *decision.Decision) { d.Role = decision.RoleContributor }, ""},
		{"six options", func(d *decision.Decision) {
			for i := len(d.Options); i < decision.MaxOptions; i++ {
				d.Options = append(d.Options, decision.Option{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, ""},
		{"no recommendation", func(d *decision.Decision) { d.Recommended = "" }, ""},
		{"every evidence kind", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{
				{Kind: decision.EvidenceTask, Ref: "ENG-1"},
				{Kind: decision.EvidencePage, Ref: "p-1"},
				{Kind: decision.EvidenceTurn, Ref: "turn-1"},
				{Kind: decision.EvidenceRun, Ref: "run-1"},
				{Kind: decision.EvidenceURL, Ref: "https://example.com/a", Label: "a"},
			}
		}, ""},
		{"an inform", func(d *decision.Decision) {
			d.Inform = &decision.Inform{Surface: decision.InformSlack, Channel: "eng"}
		}, ""},
		{"an inform on the engine's own chat", func(d *decision.Decision) {
			d.Inform = &decision.Inform{Surface: decision.InformChat, Channel: "eng"}
		}, ""},

		{"no question", func(d *decision.Decision) { d.Question = "  " }, "decision.question"},
		{"a long question", func(d *decision.Decision) {
			d.Question = long(decision.MaxQuestion + 1)
		}, "decision.question"},
		{"one option", func(d *decision.Decision) { d.Options = d.Options[:1] }, "at least 2"},
		{"seven options", func(d *decision.Decision) {
			for i := len(d.Options); i <= decision.MaxOptions; i++ {
				d.Options = append(d.Options, decision.Option{
					ID: fmt.Sprintf("o%d", i), Label: "another"})
			}
		}, "maximum is 6"},
		{"an id with a capital", func(d *decision.Decision) { d.Options[0].ID = "Ship" }, "options[0].id"},
		{"an id with a space", func(d *decision.Decision) { d.Options[1].ID = "hold on" }, "options[1].id"},
		{"a 33-byte id", func(d *decision.Decision) { d.Options[0].ID = long(33) }, "options[0].id"},
		{"two options one id", func(d *decision.Decision) { d.Options[1].ID = "ship" }, "names two options"},
		{"no label", func(d *decision.Decision) { d.Options[0].Label = "" }, "options[0].label"},
		{"a long label", func(d *decision.Decision) {
			d.Options[0].Label = long(decision.MaxOptionLabel + 1)
		}, "options[0].label"},
		{"a long detail", func(d *decision.Decision) {
			d.Options[1].Detail = long(decision.MaxOptionDetail + 1)
		}, "options[1].detail"},
		{"a recommendation not on offer", func(d *decision.Decision) { d.Recommended = "wait" }, "decision.recommended"},
		{"a long rationale", func(d *decision.Decision) {
			d.Rationale = long(decision.MaxRationale + 1)
		}, "decision.rationale"},
		{"nine pieces of evidence", func(d *decision.Decision) {
			for range decision.MaxEvidence + 1 {
				d.Evidence = append(d.Evidence, decision.Evidence{
					Kind: decision.EvidenceTurn, Ref: "turn"})
			}
		}, "maximum is 8"},
		{"an unknown evidence kind", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{{Kind: "doc", Ref: "x"}}
		}, "evidence[0].kind"},
		{"evidence with no ref", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{{Kind: decision.EvidenceTask}}
		}, "evidence[0].ref"},
		{"an http url", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{{Kind: decision.EvidenceURL, Ref: "http://example.com"}}
		}, "https://"},
		{"a script url", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{{Kind: decision.EvidenceURL, Ref: "javascript:alert(1)"}}
		}, "https://"},
		{"a relative url", func(d *decision.Decision) {
			d.Evidence = []decision.Evidence{{Kind: decision.EvidenceURL, Ref: "/doclet/pages/1"}}
		}, "https://"},
		{"no role", func(d *decision.Decision) { d.Role = "" }, "decision.role"},
		{"an unknown role", func(d *decision.Decision) { d.Role = "owner" }, "decision.role"},
		{"an unknown surface", func(d *decision.Decision) {
			d.Inform = &decision.Inform{Surface: "teams", Channel: "eng"}
		}, "inform.surface"},
		{"an inform with no channel", func(d *decision.Decision) {
			d.Inform = &decision.Inform{Surface: decision.InformMattermost}
		}, "inform.channel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := *fixture()
			d.Options = append([]decision.Option(nil), d.Options...)
			c.edit(&d)
			err := d.Validate()
			switch {
			case c.field == "" && err != nil:
				t.Fatalf("a well-formed decision was refused: %v", err)
			case c.field == "":
				return
			case err == nil:
				t.Fatalf("the decision was accepted; want a refusal naming %q", c.field)
			case !errors.Is(err, decision.ErrInvalid):
				t.Fatalf("the refusal %v is not marked invalid, so a surface "+
					"would read it as the node's failure", err)
			case !strings.Contains(err.Error(), c.field):
				t.Fatalf("the refusal %q does not name %q", err, c.field)
			}
		})
	}
}

// EVERY FAULT OF A DECISION IN ONE REFUSAL (ADR-0059): an asker writes a
// refused decision again once, and a refusal naming one fault of several
// spent that attempt on the next.
//
// Mutation: return from Validate at its first fault and only the question is
// named.
func TestEveryFaultOfADecisionIsNamedAtOnce(t *testing.T) {
	t.Parallel()
	d := fixture()
	d.Question = ""
	d.Options[0].ID = "Ship"
	d.Options[1].Label = ""
	d.Recommended = "nope"
	d.Role = "boss"
	err := d.Validate()
	if !errors.Is(err, decision.ErrInvalid) {
		t.Fatalf("Validate = %v, want an invalid refusal", err)
	}
	for _, want := range []string{"decision.question is empty", "decision.options[0].id",
		"decision.options[1].label is empty", "decision.recommended is \"nope\"", "decision.role is \"boss\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err.Error(), want)
		}
	}
}

// fixture is a fresh well-formed decision the caller may change.
func fixture() *decision.Decision {
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
