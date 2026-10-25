package assistant

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE PROMPT, assembled in cache-friendly order: what is the same for every
// turn first, what is the same for this person today next, and what changes
// with every question last, so a vendor's prompt cache answers the longest
// prefix it can (ADR-0045).
//
//  1. Identity and rules ([identityRules]), stamped [PromptVersion].
//  2. [provenance.StandingInstruction], the one sentence about fences.
//  3. The company: its name, its clock's zone and today on it (ADR-0018).
//  4. The person: their name and seat, the apps they hold, the writes they
//     may propose, and the sentence that a list may be filtered by access.
//  5. The conversation so far: its last [HistoryMessages] messages, each card
//     as a one-line summary and never a raw tool answer.
//  6. The question, after the page context fenced under the turn's boundary
//     (source `page_context`, trust colleague: a screen's labels are names
//     other people chose), or nothing about the page when the person removed
//     it.
//
// The tool catalogue and the response contract follow, rendered by the
// model's backend from the tools the surface offers (internal/providers/llm).
//
// THE PAGE CONTEXT IS IN THE QUESTION'S MESSAGE, not in the system prompt
// before the conversation: it changes with every question, and anywhere
// earlier it would invalidate every cached byte of the conversation after it
// on every turn.

// identityRules is the assistant's standing instruction. A reworded sentence
// is a behaviour change, and takes a new [PromptVersion].
const identityRules = "You are the Crewlet assistant inside the company's dashboard. " +
	"You act only as the person talking to you: every tool you call is a request made " +
	"as them, decided by their own access, and you can never see or do more than they " +
	"could themselves. You have no identity, permission or memory of your own beyond " +
	"this conversation.\n\n" +
	"Rules:\n" +
	"- Answer from what your tools return. Never invent a number, a name, a status or " +
	"a record; if a tool did not tell you, say you do not know or call a tool that can.\n" +
	"- A change is never made by you. A write tool prepares a proposal the person " +
	"reviews and approves themselves, so describe what you are proposing and why, " +
	"and never claim a change has happened before they approve it.\n" +
	"- Never propose a change that is not among the writes listed for this person. " +
	"If they ask for one, say they do not have access to it.\n" +
	"- Lists you read may be filtered by what this person may see. Never suggest " +
	"that something does not exist because they cannot see it.\n" +
	"- Never ask for, repeat or store a password, token, key or any other credential.\n" +
	"- A refusal a tool returns is the engine's decision about this person. Report it " +
	"plainly; do not try another tool to get around it.\n" +
	"- Be brief. Lead with the answer, then the detail. When a tool's result is shown " +
	"to the person as a table or a card, do not repeat its rows in prose: refer to it " +
	"and add what it does not say.\n" +
	"- Write plain sentences and simple Markdown lists. Do not write links or images: " +
	"the person opens records from the cards the engine shows."

// promptInput is everything one turn's prompt is built from.
type promptInput struct {
	// Company is the company's name, Zone its clock and Now this instant.
	Company string
	Zone    *time.Location
	Now     time.Time

	// Person is the asker, Name their seat's name, Apps what they hold
	// and Proposable the writes they may propose.
	Person     iam.Principal
	Name       string
	Apps       []iam.Grant
	Proposable []catalog.Tool

	// History is the conversation so far, oldest first.
	History []historyEntry

	// Page is the page context the question carried, nil when removed.
	Page     *wire.PageContext
	Boundary provenance.Boundary
	Question string
}

// historyEntry is one earlier message as a turn re-reads it.
type historyEntry struct {
	Role wire.MessageRole
	Text string
	// Apps are the apps what the entry says was read through: its
	// message's and its cards'.
	Apps []string
	// Cards are the one-line summaries of the cards shown with it.
	Cards []string
}

// buildMessages is the turn's conversation as the loop starts it.
func buildMessages(in promptInput) []llm.Message {
	var system strings.Builder
	system.WriteString(identityRules)
	system.WriteString("\n\nPrompt version: ")
	system.WriteString(PromptVersion)
	system.WriteString("\n\n")
	system.WriteString(provenance.StandingInstruction)
	system.WriteString("\n\n")
	system.WriteString(companyBlock(in))
	system.WriteString("\n\n")
	system.WriteString(personBlock(in))

	out := []llm.Message{{Role: llm.RoleSystem, Content: system.String()}}
	if omitted := len(in.History) - HistoryMessages; omitted > 0 {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
			"(%d earlier messages of this conversation are not shown.)", omitted)})
		in.History = in.History[omitted:]
	}
	for _, h := range in.History {
		role := llm.RoleUser
		if h.Role == wire.RoleAssistant {
			role = llm.RoleAssistant
		}
		text := h.Text
		for _, card := range h.Cards {
			text += "\n" + card
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, llm.Message{Role: role, Content: text})
	}
	out = append(out, llm.Message{Role: llm.RoleUser, Content: questionBlock(in)})
	return out
}

// companyBlock is block 3: the company, its clock, and today on it.
func companyBlock(in promptInput) string {
	zone := in.Zone
	if zone == nil {
		zone = time.UTC
	}
	now := in.Now.In(zone)
	name := strings.TrimSpace(in.Company)
	if name == "" {
		name = "this company"
	}
	return fmt.Sprintf("Company: %s. Its clock is %s, where it is %s, %s. Read every "+
		"\"today\" and \"this week\" on that clock.",
		name, zone.String(), now.Format("Monday 2 January 2006"), now.Format("15:04"))
}

// personBlock is block 4: who is asking and what they may do.
func personBlock(in promptInput) string {
	var b strings.Builder
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.Person.Login
	}
	fmt.Fprintf(&b, "You are helping %s (seat %s).", name, in.Person.Seat)
	apps := make([]string, 0, len(in.Apps))
	for _, g := range in.Apps {
		apps = append(apps, string(g))
	}
	slices.Sort(apps)
	if len(apps) == 0 {
		b.WriteString(" They hold no app: they read the company overview only.")
	} else {
		fmt.Fprintf(&b, " They hold: %s.", strings.Join(apps, ", "))
	}
	if len(in.Proposable) == 0 {
		b.WriteString(" They may propose no change through you; if they ask for one, " +
			"say so and point them to the screen where they would make it.")
	} else {
		names := make([]string, 0, len(in.Proposable))
		for _, t := range in.Proposable {
			names = append(names, t.Name)
		}
		slices.Sort(names)
		fmt.Fprintf(&b, " The changes they may propose through you are: %s. "+
			"Never propose an action that is not in this list; say they lack access instead.",
			strings.Join(names, ", "))
	}
	b.WriteString(" Every list you read is filtered by what they may see.")
	return b.String()
}

// questionBlock is block 6: the page the question was asked on, fenced, then
// the question.
func questionBlock(in promptInput) string {
	if in.Page == nil {
		return in.Question
	}
	raw, err := json.Marshal(in.Page)
	if err != nil {
		// A page context that validated always encodes; one that did not
		// is left out rather than described.
		return in.Question
	}
	fenced := provenance.Fence(in.Boundary, provenance.SourcePageContext,
		provenance.TrustColleague, nil, string(raw))
	return "The person is looking at this screen of the dashboard, described by the " +
		"screen itself:\n" + fenced + "\n\nTheir question:\n" + in.Question
}

// cardSummary is one card as one line of the conversation the model re-reads:
// what it was and how big, never its rows.
func cardSummary(c wire.Card) string {
	switch c.Type {
	case wire.CardTable:
		return fmt.Sprintf("[table: %s, %d rows]", oneLine(c.Table.Title), c.Table.RowCount)
	case wire.CardEntity:
		return fmt.Sprintf("[%s: %s]", c.Entity.Kind, oneLine(c.Entity.Title))
	case wire.CardProposal:
		return fmt.Sprintf("[proposed change: %s, %s]", oneLine(c.Proposal.Title), c.Proposal.State)
	case wire.CardOpenInApp:
		return fmt.Sprintf("[link: %s]", oneLine(c.OpenInApp.Link.Label))
	case wire.CardError:
		return fmt.Sprintf("[refused: %s]", c.Error.Refusal.Code)
	case wire.CardNotice:
		return fmt.Sprintf("[notice: %s]", oneLine(c.Notice.Text))
	}
	return "[card]"
}

// oneLine is s with its line breaks folded, so a title somebody wrote cannot
// add lines of its own to the conversation the model re-reads.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
