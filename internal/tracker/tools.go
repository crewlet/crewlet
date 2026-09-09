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
)

// Tools are the five, so a caller registering them names one thing.
func Tools() []string {
	return []string{ListWorkItemsTool, GetWorkItemTool, CreateWorkItemTool,
		UpdateWorkItemTool, CommentOnWorkTool}
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
