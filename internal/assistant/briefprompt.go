package assistant

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// BriefPromptVersion names a brief turn's system prompt ([briefRules]),
// stamped on its turn row and audit record in place of [PromptVersion]. A
// reworded sentence is a new version.
const BriefPromptVersion = "assistant.brief.v2"

// promptVersion is the prompt this turn runs on.
func (t *turn) promptVersion() string {
	if t.briefing() {
		return BriefPromptVersion
	}
	return PromptVersion
}

// briefRules is a brief turn's standing instruction: the same person, the
// same access and the same fences as every turn, a fixed shape in place of
// prose, and nothing but reads. EVERY CAP THE ENGINE HOLDS A LINE TO IS SAID
// HERE IN PLAIN WORDS, from the same constants, beside the tool's schema: a
// model that learns a cap only from a refusal spends its one retry on it.
var briefRules = fmt.Sprintf("You are the Crewlet assistant inside the company's dashboard, writing a "+
	"brief for the person you act as. Every tool you call is a request made as them, decided "+
	"by their own access. You only read: you change nothing and propose nothing.\n\n"+
	"A brief is three things, in a fixed shape, and nothing else:\n"+
	"- summary: what is going on, in one sentence with numbers and names, on one line of at "+
	"most %[1]d characters. Lead with what the screen does not already make obvious: what is "+
	"ranked first, what changed, a fact from elsewhere that bears on it.\n"+
	"- needs: at most %[2]d things that wait on this person, most pressing first, each about "+
	"one record you read (a task, a project, an agent, a page or an artifact), cited by its "+
	"key, handle or id exactly as your read returned it. Each row's text, and its detail if "+
	"it has one, is one line of at most %[3]d characters. Only what the person can act on: a "+
	"decision asked of them, a stopped agent, a budget nearly spent, work blocked on them. "+
	"An empty list is a good answer when nothing waits on them; never invent urgency.\n"+
	"- recommended: the one next step, usually the first of the needs, in one line of at "+
	"most %[3]d characters, or leave it out when nothing is worth recommending.\n"+
	"Then follow_ups: up to %[4]d short questions the person may want to ask next, each at "+
	"most %[5]d characters, as is an ask action's question.\n"+
	"Count before you call: a line over its cap is refused, never cut.\n\n"+
	"Rules:\n"+
	"- Read first, with as few reads as the brief needs. Use only facts your reads returned; "+
	"never a number, a name or a status you did not read. A row about a record you did not "+
	"read is dropped by the engine. When a read's answer says the reads are spent, call "+
	"answer_brief next with what you have.\n"+
	"- Numbers and names, no filler: no greeting, no \"here is\", no restating the question, "+
	"no adjectives a number could replace.\n"+
	"- tone is warning for something to act on and neutral for something to know. Use danger "+
	"only for an agent that is stopped or whose last turn failed, or a task marked urgent: "+
	"the engine draws any other danger as warning.\n"+
	"- An action opens the record the row cites, asks a question in this conversation, or "+
	"is none. Never write a link, a route or an address.\n"+
	"- Name seats by their names (Software Engineer), never by their handles, in every "+
	"line you write; cite a seat's handle only in ref_id.\n"+
	"- Say every time on the company's clock. Never repeat a credential.\n"+
	"- Answer by calling answer_brief once. Write no prose before or after it: it is never "+
	"shown. If the engine refuses it, fix everything it names and call it again: you may "+
	"call it twice in all.",
	wire.BriefSummaryMaxRunes, wire.BriefMaxNeeds, wire.BriefTextMaxRunes,
	wire.BriefMaxFollowUps, wire.BriefQuestionMaxRunes)

// buildBriefMessages is a brief turn's conversation as its loop starts it:
// the brief's rules, the company and the person, the conversation so far,
// and what to brief, after the page fenced as the screen's own description.
func buildBriefMessages(in promptInput) []llm.Message {
	var system strings.Builder
	system.WriteString(briefRules)
	system.WriteString("\n\nPrompt version: ")
	system.WriteString(BriefPromptVersion)
	system.WriteString("\n\n")
	system.WriteString(provenance.StandingInstruction)
	system.WriteString("\n\n")
	system.WriteString(companyBlock(in))
	system.WriteString("\n\n")
	system.WriteString(briefPersonBlock(in))
	out := []llm.Message{{Role: llm.RoleSystem, Content: system.String()}}
	out = append(out, historyMessages(in.History)...)
	return append(out, llm.Message{Role: llm.RoleUser, Content: briefQuestion(in)})
}

// briefPersonBlock is who the brief is for. A brief proposes nothing, so it
// says nothing of the changes the person may propose.
func briefPersonBlock(in promptInput) string {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.Person.Login
	}
	return fmt.Sprintf("The brief is for %s (seat %s). Every list you read is filtered by "+
		"what they may see.", name, in.Person.Seat)
}

// briefQuestion is what to brief: the one record the screen has open, or
// the whole page, then the screen's own description, fenced.
func briefQuestion(in promptInput) string {
	var b strings.Builder
	page := in.Page
	if record := page.Selection; record != nil {
		noun := string(record.Noun)
		if noun == "" {
			noun = string(record.Kind)
		}
		fmt.Fprintf(&b, "Brief the person on the %s they have open (%s %s) and nothing else on "+
			"the screen: read it, its immediate context (the question asked, its task, the "+
			"agent on it, its recent thread) and what waits on them about it.", noun,
			record.Kind, record.ID)
	} else {
		b.WriteString("Brief the person on this page: read what it shows and what on it waits " +
			"on them.")
	}
	kinds := make([]string, 0, len(catalog.BriefRefKinds))
	for _, k := range catalog.BriefRefKinds {
		kinds = append(kinds, string(k))
	}
	fmt.Fprintf(&b, " A row may cite a %s.", strings.Join(kinds, ", "))
	b.WriteString("\n\nThe screen, described by the screen itself:\n")
	b.WriteString(pageFence(in.Boundary, page))
	b.WriteString("\n\nWhat the person asked:\n")
	b.WriteString(in.Question)
	return b.String()
}

// briefTitle is a brief's conversation's title: what it covers, named at once
// with no model call, since the brief itself writes no prose to name it by.
func briefTitle(page *wire.PageContext) string {
	return conversationTitle("Brief: " + page.Title)
}

// briefSummary is a brief card as the conversation the model re-reads it: the
// whole brief, every line folded, so a follow-up question is asked of what
// the person was shown. A brief is a few short lines, so all of it fits where
// a table's rows would not.
func briefSummary(b *wire.BriefCard) string {
	var s strings.Builder
	fmt.Fprintf(&s, "[brief of %s", oneLine(b.Scope.Title))
	if r := b.Scope.Record; r != nil {
		fmt.Fprintf(&s, ", limited to %s %s", r.Kind, oneLine(r.ID))
	}
	fmt.Fprintf(&s, ": %s", oneLine(b.Summary))
	if len(b.Needs) == 0 {
		s.WriteString(" Needs you: nothing.")
	}
	for _, n := range b.Needs {
		fmt.Fprintf(&s, " Needs you (%s %s): %s", n.Ref.Kind, oneLine(n.Ref.ID), oneLine(n.Text))
		if n.Detail != "" {
			fmt.Fprintf(&s, ", %s", oneLine(n.Detail))
		}
		s.WriteString(".")
	}
	if r := b.Recommended; r != nil {
		fmt.Fprintf(&s, " Recommended: %s.", oneLine(r.Text))
	}
	s.WriteString("]")
	return s.String()
}
