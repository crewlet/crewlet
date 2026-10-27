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

// What a row or the recommendation offers, as the model names it: open the
// record it cites, ask a question, or nothing.
const (
	BriefActionOpen = "open"
	BriefActionAsk  = "ask"
	BriefActionNone = "none"
)

// brief is the tool, built and checked once.
var brief = sync.OnceValues(func() (Tool, error) {
	kinds := make([]string, 0, len(BriefRefKinds))
	for _, k := range BriefRefKinds {
		kinds = append(kinds, string(k))
	}
	tones := []string{string(wire.BriefWarning), string(wire.BriefNeutral),
		string(wire.BriefDanger)}
	actions := []string{BriefActionOpen, BriefActionAsk, BriefActionNone}
	line := func(description string, runes int) *schema { return text(description, runes*4) }
	need := object([]string{"ref_kind", "ref_id", "text", "tone", "action"}, map[string]*schema{
		"ref_kind": oneOf("The kind of the record this row is about.", kinds...),
		"ref_id": text("The record's key, handle or id exactly as a read in this turn "+
			"returned it: ENG-42, pm, a page's id.", nameBytes),
		"text": line("What waits on the person, with its number and its name.",
			wire.BriefTextMaxRunes),
		"detail": line("One fact under it: how old, how much, what it blocks.",
			wire.BriefTextMaxRunes),
		"tone": oneOf("warning for something to act on, neutral for something to know. "+
			"danger only for a seat that is stopped, a task marked urgent or a failed turn.",
			tones...),
		"action": oneOf("open the record's screen, ask a question in this conversation, or none.",
			actions...),
		"question": line("The question an ask action asks, in the person's words.",
			wire.BriefQuestionMaxRunes),
	})
	step := object([]string{"text", "action"}, map[string]*schema{
		"text": line("The one next step, as an instruction with its number and its name.",
			wire.BriefTextMaxRunes),
		"ref_kind": oneOf("The record the step is about, for an open action.", kinds...),
		"ref_id":   text("That record's key, handle or id as a read returned it.", nameBytes),
		"action": oneOf("open the record's screen, ask a question in this conversation, or none.",
			actions...),
		"question": line("The question an ask action asks.", wire.BriefQuestionMaxRunes),
	})
	t := Tool{
		Name: AnswerBriefTool, Kind: KindBrief, Tier: TierRead,
		Phrase: "write the brief",
		Description: "Answer with the brief, once you have read what it needs. Call it exactly " +
			"once; it is your whole answer, and you write nothing else.",
		schema: object([]string{"summary", "needs", "follow_ups"}, map[string]*schema{
			"summary": line("What is going on, in one sentence with numbers and names.",
				wire.BriefSummaryMaxRunes),
			"needs": {kind: "array", description: "What waits on the person, most pressing " +
				"first, at most three; empty when nothing does.", maxItems: wire.BriefMaxNeeds,
				items: need},
			"recommended": step,
			"follow_ups": {kind: "array", description: "Questions the person may ask next, " +
				"each answerable from what they can read.", maxItems: wire.BriefMaxFollowUps,
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
type BriefDraft struct {
	Summary     string
	Needs       []BriefDraftNeed
	Recommended *BriefDraftStep
	FollowUps   []string
}

// BriefDraftNeed is one row as written.
type BriefDraftNeed struct {
	Ref      wire.ResourceRef
	Text     string
	Detail   string
	Tone     wire.BriefTone
	Action   string
	Question string
}

// BriefDraftStep is the recommendation as written; Ref is set for an open.
type BriefDraftStep struct {
	Text     string
	Ref      *wire.ResourceRef
	Action   string
	Question string
}

// ParseBrief checks an [AnswerBriefTool] call and reads it into a draft: the
// schema, then what a schema cannot say (an open names its record, an ask its
// question, every line a plain line within its bound). An error names the
// field, for the model to write it again.
func ParseBrief(args map[string]any) (BriefDraft, error) {
	if err := BriefTool().Check(args); err != nil {
		return BriefDraft{}, err
	}
	a := Args(args)
	d := BriefDraft{Summary: strings.TrimSpace(a.String("summary"))}
	if err := briefLine("summary", d.Summary, wire.BriefSummaryMaxRunes, true); err != nil {
		return BriefDraft{}, err
	}
	for i, raw := range list(args["needs"]) {
		n := Args(asObject(raw))
		path := fmt.Sprintf("needs[%d]", i)
		need := BriefDraftNeed{
			Ref: wire.ResourceRef{Kind: wire.ResourceKind(n.String("ref_kind")),
				ID: strings.TrimSpace(n.String("ref_id"))},
			Text: strings.TrimSpace(n.String("text")), Detail: strings.TrimSpace(n.String("detail")),
			Tone: wire.BriefTone(n.String("tone")), Action: n.String("action"),
			Question: strings.TrimSpace(n.String("question")),
		}
		if err := errorsOf(
			refID(path, need.Ref.ID),
			briefLine(path+".text", need.Text, wire.BriefTextMaxRunes, true),
			briefLine(path+".detail", need.Detail, wire.BriefTextMaxRunes, false),
			briefAction(path, need.Action, need.Question),
		); err != nil {
			return BriefDraft{}, err
		}
		d.Needs = append(d.Needs, need)
	}
	if raw, ok := args["recommended"]; ok {
		r := Args(asObject(raw))
		step := &BriefDraftStep{Text: strings.TrimSpace(r.String("text")), Action: r.String("action"),
			Question: strings.TrimSpace(r.String("question"))}
		if step.Action == BriefActionOpen {
			ref := wire.ResourceRef{Kind: wire.ResourceKind(r.String("ref_kind")),
				ID: strings.TrimSpace(r.String("ref_id"))}
			if ref.Kind == "" {
				return BriefDraft{}, fmt.Errorf("%w: recommended opens a record and names no "+
					"ref_kind", ErrInvalidArguments)
			}
			if err := refID("recommended", ref.ID); err != nil {
				return BriefDraft{}, err
			}
			step.Ref = &ref
		}
		if err := errorsOf(
			briefLine("recommended.text", step.Text, wire.BriefTextMaxRunes, true),
			briefAction("recommended", step.Action, step.Question),
		); err != nil {
			return BriefDraft{}, err
		}
		d.Recommended = step
	}
	for i, raw := range list(args["follow_ups"]) {
		q, _ := raw.(string)
		q = strings.TrimSpace(q)
		if err := briefLine(fmt.Sprintf("follow_ups[%d]", i), q, wire.BriefQuestionMaxRunes,
			true); err != nil {
			return BriefDraft{}, err
		}
		d.FollowUps = append(d.FollowUps, q)
	}
	return d, nil
}

// briefLine holds one of a brief's lines to one plain line of at most max
// characters, and to a word at least where it is required.
func briefLine(path, s string, max int, required bool) error {
	switch {
	case required && s == "":
		return fmt.Errorf("%w: %s is empty", ErrInvalidArguments, path)
	case utf8.RuneCountInString(s) > max:
		return fmt.Errorf("%w: %s is %d characters and the most it takes is %d",
			ErrInvalidArguments, path, utf8.RuneCountInString(s), max)
	case strings.ContainsAny(s, "\r\n"):
		return fmt.Errorf("%w: %s is more than one line", ErrInvalidArguments, path)
	}
	return nil
}

// briefAction holds an action to what it needs: an ask its question, and
// nothing else a question.
func briefAction(path, action, question string) error {
	switch {
	case action == BriefActionAsk && question == "":
		return fmt.Errorf("%w: %s asks and names no question", ErrInvalidArguments, path)
	case action != BriefActionAsk && question != "":
		return fmt.Errorf("%w: %s names a question and its action is %s, not ask",
			ErrInvalidArguments, path, action)
	}
	return briefLine(path+".question", question, wire.BriefQuestionMaxRunes, false)
}

// refID holds a cited record's id to one line: never empty, never a line
// break. A seat may be cited by its name, which has spaces in it.
func refID(path, id string) error {
	if id == "" || strings.ContainsAny(id, "\r\n\t") {
		return fmt.Errorf("%w: %s names no record id, or one over more than one line",
			ErrInvalidArguments, path)
	}
	return nil
}

// errorsOf is the first error of several checks.
func errorsOf(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
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
