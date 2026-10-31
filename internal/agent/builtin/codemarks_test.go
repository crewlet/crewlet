package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CODE MARK IS A PERSON'S OWN, FROM THEIR ASSISTANT: the operator catalogue
// serves code_mark_inbox beside the code host's writes — not without them —
// no seat's registry holds it, and a person's call is one gesture on the
// record of the human seat they hold, named by the arguments' record ids and
// never by a handle the call could name — the tool takes none, and refuses one
// as it refuses every argument it does not read; a gesture that marks nothing
// is refused before anything is asked.
//
// Mutations: add code_mark_inbox to CodeTools, and a seat's registry holds it;
// offer it over the code reads alone, and a surface that changes no code marks
// it.
func TestACodeMarkIsAPersonsOwnFromTheirAssistant(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	writes := &codeWrites{}
	wired := codeDeps(host)
	wired.Writer = writes
	served := readersCatalogueOver(wired, nil)
	tool, ok := served[builtin.CodeMarkInboxTool]
	if !ok {
		t.Fatal("the operator catalogue over the code host's writes does not serve code_mark_inbox")
	}
	if _, ok := readersCatalogueOver(codeDeps(host), nil)[builtin.CodeMarkInboxTool]; ok {
		t.Error("the operator catalogue served code_mark_inbox over a code host that takes no write")
	}
	reg := codeRegistry(t, wired)
	if _, ok := reg.Lookup(builtin.CodeReadTool); !ok {
		t.Fatalf("a seat's registry over the code host holds no %s, so it proves nothing", builtin.CodeReadTool)
	}
	if _, ok := reg.Lookup(builtin.CodeMarkInboxTool); ok {
		t.Error("a seat's registry holds code_mark_inbox")
	}

	if named, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-1"}, "handle": "cto"}); err != nil ||
		!named.Failed {
		t.Errorf("a mark naming whose record answered %v %+v, want it refused", err, named)
	}
	got, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-1"}})
	if err != nil || got.Failed {
		t.Fatalf("a person's mark answered %v %q", err, got.Output)
	}
	writes.mu.Lock()
	marks := writes.marks
	writes.mu.Unlock()
	if len(marks) != 1 || marks[0].Handle != "founder" || marks[0].Asker.Seat != "founder" ||
		marks[0].Asker.Kind != code.PrincipalOperator || len(marks[0].Gesture.Read) != 1 ||
		marks[0].Gesture.Read[0] != "r-1" || marks[0].OpID == "" {
		t.Fatalf("the code host was asked %+v, want one mark of r-1 on founder's own record", marks)
	}
	empty, err := tool.Call(asBoundPerson(), map[string]any{})
	if err != nil || !empty.Failed || tools.RefusalOf(empty) != tools.RefusalInvalid {
		t.Errorf("a gesture marking nothing answered %v %+v, want it refused invalid", err, empty)
	}
	if writes.mu.Lock(); len(writes.marks) != 1 {
		t.Errorf("a refused gesture reached the code host: %+v", writes.marks)
	}
	writes.mu.Unlock()
}
