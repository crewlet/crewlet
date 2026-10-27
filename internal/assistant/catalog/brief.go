package catalog

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// AnswerBriefTool is how a brief turn answers: the brief in its fixed shape,
// as one call the engine checks, resolves and draws as a brief card.
const AnswerBriefTool = "answer_brief"

// BriefRefKinds are the records a brief's row may cite: the kinds whose
// screen the link table opens through a read of the record itself
// ([Open]'s probes), so every cited record is one the engine reads again as
// the person before it is drawn.
var BriefRefKinds = []wire.ResourceKind{
	wire.ResourceTask, wire.ResourceProject, wire.ResourceSeat, wire.ResourcePage,
	wire.ResourceArtifact,
}

// BriefScreenOf is the link table's screen for a brief ref's kind, and false
// for a kind a brief may not cite.
func BriefScreenOf(kind wire.ResourceKind) (screen, param string, ok bool) {
	switch kind {
	case wire.ResourceTask:
		return "item", "id", true
	case wire.ResourceProject:
		return "project", "key", true
	case wire.ResourceSeat:
		return "seat", "handle", true
	case wire.ResourcePage:
		return "page", "id", true
	case wire.ResourceArtifact:
		return "artifact", "id", true
	}
	return "", "", false
}

// What a row offers, as the model names it: open the record it cites, or ask
// a question. Every row is something to do, so every row offers one.
const (
	BriefActionOpen = "open"
	BriefActionAsk  = "ask"
)

// brief is the tool, built and checked once.
var brief = sync.OnceValues(func() (Tool, error) {
	kinds := make([]string, 0, len(BriefRefKinds))
	for _, k := range BriefRefKinds {
		kinds = append(kinds, string(k))
	}
	tones := make([]string, 0, len(wire.BriefTones))
	for _, t := range wire.BriefTones {
		tones = append(tones, string(t))
	}
	// EVERY LINE STATES ITS CAP, in the schema's maxLength and in its words,
	// in the characters the engine counts ([briefLine]), so a model meets it
	// as it writes rather than learning it from a refusal.
	line := func(description string, max int) *schema {
		return chars(fmt.Sprintf("%s One line, at most %d characters.", description, max), max)
	}
	need := object([]string{"ref_kind", "ref_id", "do", "tone", "action"}, map[string]*schema{
		"ref_kind": oneOf("The kind of the record this row is about.", kinds...),
		"ref_id": text("The record's key, handle or id exactly as a read in this turn "+
			"returned it: ENG-42, pm, a page's id.", nameBytes),
		"do": line("What the person has to do, as an instruction that starts with a verb "+
			"and names the record: \"Answer ENG-42: hand it to the App Engineer?\".",
			wire.BriefTextMaxRunes),
		"why": line("Why now, in one fact: how old, or what it blocks.",
			wire.BriefDetailMaxRunes),
		"tone": oneOf("danger only for an agent that is stopped or whose last turn failed, "+
			"or a task marked urgent; warning for everything else.", tones...),
		"action": oneOf("open the record's screen, or ask a question in this conversation.",
			BriefActionOpen, BriefActionAsk),
		"question": line("The question an ask action asks, in the person's words.",
			wire.BriefQuestionMaxRunes),
	})
	t := Tool{
		Name: AnswerBriefTool, Kind: KindBrief, Tier: TierRead,
		Phrase: "write the brief",
		Description: fmt.Sprintf("Answer with the brief, once you have read what it needs. "+
			"Call it exactly once; it is your whole answer, and you write nothing else. "+
			"Every line is one line within its cap: about at most %d characters, a need's "+
			"do at most %d and its why at most %d, context at most %d, a question at most %d.",
			wire.BriefAboutMaxRunes, wire.BriefTextMaxRunes, wire.BriefDetailMaxRunes,
			wire.BriefContextMaxRunes, wire.BriefQuestionMaxRunes),
		schema: object([]string{"about", "needs"}, map[string]*schema{
			"about": line("What this page or record is about right now, in plain words: "+
				"the situation, not a list of counts.", wire.BriefAboutMaxRunes),
			"needs": {kind: "array", description: fmt.Sprintf("Only what the person has to "+
				"do, most pressing first, at most %d; the first is the one you recommend. "+
				"Empty when nothing needs them, which is a good answer: never a fact to "+
				"know, never invented urgency.", wire.BriefMaxNeeds),
				maxItems: wire.BriefMaxNeeds, items: need},
			"context": line("Only when it changes what the person should do: one fact from "+
				"elsewhere that bears on it. Leave it out otherwise.", wire.BriefContextMaxRunes),
			"follow_ups": {kind: "array", description: fmt.Sprintf("Questions the person "+
				"may ask next, at most %d, each answerable from what they can read.",
				wire.BriefMaxFollowUps), maxItems: wire.BriefMaxFollowUps,
				items: line("A question.", wire.BriefQuestionMaxRunes)},
		}),
		Label: func(map[string]any) string { return "Writing the brief" },
	}
	t.Parameters = t.schema.JSON()
	if t.Name == "" || t.Description == "" || t.Phrase == "" || !t.Kind.Valid() || !t.Tier.Valid() {
		return Tool{}, fmt.Errorf("catalog: %s is declared incompletely", t.Name)
	}
	return t, nil
})

// BriefTool is the brief turn's answer tool. It is never in [Tools] or
// [Offered]: the orchestrator offers it to a brief turn alone, beside that
// turn's reads.
func BriefTool() Tool {
	t, err := brief()
	if err != nil {
		panic(err)
	}
	return t
}

// BriefDraft is an [AnswerBriefTool] call as the model wrote it, checked
// against the tool's schema and every bound of [wire.BriefCard], with
// nothing yet resolved: its rows' records are the orchestrator's to look up.
// It carries no verdict: whether anything needs the person is the number of
// rows that survive resolution, which the engine counts.
type BriefDraft struct {
	About     string
	Needs     []BriefDraftNeed
	Context   string
	FollowUps []string
}

// BriefDraftNeed is one row as written: Text is its do, Detail its why.
type BriefDraftNeed struct {
	Ref      wire.ResourceRef
	Text     string
	Detail   string
	Tone     wire.BriefTone
	Action   string
	Question string
}

// ParseBrief checks an [AnswerBriefTool] call and reads it into a draft: the
// schema, then what a schema cannot say (an ask names its question, every
// line a plain line within its bound). An error names EVERY field at fault,
// not the first: the model writes a refused brief again once, and a refusal
// that named one fault of several would spend that attempt on the next.
func ParseBrief(args map[string]any) (BriefDraft, error) {
	if args == nil {
		args = map[string]any{}
	}
	if found := BriefTool().schema.problems(AnswerBriefTool, args); len(found) > 0 {
		return BriefDraft{}, refusedFor(found)
	}
	var found []error
	note := func(errs ...error) {
		for _, err := range errs {
			if err != nil {
				found = append(found, err)
			}
		}
	}
	a := Args(args)
	d := BriefDraft{About: strings.TrimSpace(a.String("about")),
		Context: strings.TrimSpace(a.String("context"))}
	note(briefLine("about", d.About, wire.BriefAboutMaxRunes, true),
		briefLine("context", d.Context, wire.BriefContextMaxRunes, false))
	for i, raw := range list(args["needs"]) {
		n := Args(asObject(raw))
		path := fmt.Sprintf("needs[%d]", i)
		need := BriefDraftNeed{
			Ref: wire.ResourceRef{Kind: wire.ResourceKind(n.String("ref_kind")),
				ID: strings.TrimSpace(n.String("ref_id"))},
			Text: strings.TrimSpace(n.String("do")), Detail: strings.TrimSpace(n.String("why")),
			Tone: wire.BriefTone(n.String("tone")), Action: n.String("action"),
			Question: strings.TrimSpace(n.String("question")),
		}
		note(refID(path, need.Ref.ID),
			briefLine(path+".do", need.Text, wire.BriefTextMaxRunes, true),
			briefLine(path+".why", need.Detail, wire.BriefDetailMaxRunes, false),
			briefAction(path, need.Action, need.Question))
		d.Needs = append(d.Needs, need)
	}
	for i, raw := range list(args["follow_ups"]) {
		q, _ := raw.(string)
		q = strings.TrimSpace(q)
		note(briefLine(fmt.Sprintf("follow_ups[%d]", i), q, wire.BriefQuestionMaxRunes, true))
		d.FollowUps = append(d.FollowUps, q)
	}
	if len(found) > 0 {
		return BriefDraft{}, refusedFor(found)
	}
	return d, nil
}

// refusedFor is one refusal naming every fault found, in the order found.
func refusedFor(found []error) error {
	reasons := make([]string, 0, len(found))
	for _, err := range found {
		reasons = append(reasons, err.Error())
	}
	return fmt.Errorf("%w: %s", ErrInvalidArguments, strings.Join(reasons, "; "))
}

// briefLine holds one of a brief's lines to one plain line of at most max
// characters, and to a word at least where it is required.
func briefLine(path, s string, max int, required bool) error {
	switch {
	case required && s == "":
		return fmt.Errorf("%s is empty", path)
	case utf8.RuneCountInString(s) > max:
		return fmt.Errorf("%s is %d characters and the most it takes is %d", path,
			utf8.RuneCountInString(s), max)
	case strings.ContainsAny(s, "\r\n"):
		return fmt.Errorf("%s is more than one line", path)
	}
	return nil
}

// briefAction holds an action to what it needs: an ask its question, and an
// open none.
func briefAction(path, action, question string) error {
	switch {
	case action == BriefActionAsk && question == "":
		return fmt.Errorf("%s asks and names no question", path)
	case action == BriefActionOpen && question != "":
		return fmt.Errorf("%s names a question and its action is open, not ask", path)
	}
	return briefLine(path+".question", question, wire.BriefQuestionMaxRunes, false)
}

// refID holds a cited record's id to one line: never empty, never a line
// break. A seat may be cited by its name, which has spaces in it.
func refID(path, id string) error {
	if id == "" || strings.ContainsAny(id, "\r\n\t") {
		return fmt.Errorf("%s names no record id, or one over more than one line", path)
	}
	return nil
}

// list is a JSON array as decoded, or none.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// asObject is a JSON object as decoded, or an empty one.
func asObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

// BriefOpensWith is an open action's link-table arguments for a cited record:
// its screen and the one parameter that names it, the call [Open] reads.
func BriefOpensWith(ref wire.ResourceRef) (Args, bool) {
	screen, param, ok := BriefScreenOf(ref.Kind)
	if !ok {
		return nil, false
	}
	return Args{"screen": screen, param: ref.ID}, true
}
