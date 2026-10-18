package builtin

import "github.com/crewlet/crewlet/internal/tools"

// THE ARTIFACTS TOOLS: one tool per action, each named for its authority
// row (list_artifacts, get_artifact, create_artifact, publish_artifact,
// update_artifact, share_artifact, unshare_artifact, upload_artifact_asset).
// Registered for seats and in the operator catalogue from the hooks below,
// which [Register] and [OperatorTools] already append.
//
// Nothing that takes an artifact out of circulation is a tool: trash, restore,
// purge and transfer are a person's gestures on the HTTP surface, so a
// prompt-injected agent cannot take a team's sheet offline.

// ArtifactDeps are what the artifacts tools act through. The zero value
// serves none of them, which is what a node built without the artifacts
// domain has.
type ArtifactDeps struct{}

// artifactCandidates are the artifacts tools a seat's registry may offer.
func artifactCandidates(Deps) []candidate { return nil }

// artifactOperatorCandidates are the artifacts tools the operator catalogue may
// offer a person and their assistant.
func artifactOperatorCandidates(OperatorDeps) []candidate { return nil }

// artifactAnnotations classifies one artifacts tool for the delivery gate and
// the sub-agent guard; false for a name that is not one.
func artifactAnnotations(string) (tools.Annotations, bool) { return tools.Annotations{}, false }
