package artifactsapi_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD SPEAKS THE ARTIFACTS' VOCABULARY, IN THE ENGINE'S ORDER.
//
// The gallery filters by kind and scope and sorts by what the listing sorts
// by, the share dialog grants the roles a share may hold to the audiences a
// share may name, the viewer embeds a frame by the origin mode the engine
// minted it in, and the presence chrome draws what a member is doing. Every
// one of those is a closed set the engine owns, so `contract/artifacts.ts`
// declares each once and this holds it: a value the engine grew that the
// dashboard lacks is a kind it cannot draw or a role nobody can grant; one the
// dashboard keeps that the engine dropped is a choice every write refuses.
// The ORDER is held too, because the role ladder is an order (each includes
// the ones before it) and the dashboard draws the others in the engine's.
func TestTheDashboardSpeaksTheArtifactsVocabulary(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string][]string{
		"ARTIFACT_KINDS":        names(artifacts.Kinds),
		"ARTIFACT_CAPABILITIES": names(artifacts.Capabilities),
		"ARTIFACT_ROLES":        names(artifacts.Roles),
		"SHAREABLE_ROLES":       names(artifacts.ShareableRoles),
		"AUDIENCE_KINDS":        names(artifacts.AudienceKinds),
		"ORIGIN_MODES":          names(artifacts.OriginModes),
		"PRESENCE_ACTIVITIES":   names(artifacts.PresenceActivities),
		"ARTIFACT_LIST_SCOPES":  names(artifactsdomain.ListScopes),
		"ARTIFACT_LIST_SORTS":   names(artifactsdomain.ListSorts),
	} {
		body, err := clientsource.Literal(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		client := clientsource.Strings(body)
		if len(client) == 0 {
			t.Fatalf("%s names nothing, so this gate certifies nothing", name)
		}
		if !slices.Equal(client, engine) {
			t.Errorf("the dashboard's %s is %q and the engine's is %q, in that order: "+
				"change contract/artifacts.ts to the engine's", name, client, engine)
		}
	}
}

// THE DASHBOARD DRAWS EACH KIND AS THE KIND TABLE SAYS.
//
// The View tab, the side-by-side compare and the Data tab ask the kind's row
// rather than its name, so a row that disagreed with the engine's would frame
// a native kind the origin refuses to serve, draw a framed one here, or hide a
// datastore the engine keeps. Every kind is a row, in the engine's order, and
// each row's render and store are the engine's.
func TestTheDashboardDrawsEachKindAsTheKindTableSays(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ARTIFACT_KIND_SPECS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, names(artifacts.Kinds)) {
		t.Fatalf("ARTIFACT_KIND_SPECS is keyed %q and the engine's kinds are %q, in that order",
			keys, names(artifacts.Kinds))
	}
	for _, spec := range artifacts.KindSpecs() {
		row, err := clientsource.Property(body, string(spec.Kind))
		if err != nil {
			t.Fatal(err)
		}
		for field, engine := range map[string]string{
			"render": string(spec.Render), "store": string(spec.Store),
		} {
			if got := clientsource.Field(row, field); !slices.Equal(got, []string{engine}) {
				t.Errorf("ARTIFACT_KIND_SPECS.%s.%s is %q and the engine's is %q",
					spec.Kind, field, got, engine)
			}
		}
	}
}

// THE DASHBOARD BOUNDS WHAT A PERSON TYPES, AND WHAT IT TELLS A ROOM, AT THE
// ENGINE'S CAPS.
//
// The settings form and the version label dialog stop a title, a description,
// an icon word or a label at these, so a person is told at the field rather
// than refused after Save; a bound looser than the engine's is a form that
// accepts what the engine will refuse. And the presence chrome sends at most
// as many frames a second as a socket's room takes: the engine drops the rest,
// so a faster client spends its own CPU telling the room nothing.
func TestTheDashboardBoundsArtifactTextAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"ARTIFACT_TITLE_MAX_BYTES":       artifacts.MaxTitleBytes,
		"ARTIFACT_DESCRIPTION_MAX_BYTES": artifacts.MaxDescriptionBytes,
		"ARTIFACT_ICON_MAX_BYTES":        artifacts.MaxIconBytes,
		"ARTIFACT_LABEL_MAX_BYTES":       artifacts.MaxLabelBytes,
		"PRESENCE_FRAMES_PER_SECOND":     artifacts.PresenceClientRate,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d and the engine at %d: change "+
				"contract/artifacts.ts to the engine's figure", name, client, engine)
		}
	}
}

// names is a closed set's values as the strings the dashboard declares.
func names[S ~string](set []S) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}
