package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
)

// The native knowledge base's tool names.
const (
	ListPagesTool     = "list_pages"
	GetPageTool       = "get_page"
	WritePageTool     = "write_page"
	SavePageTool      = "save_page"
	CommentOnPageTool = "comment_on_page"
)

// PageTools are the five.
func PageTools() []string {
	return []string{ListPagesTool, GetPageTool, WritePageTool, SavePageTool, CommentOnPageTool}
}

// PageWrites are the three that count as a DELIVERY.
//
// WRITING SOMETHING DOWN IS AN ANSWER. A turn asked to document a decision
// answers by writing the page, and without this the gate would see only
// builtins, conclude the turn reached nobody, and correct it into another
// round — for having done exactly what was asked.
func PageWrites() []string {
	return []string{WritePageTool, SavePageTool, CommentOnPageTool}
}

// PageReader is what these tools need from the knowledge base's read side.
type PageReader interface {
	List(ctx context.Context, f pages.Filter) ([]pages.Summary, error)
	Get(ctx context.Context, ref string) (pages.Detail, error)
}

// PageWriter is what these tools need from the write side.
type PageWriter interface {
	Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error)
	SavePage(ctx context.Context, actor pages.Actor, pageID string, save pages.Save) (pages.Written, error)
	Comment(ctx context.Context, actor pages.Actor, pageID string, in pages.NewComment) (pages.Comment, pages.Written, error)
}

// PageDeps are the knowledge base's halves plus what a write needs.
type PageDeps struct {
	Reader PageReader
	Writer PageWriter

	Mentions MentionResolver

	// DefaultContainer is where a seat writes a page that names none — its
	// unit's space. Empty makes the container argument required.
	DefaultContainer func(handle string) string

	// Reserved are the containers a seat may not write to directly: the
	// tool-skills container (whose pages are machinery published by the
	// sync CLI) and the org root. A write there is refused naming the
	// container, rather than silently landing somewhere excluded from
	// every search.
	Reserved []string
}

func pageActor(turn *turnctx.Turn) (pages.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return pages.Actor{}, err
	}
	return pages.Actor{
		Handle: seat.Handle(), Kind: pages.AuthorAgent,
		TurnID: turn.ID, Chain: turn.Chain,
	}, nil
}

func (d PageDeps) reserved(container string) bool {
	for _, key := range d.Reserved {
		if strings.EqualFold(strings.TrimSpace(key), container) {
			return true
		}
	}
	return false
}

func unconfiguredKB(name string) tools.Result {
	return failed(name + " is unavailable: this company does not run the native " +
		"knowledge base. Use the tools your company has configured.")
}

// ---- list_pages -------------------------------------------------------- //

type listPages struct{ deps PageDeps }

var _ tools.SeatCallable = (*listPages)(nil)

func (t *listPages) Name() string { return ListPagesTool }

func (t *listPages) Description() string {
	return "List pages in the company's knowledge base by container, parent " +
		"or title. For BROWSING a structure you know; to find pages ABOUT a " +
		"subject, use search_knowledge, which ranks by relevance."
}

func (t *listPages) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": map[string]any{
				"type":        "string",
				"description": "A container key, e.g. ENG. Omit for every container.",
			},
			"parent": map[string]any{
				"type":        "string",
				"description": "A page id, to list its children.",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Substring of the title.",
			},
			"label": map[string]any{"type": "string"},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).",
					pages.MaxLimit, pages.DefaultLimit),
			},
		},
	}
}

func (t *listPages) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listPages) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	if _, err := turn.RequireSeat(); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(ListPagesTool), nil
	}
	if t.deps.Reader == nil {
		return unconfiguredKB(ListPagesTool), nil
	}
	// PUBLISHED ONLY, and not a parameter: a draft is somebody's
	// unfinished thought, and an agent given the option to list drafts
	// would act on one.
	got, err := t.deps.Reader.List(ctx, pages.Filter{
		Container: strings.TrimSpace(argString(args, "container")),
		ParentID:  strings.TrimSpace(argString(args, "parent")),
		Title:     strings.TrimSpace(argString(args, "title")),
		Label:     strings.TrimSpace(argString(args, "label")),
		Status:    []pages.Status{pages.StatusPublished},
		Limit:     argInt(args, "limit", 0),
	})
	if err != nil {
		return failed(readFailure(ListPagesTool, err)), nil
	}
	if len(got) == 0 {
		return tools.Result{Output: "No pages match that filter."}, nil
	}
	return jsonResult(map[string]any{"count": len(got), "pages": got})
}

// ---- get_page ---------------------------------------------------------- //

type getPage struct{ deps PageDeps }

var _ tools.SeatCallable = (*getPage)(nil)

func (t *getPage) Name() string { return GetPageTool }

func (t *getPage) Description() string {
	return "Read one page in full: its body, its comments, its revision " +
		"history and its place in the tree. Take the `version` from the " +
		"result and pass it back as `base_version` on save_page — an edit " +
		"that does not say which version it changed is refused."
}

func (t *getPage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"page": map[string]any{
				"type": "string",
				"description": "The page id, or \"CONTAINER/Title\" — e.g. " +
					"\"ENG/Deploy Runbook\".",
			},
		},
		"required": []any{"page"},
	}
}

func (t *getPage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *getPage) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	if _, err := turn.RequireSeat(); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(GetPageTool), nil
	}
	if t.deps.Reader == nil {
		return unconfiguredKB(GetPageTool), nil
	}
	ref := strings.TrimSpace(argString(args, "page"))
	if ref == "" {
		return failed("get_page needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return failed(fmt.Sprintf("There is no page %q. Check the container and "+
			"title, or use search_knowledge to find it.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(GetPageTool, err)), nil
	}
	return jsonResult(detail)
}

// ---- write_page -------------------------------------------------------- //

type writePage struct{ deps PageDeps }

var _ tools.SeatCallable = (*writePage)(nil)

func (t *writePage) Name() string { return WritePageTool }

func (t *writePage) Description() string {
	return "Write a NEW page. Search first — a second page on one subject " +
		"splits what the company knows in half, and the next reader finds " +
		"whichever they happen to search for. The title is the page's " +
		"address and must be unique in its container; to change an existing " +
		"page use save_page."
}

func (t *writePage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type": "string",
				"description": "How people will refer to this page. Unique " +
					"within the container.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The page, in markdown.",
			},
			"container": map[string]any{
				"type":        "string",
				"description": "The container key. Defaults to your team's.",
			},
			"parent": map[string]any{
				"type":        "string",
				"description": "The id of the page this belongs under.",
			},
			"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"message": map[string]any{
				"type":        "string",
				"description": "One line on why this page exists.",
			},
		},
		"required": []any{"title", "body"},
	}
}

func (t *writePage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writePage) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := pageActor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(WritePageTool), nil
	}
	if t.deps.Writer == nil {
		return unconfiguredKB(WritePageTool), nil
	}
	in := pages.NewPage{
		Title:     strings.TrimSpace(argString(args, "title")),
		Body:      argString(args, "body"),
		Container: strings.ToUpper(strings.TrimSpace(argString(args, "container"))),
		ParentID:  strings.TrimSpace(argString(args, "parent")),
		Labels:    argStrings(args, "labels"),
		Message:   strings.TrimSpace(argString(args, "message")),
	}
	if in.Container == "" {
		if t.deps.DefaultContainer != nil {
			in.Container = t.deps.DefaultContainer(actor.Handle)
		}
		if in.Container == "" {
			return failed("write_page needs a `container`: your team owns none, " +
				"so there is no default. Ask where this belongs rather than guessing."), nil
		}
	}
	if t.deps.reserved(in.Container) {
		return failed(fmt.Sprintf(
			"%s is a reserved container and pages written there are excluded "+
				"from every search. Write this somewhere a reader will find it.",
			clip(in.Container))), nil
	}

	got, err := t.deps.Writer.Create(ctx, actor, in)
	if err != nil {
		return failed(pageWriteFailure(WritePageTool, err)), nil
	}
	return jsonResult(map[string]any{
		"id": got.Page.ID, "container": got.Page.Container,
		"title": got.Page.Title, "version": got.Page.Version,
		"revision": got.Revision,
	})
}

// ---- save_page --------------------------------------------------------- //

type savePage struct{ deps PageDeps }

var _ tools.SeatCallable = (*savePage)(nil)

func (t *savePage) Name() string { return SavePageTool }

func (t *savePage) Description() string {
	return "Change an existing page: its body, title, labels or parent. " +
		"You MUST pass the `base_version` you read with get_page — an edit " +
		"against a version somebody else has already moved past is refused " +
		"rather than silently overwriting what they wrote."
}

func (t *savePage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"page": map[string]any{
				"type":        "string",
				"description": "The page id, or \"CONTAINER/Title\".",
			},
			"base_version": map[string]any{
				"type": "integer",
				"description": "The `version` from get_page. REQUIRED: it is " +
					"what makes somebody else's edit a refusal instead of a " +
					"silent overwrite.",
			},
			"body":   map[string]any{"type": "string", "description": "Replaces the page."},
			"title":  map[string]any{"type": "string", "description": "Renames it."},
			"parent": map[string]any{"type": "string", "description": "Moves it under this page."},
			"labels": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Replaces the whole label set.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "One line on what you changed and why.",
			},
			"watch": map[string]any{
				"type": "boolean",
				"description": "True to follow this page, false to stop. " +
					"Stopping sticks; a direct @-mention still reaches you.",
			},
		},
		"required": []any{"page", "base_version"},
	}
}

func (t *savePage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *savePage) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := pageActor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(SavePageTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfiguredKB(SavePageTool), nil
	}
	ref := strings.TrimSpace(argString(args, "page"))
	if ref == "" {
		return failed("save_page needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	}
	base := argInt(args, "base_version", 0)
	if base <= 0 {
		return failed("save_page needs a `base_version`: read the page with " +
			"get_page and pass its `version` back, so an edit somebody else " +
			"made in the meantime is a refusal rather than a silent overwrite."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return failed(fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(SavePageTool, err)), nil
	}

	save := pages.Save{BaseVersion: base, Message: strings.TrimSpace(argString(args, "message"))}
	if _, ok := args["body"]; ok {
		body := argString(args, "body")
		save.Body = &body
	}
	if _, ok := args["title"]; ok {
		title := strings.TrimSpace(argString(args, "title"))
		save.Title = &title
	}
	if _, ok := args["parent"]; ok {
		parent := strings.TrimSpace(argString(args, "parent"))
		save.ParentID = &parent
	}
	if _, ok := args["labels"]; ok {
		labels := argStrings(args, "labels")
		save.Labels = &labels
	}
	if watch, ok := args["watch"].(bool); ok {
		save.Watch = &watch
	}

	got, err := t.deps.Writer.SavePage(ctx, actor, detail.Page.ID, save)
	if err != nil {
		return failed(pageWriteFailure(SavePageTool, err)), nil
	}
	return jsonResult(map[string]any{
		"id": got.Page.ID, "title": got.Page.Title,
		"version": got.Page.Version, "revision": got.Revision,
	})
}

// ---- comment_on_page --------------------------------------------------- //

type commentOnPage struct{ deps PageDeps }

var _ tools.SeatCallable = (*commentOnPage)(nil)

func (t *commentOnPage) Name() string { return CommentOnPageTool }

func (t *commentOnPage) Description() string {
	return "Comment on a page — to ask about something it says, or to flag " +
		"that it has gone stale. Anyone you @-mention by handle is woken; " +
		"people watching the page are told. If the answer is a change to the " +
		"page, make the change with save_page rather than describing it here."
}

func (t *commentOnPage) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"page": map[string]any{
				"type":        "string",
				"description": "The page id, or \"CONTAINER/Title\".",
			},
			"body": map[string]any{
				"type": "string",
				"description": "The comment, in markdown. @-mention a " +
					"colleague by handle to reach them specifically.",
			},
			"reply_to": map[string]any{
				"type":        "string",
				"description": "The id of the comment you are answering.",
			},
		},
		"required": []any{"page", "body"},
	}
}

func (t *commentOnPage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *commentOnPage) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := pageActor(turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(CommentOnPageTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfiguredKB(CommentOnPageTool), nil
	}
	ref := strings.TrimSpace(argString(args, "page"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case ref == "":
		return failed("comment_on_page needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	case body == "":
		return failed("comment_on_page needs a `body`."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return failed(fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(CommentOnPageTool, err)), nil
	}

	in := pages.NewComment{
		Body:    body,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		TurnKey: turn.ID,
	}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	comment, written, err := t.deps.Writer.Comment(ctx, actor, detail.Page.ID, in)
	if err != nil {
		return failed(pageWriteFailure(CommentOnPageTool, err)), nil
	}
	return jsonResult(map[string]any{
		"comment_id": comment.ID, "page": detail.Page.Title,
		"mentioned": comment.Mentions, "revision": written.Revision,
	})
}

// pageWriteFailure explains a write that did not land, in terms the model can
// act on.
func pageWriteFailure(name string, err error) string {
	switch {
	case errors.Is(err, pages.ErrInvalid):
		return fmt.Sprintf("%s refused that: %v", name, err)
	case errors.Is(err, pages.ErrTitleTaken):
		return fmt.Sprintf("%v\n\nThat page already exists — read it with "+
			"get_page and edit it with save_page rather than writing a second "+
			"page on the same subject.", err)
	case errors.Is(err, pages.ErrStaleVersion):
		return fmt.Sprintf("%v\n\nRead the page again with get_page, re-apply "+
			"your change on top of what it says now, and save with the version "+
			"you just read.", err)
	case errors.Is(err, pages.ErrConflict):
		return fmt.Sprintf("%s could not land: %v. Somebody else is editing "+
			"this page. Read it again before retrying.", name, err)
	case errors.Is(err, pages.ErrNotFound):
		return fmt.Sprintf("%s: %v", name, err)
	}
	return fmt.Sprintf("%s did not land (%v). The change was NOT made — do not "+
		"report it as done.", name, err)
}
