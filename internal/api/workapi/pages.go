package workapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/pages"
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
	page := func(a authz.Action) authz.Policy { return kinded(a, authz.KindPage) }

	mount("POST /pages", page(authz.ActionPageCreate), s.postPage)
	mount("PUT /pages/{id}", page(authz.ActionPageSave), s.putPage)
	mount("POST /pages/{id}/rename", page(authz.ActionPageRead), s.postPageRename)
	mount("POST /pages/{id}/comments", page(authz.ActionPageComment), s.postPageComment)
	mount("PATCH /pages/{id}/comments/{cid}", page(authz.ActionPageComment),
		s.patchPageComment)
	mount("DELETE /pages/{id}/comments/{cid}", page(authz.ActionPageRead),
		s.deletePageComment)
	mount("DELETE /pages/{id}", page(authz.ActionPageRead), s.deletePage)
	mount("POST /pages/{id}/restore", page(authz.ActionPageRead), s.postPageRestore)
	// THE PURGE NEEDS NO ROW TO DECIDE, as the work purge does not.
	mount("POST /pages/{id}/purge", authz.Policy{Action: authz.ActionPagePurge},
		s.postPagePurge)
}

// ---- the tool-backed routes -------------------------------------------- //

func (s *Service) postPage(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, builtin.WritePageTool, args)
}

// putPage is save_page, with `If-Match` accepted as its required
// `base_version`: the version a caller read is the precondition HTTP already
// has a header for, and the tool refuses a save without one.
func (s *Service) putPage(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "page", r.PathValue("id")) ||
		!ifMatch(w, r, args, "base_version") {
		return
	}
	s.call(w, r, builtin.SavePageTool, args)
}

func (s *Service) postPageComment(w http.ResponseWriter, r *http.Request) {
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
func (s *Service) patchPageComment(w http.ResponseWriter, r *http.Request) {
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
// container lead's.
func (s *Service) postPageRename(w http.ResponseWriter, r *http.Request) {
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
		func(actor pages.Actor, id string) (pages.Written, error) {
			return s.store.Rename(r.Context(), actor, id, title, quiet)
		})
}

// deletePage puts a page in the trash, and postPageRestore takes it back.
func (s *Service) deletePage(w http.ResponseWriter, r *http.Request) {
	if args, ok := readArgs(w, r); !ok || !only(w, args) {
		return
	}
	s.pageGesture(w, r, authz.ActionPageTrash,
		func(actor pages.Actor, id string) (pages.Written, error) {
			return s.store.Trash(r.Context(), actor, id)
		})
}

func (s *Service) postPageRestore(w http.ResponseWriter, r *http.Request) {
	if args, ok := readArgs(w, r); !ok || !only(w, args) {
		return
	}
	s.pageGesture(w, r, authz.ActionPageRestore,
		func(actor pages.Actor, id string) (pages.Written, error) {
			return s.store.Restore(r.Context(), actor, id)
		})
}

// postPagePurge destroys a page permanently.
//
// THE CONFIRMATION IS THE PAGE'S TITLE, for the work purge's reason: the id
// in the path is what a script carries, and the title is what a person sees
// — repeating it, checked against the page the path resolves to, is the
// confirmation that they looked. A reason is required because the deletion
// marker's reason is all that survives.
func (s *Service) postPagePurge(w http.ResponseWriter, r *http.Request) {
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
	if pages.NormalizeTitle(confirm) != pages.NormalizeTitle(detail.Page.Title) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeRefused,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q and the "+
				"page is %q — nothing was destroyed", confirm, detail.Page.Title)})
		return
	}
	if !s.maySkillPage(w, r, detail.Page.Container) {
		return
	}
	key := operationKey(r)
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := s.store.Purge(r.Context(), actor, detail.Page.ID, reason)
	if err != nil {
		failErr(w, err, key)
		return
	}
	log.Info("page_purged", "page", detail.Page.ID, "title", detail.Page.Title,
		"container", detail.Page.Container, "actor", actor.Name(), "reason", reason,
		"outcome", written.Outcome.Outcome)
	answerPage(w, key, detail.Page, written)
}

// deletePageComment takes one remark down: its author, or a moderator.
//
// THE MODERATOR IS WHOEVER THE TABLE ADMITTED ON THE GRANT rather than as the
// author, and the store is told which through [pages.CommentAuthority]: the
// store holds no chart and no grants, so it states the rule — the author, or
// somebody the caller says may moderate — and this is where the second half
// is decided.
func (s *Service) deletePageComment(w http.ResponseWriter, r *http.Request) {
	if args, ok := readArgs(w, r); !ok || !only(w, args) {
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
	key := operationKey(r)
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := s.store.RemoveComment(r.Context(), actor, detail.Page.ID,
		comment.ID, pages.CommentAuthority{Moderate: d.Reason != authz.ReasonAuthor})
	if err != nil {
		failErr(w, err, key)
		return
	}
	body := pageReceipt(detail.Page, written)
	body["comment_id"], body["removed"] = comment.ID, true
	answer(w, key, written.Outcome.Outcome, body)
}

// pageGesture is the shape of the three row-decided page verbs: read the page,
// decide the verb on its container, act, answer.
func (s *Service) pageGesture(w http.ResponseWriter, r *http.Request,
	action authz.Action, act func(pages.Actor, string) (pages.Written, error)) {

	detail, ok := s.readPage(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if _, ok := s.decide(w, r, action, authz.Object{
		Kind: authz.KindPage, Container: detail.Page.Container,
	}); !ok {
		return
	}
	if !s.maySkillPage(w, r, detail.Page.Container) {
		return
	}
	key := operationKey(r)
	actor, ok := s.pageActor(w, r, key)
	if !ok {
		return
	}
	written, err := act(actor, detail.Page.ID)
	if err != nil {
		failErr(w, err, key)
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
// could still take the skill out of every seat's turn with `DELETE /pages`.
// Asked AFTER the verb's own decision, so a caller who may not trash the page
// at all is told that rather than something about skills.
func (s *Service) maySkillPage(w http.ResponseWriter, r *http.Request,
	container string) bool {

	object, skill := s.pageDeps.SkillPage(container)
	if !skill {
		return true
	}
	_, ok := s.decide(w, r, authz.ActionSkillPageWrite, object)
	return ok
}

// answerPage renders a page write the store made.
func answerPage(w http.ResponseWriter, key string, page pages.Page,
	written pages.Written) {

	answer(w, key, written.Outcome.Outcome, pageReceipt(page, written))
}

// pageReceipt is what a page write answers with: the page it was about, the
// revision it landed at, and where it is on the log.
func pageReceipt(page pages.Page, written pages.Written) map[string]any {
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
func (s *Service) readPage(w http.ResponseWriter, r *http.Request, ref string) (
	pages.Detail, bool) {

	detail, err := s.pageDeps.Reader.Get(r.Context(), strings.TrimSpace(ref),
		decisionRead)
	if err != nil {
		readFailed(w, err)
		return pages.Detail{}, false
	}
	return detail, true
}

// readComment is the page and the one remark the path names.
func (s *Service) readComment(w http.ResponseWriter, r *http.Request) (
	pages.Detail, pages.Comment, bool) {

	detail, ok := s.readPage(w, r, r.PathValue("id"))
	if !ok {
		return pages.Detail{}, pages.Comment{}, false
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
	return pages.Detail{}, pages.Comment{}, false
}
