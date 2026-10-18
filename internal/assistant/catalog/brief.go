package catalog

import (
	"fmt"
	"strings"
	"sync"

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

// ParseBrief checks an [AnswerBriefTool] call and reads it into a draft. An
// error names EVERY field at fault, in ONE PASS (ADR-0059): the schema's
// findings (a type, a closed set, a cap in characters, a member missing or
// unknown) and, beside them, what a schema cannot say about every member
// that is there to read (a line that is empty or breaks, an ask with no
// question or an open with one, a record cited by two rows). The model writes
// a refused brief again once, and a refusal that named one group of faults
// would spend that attempt on the other.
//
// THE TWO GROUPS DO NOT OVERLAP: a member the schema found missing or of the
// wrong type is not read again here, and the length of a line is the
// schema's alone, so no fault is named twice.
func ParseBrief(args map[string]any) (BriefDraft, error) {
	if args == nil {
		args = map[string]any{}
	}
	found := BriefTool().schema.problems(AnswerBriefTool, args)
	note := func(errs ...error) {
		for _, err := range errs {
			if err != nil {
				found = append(found, err)
			}
		}
	}
	note(briefLine("about", args["about"], true), briefLine("context", args["context"], false))
	cited := map[wire.ResourceRef]int{}
	for i, raw := range list(args["needs"]) {
		n := asObject(raw)
		path := fmt.Sprintf("needs[%d]", i)
		note(refID(path, n["ref_id"]), briefLine(path+".do", n["do"], true),
			briefLine(path+".why", n["why"], false),
			briefLine(path+".question", n["question"], false),
			briefAction(path, n["action"], n["question"]))
		kind, kindOK := n["ref_kind"].(string)
		id, idOK := n["ref_id"].(string)
		if !kindOK || !idOK {
			continue
		}
		ref := wire.ResourceRef{Kind: wire.ResourceKind(kind), ID: strings.TrimSpace(id)}
		if first, seen := cited[ref]; seen {
			note(fmt.Errorf("%s cites %s %s, which needs[%d] already cites: one row per "+
				"record", path, kind, ref.ID, first))
			continue
		}
		cited[ref] = i
	}
	for i, q := range list(args["follow_ups"]) {
		note(briefLine(fmt.Sprintf("follow_ups[%d]", i), q, true))
	}
	if len(found) > 0 {
		return BriefDraft{}, refusedFor(found)
	}
	a := Args(args)
	d := BriefDraft{About: strings.TrimSpace(a.String("about")),
		Context: strings.TrimSpace(a.String("context"))}
	for _, raw := range list(args["needs"]) {
		n := Args(asObject(raw))
		d.Needs = append(d.Needs, BriefDraftNeed{
			Ref: wire.ResourceRef{Kind: wire.ResourceKind(n.String("ref_kind")),
				ID: strings.TrimSpace(n.String("ref_id"))},
			Text: strings.TrimSpace(n.String("do")), Detail: strings.TrimSpace(n.String("why")),
			Tone: wire.BriefTone(n.String("tone")), Action: n.String("action"),
			Question: strings.TrimSpace(n.String("question")),
		})
	}
	for _, raw := range list(args["follow_ups"]) {
		q, _ := raw.(string)
		d.FollowUps = append(d.FollowUps, strings.TrimSpace(q))
	}
	return d, nil
}

// refusedFor is one refusal naming every fault found, in the order found, and
// after them, once each, the sentences that say how to fix them (a
// directory's choices, which several of its faults may share).
func refusedFor(found []error, notes ...string) error {
	reasons := make([]string, 0, len(found))
	for _, err := range found {
		reasons = append(reasons, err.Error())
	}
	refusal := strings.Join(reasons, "; ")
	if len(notes) > 0 {
		refusal += ". " + strings.Join(notes, " ")
	}
	return fmt.Errorf("%w: %s", ErrInvalidArguments, refusal)
}

// briefLine holds one of a brief's lines, as written, to one plain line, and
// to a word at least where it is required. A member that is not there or is
// not a string is the schema's to name, and so is its length in characters.
func briefLine(path string, v any, required bool) error {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	switch {
	case required && s == "":
		return fmt.Errorf("%s is empty", path)
	case strings.ContainsAny(s, "\r\n"):
		return fmt.Errorf("%s is more than one line", path)
	}
	return nil
}

// briefAction holds an action, as written, to what it needs: an ask its
// question, and an open none. An action outside the closed set is the
// schema's to name.
func briefAction(path string, action, question any) error {
	asked, _ := question.(string)
	asked = strings.TrimSpace(asked)
	switch action {
	case BriefActionAsk:
		if asked == "" {
			return fmt.Errorf("%s asks and names no question", path)
		}
	case BriefActionOpen:
		if asked != "" {
			return fmt.Errorf("%s names a question and its action is open, not ask", path)
		}
	}
	return nil
}

// refID holds a cited record's id, as written, to one line: never empty,
// never a line break. A seat may be cited by its name, which has spaces in
// it. A missing id is the schema's to name.
func refID(path string, v any) error {
	id, ok := v.(string)
	if !ok {
		return nil
	}
	if id = strings.TrimSpace(id); id == "" || strings.ContainsAny(id, "\r\n\t") {
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
