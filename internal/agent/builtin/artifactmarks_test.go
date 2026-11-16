package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// A COMMENT MARK IS A PERSON'S OWN: only the operator catalogue serves
// artifact_mark_inbox, a mark carries its gesture and `seen` floor, and a
// floor on another log or a gesture marking nothing is refused unasked.
func TestACommentMarkIsAPersonsOwnFromTheirAssistant(t *testing.T) {
	t.Parallel()
	f := newFakeArtifacts()
	chart := artifactChart{}
	deps := artifactDeps(f, chart, requesters{})
	var tool tools.Callable
	for _, served := range builtin.OperatorTools(builtin.OperatorDeps{Artifacts: deps,
		Authorize: builtin.Decide(chart)}) {
		if served.Name() == builtin.ArtifactMarkInboxTool {
			tool = served
		}
	}
	if tool == nil {
		t.Fatal("the operator catalogue does not serve artifact_mark_inbox")
	}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Artifacts: deps})); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := reg.Lookup(builtin.ArtifactMarkInboxTool); ok {
		t.Error("a seat's registry holds artifact_mark_inbox")
	}
	got, err := tool.Call(asBoundPerson(), map[string]any{"read": []any{"r-1"},
		"seen": "CREWLET_ARTIFACTS_LOG@1:12"})
	if err != nil || got.Failed {
		t.Fatalf("a person's mark answered %v %q", err, got.Output)
	}
	for _, bad := range []map[string]any{{"read": []any{"r-2"}, "seen": "CREWLET_CODE_LOG@1:12"}, {}} {
		refused, err := tool.Call(asBoundPerson(), bad)
		if err != nil || !refused.Failed || tools.RefusalOf(refused) != tools.RefusalInvalid {
			t.Errorf("a mark of %v answered %v %+v, want it refused invalid", bad, err, refused)
		}
	}
	f.mu.Lock()
	marks, calls := slices.Clone(f.marks), slices.Clone(f.calls)
	f.mu.Unlock()
	switch {
	case len(marks) != 1:
		t.Fatalf("the artifacts were asked %+v, want the one well-formed mark", marks)
	case !slices.Equal(marks[0].Gesture.Read, []string{"r-1"}) || marks[0].Seen.String() != "CREWLET_ARTIFACTS_LOG@1:12":
		t.Errorf("the mark reached the artifacts as %+v", marks[0])
	case calls[0].Authority.Actor.Name != "founder" || calls[0].Authority.Actor.Kind != iam.ActorHuman ||
		calls[0].OpKey == "":
		t.Errorf("the mark was made as %+v, want founder's own keyed write", calls[0])
	}
}
