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
// say (an ask names its question and nothing else does, an open
// recommendation names its record, every line is one line). Mutation: drop a
// rule and its case is accepted.
func TestABriefCallIsReadStrictly(t *testing.T) {
	t.Parallel()
	need := func(over map[string]any) map[string]any {
		n := map[string]any{"ref_kind": "task", "ref_id": "ENG-1", "text": "QA asks",
			"tone": "warning", "action": "open"}
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
		return map[string]any{"summary": "1 decision waits.", "needs": list, "follow_ups": []any{}}
	}
	d, err := catalog.ParseBrief(call(need(nil)))
	if err != nil {
		t.Fatalf("a valid call was refused: %v", err)
	}
	if len(d.Needs) != 1 || d.Needs[0].Ref != (wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-1"}) ||
		d.Needs[0].Action != catalog.BriefActionOpen {
		t.Errorf("draft = %+v", d)
	}
	refused := map[string]map[string]any{
		"no summary":          {"summary": "", "needs": []any{}, "follow_ups": []any{}},
		"an ask, no question": call(need(map[string]any{"action": "ask"})),
		"a question, no ask":  call(need(map[string]any{"question": "Why?"})),
		"two lines":           call(need(map[string]any{"text": "one\ntwo"})),
		"a kind not cited":    call(need(map[string]any{"ref_kind": "unit"})),
		"four rows":           call(need(nil), need(nil), need(nil), need(nil)),
		"an identity":         {"summary": "s", "needs": []any{}, "follow_ups": []any{}, "actor": "ada"},
		"an open step, no record": {"summary": "s", "needs": []any{}, "follow_ups": []any{},
			"recommended": map[string]any{"text": "Do it", "action": "open"}},
	}
	for name, args := range refused {
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
	step := props["recommended"].(map[string]any)["properties"].(map[string]any)
	lines := map[string]struct {
		schema map[string]any
		max    int
	}{
		"summary":              {props["summary"].(map[string]any), wire.BriefSummaryMaxRunes},
		"needs.text":           {need["text"].(map[string]any), wire.BriefTextMaxRunes},
		"needs.detail":         {need["detail"].(map[string]any), wire.BriefTextMaxRunes},
		"needs.question":       {need["question"].(map[string]any), wire.BriefQuestionMaxRunes},
		"recommended.text":     {step["text"].(map[string]any), wire.BriefTextMaxRunes},
		"recommended.question": {step["question"].(map[string]any), wire.BriefQuestionMaxRunes},
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

// A LINE IS COUNTED IN CHARACTERS, as the model counts it: a summary of 200
// two-byte characters is taken, and one of 201 is refused.
//
// Mutation: count bytes and the 200 character summary is refused.
func TestABriefLineIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	call := func(summary string) map[string]any {
		return map[string]any{"summary": summary, "needs": []any{}, "follow_ups": []any{}}
	}
	if _, err := catalog.ParseBrief(call(strings.Repeat("é", wire.BriefSummaryMaxRunes))); err != nil {
		t.Errorf("a summary at its cap was refused: %v", err)
	}
	_, err := catalog.ParseBrief(call(strings.Repeat("é", wire.BriefSummaryMaxRunes+1)))
	if !errors.Is(err, catalog.ErrInvalidArguments) ||
		!strings.Contains(err.Error(), "201 characters and the most it takes is 200") {
		t.Errorf("a summary over its cap answered %v", err)
	}
}

// A REFUSAL NAMES EVERY FAULT, the schema's and the brief's own, in one
// error: the model may write a refused brief again only once.
//
// Mutation: return at the first fault and the later ones go unnamed.
func TestABriefRefusalNamesEveryFault(t *testing.T) {
	t.Parallel()
	_, err := catalog.ParseBrief(map[string]any{
		"summary": strings.Repeat("s", 366),
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-1",
			"text": strings.Repeat("t", 130), "tone": "warning", "action": "open"}},
		"follow_ups": []any{strings.Repeat("q", 170)},
	})
	if !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Fatalf("answered %v, want ErrInvalidArguments", err)
	}
	for _, want := range []string{"summary is 366 characters", "needs[0].text is 130 characters",
		"follow_ups[0] is 170 characters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	_, err = catalog.ParseBrief(map[string]any{"summary": "s", "follow_ups": []any{},
		"needs": []any{map[string]any{"ref_kind": "task", "ref_id": "ENG-1", "text": "t",
			"tone": "warning", "action": "ask"}},
		"recommended": map[string]any{"text": "Do it", "action": "open"}})
	for _, want := range []string{"needs[0] asks and names no question",
		"recommended opens a record and names no ref_kind"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}
