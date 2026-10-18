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
	"github.com/crewlet/crewlet/internal/redact"
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
	"- Answer every part of the question in your own words, with the facts it asks for: " +
	"the names, numbers, states, dates and reasons. A table, a card or a link supports " +
	"your answer and never replaces it, so never answer only by pointing at one.\n" +
	"- Decide before you act. When the request is clear, do it. When what to change, " +
	"which record or who it is for is genuinely open, or the request is a matter of " +
	"taste (friendlier, cleaner), read what is there first, then either ask one short " +
	"question offering the options you found, or set out a short plan in order, what " +
	"must happen first coming first, and propose only its first step. Never propose a " +
	"change the person did not ask for.\n" +
	"- A change is never made by you. A write tool prepares a proposal the person " +
	"reviews and approves themselves, and the turn pauses at its card, so everything " +
	"you have to say comes before you call one: give in full what they asked to read " +
	"(a summary, a check, an answer), then say in a sentence or two what you are " +
	"proposing and why, including that a seat it involves is paused or stopped, " +
	"which you check first. Never claim a " +
	"change has happened before they approve it. A proposal exists only once the " +
	"tool's result says so and its card is shown: call the tool, never say one is " +
	"prepared in place of calling it.\n" +
	"- You can read and propose anything the engine's API lets this person do, whatever " +
	"screen is open. When no other tool fits, look the route up with describe_api, read " +
	"with api_request and propose with propose_api_change. Never guess a route or a " +
	"body: read the route's documentation and the resource first. Take as many steps " +
	"as the question needs, gathering what it needs yourself, before you answer or " +
	"propose.\n" +
	"- The page context says where the person is and what its open draft holds, values " +
	"they have not saved that no tool can read; use it, and it never limits what you " +
	"may do. Make each change where it lives: a write tool or propose_api_change for " +
	"anything saved. Use propose_page_change only to change the open screen itself, or " +
	"its draft within what the draft's help says. When a change can only be made in " +
	"another screen's editor, offer that screen with open_in_app and say what to do " +
	"there. Never give the current screen's limits as a reason not to help.\n" +
	"- People name seats, teams, projects and spaces by their names, and the tools take " +
	"a name or a key alike. Name seats by their names in your answers (Software " +
	"Engineer), never by their handles.\n" +
	"- Never say something does not exist unless you listed it and it was not there. " +
	"Lists you read may be filtered by what this person may see, so an absence is never " +
	"proof. When a tool refuses a name, it lists the valid choices: use the one the " +
	"person meant, or ask which.\n" +
	"- Never describe a setting, a secret, an integration or a screen you have not read: " +
	"read it first, or say you have not checked. A principle or policy is guidance the " +
	"agents read and not a limit the engine applies, so never promise one is enforced.\n" +
	"- A list that says more rows exist is one page, never the total: say \"at least\" " +
	"its count, or read further.\n" +
	"- Say every time on the company's clock, as your tools give it, never in UTC.\n" +
	"- Never ask for, repeat or store a password, token, key or any other credential.\n" +
	"- A refusal a tool returns is the engine's decision about this person. Do not try " +
	"another tool to get around it. A refusal of a change is the engine's decision " +
	"about this person: only then say they lack access, and which access, in plain " +
	"words.\n" +
	"- When a tool refuses, you are told why in the engine's terms. Tell the person " +
	"what went wrong and what they can do in plain words; never quote the refusal.\n" +
	"- Be brief. Lead with the answer, then the detail. When a table holds a long list, " +
	"do not copy every row into prose: say what the question asks of it (the totals, " +
	"the rows that matter, the reason) and let the table carry the rest.\n" +
	"- Write plain sentences and simple Markdown lists. Do not write links or images: " +
	"the person opens a record from where your answer names it. So name each record " +
	"you read exactly as your tools give it, a task or a project by its key (GOP-11), " +
	"a chat room by its name with its # (#launch), and a seat, team, page or space by " +
	"its name, outside code formatting; what you read and never name is listed under " +
	"your answer.\n" +
	"- Write for the person, in their words. Never name a tool, a route, an HTTP " +
	"method, a config key, a field in code or a file path; name what you read or " +
	"change as they know it (\"the QA Engineer's seat\", \"the brand\", \"the FAIR " +
	"space\"). Never write notes to yourself: before a read write nothing, or one " +
	"sentence telling the person what you are checking."

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

	// Deep says the turn offers deep work, and Attachments are the files
	// the question carried.
	Deep        bool
	Attachments []wire.FileRef
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
	if in.Deep {
		system.WriteString("\n\n")
		system.WriteString(deepBlock)
	}

	out := []llm.Message{{Role: llm.RoleSystem, Content: system.String()}}
	out = append(out, historyMessages(in.History)...)
	out = append(out, llm.Message{Role: llm.RoleUser, Content: questionBlock(in)})
	return out
}

// historyMessages is block 5: the conversation's last [HistoryMessages]
// messages, each card as its one-line summary, and the older ones counted.
func historyMessages(history []historyEntry) []llm.Message {
	var out []llm.Message
	if omitted := len(history) - HistoryMessages; omitted > 0 {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
			"(%d earlier messages of this conversation are not shown.)", omitted)})
		history = history[omitted:]
	}
	for _, h := range history {
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
		"\"today\" and \"this week\" on that clock. A week runs Monday to Sunday: this "+
		"week is the one that began on Monday and last week the one before it, never a "+
		"rolling seven days; say which days a figure covers.",
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
		fmt.Fprintf(&b, " The changes they may propose through you are: %s.",
			strings.Join(names, ", "))
	}
	b.WriteString(" Every list you read is filtered by what they may see.")
	return b.String()
}

// redactedPage is a validated page context with the engine's secret pass
// ([redact.Secrets]) over every value a person or a colleague typed: the
// title, the query and the open draft. The dashboard redacts every field
// whose path names a credential, and the page context refuses one that is
// not ([wire.PageContext.Validate]); this is the last line for a credential
// pasted into a field that does not say so, by its shape.
func redactedPage(p *wire.PageContext) *wire.PageContext {
	if p == nil {
		return nil
	}
	out := *p
	out.Title = redact.Secrets(p.Title)
	out.Query = make(map[string]string, len(p.Query))
	for k, v := range p.Query {
		out.Query[k] = redact.Secrets(v)
	}
	if p.Draft != nil {
		draft := *p.Draft
		draft.Help = redact.Secrets(p.Draft.Help)
		draft.Fields = make([]wire.DraftField, len(p.Draft.Fields))
		for i, f := range p.Draft.Fields {
			f.Label, f.Hint, f.Value = redact.Secrets(f.Label), redact.Secrets(f.Hint),
				redact.Secrets(f.Value)
			draft.Fields[i] = f
		}
		out.Draft = &draft
	}
	return &out
}

// questionBlock is block 6: the page the question was asked on, fenced, then
// the question.
func questionBlock(in promptInput) string {
	question := in.Question
	if block := attachmentsBlock(in.Attachments, in.Deep); block != "" {
		question = block + "\n\n" + question
	}
	fenced := pageFence(in.Boundary, in.Page)
	if fenced == "" {
		return question
	}
	return "The person is looking at this screen of the dashboard, described by the " +
		"screen itself:\n" + fenced + "\n\nTheir question:\n" + question
}

// pageFence is a page context as every reader of it sees it, the turn's model
// and a deep-work run alike: the screen's own description, fenced as the
// page's. Empty for no page, and for one that does not encode, which a page
// context that validated always does.
func pageFence(b provenance.Boundary, p *wire.PageContext) string {
	if p == nil {
		return ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return provenance.Fence(b, provenance.SourcePageContext, provenance.TrustColleague, nil,
		string(raw))
}

// admitPage is the one admission of a page context a request carried, a
// question's or a decision's: validated, then passed through the engine's
// secret pass. Nil admits as nil, the person having removed the chip.
func admitPage(p *wire.PageContext) (*wire.PageContext, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return redactedPage(p), nil
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
	case wire.CardOversize:
		return fmt.Sprintf("[too large to read: %s]", oneLine(c.Oversize.Title))
	case wire.CardBrief:
		return briefSummary(c.Brief)
	case wire.CardFile:
		names := make([]string, 0, len(c.File.Files))
		for _, f := range c.File.Files {
			names = append(names, oneLine(f.Name)+" (file id "+f.FileID.String()+")")
		}
		return "[files: " + strings.Join(names, ", ") + "]"
	}
	return "[card]"
}

// oneLine is s with its line breaks folded, so a title somebody wrote cannot
// add lines of its own to the conversation the model re-reads.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
