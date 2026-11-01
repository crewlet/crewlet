package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// Doclet's tool names, which the domain owns
// (internal/doclet/tools.go says why).
const (
	ListPagesTool     = doclet.ListPagesTool
	GetPageTool       = doclet.GetPageTool
	WritePageTool     = doclet.WritePageTool
	SavePageTool      = doclet.SavePageTool
	CommentOnPageTool = doclet.CommentOnPageTool
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
	List(ctx context.Context, f doclet.Filter, fresh statelog.Freshness) (doclet.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (doclet.Detail, error)
}

// PageWriter is what these tools need from the write side.
type PageWriter interface {
	Create(ctx context.Context, actor doclet.Actor, in doclet.NewPage) (doclet.Written, error)
	SavePage(ctx context.Context, actor doclet.Actor, pageID string, save doclet.Save) (doclet.Written, error)
	Rename(ctx context.Context, actor doclet.Actor, pageID, title string, quiet bool) (doclet.Written, error)
	Comment(ctx context.Context, actor doclet.Actor, pageID string, in doclet.NewComment) (doclet.Comment, doclet.Written, error)
	EditComment(ctx context.Context, actor doclet.Actor, pageID, commentID, body string) (doclet.Comment, doclet.Written, error)
}

// PageDeps are the knowledge base's halves plus what a write needs.
type PageDeps struct {
	Reader PageReader
	Writer PageWriter

	// Authorize decides the checks a tool can only take after a read, in
	// the shape and for the reason [WorkDeps.Authorize] states.
	Authorize Authorizer

	Mentions MentionResolver

	// DefaultContainer is where a write that names no container lands — the
	// writer's own unit's space — asked of the handle the write is recorded
	// under: a seat's, a person's seat, or the login of a credential nobody
	// is bound through, which no seat matches. Empty makes the container
	// argument required.
	DefaultContainer func(handle string) string

	// SkillsContainer is the tool-skills container at this instant, or
	// empty for a company that runs none. A write into it is asked
	// [authz.ActionSkillPageWrite] on top of its own verb — see
	// [PageDeps.SkillPage].
	//
	// A FUNCTION because the key is Tier B and moves on an apply, while the
	// deps outlive the epoch they were built in. NIL IS NOT "NO SKILLS"
	// BY ACCIDENT: a surface that wired none serves a company whose
	// skills container would then be written on the knowledge base alone,
	// which is exactly the hole this closes — so every surface that builds
	// these deps over a live company sets it.
	SkillsContainer func() string

	// Actor decides who a write is attributed to. Nil takes the turn's
	// seat; the operator surface sets it. See [WorkDeps.Actor] for why
	// this is a seam rather than a second copy of these five tools.
	//
	// AND IT CARRIES THE CALLER'S OPERATION KEY where the caller's
	// transport names one: a surface that takes an `Idempotency-Key` sets
	// it as [doclet.Actor.OpKey] on the actor it returns, which is the one
	// way a key reaches a page write outside a turn — the store derives
	// every write's id from it, bound to what that write says. In a turn
	// the key is the turn's ([callOpKey]), whatever the actor carries.
	Actor func(ctx context.Context, turn *turnctx.Turn) (doclet.Actor, error)

	// Await is handed the position every page write landed at, so the next
	// read sees it. See [WorkDeps.Await]: same seam, same reason, the same
	// floor the wait happens behind — and it matters more here: a page's
	// SavePage takes the version it read, so a turn that writes and then
	// re-reads from a copy that has not caught up gets a stale version and
	// its next save is refused.
	Await func(ctx context.Context, at statelog.Position) error
}

// settle hands a page write's position to [PageDeps.Await], so the next read
// sees it. Best effort; see [WorkDeps.settle].
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
			"detail", "the write landed on the fleet's log; the copy this "+
				"surface reads has not caught up, so a read in this same turn "+
				"may show the previous version")
	}
}

func pageActor(turn *turnctx.Turn) (doclet.Actor, error) {
	seat, err := turn.RequireSeat()
	if err != nil {
		return doclet.Actor{}, err
	}
	return doclet.Actor{
		Handle: seat.Handle(), Kind: doclet.AuthorAgent,
		TurnID: turn.RunID, Chain: turn.Chain,
	}, nil
}

// actor resolves who this call writes as — see [PageDeps.Actor].
func (d PageDeps) actor(ctx context.Context, turn *turnctx.Turn) (doclet.Actor, error) {
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
// A page is ordinarily anybody's to write who uses Doclet, and the
// skills container's pages are pages. But they are also the instructions the
// engine injects into a phase of every seat's turn, so writing one rewrites
// the prompt the whole company runs under, and a credential holding only the
// knowledge base could do it: internal/doclet refuses an AGENT every write
// there, and exempts every person, because whether a person may is a matter of
// capability and the store holds no grants. This is where that capability is
// asked, once the container is known — from the arguments on a create, and
// from the stored page on every other write.
//
// EXPORTED for the one surface that writes pages without a tool — the HTTP
// routes for a rename, the trash, a restore and a purge — so the question is
// spelled once however the write arrives.
//
// THE KEY IS CANONICALISED on both sides, for [doclet.ContainerKey]'s reason: a
// comparison that read `ts` and `TS` as two containers would let the one a
// caller was refused be written under the other spelling.
func (d PageDeps) SkillPage(container string) (authz.Object, bool) {
	if d.SkillsContainer == nil {
		return authz.Object{}, false
	}
	skills := doclet.ContainerKey(d.SkillsContainer())
	key := doclet.ContainerKey(container)
	if skills == "" || key != skills {
		return authz.Object{}, false
	}
	return authz.Object{Kind: authz.KindContainer, Container: key}, true
}

// PageObject is a stored page as the verbs that tidy it are decided on
// ([authz.ClassTidy]): the container it lives in, whose lead tidies every page
// there, and the person or seat that CREATED it, who tidies their own. The
// author is set by the create and never by a save, so a page's last editor is
// not handed its address.
//
// EXPORTED for the surface that renames, trashes and restores pages without a
// tool — the HTTP routes — so the object is formed once however the verb
// arrives.
func PageObject(page doclet.Page) authz.Object {
	return authz.Object{Kind: authz.KindPage, Container: page.Container,
		Author: page.Author}
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

// unconfiguredKB is the refusal when the company does not run Doclet.
func unconfiguredKB(name string) tools.Result {
	return refusedBy(tools.RefusalUnavailable, waitless(), name+" is unavailable: this "+
		"company does not run Doclet. Use the tools your company has "+
		"configured.")
}

// ---- list_pages -------------------------------------------------------- //

type listPages struct{ deps PageDeps }

var _ tools.SeatCallable = (*listPages)(nil)

func (t *listPages) Name() string { return ListPagesTool }

func (t *listPages) Description() string {
	return "List pages in Doclet, the company's knowledge base, by container, " +
		"parent or title. For BROWSING a structure you know; to find pages ABOUT a " +
		"subject, use " + SearchKnowledgeTool + ", which ranks by relevance."
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
					doclet.MaxLimit, doclet.DefaultLimit),
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
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(ListPagesTool), nil
	}
	if t.deps.Reader == nil {
		return unconfiguredKB(ListPagesTool), nil
	}
	// PUBLISHED ONLY, and not a parameter: a draft is somebody's
	// unfinished thought, and an agent given the option to list drafts
	// would act on one.
	got, err := t.deps.Reader.List(ctx, doclet.Filter{
		Container: strings.TrimSpace(argString(args, "container")),
		ParentID:  strings.TrimSpace(argString(args, "parent")),
		Title:     strings.TrimSpace(argString(args, "title")),
		Label:     strings.TrimSpace(argString(args, "label")),
		Status:    []doclet.Status{doclet.StatusPublished},
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
	// A PAGE'S TITLE AND LABELS are words its writers chose (ADR-0040): the
	// row names who saved the page last, and a title older than that save
	// had a writer it does not name.
	read := newForeignRead(turn, actor.Handle)
	for _, page := range got.Pages {
		var last []provenance.Author
		if author := strings.TrimSpace(page.Author); author != "" {
			last = append(last, read.writer(author, ""))
		}
		read.attribute(last, true)
	}
	return read.answer(ctx, ListPagesTool, out,
		"Ask for fewer with `limit`, or narrow with `container`, `parent`, `title` or `label`.")
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
		"result and pass it back as `base_version` on " + SavePageTool + " — an edit " +
		"that does not say which version it changed is refused. The body " +
		"and each comment arrive fenced as text somebody wrote; the two " +
		"fence lines are a label, not part of the page."
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
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return notInATurn(GetPageTool), nil
	}
	if t.deps.Reader == nil {
		return unconfiguredKB(GetPageTool), nil
	}
	ref := strings.TrimSpace(argString(args, "page"))
	if ref == "" {
		return failed(GetPageTool + " needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
	switch {
	case errors.Is(err, doclet.ErrNotFound):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no page %q. "+
			"Check the container and title, or use "+SearchKnowledgeTool+" to find it.",
			clip(ref))), nil
	case err != nil:
		return pageReadFailure(ctx, GetPageTool, err), nil
	}
	note(ctx, t.events, turn, knowledgeRead(turn, types.ReadViaGetPage, doclet.Backend, "",
		[]types.KnowledgeReadPage{{
			ID: detail.Page.ID, Container: detail.Page.Container, Title: detail.Page.Title,
		}}))
	// EVERYTHING ON A PAGE IS SOMEBODY'S WORDS, and the reader is the
	// handle this call reads as: its own revisions are the one text that
	// can come back `own`.
	read := newForeignRead(turn, actor.Handle)
	fencePage(read, nil, detail)
	return read.answer(ctx, GetPageTool, detail, "Narrow what you asked for.")
}

// fencePage fences what a page read holds that somebody wrote: the body under
// every writer its kept revisions name, and each comment under its author.
//
// THE BODY IS EVERY REVISION'S, not its last saver's. A save replaces the
// whole body, so the last saver did send every byte, but a seat that appends
// one line to a colleague's page has sent the colleague's words back
// unchanged, and labelling the result `own` would launder whatever the colleague
// wrote into the one trust a seat relies on most. A page keeps its last
// [doclet.RevisionsKept] revisions, each written at the version it produced
// and the first by the page's author, so a page with more versions than that
// has writers this read cannot name, and its body is labelled as such
// ([provenance.TrustForPartial]).
func fencePage(read *foreignRead, at fieldPath, d doclet.Detail) {
	var writers []provenance.Author
	partial := false
	name := func(handle string) {
		if strings.TrimSpace(handle) == "" {
			partial = true
			return
		}
		writers = append(writers, read.writer(handle, ""))
	}
	name(d.Page.Author)
	named := 0
	for _, h := range d.History {
		name(h.Author)
		if h.Version >= 2 && h.Version <= d.Page.Version {
			named++
		}
	}
	if named < d.Page.Version-1 {
		partial = true
	}
	read.fence(at.key("page").key("body"), provenance.SourcePageBody, writers, partial,
		d.Page.Body)
	// ITS NAMES — the title, its labels, its revisions' notes — are its
	// writers' too, and a rename is no revision, so the revisions this read
	// holds never name every writer of the title.
	if d.Page.Title != "" {
		read.attribute(writers, true)
	}
	for i, c := range d.Comments {
		read.fence(at.key("comments").index(i).key("body"), provenance.SourcePageComment,
			[]provenance.Author{read.writer(c.Author, iam.ActorKind(c.AuthorKind))}, false,
			c.Body)
	}
}

// ---- write_page -------------------------------------------------------- //

type writePage struct{ deps PageDeps }

var _ tools.SeatCallable = (*writePage)(nil)

func (t *writePage) Name() string { return WritePageTool }

func (t *writePage) Description() string {
	return "Write a NEW page. Search first: a second page on one subject " +
		"splits what the company knows in half, and the next reader finds " +
		"whichever they happen to search for. The title is the page's " +
		"address and must be unique in its container; to change an existing " +
		"page use " + SavePageTool + "."
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
				"type": "string",
				"description": "The page, in markdown. An EMPTY body files a " +
					"folder: a page that groups the pages filed under it and " +
					"has nothing of its own to read. " + pageLinkHelp,
			},
			"container": map[string]any{
				"type":        "string",
				"description": "The container key. Defaults to your team's.",
			},
			"parent": map[string]any{
				"type": "string",
				"description": "The id of the page this belongs under: a " +
					"page in the same container that is not in the trash. " +
					"Leave it out to file the page at the container's top.",
			},
			"labels": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": fmt.Sprintf("Labels to file the page under: at "+
					"most %d, each at most %d bytes.", doclet.MaxLabels, doclet.MaxLabelLength),
			},
			"status": map[string]any{
				"type": "string",
				"enum": []any{string(doclet.StatusPublished), string(doclet.StatusDraft)},
				"description": "published, the default, is a page every reader " +
					"and every search finds. draft is unfinished: it is listed " +
					"in its container marked as a draft, and no search returns " +
					"it until somebody publishes it.",
			},
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
	actor.OpKey = callOpKey(turn, t.Name(), nil, args, actor.OpKey)
	status, refusal := createStatus(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	// WITHOUT THE FENCE a body copied from a read still wears: those two
	// lines are this turn's label, never the page's text.
	body := unfence(turn, argString(args, "body"))
	in := doclet.NewPage{
		Title:     strings.TrimSpace(argString(args, "title")),
		Body:      body,
		Container: strings.ToUpper(strings.TrimSpace(argString(args, "container"))),
		ParentID:  strings.TrimSpace(argString(args, "parent")),
		Labels:    argStrings(args, "labels"),
		Status:    status,
		Message:   strings.TrimSpace(argString(args, "message")),
	}
	if in.Container == "" {
		// THE WRITER'S OWN TEAM, keyed on the handle the write is recorded
		// under: a seat's, and a person's, who writes as their seat. A
		// credential nobody is bound through writes under its login, which
		// no seat matches, so it names a container or is refused below.
		if t.deps.DefaultContainer != nil {
			in.Container = t.deps.DefaultContainer(actor.Handle)
		}
		if in.Container == "" {
			return failed(org.NoHomeSpace(WritePageTool)), nil
		}
	}
	// A NEW TOOL SKILL IS CONFIGURATION, and the container is known only
	// now — see [PageDeps.SkillPage].
	if refused := t.deps.maySkillPage(ctx, in.Container); refused != nil {
		return *refused, nil
	}
	// A PAGE NOBODY CAN SAY WAS WRITTEN IS NOT ONE TO REPORT: the id and
	// revision would be a create's that may never have landed. Looking first
	// is right for every caller — a second write is refused for the title.
	next := fmt.Sprintf("Read %s/%s with "+GetPageTool+" before writing it again: if "+
		"the first write landed, a second one is refused because the title is "+
		"taken.", in.Container, in.Title)
	got, err := t.deps.Writer.Create(ctx, actor, in)
	if err != nil {
		return pageWriteFailure(ctx, WritePageTool, err, next), nil
	}
	if got.Outcome.Outcome == statelog.OutcomeUnknown {
		return pageUnknown(WritePageTool, got.Outcome, next), nil
	}
	t.deps.settle(ctx, got.Outcome.Position)
	// THE TITLE IS THE CALLER'S OWN, written by this call, and read back as
	// theirs rather than as nobody's (ADR-0040).
	read := newForeignRead(turn, actor.Handle)
	read.attribute([]provenance.Author{read.writer(actor.Handle, iam.ActorKind(actor.Kind))}, false)
	return read.answer(ctx, WritePageTool, map[string]any{
		"id": got.Page.ID, "container": got.Page.Container,
		"title": got.Page.Title, "version": got.Page.Version,
		"revision": got.Revision, "outcome": outcomeOf(got.Outcome),
		"position": positionOf(got.Outcome.Position),
	}, "")
}

// createStatus is the status a new page is written in, or the sentence that
// refuses the one the call named.
//
// PUBLISHED OR DRAFT, and nothing else, whatever the store accepts: a page
// written straight into the trash would be deleted before anybody read it, and
// the trash is a state a page is MOVED into, with the thirty days that start
// then. An absent or empty status is published, which is what every caller
// that predates the argument meant.
func createStatus(args map[string]any) (doclet.Status, string) {
	raw, present := args["status"]
	if !present || raw == nil {
		return doclet.StatusPublished, ""
	}
	text, ok := argval.String(raw)
	switch status := doclet.Status(strings.TrimSpace(text)); {
	case ok && status == "":
		return doclet.StatusPublished, ""
	case ok && (status == doclet.StatusPublished || status == doclet.StatusDraft):
		return status, ""
	}
	return "", fmt.Sprintf(WritePageTool+" writes a page as `%s` or `%s`, and was "+
		"asked for %s. Leave `status` out to publish it.",
		doclet.StatusPublished, doclet.StatusDraft, clip(fmt.Sprint(raw)))
}

// ---- save_page --------------------------------------------------------- //

// pageLinkExample is how a body links another page: the address
// [doclet.Links] reads back, so the linked page lists this one under "Linked
// from". BY ID, never by title — a title is an address that a rename moves,
// and a `[[CONTAINER/Title]]` wiki link is a grammar nothing in the engine
// reads: it renders as literal brackets and is invisible to the backlinks.
const pageLinkExample = "[its title](" + doclet.AddressPrefix + "<page id>)"

// pageLinkHelp is the sentence both page-writing tools carry on `body`.
const pageLinkHelp = "Link another page as " + pageLinkExample +
	", using the `id` " + GetPageTool + " or " + ListPagesTool + " answers — not the title " +
	"and not [[wiki]] brackets, which nothing resolves."

type savePage struct{ deps PageDeps }

var _ tools.SeatCallable = (*savePage)(nil)

func (t *savePage) Name() string { return SavePageTool }

func (t *savePage) Description() string {
	return "Change an existing page: its body, title, labels or parent. " +
		"You MUST pass the `base_version` you read with " + GetPageTool + " — an edit " +
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
				"description": "The `version` from " + GetPageTool + ". REQUIRED: it is " +
					"what makes somebody else's edit a refusal instead of a " +
					"silent overwrite.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Replaces the page. " + pageLinkHelp,
			},
			"title": map[string]any{"type": "string", "description": "Renames it."},
			"parent": map[string]any{
				"type": "string",
				"description": "Moves it under this page: the id of a page in " +
					"the same container, not in the trash and not beneath " +
					"this one. An empty string moves it to the container's top.",
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
	// ONE KEY FOR THE SAVE AND THE RENAME AFTER IT: the store derives each
	// record's id from its own verb, so the two stay two operations.
	actor.OpKey = callOpKey(turn, t.Name(), nil, args, actor.OpKey)
	ref := strings.TrimSpace(argString(args, "page"))
	if ref == "" {
		return failed(SavePageTool + " needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	}
	base := argInt(args, "base_version", 0)
	if base <= 0 {
		return failed(SavePageTool + " needs a `base_version`: read the page with " +
			GetPageTool + " and pass its `version` back, so an edit somebody else " +
			"made in the meantime is a refusal rather than a silent overwrite."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
	switch {
	case errors.Is(err, doclet.ErrNotFound):
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

	// A RENAME IS ASKED FOR BY A `title`, and decided ONCE, here, for the
	// ask below and the rename after the save alike: two tests of the
	// argument could drift apart, and a rename the ask had not decided would
	// land without anybody asking whether the caller may. (A null `title`
	// never reaches a tool: the entry leaves it out, toolargs.Of.)
	_, renaming := args["title"]

	// A RENAME IS THE CONTAINER LEAD'S OR THE PAGE'S CREATOR'S, however it
	// is asked for. The tool's own gate decided the SAVE, which is anybody's
	// who uses Doclet; an address change is
	// [authz.ActionPageRename], decided on the stored page — the container
	// it is in and who created it — so it is asked here, and BEFORE the save,
	// so a caller who may not rename lands nothing rather than half of what
	// they sent. Without it a `title` on this tool was a way round the rule
	// the rename verb itself is held to.
	if renaming {
		if refused := t.deps.mayWrite(ctx, authz.ActionPageRename,
			PageObject(detail.Page)); refused != nil {
			return *refused, nil
		}
	}
	save := doclet.Save{BaseVersion: base, Message: strings.TrimSpace(argString(args, "message"))}
	if _, ok := args["body"]; ok {
		// The body get_page answered arrives fenced, and the likeliest edit
		// sends the fence back with it; see [unfence].
		body := unfence(turn, argString(args, "body"))
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
	if raw, ok := args["watch"]; ok && raw != nil {
		// A WATCH IS A YES OR A NO, and anything else is refused rather than
		// read as "no change": a caller that sent "false" as text would
		// otherwise be told the save landed while still following the page.
		watch, isBool := argval.Bool(raw)
		if !isBool {
			return failed(fmt.Sprintf(SavePageTool+" takes `watch` as true or false, "+
				"and was sent %s.", clip(fmt.Sprint(raw)))), nil
		}
		save.Watch = &watch
	}

	// A SAVE STATES A VERSION, so looking says which way it went and a save
	// against the version it found is the right one either way.
	const saveNext = "Read the page with " + GetPageTool + ": if its version moved past " +
		"the one you edited, the save landed; if it did not, save again with " +
		"the version you just read."
	got, err := t.deps.Writer.SavePage(ctx, actor, detail.Page.ID, save)
	if err != nil {
		return pageWriteFailure(ctx, SavePageTool, err, saveNext), nil
	}
	if got.Outcome.Outcome == statelog.OutcomeUnknown {
		return pageUnknown(SavePageTool, got.Outcome, saveNext), nil
	}
	at := got.Outcome.Position
	revision := got.Revision
	outcome := got.Outcome
	// A RENAME IS ITS OWN WRITE, and it goes SECOND. An address change
	// contends for the address and a content change contends for the page,
	// so one record cannot arbitrate both — and doing the content first
	// means a refused rename leaves the edit saved under the old name
	// rather than the reverse, which is the half a person can act on.
	if renaming {
		// AND IT WAITS FOR THE SAVE FIRST. A rename decides from this
		// node's own applied rows, so one issued before the save has
		// reached them reads the PRE-SAVE head and answers with its
		// version — which this result hands the model as `version` and
		// the model passes back as `base_version`, where it is refused
		// as stale. See [PageDeps.Await], which exists for exactly this.
		t.deps.settle(ctx, at)
		// BY THE CHECK'S OWN READER ([argString]), as every other argument
		// here is read (ADR-0059), and never by fmt.Sprint, a reader of its
		// own, which writes a float by `%g`: 1000000 as "1e+06".
		want := strings.TrimSpace(argString(args, "title"))
		// A RENAME STATES A TITLE, so the page's title says which way it
		// went and renaming to it again is harmless either way.
		const renameNext = "Read the page with " + GetPageTool + ": its title says " +
			"whether the rename landed, and renaming again to the same title " +
			"is harmless."
		renamed, err := t.deps.Writer.Rename(ctx, actor, detail.Page.ID, want, false)
		if err != nil {
			// THE RENAME'S CLASS, because the rename is the half that
			// failed: the save already landed and nothing about it is
			// what a caller has to act on — so the cause is the rename's
			// own, and the sentence says the edit is in.
			refusal := pageWriteFailure(ctx, SavePageTool, err, renameNext)
			said := "The edit was saved and the rename to %q was not: %s"
			if tools.UnknownOf(refusal) {
				// NOT "WAS NOT": a rename nobody answered for
				// may have landed.
				said = "The edit was saved; whether the rename to %q " +
					"landed is not known. %s"
			}
			return tools.Result{Output: fmt.Sprintf(said, clip(want), refusal.Output),
				Failed: true, Cause: refusal.Cause}, nil
		}
		if renamed.Outcome.Outcome == statelog.OutcomeUnknown {
			return failedUnknown(renamed.Outcome.OpID, renamed.Outcome.Unvouched,
				fmt.Sprintf("The edit was saved; whether the rename to %q "+
					"landed is not known. %s", clip(want),
					pageUnknownText(SavePageTool, renamed.Outcome, renameNext))), nil
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
	// THE TITLE IS THE PAGE'S, which this call may not have written: a name
	// whose writers the page does not name (ADR-0040).
	read := newForeignRead(turn, actor.Handle)
	read.attribute(nil, true)
	return read.answer(ctx, SavePageTool, map[string]any{
		"id": got.Page.ID, "title": got.Page.Title,
		"version": got.Page.Version, "revision": revision,
		"outcome": outcomeOf(outcome), "position": positionOf(at),
	}, "")
}

// ---- comment_on_page --------------------------------------------------- //

type commentOnPage struct{ deps PageDeps }

var _ tools.SeatCallable = (*commentOnPage)(nil)

func (t *commentOnPage) Name() string { return CommentOnPageTool }

func (t *commentOnPage) Description() string {
	return "Comment on a page — to ask about something it says, or to flag " +
		"that it has gone stale. Anyone you @-mention by handle is woken; " +
		"people watching the page are told. If the answer is a change to the " +
		"page, make the change with " + SavePageTool + " rather than describing it here. " +
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
	// THE CALL'S KEY, its arguments' digest and repeat count included — see
	// [callOpKey] for what a remark made again after another one cost
	// without them, however it named the page. One key for an edit and for
	// a new remark: the store derives each from its own verb.
	actor.OpKey = callOpKey(turn, t.Name(), nil, args, actor.OpKey)
	ref := strings.TrimSpace(argString(args, "page"))
	body := strings.TrimSpace(argString(args, "body"))
	switch {
	case ref == "":
		return failed(CommentOnPageTool + " needs a `page` — an id, or \"CONTAINER/Title\"."), nil
	case body == "":
		return failed(CommentOnPageTool + " needs a `body`."), nil
	}
	detail, err := t.deps.Reader.Get(ctx, ref, seatRead)
	switch {
	case errors.Is(err, doclet.ErrNotFound):
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
		const editNext = "Editing the comment again with the same body is " +
			"harmless: it replaces the text with what it already says if the " +
			"first edit landed."
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		comment, written, err := t.deps.Writer.EditComment(ctx, actor, detail.Page.ID, edit, body)
		if err != nil {
			return pageWriteFailure(ctx, CommentOnPageTool, err, editNext), nil
		}
		if written.Outcome.Outcome == statelog.OutcomeUnknown {
			return pageUnknown(CommentOnPageTool, written.Outcome, editNext), nil
		}
		t.deps.settle(ctx, written.Outcome.Position)
		return pageReceipt(ctx, turn, actor, map[string]any{
			"comment_id": comment.ID, "page": detail.Page.Title,
			"edited": true, "revision": written.Revision,
			"outcome":  outcomeOf(written.Outcome),
			"position": positionOf(written.Outcome.Position),
		})
	}

	in := doclet.NewComment{
		Body:    body,
		ReplyTo: strings.TrimSpace(argString(args, "reply_to")),
	}
	if t.deps.Mentions != nil {
		in.Mentions = t.deps.Mentions.Mentions(body)
	}
	// WHAT A REPEAT IS DEPENDS ON THE CALLER. A seat's comment is derived
	// from its turn, so the same call again is the same comment: under a
	// lost acknowledgement it posts once, and where this node's ledger
	// cannot vouch for it the repeat publishes nothing and answers the same
	// way — safe, and no answer sooner than looking. A person's retry of one
	// request is the same comment too, under the request's own key. An
	// operator's assistant names no operation for a page write, so its
	// repeat is a new comment, and a second one if the first landed.
	// [unknownNext] is the rule every tracker write already answers by.
	again := callRepeat(turn, actor.OpKey, CommentOnPageTool)
	commentNext := func(unvouched bool) string {
		return unknownNext(unvouched, again,
			"Read the page's comments with "+GetPageTool, "that is a second comment")
	}
	comment, written, err := t.deps.Writer.Comment(ctx, actor, detail.Page.ID, in)
	if err != nil {
		return pageWriteFailure(ctx, CommentOnPageTool, err, commentNext(false)), nil
	}
	if written.Outcome.Outcome == statelog.OutcomeUnknown {
		return pageUnknown(CommentOnPageTool, written.Outcome,
			commentNext(written.Outcome.Unvouched)), nil
	}
	t.deps.settle(ctx, written.Outcome.Position)
	return pageReceipt(ctx, turn, actor, map[string]any{
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
// sentinels ([doclet.ErrInvalid] included), so what reaches the end of this
// switch is a log that refused the append, which is a CONDITION
// ([tools.RefusalUnavailable]), or a read or an encode that broke, which is a
// FAULT ([tools.RefusalInternalError], its error in the log — [faulted]).
//
// AND ONE ERROR IS NEITHER: a write the estate's router sent and never heard
// back about ([estate.ErrOutcomeUnknown]). The node it went to may have run it
// before it went quiet, so it is an UNKNOWN outcome ([UnknownCause]), never
// "NOT made" and never a fault: it was the latter, which told an operator's
// assistant that the node had broken and to say the write could not be made.
//
// `next` IS WHAT THIS CALLER DOES ABOUT IT — the sentence the tool's own
// unknown answer gives for the same call — because TWO CLASSES REACH IT and
// they retry differently ([estate.ErrOutcomeUnknown]): a write carrying no key
// of its caller's, reported at the first data node that did not answer, whose
// repeat is a second write; and a KEYED one — a turn's call, a person's
// request — asked of every other data node first and reported only when none
// ran it, whose same call IS the safe retry. One fixed "read the page before
// writing it again: writing it again makes it twice" was false for the second.
func pageWriteFailure(ctx context.Context, name string, err error, next string) tools.Result {
	switch {
	case errors.Is(err, estate.ErrOutcomeUnknown):
		return tools.Result{Output: fmt.Sprintf("%s: %s. It may be on the page "+
			"and it may not — do not report it as done, and do not report it as "+
			"failed. %s", name, Unanswered, next),
			Failed: true, Cause: UnknownCause("", err)}
	case errors.Is(err, doclet.ErrInvalid):
		// THE WRITER'S SENTENCE ALONE, for the reason writeFailure gives:
		// it names what the caller changes its call by.
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: %s",
			name, doclet.Sentence(err)))
	case errors.Is(err, doclet.ErrReserved):
		// THE STORE'S RULE, not a tool's: a reserved container refuses an
		// agent there whichever surface the write came by, and its own
		// sentence says which container and why.
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: %s. "+
			"Write this somewhere a reader will find it.", name, doclet.Sentence(err)))
	case errors.Is(err, doclet.ErrTitleTaken):
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s.\n\nThat page already "+
			"exists — read it with "+GetPageTool+" and edit it with "+SavePageTool+" rather "+
			"than writing a second page on the same subject.", doclet.Sentence(err)))
	case errors.Is(err, doclet.ErrStaleVersion):
		return refusedBy(tools.RefusalStaleVersion, err, fmt.Sprintf("%s.\n\nRead the "+
			"page again with "+GetPageTool+", re-apply your change on top of what it "+
			"says now, and save with the version you just read.", doclet.Sentence(err)))
	case errors.Is(err, doclet.ErrConflict):
		// A LOST RACE, in words that say which read comes before a
		// retry. A tool of another domain tells it in [statelog.LostRace]
		// ([WriterSentence], [doclet.Sentence]), and a person's surface in
		// its code's own sentence (ADR-0060).
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s could not land: "+
			"somebody else is editing this page, so nothing was written. Read it "+
			"again before retrying.", name))
	case errors.Is(err, doclet.ErrNotFound):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s.", name,
			doclet.Sentence(err)))
	}
	if class, ok := WriterClass(err); ok {
		// ANY OTHER REFUSAL A DOMAIN DECIDED, in its own class and the
		// deciding domain's words — see [writeFailure]'s same arm.
		return refusedBy(class, err, domainRefusal(name, err))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, sentence(fmt.Sprintf("%s did not "+
			"land (%s). The change was NOT made — do not report it as done.",
			name, why), conditionNext(err, "", stuckWrite)))
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
			"read Doclet%s (%s). This is NOT an empty result: do not "+
			"conclude the page does not exist. %s", name,
			rightNow(err), why,
			conditionNext(err, "Try again, or say you could not check.", stuckRead)))
	}
	return faulted(ctx, name, err, fmt.Sprintf("%s could not read Doclet: %s. "+
		"This is NOT an empty result: do not conclude the page does not exist. "+
		"Say you could not check.", name, faultSaid))
}

// pageReceipt is a comment's receipt, which names the page it went on by its
// title: somebody's words whose writers the page does not name, neutralised
// and labelled as such (ADR-0040).
func pageReceipt(ctx context.Context, turn *turnctx.Turn, actor doclet.Actor,
	out map[string]any) (tools.Result, error) {

	read := newForeignRead(turn, actor.Handle)
	read.attribute(nil, true)
	return read.answer(ctx, CommentOnPageTool, out, "")
}
