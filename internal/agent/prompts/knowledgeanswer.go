package prompts

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// The KNOWLEDGE ANSWER: one question a person typed into the dashboard's
// command palette, answered from what the company has written down.
//
// It is no turn's phase, so it has none of a phase's scaffolding (identity,
// policies, roster, tools): only the rule that it says what the sources say,
// cites which, and says so when they do not answer. An invented runbook step
// would send a person to do something nobody decided.

// KnowledgeAnswerSystem is the whole contract the answer model is held to.
//
// IT ENDS ON THE STANDING SENTENCE ON FENCES ([provenance.StandingInstruction]):
// every source reaches the model fenced as who wrote it (ADR-0040), so a page
// addressing the model reads as the page's words, not an instruction.
const KnowledgeAnswerSystem = "You answer a question from a person who runs " +
	"this company, using ONLY the numbered sources in the message — pages " +
	"from the company's knowledge base and items from its work tracker.\n" +
	"\n" +
	"Rules:\n" +
	"- Say only what the sources say. Do not add steps, names, numbers or " +
	"decisions they do not contain, and do not fill a gap from general " +
	"knowledge.\n" +
	"- Cite the source of every claim as [n], using the source's number.\n" +
	"- If the sources do not answer the question, say so in one sentence and " +
	"name the closest source, if any. That is a complete answer.\n" +
	"- If two sources disagree, say that they do and cite both.\n" +
	"- Answer in Markdown, in at most a few short paragraphs or a short list.\n" +
	"\n" + provenance.StandingInstruction

// KnowledgeSource is one retrieved document as the answer model reads it.
type KnowledgeSource struct {
	// Kind is "page" or "work item", as the model is told it.
	Kind string
	// Label is how the model is told to recognise it: a page's title, or
	// a work item's key and title.
	Label string
	// Text is the source's own words, whole or condensed by the caller and
	// marked so. Never cut: a source cut short reads as one that ends there.
	Text string
	// Written is who wrote Text, as the backend that found it labels it:
	// the fence it is read in ([BuildKnowledgeAnswer]).
	Written provenance.Label
}

// BuildKnowledgeAnswer renders the user message for one question: the numbered
// sources, then the question.
//
// NUMBERED FROM ONE, IN THE ORDER GIVEN: the caller lists the sources back in
// that order, so [n] must name the n-th. The question comes LAST, freshest
// before the answer.
//
// EACH SOURCE'S TEXT IS FENCED as who wrote it ([KnowledgeSource] Written,
// ADR-0040), and its title and the question are neutralised, so no page passes
// for instructions and nothing opens or closes a fence. fence is minted for
// the call, never a turn's.
func BuildKnowledgeAnswer(fence provenance.Boundary, question string, sources []KnowledgeSource) string {
	var b strings.Builder
	b.WriteString("## Sources\n")
	for i, src := range sources {
		fmt.Fprintf(&b, "\n### [%d] %s: %s\n", i+1, src.Kind, provenance.Neutralise(fence, src.Label))
		if text := strings.TrimSpace(src.Text); text != "" {
			b.WriteString(src.Written.Fence(fence, text))
			b.WriteString("\n")
		} else {
			b.WriteString("(no text)\n")
		}
	}
	b.WriteString("\n## Question\n")
	b.WriteString(provenance.Neutralise(fence, strings.TrimSpace(question)))
	b.WriteString("\n")
	return b.String()
}
