package catalog

// THE CURATED NAMES THAT ARE ALSO AN ENGINE TOOL'S. A curated tool's name is
// the assistant's own word, chosen for the model that calls it: every step and
// proposal a conversation keeps records it, and the eval suites operators write
// name it (docs/reference/cli.md). Most are no engine tool's name at all
// (pause_agent executes as pause_seat); where the word is one, it is still the
// assistant's, and a rename of the engine's tool must not move it. So each such
// word is declared once, here, and every other file of the catalogue reads it:
// no literal elsewhere in the package spells an engine tool's name (a write's
// act tool is read off its verb, see [write]), and internal/toolname's walk,
// which refuses a copy of one, exempts this file alone for these words.
const (
	searchKnowledgeTool = "search_knowledge"
	listProjectsTool    = "list_projects"
	getPageTool         = "get_page"
	listArtifactsTool   = "list_artifacts"
	getArtifactTool     = "get_artifact"
	markInboxTool       = "mark_inbox"
	steerTurnTool       = "steer_turn"
	shareArtifactTool   = "share_artifact"
	unshareArtifactTool = "unshare_artifact"
)
