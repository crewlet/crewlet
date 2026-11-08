package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CODE MARK IS A PERSON'S OWN, FROM THEIR ASSISTANT: the operator catalogue
// serves code_mark_inbox beside Gitlet's writes — not without them —
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
	host := newGitletHost()
	writes := &codeWrites{}
	wired := codeDeps(host)
	wired.Writer = writes
	served := readersCatalogueOver(wired, nil)
	tool, ok := served[builtin.CodeMarkInboxTool]
	if !ok {
		t.Fatal("the operator catalogue over Gitlet's writes does not serve code_mark_inbox")
	}
	if _, ok := readersCatalogueOver(codeDeps(host), nil)[builtin.CodeMarkInboxTool]; ok {
		t.Error("the operator catalogue served code_mark_inbox over Gitlet with no writes wired")
	}
	reg := codeRegistry(t, wired)
	if _, ok := reg.Lookup(builtin.CodeReadTool); !ok {
		t.Fatalf("a seat's registry over Gitlet holds no %s, so it proves nothing", builtin.CodeReadTool)
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
		t.Fatalf("Gitlet was asked %+v, want one mark of r-1 on founder's own record", marks)
	}
	empty, err := tool.Call(asBoundPerson(), map[string]any{})
	if err != nil || !empty.Failed || tools.RefusalOf(empty) != tools.RefusalInvalid {
		t.Errorf("a gesture marking nothing answered %v %+v, want it refused invalid", err, empty)
	}
	if writes.mu.Lock(); len(writes.marks) != 1 {
		t.Errorf("a refused gesture reached Gitlet: %+v", writes.marks)
	}
	writes.mu.Unlock()
}

// A CODE MARK CARRIES THE CALLER'S FLOOR: `seen`, how far the caller read the
// code log, reaches Gitlet as the mark's Seen — which its decide waits
// for, so a notice the person was shown is never refused by a node not yet at
// it — a call naming none carries the zero position, and a `seen` that is no
// position, or one on another log, is refused before anything is asked.
//
// Mutation: drop `Seen: seen` from the write the tool asks for, and the floor
// never reaches Gitlet, red; accept a position on any log, and
// Worklet's position is asked, red.
func TestACodeMarkCarriesTheCallersFloor(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	writes := &codeWrites{}
	wired := codeDeps(host)
	wired.Writer = writes
	tool, ok := readersCatalogueOver(wired, nil)[builtin.CodeMarkInboxTool]
	if !ok {
		t.Fatal("the operator catalogue over Gitlet's writes does not serve code_mark_inbox")
	}
	floor := codeAt(12)
	got, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-1"}, "seen": floor.String()})
	if err != nil || got.Failed {
		t.Fatalf("a mark carrying its floor answered %v %q", err, got.Output)
	}
	bare, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-2"}})
	if err != nil || bare.Failed {
		t.Fatalf("a mark carrying no floor answered %v %q", err, bare.Output)
	}
	for _, bad := range []string{"not-a-position", "CREWLET_TRACKER_LOG@1:12"} {
		refused, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-3"}, "seen": bad})
		if err != nil || !refused.Failed || tools.RefusalOf(refused) != tools.RefusalInvalid {
			t.Errorf("a mark seen at %q answered %v %+v, want it refused invalid", bad, err, refused)
		}
	}
	writes.mu.Lock()
	marks := slices.Clone(writes.marks)
	writes.mu.Unlock()
	switch {
	case len(marks) != 2:
		t.Fatalf("Gitlet was asked %+v, want the two well-formed marks and no other", marks)
	case marks[0].Seen != floor:
		t.Errorf("the mark seen at %s reached Gitlet seen at %s", floor, marks[0].Seen)
	case !marks[1].Seen.IsZero():
		t.Errorf("the mark naming no floor reached Gitlet seen at %s, want the zero position",
			marks[1].Seen)
	}
}
