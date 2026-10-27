package catalog_test

import (
	"errors"
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
