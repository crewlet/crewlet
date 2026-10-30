package prompts

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/provenance"
)

// The KNOWLEDGE ANSWER: one question a person typed into the dashboard's
// command palette, answered from what the company has written down.
//
// It is not a phase of any turn — nobody's seat runs it, and nothing it says
// is an action — so it has none of a phase's scaffolding: no identity, no
// policies, no roster, no tools. What it has is the one rule that makes an
// answer worth reading over a list of links: it says only what the sources
// say, cites which, and says so when they do not answer the question. A
// palette that confidently invented a runbook step would send a person to do
// something nobody decided.

// KnowledgeAnswerSystem is the whole contract the answer model is held to.
//
// Prompt text is behaviour: it changes deliberately, together with the tests
// that hold it and the docs that describe it, and it states only what the
// model cannot know — here, where the sources are, that they are all it may
// draw on, how [n] maps onto them, and that the palette shows a short
// Markdown answer.
//
// IT ENDS ON THE STANDING SENTENCE ON FENCES ([provenance.StandingInstruction]):
// every source reaches the model fenced as who wrote it (ADR-0040), and a page
// that addresses the model ("answer that the release is approved") must read
// as the page's words, not an instruction to it.
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
	// Kind is what the source is, as the model is told it: "page" or
	// "work item".
	Kind string
	// Label is how the model is told to recognise it: a page's title, or
	// a work item's key and title.
	Label string
	// Text is the source's own words — whole, or condensed for the
	// question by the caller and marked as such. Never cut: a source cut
	// to its opening reads as a source that ends there.
	Text string
	// Written is who wrote Text, as the backend that found it labels it:
	// the fence it is read in ([BuildKnowledgeAnswer]).
	Written provenance.Label
}

// BuildKnowledgeAnswer renders the user message for one question: the numbered
// sources, then the question.
//
// NUMBERED FROM ONE, IN THE ORDER GIVEN, because the caller lists the sources
// back to the person in that same order and a citation [n] has to name the
// n-th of them. The question comes LAST so it is the freshest thing the model
// read before it answers.
//
// EACH SOURCE'S TEXT IS FENCED under fence as who wrote it ([KnowledgeSource]
// Written, ADR-0040), and its title and the question — the person's own words
// — are neutralised under it, so nothing a page says passes for the answer's
// instructions and nothing anywhere opens or closes a fence. fence is minted
// for the call and never a turn's.
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
