package builtin_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// The two skills that teach the artifact design contract: the bundled Tool
// Skill a seat loads, and the founder-side skill an operator's own assistant
// reads over /operator/mcp.
const (
	artifactToolSkill    = "examples/tool-skills/artifacts.md"
	artifactFounderSkill = "skills/crewlet-artifacts/SKILL.md"
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// contractOf is the contract section of a skill: the text between its two
// markers.
func contractOf(t *testing.T, rel, text string) string {
	t.Helper()
	const begin, end = "<!-- contract:begin -->", "<!-- contract:end -->"
	i, j := strings.Index(text, begin), strings.Index(text, end)
	if i < 0 || j < i {
		t.Fatalf("%s carries no contract section between %q and %q", rel, begin, end)
	}
	return text[i : j+len(end)]
}

// ONE CONTRACT, TWO READERS: what a seat is taught before it publishes and
// what an operator's assistant reads cannot drift apart, so the contract
// section is held byte for byte equal across the two skills.
func TestTheArtifactSkillsTeachOneContract(t *testing.T) {
	t.Parallel()
	seat, founder := readRepoFile(t, artifactToolSkill), readRepoFile(t, artifactFounderSkill)
	if contractOf(t, artifactToolSkill, seat) != contractOf(t, artifactFounderSkill, founder) {
		t.Fatalf("the contract sections of %s and %s differ: edit one, copy it to the other",
			artifactToolSkill, artifactFounderSkill)
	}
}

// THE TOOL SKILL IS LOADED BEFORE THE FIRST BYTE IS PUBLISHED OR WRITTEN: it is
// enforced, and its trigger covers every tool that writes an artifact's bytes
// or its datastore's schema or rows, and no read (a read never waits on the
// design contract).
func TestTheArtifactToolSkillGuardsEveryPublishingTool(t *testing.T) {
	t.Parallel()
	s, err := skills.Parse(readRepoFile(t, artifactToolSkill), skills.Source{PageID: "artifacts.md"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Key != builtin.ArtifactsToolSkillKey || !s.Required {
		t.Fatalf("key %q, required %v: want an enforced skill:artifacts", s.Key, s.Required)
	}
	for _, name := range []string{builtin.CreateArtifactTool, builtin.PublishArtifactTool,
		builtin.UploadArtifactAssetTool, builtin.SetArtifactSchemaTool, builtin.WriteArtifactDataTool} {
		if !s.Trigger.Covers(name, "") {
			t.Errorf("the trigger does not cover %s", name)
		}
	}
	for _, name := range []string{builtin.ListArtifactsTool, builtin.GetArtifactTool,
		builtin.ReadArtifactDataTool, builtin.QueryArtifactDataTool} {
		if s.Trigger.Covers(name, "") {
			t.Errorf("the trigger covers the read %s, which would make every read wait", name)
		}
	}
}

// EVERY ARTIFACT TOOL A SKILL NAMES IS ONE THIS BUILD REGISTERS: a renamed tool
// would leave both skills teaching a call nothing answers.
func TestTheArtifactSkillsNameOnlyRegisteredTools(t *testing.T) {
	t.Parallel()
	named := regexp.MustCompile("`([a-z_]*artifacts?[a-z_]*)`")
	for _, rel := range []string{artifactToolSkill, artifactFounderSkill} {
		for _, m := range named.FindAllStringSubmatch(readRepoFile(t, rel), -1) {
			if !slices.Contains(builtin.ArtifactTools(), m[1]) &&
				!slices.Contains(builtin.ArtifactDataTools(), m[1]) {
				t.Errorf("%s names `%s`, which is no artifacts tool", rel, m[1])
			}
		}
	}
}

// A SEAT IS TAUGHT EVERY SOURCE THE INLINE BOUND SENDS IT TO. Past the bound a
// publish is refused, naming the sources that carry a larger build; a seat
// that learned one only from that refusal would have spent the call and the
// bytes it carried. So each source the refusal names is in the Tool Skill a
// seat loads before its first publish. Mutation: drop `from_sandbox` from the
// skill's contract and this fails.
func TestASeatIsTaughtTheSourcesTheInlineBoundNames(t *testing.T) {
	t.Parallel()
	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, map[string]any{
		"kind": "html", "title": "Big", "content": strings.Repeat("a", int(artifacts.MaxInlineBytes+1))})
	named := regexp.MustCompile("`(from_[a-z_]+)`").FindAllStringSubmatch(got.Output, -1)
	if !got.Failed || len(named) == 0 {
		t.Fatalf("an over-bound inline publish answered %q, want a refusal naming its sources", got.Output)
	}
	seat := readRepoFile(t, artifactToolSkill)
	for _, m := range named {
		if !strings.Contains(seat, "`"+m[1]+"`") {
			t.Errorf("%s does not teach `%s`, which the inline bound's refusal sends a seat to",
				artifactToolSkill, m[1])
		}
	}
}
