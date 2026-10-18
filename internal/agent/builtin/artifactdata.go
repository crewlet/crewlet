package builtin

import "github.com/crewlet/crewlet/internal/tools"

// THE DATASTORE TOOLS: read_artifact_data, query_artifact_data,
// write_artifact_data and set_artifact_schema, each named for its authority
// row, registered from the hooks below, which [Register] and [OperatorTools]
// already append. Every row they return is fenced as text somebody else wrote.

// ArtifactDataDeps are what the datastore tools act through. The zero value
// serves none of them.
type ArtifactDataDeps struct{}

// artifactDataCandidates are the datastore tools a seat's registry may offer.
func artifactDataCandidates(Deps) []candidate { return nil }

// artifactDataOperatorCandidates are the datastore tools the operator catalogue
// may offer.
func artifactDataOperatorCandidates(OperatorDeps) []candidate { return nil }

// artifactDataAnnotations classifies one datastore tool; false for a name that
// is not one.
func artifactDataAnnotations(string) (tools.Annotations, bool) {
	return tools.Annotations{}, false
}
