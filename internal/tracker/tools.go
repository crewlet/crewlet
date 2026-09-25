package tracker

// The native tracker's tool names.
//
// BARE, not prefixed. A first-party tool may be named in a prompt — this build
// registers it under a name this build chose — and a prefix would cost tokens
// on every catalogue line for every seat to disambiguate from nothing. The
// registry refuses a duplicate, so an operator's MCP server shipping a tool by
// one of these names is reported rather than silently shadowing it.
//
// # Why they live in the DOMAIN rather than beside the implementations
//
// Two packages need them and neither may import the other: the builtin surface
// implements the tools, and this package's own notification prompt NAMES them —
// it tells a woken seat which tool to reach for, and a prompt that named a tool
// this build does not register is a seat instructed to call something that is
// not there. The domain owns its vocabulary.
const (
	ListWorkItemsTool  = "list_work_items"
	GetWorkItemTool    = "get_work_item"
	CreateWorkItemTool = "create_work_item"
	UpdateWorkItemTool = "update_work_item"
	CommentOnWorkTool  = "comment_on_work_item"

	// MergeWorkItemTool folds one item into another, and it is a SEAT's
	// for the reason the trash tools are not.
	//
	// The trash's argument is about INVISIBILITY — a removal takes an item
	// off every board and leaves an absence nobody reads — and none of it
	// applies here. A merge leaves the item exactly where it was, cancelled,
	// linked to the one that survives, with a history row naming who folded
	// it; every part of that is something a person sees.
	//
	// And a seat can already do the damage this verb is careful about. It
	// can cancel the item and it can draw the `duplicates` link, both with
	// `update_work_item`, and what it CANNOT do by hand is move the
	// children — so withholding this verb does not prevent the gesture, it
	// only guarantees the orphans. Deciding that two items are one is
	// triage, which is the work itself rather than furniture around it.
	MergeWorkItemTool = "merge_work_item"

	// SearchWorkItemsTool finds an item by what it SAYS, ranked over every
	// item's title and description.
	//
	// ITS OWN VERB beside [ListWorkItemsTool], because the two answer
	// differently shaped questions: a board narrows a list and keeps the
	// board's order, and this ranks a corpus so its answer IS the order.
	// The board's own `q` is a substring of the key or the title and
	// cannot see a description at all — so the whole of what somebody
	// wrote down about a piece of work was unreachable from a seat, while
	// the engine had been paying to embed every one of those descriptions.
	SearchWorkItemsTool = "search_work_items"

	// The PROJECT reads, and they are a seat's for the reason
	// [GetWorkCatalogueTool] is: a create refuses a project the company
	// does not have, a type it has not declared and a required field left
	// empty, and a model that cannot READ any of that can only guess.
	//
	// There is no companion that WRITES. A project's name, purpose and
	// owning unit are chart-owned — written by the epoch apply and by
	// nothing else — so a seat editing them would be editing the company's
	// structure through the back door; its field declarations are a
	// lead's.
	ListProjectsTool    = "list_projects"
	DescribeProjectTool = "describe_project"

	// WriteProjectTool is the one project WRITE a seat holds, and it is
	// held for one facet: `tags.add`. A tag is how work is grouped for a
	// week, declaring one is open to every seat by design, and a create
	// refuses a label the project has not declared — so a seat without
	// this verb could never use the `labels` argument on the tools it does
	// hold. Every other facet it carries is gated inside, on the project's
	// lead or on a person's own credential, and the refusals name which.
	WriteProjectTool = "write_project"

	// TaskActivityTool is what HAPPENED, which no board can answer: a
	// board is about what is there now, and every question about a change
	// — who moved this, when did it stop being blocked, what did that
	// bulk edit do — is about the ordered log instead.
	TaskActivityTool = "task_activity"

	// MyWorkTool is the one call a turn opens with. Seven lists in one
	// answer rather than seven calls, because assembled separately a seat
	// could see a task in `assigned` that had already moved out of it by
	// the time `priorities` was read.
	MyWorkTool = "my_work"
)

// The OPERATOR-ONLY tools, which no seat is given.
//
// A saved view is a person arranging their own tab strip, and the five above
// are deliberately few because an agent's job is the WORK rather than the
// furniture around it: a seat that could rearrange a shared board would be one
// more thing a founder has to supervise, for no delivery.
const (
	ListWorkViewsTool = "list_work_views"
	SaveWorkViewTool  = "save_work_view"

	// WriteWorkCatalogueTool is the company's own VOCABULARY — what a
	// task may be and what it may carry. A seat adding a type to make its
	// own create succeed is a seat editing the rules it is judged by, and
	// the refusal it was working around is the signal a person needs.
	WriteWorkCatalogueTool = "write_work_catalogue"

	// The PERSON tools. A person's inbox, queue and pins are written on
	// behalf of the person whose they are, and the operator surface is the
	// one place a person acts through their own credential — a seat's
	// registry has a SEAT, which is not a human and has no inbox.
	GetPersonTool     = "get_person"
	SetPrioritiesTool = "set_priorities"
	SetPinsTool       = "set_pins"
	MarkInboxTool     = "mark_inbox"

	// WorkInboxTool is the other half of the inbox, and the two are
	// deliberately separate verbs: `get_person` is somebody's own MARKS
	// over the feed — what they read, what they snoozed, how far they got
	// — and this is the FEED, written by the applier when each change
	// landed. A single verb would have to read both on every call, and
	// the common question is one or the other.
	WorkInboxTool = "work_inbox"

	// The TRASH tools. A removal hides a task from every list in the
	// company, and a seat that could hide work it did not want to do would
	// be marking its own homework in the one way nobody notices — the
	// board simply has one fewer item on it. A restore is its inverse and
	// is an operator's for the same reason, which is also why neither is a
	// facet of `update_work_item`: that verb refuses a removed task
	// outright, and a freeze somebody can lift with an ordinary field
	// write is not a freeze.
	RemoveWorkItemTool  = "remove_work_item"
	RestoreWorkItemTool = "restore_work_item"

	// MoveWorkItemTool is a board DRAG: a card dropped beside another, in
	// its own lane or the next. The order is furniture a person arranges —
	// where a card sits says what somebody wants looked at first — for the
	// reason the view tools above are a person's; the lane half is an
	// ordinary status change every seat already makes with
	// `update_work_item`.
	MoveWorkItemTool = "move_work_item"
)

// GetWorkCatalogueTool is the one catalogue verb a SEAT does hold.
//
// Reading is not writing, and a model that cannot read the catalogue can only
// guess at a type — which is exactly how `Bug`, `bugfix` and `BUG` come to sit
// beside `bug`. Telling the model what exists is cheaper than refusing it
// repeatedly, so this one is in [Tools].
const GetWorkCatalogueTool = "get_work_catalogue"

// OperatorOnlyTools are the ones the operator surface adds to [Tools].
func OperatorOnlyTools() []string {
	return []string{
		ListWorkViewsTool, SaveWorkViewTool,
		WriteWorkCatalogueTool,
		GetPersonTool, SetPrioritiesTool, SetPinsTool, MarkInboxTool,
		WorkInboxTool,
		RemoveWorkItemTool, RestoreWorkItemTool,
		MoveWorkItemTool,
	}
}

// Tools are the thirteen a seat holds, so a caller registering them names one
// thing.
func Tools() []string {
	return []string{ListWorkItemsTool, GetWorkItemTool, CreateWorkItemTool,
		UpdateWorkItemTool, CommentOnWorkTool, MergeWorkItemTool,
		SearchWorkItemsTool, GetWorkCatalogueTool, ListProjectsTool, DescribeProjectTool,
		WriteProjectTool,
		TaskActivityTool, MyWorkTool}
}

// WriteTools are the four that count as a DELIVERY.
//
// A turn woken by an assignment answers by moving the task, commenting on it,
// or filing the follow-up work — and the delivery gate has to know that, or
// such a turn is corrected and looped for having "done nothing". Reading is not
// delivering, which is why get and list are not here: a turn that only read is
// exactly the turn the gate exists to catch.
func WriteTools() []string {
	return []string{CreateWorkItemTool, UpdateWorkItemTool, CommentOnWorkTool,
		MergeWorkItemTool}
}
