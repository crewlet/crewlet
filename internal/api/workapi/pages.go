package workapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
)

// pageRoutes mounts the knowledge-base half.
//
// SIX OF ITS NINE VERBS HAVE NO TOOL — a rename as its own gesture, the three
// ways a page leaves or returns to circulation, and taking a remark down —
// and the four destructive ones had authority rules and no caller at all
// until this surface. Each is decided here, through the same table, once the
// page's CONTAINER is read: which space a page is in is a stored row, never a
// path segment, so the route admits a reader of the pages and the handler
// decides the verb.
func (s *Service) pageRoutes(mount mounter) {
	// EVERY ROUTE OF THIS HALF asks for it when a request arrives, and is
	// handed the halves its request found — see [Service.on].
	route := func(pattern string, p authz.Policy, h servedHandler) {
		mount(pattern, p, s.onPages(h))
	}
	page := func(a authz.Action) authz.Policy { return kinded(a, authz.KindPage) }

	route("POST /doclet/pages", page(authz.ActionPageCreate).Creating(), (*served).postPage)
	// A SAVE NAMES THE VERSION IT EDITS, so it never creates a page.
	route("PUT /doclet/pages/{id}", page(authz.ActionPageSave).WhenAbsent(authz.AddedElsewhere(
		"A page is created with POST /doclet/pages.")), (*served).putPage)
	route("POST /doclet/pages/{id}/rename", page(authz.ActionPageRead).Rewriting(),
		(*served).postPageRename)
	route("POST /doclet/pages/{id}/comments", page(authz.ActionPageComment).Creating(),
		(*served).postPageComment)
	route("PATCH /doclet/pages/{id}/comments/{cid}", page(authz.ActionPageComment),
		(*served).patchPageComment)
	route("DELETE /doclet/pages/{id}/comments/{cid}", page(authz.ActionPageRead),
		(*served).deletePageComment)
	route("DELETE /doclet/pages/{id}", page(authz.ActionPageRead), (*served).deletePage)
	route("POST /doclet/pages/{id}/restore", page(authz.ActionPageRead).Rewriting(),
		(*served).postPageRestore)
	// THE PURGE NEEDS NO ROW TO DECIDE, as the work purge does not.
	route("POST /doclet/pages/{id}/purge", authz.Policy{Action: authz.ActionPagePurge}.Destroying(),
		(*served).postPagePurge)
}

// ---- the tool-backed routes -------------------------------------------- //

func (s *served) postPage(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, builtin.WritePageTool, args)
}

// putPage is save_page, with `If-Match` accepted as its required
// `base_version`: the version a caller read is the precondition HTTP already
// has a header for, and the tool refuses a save without one.
func (s *served) putPage(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "page", r.PathValue("id")) {
		return
	}
	s.callIfMatched(w, r, builtin.SavePageTool, args, "base_version")
}

func (s *served) postPageComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "body", "reply_to") ||
		!fromPath(w, args, "page", r.PathValue("id")) {
		return
	}
	s.call(w, r, builtin.CommentOnPageTool, args)
}

// patchPageComment rewrites one remark, AS ITS AUTHOR.
//
// comment_on_page with `edit`, which is the tool's own spelling of the
// gesture — decided first on the table's authored class, and then by the
// store's own rule that only the author may rewrite a remark (see
// [Service.patchItemComment] for why those are two questions).
func (s *served) patchPageComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "body") {
		return
	}
	detail, comment, ok := s.readComment(w, r)
	if !ok {
		return
	}
	if _, ok := s.decide(w, r, authz.ActionPageCommentEdit, authz.Object{
		Kind: authz.KindPage, Container: detail.Page.Container, Author: comment.Author,
	}); !ok {
		return
	}
	args["page"], args["edit"] = detail.Page.ID, comment.ID
	s.call(w, r, builtin.CommentOnPageTool, args)
}

// ---- the gestures no tool makes ---------------------------------------- //

// postPageRename moves a page to a new title — its ADDRESS — which is the
// container lead's or the page's creator's.
func (s *served) postPageRename(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "title", "quiet") {
		return
	}
	title, _ := args["title"].(string)
	if strings.TrimSpace(title) == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "a rename needs a `title`"})
		return
	}
	quiet, _ := args["quiet"].(bool)
	s.pageGesture(w, r, authz.ActionPageRename,
		func(ctx context.Context, actor doclet.Actor, id string) (doclet.Written, error) {
			return s.PageStore.Rename(ctx, actor, id, title, quiet)
		})
}

// deletePage puts a page in the trash, and postPageRestore takes it back.
func (s *served) deletePage(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	s.pageGesture(w, r, authz.ActionPageTrash, s.PageStore.Trash)
}

func (s *served) postPageRestore(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	s.pageGesture(w, r, authz.ActionPageRestore, s.PageStore.Restore)
}

// postPagePurge destroys a page permanently.
//
// THE CONFIRMATION IS THE PAGE'S TITLE, for the work purge's reason: the id
// in the path is what a script carries, and the title is what a person sees
// — repeating it, checked against the page the path resolves to, is the
// confirmation that they looked. A reason is required because the deletion
// marker's reason is all that survives.
func (s *served) postPagePurge(w http.ResponseWriter, r *http.Request) {
	if args, ok := readArgs(w, r); !ok || !only(w, args) {
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if confirm == "" || reason == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "a purge destroys the page and nothing " +
				"undoes it: repeat its TITLE in ?confirm= and state why in ?reason="})
		return
	}
	detail, ok := s.readPage(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if doclet.NormalizeTitle(confirm) != doclet.NormalizeTitle(detail.Page.Title) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q and the "+
				"page is %q, so nothing was destroyed", confirm, detail.Page.Title)})
		return
	}
	if !s.maySkillPage(w, r, detail.Page.Container) {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := s.PageStore.Purge(r.Context(), actor, detail.Page.ID, reason)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionPagePurge), key, written.Outcome.Outcome,
		written.Outcome.Position, err)
	if err != nil {
		failErr(w, r, err, key)
		return
	}
	log.Info("page_purged", "page", detail.Page.ID, "title", detail.Page.Title,
		"container", detail.Page.Container, "actor", actor.Name(), "reason", reason,
		"outcome", written.Outcome.Outcome)
	answerPage(w, key, detail.Page, written)
}

// deletePageComment takes one remark down: its author, or a moderator.
//
// THE MODERATOR IS WHOEVER THE TABLE ADMITTED AS AN ADMIN rather than as the
// author, and the store is told which through [doclet.CommentAuthority]: the
// store holds no chart and no permissions, so it states the rule — the author, or
// somebody the caller says may moderate — and this is where the second half
// is decided.
func (s *served) deletePageComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	detail, comment, ok := s.readComment(w, r)
	if !ok {
		return
	}
	d, ok := s.decide(w, r, authz.ActionPageCommentRemove, authz.Object{
		Kind: authz.KindPage, Container: detail.Page.Container, Author: comment.Author,
	})
	if !ok {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := s.PageStore.RemoveComment(r.Context(), actor, detail.Page.ID,
		comment.ID, doclet.CommentAuthority{Moderate: d.Reason != authz.ReasonAuthor})
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionPageCommentRemove), key, written.Outcome.Outcome,
		written.Outcome.Position, err)
	if err != nil {
		failErr(w, r, err, key)
		return
	}
	body := pageReceipt(detail.Page, written)
	body["comment_id"], body["removed"] = comment.ID, true
	answer(w, key, written.Outcome.Outcome, written.Outcome.Unvouched, body)
}

// pageGesture is the shape of the three row-decided page verbs: read the page,
// decide the verb on its container and its creator, act, answer. act is handed the request's
// context, the one every other step here reads under.
func (s *served) pageGesture(w http.ResponseWriter, r *http.Request,
	action authz.Action,
	act func(context.Context, doclet.Actor, string) (doclet.Written, error)) {

	detail, ok := s.readPage(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// THE ROW DECIDES: the container's lead tidies every page in it and
	// whoever created the page tidies their own (authz.ClassTidy), so the
	// object carries both — formed by the tools' own function.
	if _, ok = s.decide(w, r, action, builtin.PageObject(detail.Page)); !ok {
		return
	}
	if !s.maySkillPage(w, r, detail.Page.Container) {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := act(r.Context(), actor, detail.Page.ID)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork, string(action),
		key, written.Outcome.Outcome, written.Outcome.Position, err)
	if err != nil {
		failErr(w, r, err, key)
		return
	}
	page := written.Page
	if page.ID == "" {
		page = detail.Page
	}
	answerPage(w, key, page, written)
}

// maySkillPage decides [authz.ActionSkillPageWrite] for a write into a page's
// container when that container is the tool-skills one, and reports whether
// the write may go ahead — answering the refusal itself when it may not.
//
// THE TOOLS ASK THE SAME QUESTION through [builtin.PageDeps.SkillPage], and
// this surface's rename, trash, restore and purge reach the store without a
// tool — so without this a caller refused a tool skill's body by `save_page`
// could still take the skill out of every seat's turn with `DELETE /doclet/pages`.
// Asked AFTER the verb's own decision, so a caller who may not trash the page
// at all is told that rather than something about skills.
func (s *served) maySkillPage(w http.ResponseWriter, r *http.Request,
	container string) bool {

	object, skill := s.Pages.SkillPage(container)
	if !skill {
		return true
	}
	_, ok := s.decide(w, r, authz.ActionSkillPageWrite, object)
	return ok
}

// answerPage renders a page write the store made.
func answerPage(w http.ResponseWriter, key string, page doclet.Page,
	written doclet.Written) {

	answer(w, key, written.Outcome.Outcome, written.Outcome.Unvouched,
		pageReceipt(page, written))
}

// pageReceipt is what a page write answers with: the page it was about, the
// revision it landed at, and where it is on the log.
func pageReceipt(page doclet.Page, written doclet.Written) map[string]any {
	outcome := written.Outcome.Outcome
	if outcome == "" {
		// A WRITE THAT CHANGED NOTHING APPENDED NOTHING, and there is
		// nothing for this node to be behind on.
		outcome = "applied"
	}
	return map[string]any{
		"id": page.ID, "title": page.Title, "container": page.Container,
		"revision": written.Revision, "outcome": string(outcome),
		"position": positionOf(written.Outcome.Position),
	}
}

// readPage is a page read before a decision is taken on it.
func (s *served) readPage(w http.ResponseWriter, r *http.Request, ref string) (
	doclet.Detail, bool) {

	detail, err := s.Pages.Reader.Get(r.Context(), strings.TrimSpace(ref),
		decisionRead)
	if err != nil {
		readFailed(w, r, err)
		return doclet.Detail{}, false
	}
	return detail, true
}

// readComment is the page and the one remark the path names.
func (s *served) readComment(w http.ResponseWriter, r *http.Request) (
	doclet.Detail, doclet.Comment, bool) {

	detail, ok := s.readPage(w, r, r.PathValue("id"))
	if !ok {
		return doclet.Detail{}, doclet.Comment{}, false
	}
	cid := strings.TrimSpace(r.PathValue("cid"))
	for _, comment := range detail.Comments {
		if comment.ID == cid {
			return detail, comment, true
		}
	}
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
		map[string]string{"detail": "there is no comment " + cid + " on " +
			detail.Page.Title})
	return doclet.Detail{}, doclet.Comment{}, false
}
