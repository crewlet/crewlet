package worklet

import "github.com/crewlet/crewlet/internal/toolname"

// Worklet's tool names.
//
// BARE, not prefixed: a prefix costs tokens on every catalogue line for every
// seat and disambiguates nothing, since the registry refuses a duplicate and
// reports an operator's MCP tool of the same name rather than let it shadow one.
//
// They live in the DOMAIN because the builtin surface implements them and this
// package's notification prompt names them, and neither may import the other:
// a prompt naming a tool this build does not register sends a seat to call
// something that is not there.
const (
	ListWorkItemsTool  = "list_work_items"
	GetWorkItemTool    = toolname.GetWorkItem
	CreateWorkItemTool = toolname.CreateWorkItem
	UpdateWorkItemTool = toolname.UpdateWorkItem
	CommentOnWorkTool  = toolname.CommentOnWorkItem
	WithdrawAskTool    = toolname.WithdrawAsk

	// MergeWorkItemTool folds one item into another. A SEAT's, though the
	// trash tools are not: a merge leaves the item visible (cancelled,
	// linked to the survivor, a history row naming who folded it) where a
	// removal leaves an absence nobody reads. A seat can already cancel and
	// draw the `duplicates` link with `update_work_item`; what it cannot do
	// by hand is move the children, so withholding this verb only
	// guarantees the orphans. Deciding two items are one is triage: work.
	MergeWorkItemTool = "merge_work_item"

	// MoveWorkItemTool moves a top-level item, and everything under it,
	// to another project: a seat's, behind a re-route's gate (the lead of
	// the item's project, or a person). It re-keys what it carries, so it
	// could not be a patch field, and it is the one gesture that clears a
	// subtask filed in another project than its root.
	MoveWorkItemTool = "move_work_item"

	// SearchWorkItemsTool finds an item by what it SAYS, ranked over every
	// item's title and description. Its own verb beside
	// [ListWorkItemsTool]: a board narrows a list and keeps the board's
	// order, this ranks a corpus so its answer IS the order, and the
	// board's `q` matches the key or title and never sees a description.
	SearchWorkItemsTool = "search_work_items"

	// The PROJECT reads are a seat's for [GetWorkCatalogueTool]'s reason:
	// a create refuses an unknown project, an undeclared type and an empty
	// required field, and a model that cannot read them can only guess.
	// No companion WRITES: a project's name, purpose and owning unit are
	// chart-owned (only the epoch apply writes them), and its field
	// declarations are a lead's.
	ListProjectsTool    = "list_projects"
	DescribeProjectTool = "describe_project"

	// WriteProjectTool is the one project WRITE a seat holds, for
	// `tags.add`: declaring a tag is open to every seat by design, and a
	// create refuses an undeclared label, so without it a seat could never
	// use the `labels` argument it does hold. Every other facet is gated
	// inside on the project's lead or an overriding admin (archiving also
	// on a principal that is not an agent), and the refusals name which.
	WriteProjectTool = toolname.WriteProject

	// TaskActivityTool is what HAPPENED, which no board answers: a board
	// is what is there now, and who moved this, when it stopped being
	// blocked or what a merge did are questions for the ordered log.
	TaskActivityTool = "task_activity"

	// MyWorkTool is the one call a turn opens with: seven lists in one
	// answer, because as seven calls a task could show in `assigned` after
	// moving out of it by the time `priorities` was read.
	MyWorkTool = "my_work"

	// The FILE tools: a project's shared files, the work's own artefacts
	// (a report, a spec, a dataset). A seat holds all four: one that could
	// write a report but never replace or tidy one fills the project with
	// `report-final-2.md`. Every write names its author in the file's
	// history, so none is invisible the way a removed task is.
	ListProjectFilesTool  = "list_project_files"
	ReadProjectFileTool   = "read_project_file"
	WriteProjectFileTool  = "write_project_file"
	RemoveProjectFileTool = "remove_project_file"
)

// The OPERATOR-ONLY tools, which no seat is given: a saved view is a person's
// own tab strip, and an agent's job is the WORK, not the furniture around it.
// A seat that could rearrange a shared board is one more thing a founder
// supervises, for no delivery.
const (
	ListWorkViewsTool  = "list_work_views"
	SaveWorkViewTool   = "save_work_view"
	DeleteWorkViewTool = "delete_work_view"

	// WriteWorkCatalogueTool writes the company's VOCABULARY, what a task
	// may be and carry. A seat adding a type to make its own create pass
	// edits the rules it is judged by, and hides the refusal a person
	// needed to see.
	WriteWorkCatalogueTool = "write_work_catalogue"

	// The PERSON tools write a person's inbox, queue and pins on their
	// behalf, and the operator surface is the one place a person acts
	// through their own credential: a seat is not a human, with no inbox.
	GetPersonTool     = "get_person"
	SetPrioritiesTool = "set_priorities"
	SetPinsTool       = "set_pins"
	MarkInboxTool     = "mark_inbox"

	// WorkInboxTool is the inbox's FEED, written by the applier as each
	// change landed; `get_person` reads the person's own MARKS over it
	// (read, snoozed, how far). Separate verbs because the common question
	// is one or the other, and one verb would read both on every call.
	WorkInboxTool = "work_inbox"

	// The TRASH tools: a removal hides a task from every list, so a seat
	// that could remove work it did not want would mark its own homework
	// where nobody notices, and a restore is its inverse. Neither is a
	// facet of `update_work_item`, which refuses a removed task outright:
	// a freeze an ordinary field write could lift is no freeze.
	RemoveWorkItemTool  = "remove_work_item"
	RestoreWorkItemTool = "restore_work_item"

	// PlaceWorkItemTool is a board DRAG: a card dropped beside another, in
	// its own lane or the next. The order is furniture a person arranges,
	// a person's for the view tools' reason; the lane half is the status
	// change every seat makes with `update_work_item`. Its own verb beside
	// [MoveWorkItemTool], which differs in every registered property: a
	// placement is a person's and idempotent (the same drop finds the card
	// there), a move a seat's behind a lead's gate, a delivery, and
	// refused when made again under a new operation since it re-keys.
	PlaceWorkItemTool = "place_work_item"
)

// GetWorkCatalogueTool is the one catalogue verb a SEAT holds: a model that
// cannot read the catalogue guesses at a type, which is how `Bug`, `bugfix` and
// `BUG` come to sit beside `bug`.
const GetWorkCatalogueTool = "get_work_catalogue"

// OperatorOnlyTools are the ones the operator surface adds to [Tools].
func OperatorOnlyTools() []string {
	return []string{
		ListWorkViewsTool, SaveWorkViewTool, DeleteWorkViewTool,
		WriteWorkCatalogueTool,
		GetPersonTool, SetPrioritiesTool, SetPinsTool, MarkInboxTool,
		WorkInboxTool,
		RemoveWorkItemTool, RestoreWorkItemTool,
		PlaceWorkItemTool,
	}
}

// Tools are the ones a seat holds, so a caller registering them names one
// thing.
func Tools() []string {
	return []string{ListWorkItemsTool, GetWorkItemTool, CreateWorkItemTool,
		UpdateWorkItemTool, CommentOnWorkTool, MergeWorkItemTool, MoveWorkItemTool,
		SearchWorkItemsTool, GetWorkCatalogueTool, ListProjectsTool, DescribeProjectTool,
		WriteProjectTool,
		TaskActivityTool, MyWorkTool,
		ListProjectFilesTool, ReadProjectFileTool, WriteProjectFileTool,
		RemoveProjectFileTool}
}

// WriteTools are the five that count as a DELIVERY: a turn woken by an
// assignment answers by updating, commenting, filing follow-up work, merging a
// duplicate or moving a misfiled item (whose root's move wakes the people on
// it), and the delivery gate must know that or it loops such a turn for having
// "done nothing". Get and list are absent: a turn that only read is the one the
// gate exists to catch.
func WriteTools() []string {
	return []string{CreateWorkItemTool, UpdateWorkItemTool, CommentOnWorkTool,
		MergeWorkItemTool, MoveWorkItemTool}
}
