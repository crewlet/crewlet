package workapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// workRoutes mounts the tracker half.
func (s *Service) workRoutes(mount mounter) {
	// EVERY ROUTE OF THIS HALF asks for it when a request arrives, and is
	// handed the halves its request found — see [Service.on].
	route := func(pattern string, p authz.Policy, h servedHandler) {
		mount(pattern, p, s.onWork(h))
	}
	task := func(a authz.Action) authz.Policy { return kinded(a, authz.KindTask) }
	// A PROJECT IS NAMED BY ITS PATH, so its routes decide the verb there.
	project := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindProject,
				Container: worklet.ProjectKey(r.PathValue("key"))}
		}}
	}
	// A PERSON IS NAMED BY THE PATH AND IS NOT THE PATH. `{handle}` is a seat's
	// handle or a login, and whose record it addresses is known only once the
	// identity directory resolves it (a bound person's login names their SEAT's
	// record). Decided on the spelling, a lead naming a report by login was
	// refused and an administrator admitted to the wrong record.
	//
	// So the route admits on the weakest honest precondition, the verb on the
	// CALLER's own record, and the verb is decided once the name has resolved,
	// by the function a tool and the query surface use (builtin's person verbs,
	// over [iam.OwnerOf]).
	person := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(r *http.Request) authz.Object {
			principal, _ := iam.From(r.Context())
			return authz.Object{Kind: authz.KindPerson,
				Owner: iam.RecordOwner(principal)}
		}}
	}

	route("POST /worklet/items", task(authz.ActionWorkCreate).Creating(), (*served).postItem)
	route("PATCH /worklet/items/{key}", task(authz.ActionWorkUpdate), (*served).patchItem)
	// WHO MAY POST TO A THREAD IS A ROW: a tracker holder may say anything
	// there, and somebody an OPEN ASK on the item names may post the answer
	// to it and nothing else (internal/authz's asked arm) — and which ask
	// is open, and whom it names, is only known once the item is read. So
	// the route admits everybody (`work.thread`) and the tool decides,
	// exactly as it decides the same call made through the act route or an
	// assistant. Decided here as `comment_on_work_item` on the bare kind, a
	// person asked a question without Worklet could never answer it.
	route("POST /worklet/items/{key}/comments", task(authz.ActionWorkThread).Creating(),
		(*served).postItemComment)
	// WHO WROTE THE REMARK IS A ROW, so the route admits whoever may comment
	// with the tracker and the handler decides the edit once it has read
	// the author. An answer is never edited by somebody without it: the
	// asked arm covers answering an open ask, not rewriting what was said.
	route("PATCH /worklet/items/{key}/comments/{cid}", task(authz.ActionWorkComment),
		(*served).patchItemComment)
	// AND TAKING IT DOWN, decided the same way once the author is read: the
	// remark's author, or an admin moderating. Admitted on the tracker's
	// read, as a page comment's removal is on the knowledge base's, because
	// a moderator is admitted by the row and not by commenting.
	route("DELETE /worklet/items/{key}/comments/{cid}", task(authz.ActionWorkRead),
		(*served).deleteItemComment)
	// A PLACE ON A BOARD IS `place_work_item`, decided on the task's kind
	// as the tool's own gate decides it.
	route("POST /worklet/items/{key}/rank", task(authz.ActionWorkPlace).Rewriting(),
		(*served).postRank)
	// DEPENDING AND RELATING ARE EDITS OF THE ITEM, made through
	// update_work_item's own set-valued arguments; the routes are narrower
	// doors onto the same verb.
	route("POST /worklet/items/{key}/depend", task(authz.ActionWorkUpdate).Rewriting(),
		(*served).postDepend)
	route("POST /worklet/items/{key}/relate", task(authz.ActionWorkUpdate).Rewriting(),
		(*served).postRelate)
	// THE TRASH IS THE ITEM'S PROJECT LEAD'S OR ITS REPORTER'S, and both the
	// project and the reporter come out of the row: the route admits a
	// tracker holder — nobody is ASKED to remove an item, so the asked arm
	// admits nobody here — and the tool decides the verb once it has read
	// the item. Every arm of that verb needs the tracker too.
	route("DELETE /worklet/items/{key}", task(authz.ActionWorkRead), (*served).deleteItem)
	route("POST /worklet/items/{key}/restore", task(authz.ActionWorkRead).Rewriting(),
		(*served).postRestore)
	// A MOVE IS THE LEAD'S OF THE PROJECT THE ITEM IS IN, which is the row's
	// for the trash's reason — and a move into the project it is already in
	// is the retry of one that landed, which the tool answers from the
	// move's own ledger rather than asking anybody's authority again.
	route("POST /worklet/items/{key}/move", task(authz.ActionWorkRead).Rewriting(),
		(*served).postMove)
	// THE PURGE NEEDS NO ROW TO DECIDE: it is admin's, after a recent
	// sign-in, and never an agent's, whatever the item is — an author tidies
	// their own item into the trash and back, never out of the estate.
	route("POST /worklet/items/{key}/purge", authz.Policy{Action: authz.ActionWorkPurge}.Destroying(),
		(*served).postPurge)
	// A PROJECT IS THE CHART'S, declared in the company document, so this
	// route writes the policy of one that exists and creates none.
	route("PUT /worklet/projects/{key}", project(authz.ActionProjectWrite).WhenAbsent(
		authz.AddedElsewhere("A project is declared in the company configuration "+
			"(PATCH /config), and its policy is written here once it exists.")),
		(*served).putProject)
	route("POST /worklet/projects/{key}/tags", project(authz.ActionProjectWrite).Rewriting(),
		(*served).postProjectTags)
	// A VIEW'S AUTHORITY IS IN ITS BODY — personal to its owner or shared on
	// its container — so the route admits a reader of the views and the
	// tool's own gate decides the save on what the body says.
	route("POST /worklet/views", kinded(authz.ActionViewList, authz.KindView).Creating(),
		(*served).postView)
	// AND A DELETE'S IS IN THE STORED VIEW, which only the tool reads: the
	// route admits a reader of the views and the tool decides the delete on
	// the view's own owner and container, as a save decides the view it
	// replaces.
	route("DELETE /worklet/views/{id}", kinded(authz.ActionViewList, authz.KindView),
		(*served).deleteView)
	route("PUT /worklet/catalogue", authz.Policy{Action: authz.ActionCatalogueWrite},
		(*served).putCatalogue)
	route("PUT /worklet/people/{handle}/inbox", person(authz.ActionInboxMark), (*served).putInbox)
	route("PUT /worklet/people/{handle}/pins", person(authz.ActionPinsSet), (*served).putPins)
	route("PUT /worklet/people/{handle}/priorities", person(authz.ActionPrioritiesSet),
		(*served).putPriorities)
}

// ---- the tool-backed routes -------------------------------------------- //

func (s *served) postItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, worklet.CreateWorkItemTool, args)
}

func (s *served) patchItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.callIfMatched(w, r, worklet.UpdateWorkItemTool, args, "if_match")
}

func (s *served) postItemComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, worklet.CommentOnWorkTool, args)
}

func (s *served) postDepend(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "waiting_on", "blocking", "dependency_note", "if_match") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.callIfMatched(w, r, worklet.UpdateWorkItemTool, args, "if_match")
}

func (s *served) postRelate(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "linked", "linked_pages", "linked_entries", "if_match") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.callIfMatched(w, r, worklet.UpdateWorkItemTool, args, "if_match")
}

func (s *served) deleteItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "subtree") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, worklet.RemoveWorkItemTool, args)
}

func (s *served) postRestore(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) || !fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, worklet.RestoreWorkItemTool, args)
}

// postMove is move_work_item: the item and everything under it, re-keyed into
// the project the body names.
//
// THE PERSON'S OWN DOOR ONTO IT. The tool was served to a seat and to the
// operator's assistant, and a person who had filed an item in the wrong
// project could only ask one of them to move it.
func (s *served) postMove(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "project") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, worklet.MoveWorkItemTool, args)
}

// putProject and postProjectTags are write_project's two facets: a project's
// POLICY, which is its lead's, and its TAGS, whose declaring is any
// colleague's. Two routes because the tool's two halves are two records on
// two subjects with two authorities — a request that could carry both would
// be answered for one of them.
func (s *served) putProject(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "fields", "default_assignee", "archived") ||
		!fromPath(w, args, "project", worklet.ProjectKey(r.PathValue("key"))) {
		return
	}
	s.call(w, r, worklet.WriteProjectTool, args)
}

func (s *served) postProjectTags(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "tags_add", "tags_rename", "tags_archive") ||
		!fromPath(w, args, "project", worklet.ProjectKey(r.PathValue("key"))) {
		return
	}
	s.call(w, r, worklet.WriteProjectTool, args)
}

func (s *served) postView(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, worklet.SaveWorkViewTool, args)
}

func (s *served) deleteView(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) || !fromPath(w, args, "id", r.PathValue("id")) {
		return
	}
	s.call(w, r, worklet.DeleteWorkViewTool, args)
}

func (s *served) putCatalogue(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, worklet.WriteWorkCatalogueTool, args)
}

// putPriorities is set_priorities, which names whose queue it sets and asks
// the lead relation itself.
func (s *served) putPriorities(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "items") ||
		!fromPath(w, args, "handle", strings.TrimSpace(r.PathValue("handle"))) {
		return
	}
	s.call(w, r, worklet.SetPrioritiesTool, args)
}

// putInbox and putPins write one person's inbox or pins.
//
// # Through the tools' own parsing and writer, never widening the tools
//
// `mark_inbox` and `set_pins` write the CALLER's record and take no handle,
// deliberately ([builtin.MarkInboxFor]). These routes reach the same parsing
// and writer through [builtin.MarkInboxFor] and [builtin.SetPinsFor], which
// resolve the path's name to its record and decide the verb on THAT (the
// caller's own, or somebody else's for the owner or an admin), under the
// tracker's rule that a SEAT never writes a colleague's record. The tools are
// not widened: no seat or assistant can name another person's record.
func (s *served) putInbox(w http.ResponseWriter, r *http.Request) {
	s.personRecord(w, r, worklet.MarkInboxTool, builtin.MarkInboxFor)
}

func (s *served) putPins(w http.ResponseWriter, r *http.Request) {
	s.personRecord(w, r, worklet.SetPinsTool, builtin.SetPinsFor)
}

// personRecord is the one body both person routes share: the write, through
// the operator surface's dispatch, so it is decided, answered and recorded as
// the tool behind it would be.
func (s *served) personRecord(w http.ResponseWriter, r *http.Request, tool string,
	write operator.RecordWrite) {

	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	if !operator.NoOperationArg(args) {
		operator.RefuseOperationArg(w, tool)
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	handle := strings.TrimSpace(r.PathValue("handle"))
	result, err := s.operator.DispatchRecord(r.Context(), operator.Call{
		Transport: types.TransportWork, Key: key, Tool: tool, Args: args,
	}, handle, write)
	about := operator.About(tool, args)
	about["handle"] = handle
	s.answerDispatched(w, r, key, tool, about, result, err, false)
}

// ---- the gestures no tool makes ---------------------------------------- //

// postRank is place_work_item: one item placed beside a neighbour in its
// project's order, and into another lane where the body names one.
//
// AN ADAPTER, where it used to be a gesture of this surface's own that read
// both neighbours and minted the rank itself: the tool mints the place
// between the neighbours as the board stands when the write LANDS, inside the
// tracker's own decide, so two people dropping cards into one gap cannot be
// handed one key — a rank minted here from a read taken before the write was a
// second implementation of the order, and the one the dashboard did not use.
// The version the board read is the tool's required `if_match`, which an
// `If-Match` header supplies as on every other route.
func (s *served) postRank(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "after", "before", "status", "if_match") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.callIfMatched(w, r, worklet.PlaceWorkItemTool, args, "if_match")
}

// patchItemComment rewrites one remark on a work item, AS ITS AUTHOR.
//
// DECIDED TWICE, AND THE TWO ARE DIFFERENT QUESTIONS: the table answers "may
// you act on this remark" ([authz.ClassAuthored] — its author, or an
// admin), and the tracker's writer answers "may you rewrite it" —
// only its author, because a remark somebody else can rewrite is one
// attributed to a person who did not make it. An administrator is admitted by
// the first and refused by the second, which is internal/doclet's own rule for
// the same gesture.
func (s *served) patchItemComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "body") {
		return
	}
	body, _ := args["body"].(string)
	cid := strings.TrimSpace(r.PathValue("cid"))
	detail, ok := s.readTask(w, r, r.PathValue("key"), worklet.DetailWants{Comment: cid})
	if !ok {
		return
	}
	if len(detail.Comments) == 0 {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "there is no comment " + cid + " on " +
				detail.Named()})
		return
	}
	stored := detail.Comments[0]
	if _, ok = s.decide(w, r, authz.ActionWorkCommentEdit, authz.Object{
		Kind: authz.KindTask, Container: detail.Task.Project, Author: stored.Author,
	}); !ok {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	edited := stored
	edited.Body = strings.TrimSpace(body)
	notify := worklet.Wake{
		Kind: worklet.ChangeCommentEdited, Before: detail.Task, After: detail.Task,
		Comment: &edited, Mentions: edited.Mentions,
	}.Notify(s.Work.Leads)
	opID, ok := keyedOp(w, r, key, "comment-edit", cid, args)
	if !ok {
		return
	}
	result, err := s.Tracker(actor).EditComment(r.Context(), opID, detail.Task.ID,
		detail.Task.Project, cid, body, notify)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionWorkCommentEdit), key, result.Outcome, result.Position, err)
	if err != nil {
		failErr(w, r, err, key)
		return
	}
	answer(w, key, result.Outcome, result.Unvouched, builtin.ReceiptOf(map[string]any{
		"comment_id": cid, "edited": true,
		"outcome": string(result.Outcome), "position": positionOf(result.Position),
		"version": result.Version,
	}, detail))
}

// deleteItemComment takes one remark on a work item down, AS ITS AUTHOR or as
// an admin moderating.
//
// DECIDED TWICE, as an edit is: the table answers "may you act on this remark"
// ([authz.ClassAuthored]: its author, or an admin), and the tracker's writer
// takes the removal from the author, or from a caller the table admitted for
// some other reason as a moderator, and refuses a remark that asks or answers
// ([worklet.Writer.RemoveComment]).
func (s *served) deleteItemComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	cid := strings.TrimSpace(r.PathValue("cid"))
	detail, ok := s.readTask(w, r, r.PathValue("key"), worklet.DetailWants{Comment: cid})
	if !ok {
		return
	}
	if len(detail.Comments) == 0 {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "there is no comment " + cid + " on " +
				detail.Named()})
		return
	}
	stored := detail.Comments[0]
	d, ok := s.decide(w, r, authz.ActionWorkCommentRemove, authz.Object{
		Kind: authz.KindTask, Container: detail.Task.Project, Author: stored.Author,
	})
	if !ok {
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	opID, ok := keyedOp(w, r, key, "comment-remove", cid, args)
	if !ok {
		return
	}
	result, err := s.Tracker(actor).RemoveComment(r.Context(), opID, detail.Task.ID,
		detail.Task.Project, cid, worklet.CommentAuthority{Moderate: d.Reason != authz.ReasonAuthor})
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionWorkCommentRemove), key, result.Outcome, result.Position, err)
	if err != nil {
		failErr(w, r, err, key)
		return
	}
	answer(w, key, result.Outcome, result.Unvouched, builtin.ReceiptOf(map[string]any{
		"comment_id": cid, "removed": true,
		"outcome": string(result.Outcome), "position": positionOf(result.Position),
		"version": result.Version,
	}, detail))
}

// postPurge destroys an item and every row it produced, on every data node.
//
// The confirmation is the item's KEY, CHECKED against the item the path
// resolves to: the path may use an id nobody reads, and repeating the key a
// person sees on the board confirms they looked. The record arbitrates under
// the stored row's project (an id names none): a purge filed under the wrong
// one blocks writes to a project it is not about. A reason is required, as the
// deletion marker's is all that survives. `unknown` is the one outcome to
// retry, under the SAME key: a fresh one would append a second purge.
func (s *served) postPurge(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	switch {
	case confirm == "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "repeat the item's KEY in ?confirm=. " +
				"A purge removes the item and every row it produced on every " +
				"data node, and there is nothing that undoes it"})
		return
	case reason == "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "state why in ?reason=. The rows are " +
				"destroyed and the marker's reason is the only account of them " +
				"that survives"})
		return
	}
	detail, ok := s.readTask(w, r, r.PathValue("key"), worklet.DetailWants{})
	if !ok {
		return
	}
	if !strings.EqualFold(confirm, detail.Task.Key) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q and the "+
				"item is %s, so nothing was destroyed", confirm, detail.Task.Key)})
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.actor(w, r)
	if !ok {
		return
	}
	opID, ok := keyedOp(w, r, key, "purge", detail.Task.ID, purgeArgs(confirm, reason))
	if !ok {
		return
	}
	result, err := s.Tracker(actor).PurgeTask(r.Context(), opID, detail.Task.ID,
		detail.Task.Project, reason)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionWorkPurge), key, result.Outcome, result.Position, err)
	if err != nil {
		log.Warn("api_purge_failed", "task", detail.Task.ID, "actor", actor.Handle,
			"error", err)
		failErr(w, r, err, key)
		return
	}
	// LOGGED AT INFO WITH THE REASON, because this is the one gesture whose
	// subject no longer exists to be inspected afterwards.
	log.Info("task_purged", "task", detail.Task.ID, "key", detail.Task.Key,
		"project", detail.Task.Project, "actor", actor.Handle, "reason", reason,
		"outcome", result.Outcome)
	answer(w, key, result.Outcome, result.Unvouched, map[string]any{
		"task": detail.Task.ID, "key": detail.Task.Key,
		"project": detail.Task.Project, "outcome": string(result.Outcome),
		"position": positionOf(result.Position),
	})
}

// purgeArgs is what a purge asks, as its operation is bound to it ([keyedOp]):
// the two query parameters, since a purge takes no body.
func purgeArgs(confirm, reason string) map[string]any {
	return map[string]any{"confirm": confirm, "reason": reason}
}

// readTask is a work item read before a decision is taken on it.
func (s *served) readTask(w http.ResponseWriter, r *http.Request, ref string,
	want worklet.DetailWants) (worklet.TaskDetail, bool) {

	detail, err := s.Work.Reader.Task(r.Context(), strings.TrimSpace(ref),
		want, decisionRead)
	if err != nil {
		readFailed(w, r, err)
		return worklet.TaskDetail{}, false
	}
	return detail, true
}
