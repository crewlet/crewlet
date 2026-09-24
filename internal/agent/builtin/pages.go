package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
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
//
// THE LEVEL IS PART OF THE CALL, because a seat's own read is not the same
// question a dashboard poll asks. A seat reads at [seatReadLevel] — the seat
// surface's own default, which is `linearizable` — because it must see its own
// writes, and that is what stops a turn that just created a page from
// concluding the page does not exist and creating it again.
type PageReader interface {
	List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error)
}

// PageWriter is what these tools need from the write side.
type PageWriter interface {
	Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error)
	SavePage(ctx context.Context, actor pages.Actor, pageID string, save pages.Save) (pages.Written, error)
	Rename(ctx context.Context, actor pages.Actor, pageID, title string, quiet bool) (pages.Written, error)
	Comment(ctx context.Context, actor pages.Actor, pageID string, in pages.NewComment) (pages.Comment, pages.Written, error)
	EditComment(ctx context.Context, actor pages.Actor, pageID, commentID, body string) (pages.Comment, pages.Written, error)
}

// PageDeps are the knowledge base's halves plus what a write needs.
type PageDeps struct {
	Reader PageReader
	Writer PageWriter

	Mentions MentionResolver

	// DefaultContainer is where a seat writes a page that names none — its
	// unit's space. Empty makes the container argument required.
	DefaultContainer func(handle string) string

	// Reserved are the containers this surface may not write into: create
	// a page in, or change a page of. A write there is refused naming the
	// container.
	//
	// A SEAT'S SURFACE RESERVES TWO, and they are reserved for different
	// reasons. The tool-skills container holds the guidance the engine
	// injects into seats' phases, so a seat writing there would be
	// rewriting its own instructions — self-modification nobody reviewed.
	// The org root holds the organisation's own pages, starting with the
	// Onboarding page every seat reads first, which the company publishes
	// rather than any one seat.
	//
	// THE OPERATOR'S SURFACE RESERVES NONE: a person's own assistant is
	// how a company on the native knowledge base publishes both, and
	// write_page is the only thing that creates a native page.
	//
	// A FUNCTION, asked at every write and never cached by the tools, and
	// nil reserves nothing. Both containers are named in Tier B config, so
	// an apply can move either, and what a write is refused on is whatever
	// the wiring answers at the moment of the write.
	Reserved func() []string

	// Actor decides who a write is attributed to. Nil takes the turn's
	// seat; the operator surface sets it. See [WorkDeps.Actor] for why
	// this is a seam rather than a second copy of these five tools.
	Actor func(ctx context.Context, turn *turnctx.Turn) (pages.Actor, error)

	// Await blocks until this node has applied the log through a position.
	// See [WorkDeps.Await]: same seam, same reason, and it matters more
	// here — a page's SavePage takes the version it read, so a turn that
	// writes and then re-reads rows that have not caught up gets a stale
	// version and its next save is refused.
	Await func(ctx context.Context, at statelog.Position) error
}

// settle waits for a write to reach this node's own applied rows. Best effort;
// see [WorkDeps.settle].
//
// IT TAKES A POSITION rather than a revision, which is what the log answers
// with and what a bucket revision could never be: a place on a stream that
// names its own stream, so nothing has to be told which family it belongs to.
func (d PageDeps) settle(ctx context.Context, at statelog.Position) {
	if d.Await == nil || at.Seq == 0 {
		return
	}
	if err := d.Await(ctx, at); err != nil {
		log.WarnContext(ctx, "page_write_not_applied_yet",
			"at", at.String(), "error", err.Error(),
			"detail", "the write landed on the fleet's log; this node's own "+
				"copy has not caught up, so a read in this same turn may show "+
				"the previous version")
	}
}

func pageActor(turn *turnctx.Turn) (pages.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return pages.Actor{}, err
	}
	return pages.Actor{
		Handle: seat.Handle(), Kind: pages.AuthorAgent,
		TurnID: turn.RunID, Chain: turn.Chain,
	}, nil
}

// actor resolves who this call writes as — see [PageDeps.Actor].
func (d PageDeps) actor(ctx context.Context, turn *turnctx.Turn) (pages.Actor, error) {
	if d.Actor != nil {
		return d.Actor(ctx, turn)
	}
	return pageActor(turn)
}

func (d PageDeps) reserved(container string) bool {
	if d.Reserved == nil {
		return false
	}
	for _, key := range d.Reserved() {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(container)) {
			return true
		}
	}
	return false
}

// refuseReserved is the answer to a write into a reserved container — see
// [PageDeps.Reserved] for which are and why.
//
// IT SAYS WHERE THE WRITE CAN GO INSTEAD, because the refusal is the one
// failure here a model cannot fix by retrying: the container is not wrong in
// its arguments, it is closed to this surface.
func refuseReserved(name, container string) tools.Result {
	return failed(fmt.Sprintf("%s refused that: %s is reserved for pages the "+
		"company publishes — its tool skills, or the organisation's own pages — "+
		"and a seat may not write there. Write this in your team's container "+
		"instead, or ask a person to publish it.", name, clip(container)))
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
		"subject, use search_knowledge, which ranks by relevance. A " +
		"`truncated` answer holds only the page you asked for — pass its " +
		"`next_cursor` back as `after`, with the same filters, to see the rest."
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
			// THE WAY PAST A FULL PAGE, and a CURSOR rather than an
			// offset. An offset counts rows, so a page that leaves the
			// part already read between two calls — trashed, renamed past
			// it — moves every later page up by one, and the next call
			// skips one with nothing on either answer to say so; one that
			// enters it repeats one. The cursor names the last page
			// returned, so the next call starts exactly after it. See
			// [pages.Filter.After].
			"after": map[string]any{
				"type": "string",
				"description": "Where to continue, passed back unchanged: " +
					"the `next_cursor` of a list_pages answer, or the " +
					"`children_cursor` of a get_page answer with `parent` " +
					"set to that page.",
			},
		},
	}
}

func (t *listPages) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listPages) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	// [PageDeps.actor] rather than the turn: a seat with no turn still
	// refuses, and the operator surface, which supplies its own actor, is
	// answered rather than told it is not in a turn.
	if _, err := t.deps.actor(ctx, turn); err != nil {
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
		After:     strings.TrimSpace(argString(args, "after")),
	}, seatRead)
	switch {
	case errors.Is(err, pages.ErrInvalid):
		// A REFUSAL OF THE REQUEST, not a read that failed: the reader
		// refuses an `after` that does not decode as a cursor, naming it,
		// and the read failure's "try again" would send the same value back.
		return failed(fmt.Sprintf("%s refused that: %v", ListPagesTool, err)), nil
	case err != nil:
		return failed(readFailure(ListPagesTool, err)), nil
	}
	// "NOTHING MATCHES" ONLY FROM A COMPLETE ANSWER. An empty listing over
	// a deferred scope is one that could not account for everything, and
	// that is the answer below, flag and all.
	if len(got.Pages) == 0 && got.Complete {
		return tools.Result{Output: "No pages match that filter."}, nil
	}
	rows := got.Pages
	if rows == nil {
		// AN EMPTY LIST, NOT JSON NULL: the one answer that reaches here
		// with no rows is the incomplete one, and it should read as a list
		// that holds nothing rather than as a field that is missing.
		rows = []pages.Summary{}
	}
	out := map[string]any{"count": len(rows), "pages": rows}
	// AND WHAT THE ANSWER COULD NOT ACCOUNT FOR. A listing served over a
	// deferred scope may be missing pages, and a model that reads a short
	// list as the whole truth writes the duplicate.
	if !got.Complete {
		out["complete"] = false
	}
	// THE OTHER KIND OF SHORT LIST, which `complete` never covers: the
	// page filled its limit and more pages match. Same consequence — a
	// model reading it as the whole truth writes the duplicate — so the
	// answer carries the cursor that reaches the rest, and a marker with
	// no way past it would be a pointer at nothing.
	if got.Truncated {
		out["truncated"] = true
		out["next_cursor"] = got.NextCursor
	}
	return jsonResult(out)
}

// ---- get_page ---------------------------------------------------------- //

type getPage struct{ deps PageDeps }

var _ tools.SeatCallable = (*getPage)(nil)

func (t *getPage) Name() string { return GetPageTool }

func (t *getPage) Description() string {
	return "Read one page in full: its body, its comments, its revision " +
		"history and its place in the tree. `children` are its published " +
		"children, the first page of them: when `children_truncated` is " +
		"true it has more, and list_pages with `parent` set to this page " +
		"and `after` set to `children_cursor` lists the rest. Take the " +
		"`version` from the result and pass it back as `base_version` on " +
		"save_page — an edit that does not say which version it changed is " +
		"refused."
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
	// [PageDeps.actor] rather than the turn — see [listPages.CallForTurn].
	if _, err := t.deps.actor(ctx, turn); err != nil {
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
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
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
	actor, err := t.deps.actor(ctx, turn)
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
		return refuseReserved(WritePageTool, in.Container), nil
	}

	got, err := t.deps.Writer.Create(ctx, actor, in)
	if err != nil {
		return failed(pageWriteFailure(WritePageTool, err)), nil
	}
	t.deps.settle(ctx, got.Outcome.Position)
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
			"body":  map[string]any{"type": "string", "description": "Replaces the page."},
			"title": map[string]any{"type": "string", "description": "Renames it."},
			"parent": map[string]any{
				"type": "string",
				"description": "The id of the page to move it under — refused " +
					"when it is this page, a page beneath it, or a page " +
					"that does not exist. An empty string moves it to the " +
					"top of its container.",
			},
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
	actor, err := t.deps.actor(ctx, turn)
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
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return failed(fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(SavePageTool, err)), nil
	}
	// A CHANGE IS A WRITE TOO. Reserving a container against creates alone
	// would leave every page already in it open to a seat's edit — a
	// tool skill's body included, which is the write the reservation
	// exists to stop. Judged on the page's own container, read above.
	if t.deps.reserved(detail.Page.Container) {
		return refuseReserved(SavePageTool, detail.Page.Container), nil
	}

	save := pages.Save{BaseVersion: base, Message: strings.TrimSpace(argString(args, "message"))}
	if _, ok := args["body"]; ok {
		body := argString(args, "body")
		save.Body = &body
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
	at := got.Outcome.Position
	revision := got.Revision
	// A RENAME IS ITS OWN WRITE, and it goes SECOND. An address change
	// contends for the address and a content change contends for the page,
	// so one record cannot arbitrate both — and doing the content first
	// means a refused rename leaves the edit saved under the old name
	// rather than the reverse, which is the half a person can act on.
	if title, renaming := args["title"]; renaming {
		// AND IT WAITS FOR THE SAVE FIRST. A rename decides from this
		// node's own applied rows, so one issued before the save has
		// reached them reads the PRE-SAVE head and answers with its
		// version — which this result hands the model as `version` and
		// the model passes back as `base_version`, where it is refused
		// as stale. See [PageDeps.Await], which exists for exactly this.
		t.deps.settle(ctx, at)
		want := strings.TrimSpace(fmt.Sprint(title))
		renamed, err := t.deps.Writer.Rename(ctx, actor, detail.Page.ID, want, false)
		if err != nil {
			return failed(fmt.Sprintf("The edit was saved and the rename to %q "+
				"was not: %s", clip(want),
				pageWriteFailure(SavePageTool, err))), nil
		}
		got.Page = renamed.Page
		// THE LATER OF THE TWO, never the rename's outright. A rename to
		// the title a page already displays appends no record at all, so
		// its position is zero and its revision is whatever this node
		// had applied when it decided — which can be BELOW the save's.
		// Overwriting with it told the model the edit was at a revision
		// that predates it, and handed `settle` the earlier of the two
		// positions to wait for, so a re-read in the same turn could
		// still show the old title.
		if renamed.Revision > revision {
			revision = renamed.Revision
		}
		if renamed.Outcome.Position.Packed() > at.Packed() {
			at = renamed.Outcome.Position
		}
	}
	t.deps.settle(ctx, at)
	return jsonResult(map[string]any{
		"id": got.Page.ID, "title": got.Page.Title,
		"version": got.Page.Version, "revision": revision,
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
		"page, make the change with save_page rather than describing it here. " +
		"Pass `edit` with the id of a comment YOU wrote to replace what it " +
		"says instead of adding another."
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
			"edit": map[string]any{
				"type": "string",
				"description": "The id of one of YOUR OWN comments to " +
					"replace with this body. Somebody else's is refused — " +
					"add a comment saying what changed instead.",
			},
		},
		"required": []any{"page", "body"},
	}
}

func (t *commentOnPage) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *commentOnPage) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
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
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return failed(fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return failed(readFailure(CommentOnPageTool, err)), nil
	}

	// AN EDIT IS THE SAME GESTURE, which is why it is this tool rather than
	// a sixth name in the registry: a model correcting its own remark is
	// putting words on a page, and the guard that matters — only the author
	// — lives in the store either way.
	if edit := strings.TrimSpace(argString(args, "edit")); edit != "" {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		comment, written, err := t.deps.Writer.EditComment(ctx, actor, detail.Page.ID, edit, body)
		if err != nil {
			return failed(pageWriteFailure(CommentOnPageTool, err)), nil
		}
		t.deps.settle(ctx, written.Outcome.Position)
		return jsonResult(map[string]any{
			"comment_id": comment.ID, "page": detail.Page.Title,
			"edited": true, "revision": written.Revision,
		})
	}

	in := pages.NewComment{
		Body:    body,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
		TurnKey: turnKey(turn),
	}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	comment, written, err := t.deps.Writer.Comment(ctx, actor, detail.Page.ID, in)
	if err != nil {
		return failed(pageWriteFailure(CommentOnPageTool, err)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
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
