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
	// A PERSON IS NAMED BY THE PATH AND IS NOT THE PATH. `{handle}` is a
	// name — a seat's handle, a person's or a machine's login — and the
	// record it addresses is only known once the identity directory has
	// said whose a login is: a bound person's login names their SEAT's
	// record. Decided here on the spelling, a lead naming their report by
	// login was refused as leading nobody, and an administrator was
	// admitted to a record under the login that nothing of that person's
	// reads — which is where the write then landed.
	//
	// So the route admits on the weakest honest precondition — the verb
	// on the CALLER's own record, which every principal that has one and
	// may act at all is admitted to — and the verb is decided once the
	// name has resolved, by the same function a tool and the query surface
	// resolve it through (builtin's person verbs, over [iam.OwnerOf]).
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
	if !ok || !only(w, args, "linked", "linked_pages", "if_match") ||
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
// deliberately — see [builtin.MarkInboxFor]. So these routes reach the same
// parsing and the same writer through [builtin.MarkInboxFor] and
// [builtin.SetPinsFor], which resolve the path's name to the record it
// addresses and decide the verb on THAT — the caller's own by either of their
// names, somebody else's for the owner or an admin — and the tracker's
// own rule sits on top: a SEAT never writes a colleague's record. The tools
// are not widened: no seat, and no assistant, gains a way to name another
// person's record.
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

// postPurge destroys an item and every row it produced, on every data node.
//
// # The confirmation is the item's KEY, and it is CHECKED
//
// The path may name the item by its id, which is a uuid nobody reads; the key
// is what a person sees on the board and in the ticket they were asked to act
// on, so repeating it is the confirmation that they looked. It is compared
// against the item the path resolves to — the route this replaced echoed it
// back unread, so a confirmation naming a different item confirmed nothing.
//
// # The project is the row's
//
// The record arbitrates under the item's own project, and the stored row is
// the one place that says which that is: the path may name the item by its
// id, which names no project. The route this replaced took it as a
// parameter, and a purge filed under the wrong one blocks writes to a project
// it is not about.
//
// # A reason is required, and a retry reuses the operation
//
// The reason is the only thing that survives: the rows are gone and the
// deletion marker's reason is the whole account of what was at that key. And
// `unknown` is the one outcome to retry — under the SAME key, because a fresh
// one would append a second purge of an item the first may already have
// destroyed.
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
