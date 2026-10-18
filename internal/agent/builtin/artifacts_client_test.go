package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
)

// A TRANSCRIPT LINKS THE ARTIFACT A PUBLISH WROTE.
//
// A turn's transcript draws a link to the artifact beside every call that
// landed a version: it matches the call by the tool's NAME and reads the
// artifact's id and the version's number out of the answer by two FIELDS, all
// three declared once in `contract/artifacts.ts`. A rename of a tool or of
// either field would leave every publish in every transcript as a bare tool
// call, with nothing on any screen to say the link had gone, so this runs the
// tools and reads their answers the way the dashboard does.
func TestATranscriptLinksTheArtifactAPublishWrote(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	body, err := clientsource.Literal(tree, "ARTIFACT_PUBLISH_TOOLS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	engine := []string{builtin.CreateArtifactTool, builtin.PublishArtifactTool}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard links the artifact off calls named %q and the tools that "+
			"land a version are %q: change ARTIFACT_PUBLISH_TOOLS in contract/artifacts.ts",
			client, engine)
	}
	idField, err := clientsource.Scalar(tree, "ARTIFACT_ANSWER_ID")
	if err != nil {
		t.Fatal(err)
	}
	versionField, err := clientsource.Scalar(tree, "ARTIFACT_ANSWER_VERSION")
	if err != nil {
		t.Fatal(err)
	}

	r := newArtifactRig(t, artifactChart{}, requesters{}, nil)
	read := func(tool string, args map[string]any) (string, float64) {
		t.Helper()
		got := r.call(t, turnFor(t, "agent-cto"), tool, args)
		if got.Failed {
			t.Fatalf("%s failed: %s", tool, got.Output)
		}
		out := decode(t, got)
		id, ok := out[idField].(string)
		if !ok {
			t.Fatalf("%s answers no string under %q, which the transcript reads the "+
				"artifact's id from: %v", tool, idField, out)
		}
		if _, err := artifacts.ParseID(id); err != nil {
			t.Errorf("%s answers %q under %q, which is not an artifact id: %v", tool, id, idField, err)
		}
		version, ok := out[versionField].(float64)
		if !ok || version < 1 {
			t.Errorf("%s answers %v under %q, which the transcript reads the version from",
				tool, out[versionField], versionField)
		}
		return id, version
	}
	created, _ := read(builtin.CreateArtifactTool, map[string]any{
		"kind": "html", "title": "Review sheet", "content": "<p>one</p>"})
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>one</p>"})
	published, _ := read(builtin.PublishArtifactTool, map[string]any{
		"artifact": h.ID.String(), "base_version": 1, "content": "<p>two</p>"})
	if created == "" || published != h.ID.String() {
		t.Errorf("publish answered artifact %q for a publish to %s", published, h.ID)
	}
}
