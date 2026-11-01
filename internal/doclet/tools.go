package doclet

// Doclet's tool names.
//
// # Why they live in the DOMAIN rather than beside the implementations
//
// Worklet's reason (internal/worklet/tools.go), arriving here too: the
// builtin surface implements these tools and imports this package, while this
// package's own notification prompt and its refusals NAME them, telling a
// woken seat which tool to reach for. Spelled in both places, a rename of the
// tool compiled and left a seat told to call something this build does not
// register. The domain owns its vocabulary, and the builtins read it.
const (
	ListPagesTool     = "list_pages"
	GetPageTool       = "get_page"
	WritePageTool     = "write_page"
	SavePageTool      = "save_page"
	CommentOnPageTool = "comment_on_page"
)
