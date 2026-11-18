package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- get_artifact_format ------------------------------------------------ //

// ArtifactsToolSkillKey is the key of the tool skill that teaches the artifact
// design contract (examples/tool-skills/artifacts.md). create_artifact's
// description names the key, not the words: "artifacts" is no catalogue's key.
const ArtifactsToolSkillKey = "skill:artifacts"

// artifactFormat answers the exact format a kind's files are validated
// against at publish, so an agent reads it before writing rather than learning
// it from refusals. Each reference is built by its format package from its
// strict decode's form and its rules' constants ([brandformat.Reference]), so
// what an agent is told and what a publish admits cannot drift.
type artifactFormat struct{}

var _ tools.SeatCallable = (*artifactFormat)(nil)

// formatReferences are the references by format: every format the kind table
// validates except [artifacts.FormatNone].
var formatReferences = map[artifacts.Format]func() string{
	artifacts.FormatBrand:     brandformat.Reference,
	artifacts.FormatSlides:    slideformat.Reference,
	artifacts.FormatDoc:       docindex.Reference,
	artifacts.FormatDesign:    canvasformat.Reference,
	artifacts.FormatDashboard: dashspec.Reference,
}

// formatKinds are the kinds whose files a closed format validates, in the
// kind table's order.
func formatKinds() []string {
	var out []string
	for _, s := range artifacts.KindSpecs() {
		if _, ok := formatReferences[s.Format]; ok {
			out = append(out, string(s.Kind))
		}
	}
	return out
}

func (t *artifactFormat) Name() string { return ArtifactFormatTool }

func (t *artifactFormat) Description() string {
	return "Read the exact format a kind's files are checked against at publish: every key " +
		"of its index (`brand.json`, `deck.json`, `canvas.json`, `dashboard.json`, " +
		"`doc.json`) with its type and rule, the bounds and closed sets, and for a brand " +
		"kit an example that reads clean. Read it BEFORE you create or publish a " +
		strings.Join(formatKinds(), ", ") + " artifact, rather than learning the format " +
		"from refusals."
}

func (t *artifactFormat) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string", "enum": stringsToAny(formatKinds()),
				"description": "The artifact kind whose format to read.",
			},
		},
		"required": []any{"kind"},
	}
}

func (t *artifactFormat) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *artifactFormat) CallForTurn(_ context.Context, _ *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	kind := artifacts.Kind(strings.TrimSpace(argString(args, "kind")))
	spec, ok := kind.Spec()
	if !ok {
		return refused(tools.RefusalInvalid, fmt.Sprintf("%s takes `kind` as one of %s.",
			ArtifactFormatTool, words.Or(formatKinds()))), nil
	}
	reference, ok := formatReferences[spec.Format]
	if !ok {
		return refused(tools.RefusalInvalid, fmt.Sprintf("A %s artifact has no closed format: "+
			"%s. The kinds with one are %s.", kind, spec.Summary, words.And(formatKinds()))), nil
	}
	return tools.Result{Output: reference()}, nil
}

// formatHint is the sentence a refusal of a kind's format ends with, pointing
// at the reference; "" for a kind with no reference.
func formatHint(kind artifacts.Kind) string {
	spec, ok := kind.Spec()
	if !ok {
		return ""
	}
	if _, ok := formatReferences[spec.Format]; !ok {
		return ""
	}
	return fmt.Sprintf("\nThe whole %s format, every key with its type and rule, is one "+
		"%s call (`kind: %q`) away: read it before the next attempt rather than guessing.",
		kind, ArtifactFormatTool, kind)
}

// MaxRefusalFindings bounds the findings one policy refusal lists, so a
// version far outside its format still fits a tool answer; the rest are
// counted, and surface on the next publish.
const MaxRefusalFindings = 20

// formatCodes maps each closed format's policy code to its kind.
var formatCodes = map[lint.Code]artifacts.Kind{
	lint.CodeBrandFormat:     artifacts.KindBrand,
	lint.CodeSlideFormat:     artifacts.KindSlides,
	lint.CodeDocFormat:       artifacts.KindDoc,
	lint.CodeDashboardFormat: artifacts.KindDashboard,
	lint.CodeDesignFormat:    artifacts.KindDesign,
}

// policyFindings is a policy refusal's findings as a model reads them: a line
// each, at most [MaxRefusalFindings] plus the rest's count, and a closed
// format's [formatHint].
func policyFindings(findings []lint.Finding) string {
	lines := make([]string, 0, min(len(findings), MaxRefusalFindings)+2)
	hinted := map[artifacts.Kind]bool{}
	for i, f := range findings {
		if i < MaxRefusalFindings {
			lines = append(lines, "- "+f.String())
		}
		if kind, ok := formatCodes[f.Code]; ok {
			hinted[kind] = true
		}
	}
	if rest := len(findings) - MaxRefusalFindings; rest > 0 {
		lines = append(lines, fmt.Sprintf("- and %d more findings: fix these and publish "+
			"again to see the rest", rest))
	}
	out := strings.Join(lines, "\n")
	for _, kind := range artifacts.Kinds {
		if hinted[kind] {
			out += formatHint(kind)
		}
	}
	return out
}
