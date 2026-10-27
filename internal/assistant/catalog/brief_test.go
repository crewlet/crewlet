package catalog_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// AN ANSWER_BRIEF CALL IS READ STRICTLY: its schema, then what a schema cannot
// say (an ask names its question and an open none, every line is one line).
// A row is something to do, so it has no quiet tone and no row without a
// press; a verdict is the engine's to count, so a call that writes one is
// refused. Mutation: drop a rule and its case is accepted.
func TestABriefCallIsReadStrictly(t *testing.T) {
	t.Parallel()
	need := func(over map[string]any) map[string]any {
		n := map[string]any{"ref_kind": "task", "ref_id": "ENG-1",
			"do": "Answer ENG-1: which build QA tests", "tone": "warning", "action": "open"}
		for k, v := range over {
			n[k] = v
		}
		return n
	}
	call := func(needs ...map[string]any) map[string]any {
		list := make([]any, 0, len(needs))
		for _, n := range needs {
			list = append(list, n)
		}
		return map[string]any{"about": "QA asks which build to test.", "needs": list}
	}
	d, err := catalog.ParseBrief(call(need(map[string]any{"why": "Asked 2 hours ago"})))
	if err != nil {
		t.Fatalf("a valid call was refused: %v", err)
	}
	if len(d.Needs) != 1 || d.Needs[0].Ref != (wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1"}) ||
		d.Needs[0].Action != catalog.BriefActionOpen || d.Needs[0].Detail != "Asked 2 hours ago" {
		t.Errorf("draft = %+v", d)
	}
	if _, err := catalog.ParseBrief(map[string]any{"about": "Nothing is waiting.",
		"needs": []any{}}); err != nil {
		t.Errorf("a brief with nothing to do was refused: %v", err)
	}
	refused := map[string]map[string]any{
		"no about":            {"about": "", "needs": []any{}},
		"no needs":            {"about": "s"},
		"an ask, no question": call(need(map[string]any{"action": "ask"})),
		"a question, no ask":  call(need(map[string]any{"question": "Why?"})),
		"no action":           call(need(map[string]any{"action": nil})),
		"an action of none":   call(need(map[string]any{"action": "none"})),
		"a neutral row":       call(need(map[string]any{"tone": "neutral"})),
		"no do":               call(need(map[string]any{"do": nil})),
		"two lines":           call(need(map[string]any{"do": "one\ntwo"})),
		"a kind not cited":    call(need(map[string]any{"ref_kind": "unit"})),
		"four rows":           call(need(nil), need(nil), need(nil), need(nil)),
		"three follow-ups":    {"about": "s", "needs": []any{}, "follow_ups": []any{"a?", "b?", "c?"}},
		"a written verdict":   {"about": "s", "needs": []any{}, "verdict": "Nothing needs you"},
		"a recommendation":    {"about": "s", "needs": []any{}, "recommended": map[string]any{"text": "Do it"}},
		"a summary":           {"about": "s", "needs": []any{}, "summary": "1 decision waits."},
		"an identity":         {"about": "s", "needs": []any{}, "actor": "ada"},
	}
	for name, args := range refused {
		for k, v := range args {
			if v == nil {
				delete(args, k)
			}
		}
		if needs, ok := args["needs"].([]any); ok {
			for _, n := range needs {
				for k, v := range n.(map[string]any) {
					if v == nil {
						delete(n.(map[string]any), k)
					}
				}
			}
		}
		if _, err := catalog.ParseBrief(args); !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s: answered %v, want ErrInvalidArguments", name, err)
		}
	}
}

// A BRIEF'S CITABLE RECORDS ARE EXACTLY THOSE THE LINK TABLE OPENS THROUGH A
// READ OF THE RECORD, so every row is read again as the person before it is
// drawn. Mutation: add a kind with no probe and its open builds no request.
func TestEveryCitableRecordIsReadAgainBeforeItIsDrawn(t *testing.T) {
	t.Parallel()
	ids := map[wire.ResourceKind]string{wire.ResourceTask: "ENG-1", wire.ResourceProject: "ENG",
		wire.ResourceSeat: "qa", wire.ResourcePage: "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e10",
		wire.ResourceArtifact: "0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e11"}
	for _, kind := range catalog.BriefRefKinds {
		args, ok := catalog.BriefOpensWith(wire.ResourceRef{Kind: kind, ID: ids[kind]})
		if !ok {
			t.Errorf("%s has no screen", kind)
			continue
		}
		_, probe, err := catalog.Open(args)
		if err != nil || probe == nil {
			t.Errorf("%s opens with no read of the record (%v)", kind, err)
		}
	}
}

// EVERY CAP IS HANDED TO THE MODEL BEFORE IT WRITES, as the bound it is held
// to: each line's maxLength is its cap in characters and its description
// says so, and each list's maxItems is its cap. The live failure was a schema
// that advertised four times the cap in bytes, so a model that respected it
// wrote a 366 character summary into a 200 character line.
//
// Mutation: hand the model a bytes bound, or drop a cap from a description,
// and this fails.
func TestEveryBriefCapIsInTheSchemaTheModelReads(t *testing.T) {
	t.Parallel()
	params := catalog.BriefTool().Parameters
	props := params["properties"].(map[string]any)
	need := props["needs"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	lines := map[string]struct {
		schema map[string]any
		max    int
	}{
		"about":          {props["about"].(map[string]any), wire.BriefAboutMaxRunes},
		"context":        {props["context"].(map[string]any), wire.BriefContextMaxRunes},
		"needs.do":       {need["do"].(map[string]any), wire.BriefTextMaxRunes},
		"needs.why":      {need["why"].(map[string]any), wire.BriefDetailMaxRunes},
		"needs.question": {need["question"].(map[string]any), wire.BriefQuestionMaxRunes},
		"follow_ups": {props["follow_ups"].(map[string]any)["items"].(map[string]any),
			wire.BriefQuestionMaxRunes},
	}
	for name, l := range lines {
		if got := l.schema["maxLength"]; got != l.max {
			t.Errorf("%s: maxLength = %v, want %d characters", name, got, l.max)
		}
		desc, _ := l.schema["description"].(string)
		if want := fmt.Sprintf("at most %d characters", l.max); !strings.Contains(desc, want) {
			t.Errorf("%s: description %q does not say %q", name, desc, want)
		}
	}
	for name, max := range map[string]int{"needs": wire.BriefMaxNeeds,
		"follow_ups": wire.BriefMaxFollowUps} {
		list := props[name].(map[string]any)
		if list["maxItems"] != max {
			t.Errorf("%s: maxItems = %v, want %d", name, list["maxItems"], max)
		}
		if desc, _ := list["description"].(string); !strings.Contains(desc,
			fmt.Sprintf("at most %d", max)) {
			t.Errorf("%s: description %q does not state its cap", name, desc)
		}
	}
}

// THE SCHEMA ASKS FOR A SHAPE WHOSE CONCISION IS STRUCTURAL: the about line
// and the needs are all it requires, it has no field a verdict, a
// recommendation or a fact to know could be written in, a row's tone is
// warning or danger with danger kept to the facts the engine checks, and a
// row's do is asked for as an instruction that starts with a verb and names
// its record. Mutation: require follow_ups or context, add a summary,
// verdict or recommended property, put neutral back, or drop the words that
// ask for a verb and a record, and this fails.
func TestTheBriefSchemaAsksForAVerdictItCanCount(t *testing.T) {
	t.Parallel()
	params := catalog.BriefTool().Parameters
	props := params["properties"].(map[string]any)
	required := fmt.Sprint(params["required"])
	if required != "[about needs]" {
		t.Errorf("the brief requires %s, want [about needs]", required)
	}
	for _, gone := range []string{"summary", "verdict", "recommended"} {
		if _, ok := props[gone]; ok {
			t.Errorf("the brief schema has a %s field", gone)
		}
	}
	need := props["needs"].(map[string]any)["items"].(map[string]any)
	if got := fmt.Sprint(need["required"]); got != "[ref_kind ref_id do tone action]" {
		t.Errorf("a need requires %s, want its record, its do, its tone and its action", got)
	}
	needProps := need["properties"].(map[string]any)
	tone := needProps["tone"].(map[string]any)
	if got := fmt.Sprint(tone["enum"]); got != "[danger warning]" {
		t.Errorf("a need's tones are %s, want [danger warning]", got)
	}
	for _, fact := range []string{"stopped", "last turn failed", "urgent"} {
		if desc, _ := tone["description"].(string); !strings.Contains(desc, fact) {
			t.Errorf("the tone's description does not keep danger to %q: %q", fact, desc)
		}
	}
	if got := fmt.Sprint(needProps["action"].(map[string]any)["enum"]); got != "[open ask]" {
		t.Errorf("a need's actions are %s, want [open ask]", got)
	}
	do, _ := needProps["do"].(map[string]any)["description"].(string)
	for _, want := range []string{"starts with a verb", "names the record"} {
		if !strings.Contains(do, want) {
			t.Errorf("a need's do does not ask that it %s: %q", want, do)
		}
	}
}

// A LINE IS COUNTED IN CHARACTERS, as the model counts it: an about line of
// its cap in two-byte characters is taken, and one more is refused.
//
// Mutation: count bytes and the about line at its cap is refused.
func TestABriefLineIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	call := func(about string) map[string]any {
		return map[string]any{"about": about, "needs": []any{}}
	}
	if _, err := catalog.ParseBrief(call(strings.Repeat("é", wire.BriefAboutMaxRunes))); err != nil {
		t.Errorf("an about line at its cap was refused: %v", err)
	}
	_, err := catalog.ParseBrief(call(strings.Repeat("é", wire.BriefAboutMaxRunes+1)))
	want := fmt.Sprintf("%d characters and the most it takes is %d", wire.BriefAboutMaxRunes+1,
		wire.BriefAboutMaxRunes)
	if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), want) {
		t.Errorf("an about line over its cap answered %v", err)
	}
}

// A REFUSAL NAMES EVERY FAULT, the schema's and the brief's own, in one
// error: the model may write a refused brief again only once.
//
// Mutation: return at the first fault and the later ones go unnamed.
func TestABriefRefusalNamesEveryFault(t *testing.T) {
	t.Parallel()
	_, err := catalog.ParseBrief(map[string]any{
		"about":   strings.Repeat("a", 200),
		"context": strings.Repeat("c", 130),
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-1",
			"do": strings.Repeat("d", 120), "why": strings.Repeat("w", 90), "tone": "warning",
			"action": "open"}},
		"follow_ups": []any{strings.Repeat("q", 170)},
	})
	if !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Fatalf("answered %v, want ErrInvalidArguments", err)
	}
	for _, want := range []string{"about is 200 characters", "context is 130 characters",
		"needs[0].do is 120 characters", "needs[0].why is 90 characters",
		"follow_ups[0] is 170 characters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	_, err = catalog.ParseBrief(map[string]any{"about": "s",
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-1", "do": "Answer it",
			"tone": "warning", "action": "ask"}, map[string]any{"ref_kind": "task",
			"ref_id": "ENG-2", "do": "Open it", "tone": "warning", "action": "open",
			"question": "Why?"}}})
	for _, want := range []string{"needs[0] asks and names no question",
		"needs[1] names a question and its action is open"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}
