package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WRITE ON ONE ARTIFACT OR ONE WORK ITEM NAMES IT to the refusal-loop guard,
// so a run of refusals is counted per target (ADR-0059); create_artifact, which
// has none yet, is counted per tool.
//
// Mutation: drop the embedding from publish_artifact and its refusals across
// artifacts count as one run.
func TestAWriteNamesTheTargetItsRefusalsAreCountedOn(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	for name, args := range map[string]map[string]any{
		builtin.PublishArtifactTool:     {"artifact": "a1"},
		builtin.UpdateArtifactTool:      {"artifact": "a1"},
		builtin.UploadArtifactAssetTool: {"artifact": "a1"},
		builtin.ShareArtifactTool:       {"artifact": "a1"},
	} {
		entry, ok := r.reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if got := tools.TargetOf(entry.Tool, args); got != "artifact a1" {
			t.Errorf("%s names %q", name, got)
		}
		if got := tools.TargetOf(entry.Tool, map[string]any{}); got != "" {
			t.Errorf("%s names %q for a call naming no artifact", name, got)
		}
	}
	entry, ok := r.reg.Lookup(builtin.CreateArtifactTool)
	if !ok {
		t.Fatal("create_artifact is not registered")
	}
	if tools.TargetOf(entry.Tool, map[string]any{"title": "x"}) != "" {
		t.Error("create_artifact names a target before the artifact exists")
	}
}
