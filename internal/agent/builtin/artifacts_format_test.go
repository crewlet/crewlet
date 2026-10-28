package builtin_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY KIND A CLOSED FORMAT VALIDATES HAS ITS FORMAT ONE READ AWAY, before an
// artifact of it exists: the answer is the format package's own reference,
// naming the kind's index, and a kind with no closed format is told so.
//
// Mutation: drop a format from formatReferences and its kind answers nothing.
func TestEveryValidatedKindsFormatIsOneReadAway(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	turn := turnFor(t, "agent-cto")
	for _, spec := range artifacts.KindSpecs() {
		got := r.call(t, turn, builtin.ArtifactFormatTool, map[string]any{"kind": string(spec.Kind)})
		if spec.Format == artifacts.FormatNone {
			if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid {
				t.Errorf("%s has no closed format, and was answered %q", spec.Kind, got.Output)
			}
			continue
		}
		if got.Failed {
			t.Errorf("%s's format was refused: %s", spec.Kind, got.Output)
			continue
		}
		index := spec.Entry.Defaults[len(spec.Entry.Defaults)-1]
		if !strings.Contains(got.Output, index) {
			t.Errorf("%s's format does not name its index %s", spec.Kind, index)
		}
	}
	if bad := r.call(t, turn, builtin.ArtifactFormatTool, map[string]any{"kind": "poster"}); !bad.Failed {
		t.Errorf("an unknown kind was answered %q", bad.Output)
	}
}

var briefExample = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// A MODEL GIVEN THE FORMAT WRITES A VALID BRAND KIT FIRST TIME: the brand
// answer carries every key, the one-source rule, the SVG rule and an example,
// and the example is a brand.json the publish's own reader takes as written.
//
// Mutation: answer a hand-written summary that drifts from the reader, and the
// example is refused.
func TestTheBrandFormatAnswerIsEnoughToWriteAKitFirstTime(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.ArtifactFormatTool, map[string]any{"kind": "brand"})
	if got.Failed {
		t.Fatalf("the brand format was refused: %s", got.Output)
	}
	for _, want := range []string{"`voice.write_like`", "`type.body.files[].weight`", "`logos.mark`",
		"EXACTLY ONE of kit, stack and family", "No `<style>` element", "GoPhrase.me"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the brand format does not say %q", want)
		}
	}
	m := briefExample.FindStringSubmatch(got.Output)
	if m == nil {
		t.Fatal("the brand format carries no example")
	}
	if _, err := brandformat.Decode([]byte(m[1])); err != nil {
		t.Errorf("the example the brand format hands an agent is refused: %v", err)
	}
}

// CREATE_ARTIFACT NAMES THE SKILL BY ITS KEY AND THE FORMAT BY ITS TOOL: a
// description that said "the artifacts tool skill" sent a model to
// load_tool_skill("artifacts"), which no catalogue lists.
func TestCreateArtifactNamesTheSkillKeyAndTheFormatTool(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	entry, ok := r.reg.Lookup(builtin.CreateArtifactTool)
	if !ok {
		t.Fatal("create_artifact is not registered")
	}
	desc := entry.Tool.Description()
	for _, want := range []string{"`" + builtin.ArtifactsToolSkillKey + "`", builtin.ArtifactFormatTool} {
		if !strings.Contains(desc, want) {
			t.Errorf("create_artifact's description does not name %s", want)
		}
	}
}

// A POLICY REFUSAL LISTS AT MOST TWENTY FINDINGS, counts the rest, and points
// a closed format's refusal at its reference rather than at another guess.
//
// Mutation: list every finding and the answer grows without bound.
func TestAPolicyRefusalIsBoundedAndPointsAtTheFormat(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	refusal := &lint.Refusal{}
	for i := range builtin.MaxRefusalFindings + 6 {
		refusal.Findings = append(refusal.Findings, lint.Finding{Path: "brand.json", Line: i + 1,
			Code: lint.CodeBrandFormat, Message: fmt.Sprintf("index: finding %d", i)})
	}
	r.f.publishErr = refusal
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, map[string]any{
		"kind": "brand", "title": "GoPhrase brand", "content": "{}"})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Fatalf("a policy refusal answered %q", got.Output)
	}
	if !strings.Contains(got.Output, fmt.Sprintf("finding %d", builtin.MaxRefusalFindings-1)) ||
		strings.Contains(got.Output, fmt.Sprintf("finding %d", builtin.MaxRefusalFindings)) {
		t.Errorf("the refusal does not list exactly the first %d findings:\n%s", builtin.MaxRefusalFindings, got.Output)
	}
	for _, want := range []string{"and 6 more findings", builtin.ArtifactFormatTool + " call (`kind: \"brand\"`)"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got.Output)
		}
	}
}
