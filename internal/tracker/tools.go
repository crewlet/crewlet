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

	// The PROJECT reads, and they are a seat's for the reason
	// [GetWorkCatalogueTool] is: a create refuses a project the company
	// does not have, a type it has not declared and a required field left
	// empty, and a model that cannot READ any of that can only guess.
	//
	// There is no companion that WRITES. A project's name, purpose and
	// owning unit are chart-owned — written by the epoch apply and by
	// nothing else — so a seat editing them would be editing the company's
	// structure through the back door; its sprint policy and its field
	// declarations are a lead's.
	ListProjectsTool    = "list_projects"
	DescribeProjectTool = "describe_project"

	// SprintReportTool is how a sprint went, which a seat asking "what did
	// we take on and what landed" needs and cannot derive from the board:
	// every figure is over a PAST window, and the board is about now.
	SprintReportTool = "sprint_report"
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

	// The GOAL tools, and the same rule puts them here: a goal is an
	// outcome a PERSON commits the company to, with owners who report on
	// it. A seat setting its own goals is a seat marking its own homework,
	// and the delegation this engine is built on already gives a founder a
	// better lever — the work itself.
	ListWorkGoalsTool = "list_work_goals"
	WriteWorkGoalTool = "write_work_goal"

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
		ListWorkGoalsTool, WriteWorkGoalTool,
		WriteWorkCatalogueTool,
		GetPersonTool, SetPrioritiesTool, SetPinsTool, MarkInboxTool,
		RemoveWorkItemTool, RestoreWorkItemTool,
	}
}

// Tools are the nine a seat holds, so a caller registering them names one thing.
func Tools() []string {
	return []string{ListWorkItemsTool, GetWorkItemTool, CreateWorkItemTool,
		UpdateWorkItemTool, CommentOnWorkTool, GetWorkCatalogueTool,
		ListProjectsTool, DescribeProjectTool, SprintReportTool}
}

// WriteTools are the three that count as a DELIVERY.
//
// A turn woken by an assignment answers by moving the task, commenting on it,
// or filing the follow-up work — and the delivery gate has to know that, or
// such a turn is corrected and looped for having "done nothing". Reading is not
// delivering, which is why get and list are not here: a turn that only read is
// exactly the turn the gate exists to catch.
func WriteTools() []string {
	return []string{CreateWorkItemTool, UpdateWorkItemTool, CommentOnWorkTool}
}
