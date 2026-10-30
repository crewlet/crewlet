package assistant

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/words"
)

// BriefPromptVersion names a brief turn's system prompt ([briefRules]),
// stamped on its turn row and audit record in place of [PromptVersion]. A
// reworded sentence is a new version.
const BriefPromptVersion = "assistant.brief.v4"

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
//
// A BRIEF ANSWERS TWO QUESTIONS: what is this about, and do I have to do
// anything. The second is never written: it is the number of needs, which the
// engine counts and draws as the card's verdict, so the rules spend their
// words on keeping needs to things to do.
var briefRules = fmt.Sprintf("You are the Crewlet assistant inside the company's dashboard, writing a "+
	"brief for the person you act as. Every tool you call is a request made as them, decided "+
	"by their own access. You only read: you change nothing and propose nothing.\n\n"+
	"A brief tells the person, in as few words as possible, what this is about and whether "+
	"they have to do anything. It has a fixed shape and nothing else:\n"+
	"- about: what the page or record is about right now, as a colleague would say it in one "+
	"breath, on one line of at most %[1]d characters. The situation, not a list of counts.\n"+
	"- needs: only what the person has to do, at most %[2]d, most pressing first. The first is "+
	"the one you recommend doing first. Each is about one record you read (%[8]s), cited by "+
	"its key, handle or id exactly as your read returned it, a chat room by its id or its "+
	"name with its #. Its do is an instruction that starts with a verb and names the record "+
	"(\"Answer ENG-42: ship Friday or wait?\", \"Raise Software Engineer's daily budget\"), "+
	"at most %[3]d characters; its why, if it has one, is one fact, at most %[4]d characters. "+
	"A need is something only this person can move: a decision asked of them, a stopped "+
	"agent, a budget nearly spent, work blocked on them. A fact to know is never a need. An "+
	"empty list is the right answer when nothing needs them: the engine then tells them "+
	"nothing needs them, so never invent urgency.\n"+
	"- context: leave it out unless one fact from elsewhere changes what they should do; then "+
	"one line of at most %[5]d characters.\n"+
	"- follow_ups: up to %[6]d short questions they may want to ask next, each at most %[7]d "+
	"characters, as is an ask action's question.\n"+
	"Count before you call: a line over its cap is refused, never cut.\n\n"+
	"Rules:\n"+
	"- Read first, with as few reads as the brief needs. Use only facts your reads returned; "+
	"never a number, a name or a status you did not read. A need about a record you did not "+
	"read is dropped by the engine. When a read's answer says the reads are spent, call "+
	"answer_brief next with what you have.\n"+
	"- Plain words, no filler: no greeting, no \"here is\", no restating the question, no "+
	"verdict of your own (\"all good\", \"nothing urgent\"): the needs are the verdict.\n"+
	"- tone is warning. Use danger only for an agent that is stopped or whose last turn "+
	"failed, or a task marked urgent: the engine draws any other danger as warning.\n"+
	"- A need's action opens the record it cites, or asks a question in this conversation. "+
	"Never write a link, a route or an address.\n"+
	"- Name seats by their names (Software Engineer), never by their handles, in every "+
	"line you write; cite a seat's handle only in ref_id.\n"+
	"- Say every time on the company's clock. Never repeat a credential.\n"+
	"- Answer by calling answer_brief once. Write no prose before or after it: it is never "+
	"shown. If the engine refuses it, fix everything it names and call it again: you may "+
	"call it twice in all.",
	wire.BriefAboutMaxRunes, wire.BriefMaxNeeds, wire.BriefTextMaxRunes,
	wire.BriefDetailMaxRunes, wire.BriefContextMaxRunes, wire.BriefMaxFollowUps,
	wire.BriefQuestionMaxRunes, briefCitable())

// briefCiteWords are the records a brief's row may cite as its rules name
// them, by kind.
var briefCiteWords = map[wire.ResourceKind]string{
	wire.ResourceTask: "a task", wire.ResourceProject: "a project",
	wire.ResourceSeat: "an agent", wire.ResourcePage: "a page",
	wire.ResourceArtifact: "an artifact", wire.ResourceRoom: "a chat room",
}

// briefCitable is the records a brief's row may cite, in words: built from
// [catalog.BriefRefKinds], so the rules can never name fewer kinds than the
// answer's schema takes, or more. A kind with no words of its own is named by
// itself, and TestTheBriefRulesNameEveryCitableKind holds every kind to words.
// Each kind's words are a phrase rather than a value, so they are said through
// [words.Or] as [words.Said], unquoted.
func briefCitable() string {
	said := make([]words.Said, 0, len(catalog.BriefRefKinds))
	for _, k := range catalog.BriefRefKinds {
		w, ok := briefCiteWords[k]
		if !ok {
			w = "a " + string(k)
		}
		said = append(said, words.Said(w))
	}
	return words.Or(said)
}

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
			"the screen: read it and its immediate context (the question asked, its task, the "+
			"agent on it, its recent thread), then say what it is about and what, if anything, "+
			"they have to do about it.", noun, record.Kind, record.ID)
	} else {
		b.WriteString("Brief the person on this page: read what it shows, then say what it is " +
			"about and what, if anything, on it they have to do.")
	}
	// THE KINDS AS THE ANSWER WRITES THEM, quoted as the values they are: the
	// rules name the records in words, and a need's ref_kind takes these.
	fmt.Fprintf(&b, " A need's ref_kind is one of %s.", words.Or(catalog.BriefRefKinds))
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
// the person was shown, verdict first, as the card draws it. A brief is a few
// short lines, so all of it fits where a table's rows would not.
func briefSummary(b *wire.BriefCard) string {
	var s strings.Builder
	fmt.Fprintf(&s, "[brief of %s", oneLine(b.Scope.Title))
	if r := b.Scope.Record; r != nil {
		fmt.Fprintf(&s, ", limited to %s %s", r.Kind, oneLine(r.ID))
	}
	fmt.Fprintf(&s, ": %s. %s.", b.Verdict(), oneLine(b.About))
	for i, n := range b.Needs {
		fmt.Fprintf(&s, " %d (%s %s): %s", i+1, n.Ref.Kind, oneLine(n.Ref.ID), oneLine(n.Text))
		if n.Detail != "" {
			fmt.Fprintf(&s, ", %s", oneLine(n.Detail))
		}
		s.WriteString(".")
	}
	if b.Context != "" {
		fmt.Fprintf(&s, " Context: %s.", oneLine(b.Context))
	}
	s.WriteString("]")
	return s.String()
}
