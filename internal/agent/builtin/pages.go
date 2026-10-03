package builtin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
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

	// Authorize decides the checks a tool can only take after a read, in
	// the shape and for the reason [WorkDeps.Authorize] states.
	Authorize Authorizer

	Mentions MentionResolver

	// DefaultContainer is where a seat writes a page that names none — its
	// unit's space. Empty makes the container argument required.
	DefaultContainer func(handle string) string

	// SkillsContainer is the tool-skills container at this instant, or
	// empty for a company that runs none. A write into it is asked
	// [authz.ActionSkillPageWrite] on top of its own verb — see
	// [PageDeps.SkillPage].
	//
	// A FUNCTION because the key is Tier B and moves on an apply, while the
	// deps outlive the epoch they were built in. NIL IS NOT "NO SKILLS"
	// BY ACCIDENT: a surface that wired none serves a company whose
	// skills container would then be written on the colleague grant alone,
	// which is exactly the hole this closes — so every surface that builds
	// these deps over a live company sets it.
	SkillsContainer func() string

	// Actor decides who a write is attributed to. Nil takes the turn's
	// seat; the operator surface sets it. See [WorkDeps.Actor] for why
	// this is a seam rather than a second copy of these five tools.
	//
	// AND IT CARRIES THE CALLER'S OPERATION KEY where the caller's
	// transport names one: a surface that takes an `Idempotency-Key` sets
	// it as [pages.Actor.OpKey] on the actor it returns, which is the one
	// way a key reaches a page write outside a turn — the store derives
	// every write's id from it, bound to what that write says. In a turn
	// the key is the turn's ([pageOpKey]), whatever the actor carries.
	Actor func(ctx context.Context, turn *turnctx.Turn) (pages.Actor, error)

	// Await blocks until this node's projection has applied a revision.
	// See [WorkDeps.Await]: same seam, same reason, and it matters more
	// here — a page's SavePage takes the version it read, so a turn that
	// writes and then re-reads through a projection that has not caught
	// up gets a stale version and its next save is refused.
	Await func(ctx context.Context, at statelog.Position) error
}

// pageOpKey is the operation key every page write this call makes is derived
// from, set on [pages.Actor.OpKey] — the package doc's one rule, "How a write
// is made once", for the knowledge base, whose store derives each write's id
// from the key, the verb, the object and a digest of what the write says
// ([pages.Store]):
//
//   - IN A TURN, a key derived from the turn's seed ([turnKey]), the instant
//     it began ([turnSince]), the tool and this call's repeat count in the
//     run ([turnctx.CallLog.Ordinal]) — so a re-run's write is the first
//     run's, and a page saved A, then B, then A again in one run is three
//     saves rather than the third collapsing into the first, which the
//     store's digest of what each save says would otherwise make it;
//   - OUTSIDE ONE, the key the caller's transport put on the actor
//     ([PageDeps.Actor]), as it was sent: it already names one request,
//     held to the operation grammar and scoped to the principal that sent
//     it by internal/api/opkey;
//   - or none, and the store mints a fresh id for each write.
func pageOpKey(turn *turnctx.Turn, tool string, args map[string]any,
	actor pages.Actor) string {

	seed := turnKey(turn)
	if seed == "" {
		return actor.OpKey
	}
	identity := []string{pageCallNamespace, seed, tool}
	if n := turn.CallLog().Ordinal(tool, args); n > 0 {
		identity = append(identity, "repeat:"+strconv.Itoa(n))
	}
	return statelog.DeriveOpID(turnSince(turn), "", identity...)
}

// pageCallNamespace scopes [pageOpKey]'s derivation. FIXED for the life of
// the format: a new one would make every re-run turn's page write a second
// one.
const pageCallNamespace = "crewlet.pages.call"

// pageRepeat says how this caller makes a page write again as the same
// operation, or "" where no repeat is the same operation: a turn's call made
// again unchanged, a request sent again under its key, and nothing for a call
// that carried no key at all.
func pageRepeat(turn *turnctx.Turn, key, tool string) string {
	switch {
	case turnKey(turn) != "":
		return fmt.Sprintf("call %s again with exactly the same arguments, "+
			"before calling it with any others", tool)
	case key != "":
		return "send the same request again, unchanged"
	}
	return ""
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

// SkillPage reports whether a page container is the tool-skills container, and
// the object [authz.ActionSkillPageWrite] is decided on when it is.
//
// # Why a page write asks a second question here
//
// A page is ordinarily a colleague's to write — `knowledge:write` — and the
// skills container's pages are pages. But they are also the instructions the
// engine injects into a phase of every seat's turn, so writing one rewrites
// the prompt the whole company runs under, and a credential holding only the
// colleague grant could do it: internal/pages refuses an AGENT every write
// there, and exempts every person, because whether a person may is a matter of
// capability and the store holds no grants. This is where that capability is
// asked, once the container is known — from the arguments on a create, and
// from the stored page on every other write.
//
// EXPORTED for the one surface that writes pages without a tool — the HTTP
// routes for a rename, the trash, a restore and a purge — so the question is
// spelled once however the write arrives.
//
// THE KEY IS CANONICALISED on both sides, for [pages.ContainerKey]'s reason: a
// comparison that read `ts` and `TS` as two containers would let the one a
// caller was refused be written under the other spelling.
func (d PageDeps) SkillPage(container string) (authz.Object, bool) {
	if d.SkillsContainer == nil {
		return authz.Object{}, false
	}
	skills := pages.ContainerKey(d.SkillsContainer())
	key := pages.ContainerKey(container)
	if skills == "" || key != skills {
		return authz.Object{}, false
	}
	return authz.Object{Kind: authz.KindContainer, Container: key}, true
}

// maySkillPage asks [authz.ActionSkillPageWrite] for a write into container,
// and answers nil — the allow — for a write anywhere else.
func (d PageDeps) maySkillPage(ctx context.Context, container string) *tools.Result {
	object, skill := d.SkillPage(container)
	if !skill {
		return nil
	}
	return d.mayWrite(ctx, authz.ActionSkillPageWrite, object)
}

func unconfiguredKB(name string) tools.Result {
	return refused(tools.RefusalUnavailable, name+" is unavailable: this "+
		"company does not run the native knowledge base. Use the tools your "+
		"company has configured.")
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
	}, seatRead)
	if err != nil {
		return pageReadFailure(ctx, ListPagesTool, err), nil
	}
	if len(got.Pages) == 0 {
		return tools.Result{Output: "No pages match that filter."}, nil
	}
	out := map[string]any{"count": len(got.Pages), "pages": got.Pages}
	// AND HOW MANY THE FILTER MATCHED IN ALL, when that is more than this
	// answer holds. Without it a listing cut at its limit read as the whole
	// container, and a model that believes a short list writes the page that
	// is already there.
	if got.Total > len(got.Pages) {
		out["total"] = got.Total
	}
	// AND WHAT THE ANSWER COULD NOT ACCOUNT FOR. A listing served over a
	// deferred scope may be missing pages, and a model that reads a short
	// list as the whole truth writes the duplicate.
	if !got.Complete {
		out["complete"] = false
	}
	return jsonResult(out)
}

// ---- get_page ---------------------------------------------------------- //

type getPage struct {
	deps PageDeps

	// events receives the `knowledge_read` a seat's read records. Nil
	// records nothing: a registry built outside an engine, and the
	// operator's catalogue, whose reader is a person rather than a seat.
	events Telemetry
}

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
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no page %q. "+
			"Check the container and title, or use search_knowledge to find it.",
			clip(ref))), nil
	case err != nil:
		return pageReadFailure(ctx, GetPageTool, err), nil
	}
	note(ctx, t.events, turn, knowledgeRead(turn, types.ReadViaGetPage, pages.Backend, "",
		[]types.KnowledgeReadPage{{
			ID: detail.Page.ID, Container: detail.Page.Container, Title: detail.Page.Title,
		}}))
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
				"description": "The page, in markdown. " + pageLinkHelp,
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
	actor.OpKey = pageOpKey(turn, t.Name(), args, actor)
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
	// A NEW TOOL SKILL IS CONFIGURATION, and the container is known only
	// now — see [PageDeps.SkillPage].
	if refused := t.deps.maySkillPage(ctx, in.Container); refused != nil {
		return *refused, nil
	}
	got, err := t.deps.Writer.Create(ctx, actor, in)
	if err != nil {
		return pageWriteFailure(ctx, WritePageTool, err), nil
	}
	if got.Outcome.Outcome == statelog.OutcomeUnknown {
		// A PAGE NOBODY CAN SAY WAS WRITTEN IS NOT ONE TO REPORT: the id and
		// revision below would be a create's that may never have landed.
		return pageUnknown(WritePageTool, got.Outcome, fmt.Sprintf(
			"Read %s/%s with get_page before writing it again: if the first "+
				"write landed, a second one is refused because the title is taken.",
			in.Container, in.Title)), nil
	}
	t.deps.settle(ctx, got.Outcome.Position)
	return jsonResult(map[string]any{
		"id": got.Page.ID, "container": got.Page.Container,
		"title": got.Page.Title, "version": got.Page.Version,
		"revision": got.Revision, "outcome": outcomeOf(got.Outcome),
		"position": positionOf(got.Outcome.Position),
	})
}

// ---- save_page --------------------------------------------------------- //

// pageLinkExample is how a body links another page: the address
// [pages.Links] reads back, so the linked page lists this one under "Linked
// from". BY ID, never by title — a title is an address that a rename moves,
// and a `[[CONTAINER/Title]]` wiki link is a grammar nothing in the engine
// reads: it renders as literal brackets and is invisible to the backlinks.
const pageLinkExample = "[its title](" + pages.AddressPrefix + "<page id>)"

// pageLinkHelp is the sentence both page-writing tools carry on `body`.
const pageLinkHelp = "Link another page as " + pageLinkExample +
	", using the `id` get_page or list_pages answers — not the title " +
	"and not [[wiki]] brackets, which nothing resolves."

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
			"body": map[string]any{
				"type":        "string",
				"description": "Replaces the page. " + pageLinkHelp,
			},
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
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(SavePageTool), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfiguredKB(SavePageTool), nil
	}
	// ONE KEY FOR THE SAVE AND THE RENAME AFTER IT: the store derives each
	// record's id from its own verb, so the two stay two operations.
	actor.OpKey = pageOpKey(turn, t.Name(), args, actor)
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
		return refusedBy(tools.RefusalNotFound, err,
			fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return pageReadFailure(ctx, SavePageTool, err), nil
	}

	// A CHANGED TOOL SKILL IS CONFIGURATION, and which container the page
	// is in is the stored row's — see [PageDeps.SkillPage]. Asked before
	// anything lands, so a caller who may not lands nothing.
	if refused := t.deps.maySkillPage(ctx, detail.Page.Container); refused != nil {
		return *refused, nil
	}

	// A RENAME IS THE CONTAINER LEAD'S, however it is asked for. The tool's
	// own gate decided the SAVE, which is any colleague's; an address change
	// is [authz.ActionPageRename] and the container it is decided on is the
	// stored page's, so it is asked here — and BEFORE the save, so a caller
	// who may not rename lands nothing rather than half of what they sent.
	// Without it a `title` on this tool was a way round the rule the rename
	// verb itself is held to.
	if _, renaming := args["title"]; renaming {
		if refused := t.deps.mayWrite(ctx, authz.ActionPageRename, authz.Object{
			Kind: authz.KindPage, Container: detail.Page.Container,
		}); refused != nil {
			return *refused, nil
		}
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
		return pageWriteFailure(ctx, SavePageTool, err), nil
	}
	if got.Outcome.Outcome == statelog.OutcomeUnknown {
		return pageUnknown(SavePageTool, got.Outcome, "Read the page with "+
			"get_page: if its version moved past the one you edited, the save "+
			"landed; if it did not, save again with the version you just read."), nil
	}
	at := got.Outcome.Position
	revision := got.Revision
	outcome := got.Outcome
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
			// THE RENAME'S CLASS, because the rename is the half that
			// failed: the save already landed and nothing about it is
			// what a caller has to act on — so the cause is the rename's
			// own, and the sentence says the edit is in.
			refusal := pageWriteFailure(ctx, SavePageTool, err)
			return tools.Result{Output: fmt.Sprintf("The edit was saved and the "+
				"rename to %q was not: %s", clip(want), refusal.Output),
				Failed: true, Cause: refusal.Cause}, nil
		}
		if renamed.Outcome.Outcome == statelog.OutcomeUnknown {
			return failedUnknown(renamed.Outcome.OpID, renamed.Outcome.Unvouched,
				fmt.Sprintf("The edit was saved; whether the rename to %q "+
					"landed is not known. %s", clip(want),
					pageUnknownText(SavePageTool, renamed.Outcome, "Read the "+
						"page with get_page: its title says whether the rename "+
						"landed, and renaming again to the same title is "+
						"harmless."))), nil
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
		at = statelog.Later(at, renamed.Outcome.Position)
		// AND THE LESS CERTAIN OF THE TWO OUTCOMES, where the rename
		// appended anything, because the answer is about the whole
		// gesture: `applied` over a rename the broker never confirmed
		// would tell a caller it can stop looking at a write it cannot
		// vouch for, and a save this node has applied beside a rename it
		// has not is a page this node would still show under its old
		// name. A rename that appended nothing — the title the page
		// already shows — has no outcome of its own to weigh.
		if renamed.Outcome.Wrote() &&
			statelog.LessCertain(outcome.Outcome, renamed.Outcome.Outcome) != outcome.Outcome {
			outcome = renamed.Outcome
		}
	}
	t.deps.settle(ctx, at)
	return jsonResult(map[string]any{
		"id": got.Page.ID, "title": got.Page.Title,
		"version": got.Page.Version, "revision": revision,
		"outcome": outcomeOf(outcome), "position": positionOf(at),
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
	// THE CALL'S KEY, repeat count included — see [pageOpKey] for what a
	// remark made again after another one cost without it. One key for an
	// edit and for a new remark: the store derives each from its own verb.
	actor.OpKey = pageOpKey(turn, t.Name(), args, actor)
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
		return refusedBy(tools.RefusalNotFound, err,
			fmt.Sprintf("There is no page %q.", clip(ref))), nil
	case err != nil:
		return pageReadFailure(ctx, CommentOnPageTool, err), nil
	}

	// AN EDIT IS THE SAME GESTURE, which is why it is this tool rather than
	// a sixth name in the registry: a model correcting its own remark is
	// putting words on a page, and the guard that matters — only the author
	// — lives in the store either way.
	if edit := strings.TrimSpace(argString(args, "edit")); edit != "" {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		comment, written, err := t.deps.Writer.EditComment(ctx, actor, detail.Page.ID, edit, body)
		if err != nil {
			return pageWriteFailure(ctx, CommentOnPageTool, err), nil
		}
		if written.Outcome.Outcome == statelog.OutcomeUnknown {
			return pageUnknown(CommentOnPageTool, written.Outcome,
				"Editing the comment again with the same body is harmless: it "+
					"replaces the text with what it already says if the first "+
					"edit landed."), nil
		}
		t.deps.settle(ctx, written.Outcome.Position)
		return jsonResult(map[string]any{
			"comment_id": comment.ID, "page": detail.Page.Title,
			"edited": true, "revision": written.Revision,
			"outcome":  outcomeOf(written.Outcome),
			"position": positionOf(written.Outcome.Position),
		})
	}

	in := pages.NewComment{
		Body:    body,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
	}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	comment, written, err := t.deps.Writer.Comment(ctx, actor, detail.Page.ID, in)
	if err != nil {
		return pageWriteFailure(ctx, CommentOnPageTool, err), nil
	}
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		// WHAT A REPEAT IS DEPENDS ON THE CALLER. A seat's comment is
		// derived from its turn, so the same call again is the same
		// comment: under a lost acknowledgement it posts once, and where
		// this node's ledger cannot vouch for it the repeat publishes
		// nothing and answers the same way — safe, and no answer sooner
		// than looking. A person's retry of one request is the same
		// comment too, under the request's own key. An operator's
		// assistant names no operation for a page write, so its repeat is
		// a new comment, and a second one if the first landed.
		// [unknownNext] is the rule every tracker write already answers by.
		again := pageRepeat(turn, actor.OpKey, CommentOnPageTool)
		return pageUnknown(CommentOnPageTool, written.Outcome,
			unknownNext(written.Outcome.Unvouched, again,
				"Read the page's comments with get_page",
				"that is a second comment")), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return jsonResult(map[string]any{
		"comment_id": comment.ID, "page": detail.Page.Title,
		"mentioned": comment.Mentions, "revision": written.Revision,
		"outcome":  outcomeOf(written.Outcome),
		"position": positionOf(written.Outcome.Position),
	})
}

// outcomeOf is a page write's outcome as its receipt states it.
//
// THE SAME KEY EVERY WORK WRITE'S RECEIPT CARRIES, which these went without:
// a caller reading the answer could not tell a write this node had applied
// from one still on its way, and the HTTP surface that serves the same tools
// answers the two differently (200 and 202), so it had nothing to decide on.
//
// AN EMPTY OUTCOME IS `applied`: the store answers a write that changed
// nothing — the same title, the same comment body — with no record at all,
// and there is nothing for this node to be behind on.
func outcomeOf(r statelog.Result) string {
	if r.Outcome == "" {
		return string(statelog.OutcomeApplied)
	}
	return string(r.Outcome)
}

// pageUnknown explains a page write whose outcome is unknown: it may have
// landed and it may not, so it is reported as neither — and never as done,
// which is what answering with an id and a revision did.
//
// AN UNVOUCHED ONE SAYS SO, because it sends the caller somewhere else: this
// node's operation ledger may have lost the record of the operation, so the
// same OPERATION asked here answers the same way every time, and another node
// — or a person looking at the page — is what can tell. Whether a caller's
// repeat is that same operation is `next`'s to say: a seat's comment is, and
// an operator's is not.
//
// A FAILED RESULT CARRYING [UnknownOutcome], for the reason [unknownWrite]
// carries one: the sentence is for a model, and a caller answering in status
// codes reads the cause. [pageUnknownText] is the sentence alone, for the one
// answer that says something before it.
func pageUnknown(name string, result statelog.Result, next string) tools.Result {
	return failedUnknown(result.OpID, result.Unvouched,
		pageUnknownText(name, result, next))
}

// pageUnknownText is [pageUnknown]'s sentence.
func pageUnknownText(name string, result statelog.Result, next string) string {
	why := "the write's acknowledgement was lost"
	if result.Unvouched {
		why = "this node's operation ledger may have lost the record of it, so " +
			"this node cannot tell"
	}
	return fmt.Sprintf("%s: whether this landed is unknown (%s; operation %s). "+
		"It may be on the page and it may not — do not report it as done, and "+
		"do not report it as failed. %s", name, why, result.OpID, next)
}

// pageWriteFailure explains a write that did not land, in terms the model can
// act on, classed for a reader that is not a model — the class stated beneath
// the store's own error ([refusedBy]), so a surface answering in status codes
// reads either.
//
// THE UNMARKED REMAINDER IS THE NODE'S, as the tracker's is ([writeFailure]):
// the pages writer wraps every refusal it decides on content in one of its own
// sentinels — [pages.ErrInvalid] included — so what reaches the end of this
// switch is a log that refused the append, which is a CONDITION
// ([tools.RefusalUnavailable]), or a read or an encode that broke, which is a
// FAULT ([tools.RefusalInternalError], its error in the log — [faulted]).
func pageWriteFailure(ctx context.Context, name string, err error) tools.Result {
	switch {
	case errors.Is(err, pages.ErrInvalid):
		// THE WRITER'S SENTENCE ALONE, for the reason writeFailure gives:
		// a person's surface prints this class as it stands.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: %s",
			name, pages.Sentence(err)))
	case errors.Is(err, pages.ErrReserved):
		// THE STORE'S RULE, not a tool's: a reserved container refuses an
		// agent there whichever surface the write came by, and its own
		// sentence says which container and why.
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: %v. "+
			"Write this somewhere a reader will find it.", name, err))
	case errors.Is(err, pages.ErrTitleTaken):
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%v\n\nThat page already "+
			"exists — read it with get_page and edit it with save_page rather "+
			"than writing a second page on the same subject.", err))
	case errors.Is(err, pages.ErrStaleVersion):
		return refusedBy(tools.RefusalStaleVersion, err, fmt.Sprintf("%v\n\nRead the "+
			"page again with get_page, re-apply your change on top of what it "+
			"says now, and save with the version you just read.", err))
	case errors.Is(err, pages.ErrConflict):
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s could not land: "+
			"%v. Somebody else is editing this page. Read it again before "+
			"retrying.", name, err))
	case errors.Is(err, pages.ErrNotFound):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %v", name, err))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not "+
			"land (%s). The change was NOT made — do not report it as done.",
			name, why))
	}
	// NOT "NOT MADE", for [writeFailure]'s reason: a fault while the
	// append resolved leaves a record nobody here can account for.
	return faulted(ctx, name, err, fmt.Sprintf("%s failed: %s. Do not report "+
		"it as done, and do not make it again by other means — say it could "+
		"not be made.", name, faultSaid))
}

// pageReadFailure explains a knowledge-base read that could not be served.
//
// ITS OWN SENTENCE rather than [readFailure]'s, which told a model reading a
// PAGE that it "could not read the tracker" — a different store, and advice
// ("do not conclude the item or the list does not exist") about objects the
// call never asked for. The rules are the tracker read's, though: never
// "nothing found" and never not-found, with the read's own error beneath the
// class — [tools.RefusalUnavailable] for a CONDITION waiting clears, and
// [tools.RefusalInternalError] for a FAULT, whose error is the log's
// ([faulted]).
func pageReadFailure(ctx context.Context, name string, err error) tools.Result {
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not "+
			"read the knowledge base right now (%s). This is NOT an empty "+
			"result — do not conclude the page does not exist. Try again, or say "+
			"you could not check.", name, why))
	}
	return faulted(ctx, name, err, fmt.Sprintf("%s could not read the knowledge "+
		"base: %s. This is NOT an empty result — do not conclude the page does "+
		"not exist. Say you could not check.", name, faultSaid))
}
