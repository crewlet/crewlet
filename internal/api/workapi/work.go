package workapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// workRoutes mounts the tracker half.
func (s *Service) workRoutes(mount mounter) {
	task := func(a authz.Action) authz.Policy { return kinded(a, authz.KindTask) }
	// A PROJECT IS NAMED BY ITS PATH, so its routes decide the verb there.
	project := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindProject,
				Container: tracker.ProjectKey(r.PathValue("key"))}
		}}
	}
	// SO IS A PERSON, and the record is keyed on the SEAT handle — which is
	// what the own-record classes compare the caller's seat against.
	person := func(a authz.Action) authz.Policy {
		return authz.Policy{Action: a, Object: func(r *http.Request) authz.Object {
			return authz.Object{Kind: authz.KindPerson,
				Owner: strings.TrimSpace(r.PathValue("handle"))}
		}}
	}

	mount("POST /work/items", task(authz.ActionWorkCreate), s.postItem)
	mount("PATCH /work/items/{key}", task(authz.ActionWorkUpdate), s.patchItem)
	mount("POST /work/items/{key}/comments", task(authz.ActionWorkComment),
		s.postItemComment)
	// WHO WROTE THE REMARK IS A ROW, so the route admits the colleague
	// write that commenting is and the handler decides the edit once it
	// has read the author.
	mount("PATCH /work/items/{key}/comments/{cid}", task(authz.ActionWorkComment),
		s.patchItemComment)
	mount("POST /work/items/{key}/rank", task(authz.ActionWorkRank), s.postRank)
	// DEPENDING AND RELATING ARE EDITS OF THE ITEM, made through
	// update_work_item's own set-valued arguments; the routes are narrower
	// doors onto the same verb.
	mount("POST /work/items/{key}/depend", task(authz.ActionWorkUpdate), s.postDepend)
	mount("POST /work/items/{key}/relate", task(authz.ActionWorkUpdate), s.postRelate)
	// THE TRASH IS THE ITEM'S PROJECT'S LEAD'S, and which project that is
	// comes out of the row: the route admits a reader of the board and the
	// tool decides the verb once it has read it.
	mount("DELETE /work/items/{key}", task(authz.ActionWorkRead), s.deleteItem)
	mount("POST /work/items/{key}/restore", task(authz.ActionWorkRead), s.postRestore)
	// THE PURGE NEEDS NO ROW TO DECIDE: it is the fleet's grant and never
	// an agent's, whatever the item is.
	mount("POST /work/items/{key}/purge", authz.Policy{Action: authz.ActionWorkPurge},
		s.postPurge)
	mount("PUT /work/projects/{key}", project(authz.ActionProjectWrite), s.putProject)
	mount("POST /work/projects/{key}/tags", project(authz.ActionProjectWrite),
		s.postProjectTags)
	// A VIEW'S AUTHORITY IS IN ITS BODY — personal to its owner or shared on
	// its container — so the route admits a reader of the views and the
	// tool's own gate decides the save on what the body says.
	mount("POST /work/views", kinded(authz.ActionViewList, authz.KindView), s.postView)
	mount("PUT /work/catalogue", authz.Policy{Action: authz.ActionCatalogueWrite},
		s.putCatalogue)
	mount("PUT /work/people/{handle}/inbox", person(authz.ActionInboxMark), s.putInbox)
	mount("PUT /work/people/{handle}/pins", person(authz.ActionPinsSet), s.putPins)
	mount("PUT /work/people/{handle}/priorities", person(authz.ActionPrioritiesSet),
		s.putPriorities)
}

// ---- the tool-backed routes -------------------------------------------- //

func (s *Service) postItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, tracker.CreateWorkItemTool, args)
}

func (s *Service) patchItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "item", r.PathValue("key")) ||
		!ifMatch(w, r, args, "if_match") {
		return
	}
	s.call(w, r, tracker.UpdateWorkItemTool, args)
}

func (s *Service) postItemComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, tracker.CommentOnWorkTool, args)
}

func (s *Service) postDepend(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "waiting_on", "blocking", "dependency_note", "if_match") ||
		!fromPath(w, args, "item", r.PathValue("key")) ||
		!ifMatch(w, r, args, "if_match") {
		return
	}
	s.call(w, r, tracker.UpdateWorkItemTool, args)
}

func (s *Service) postRelate(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "linked", "linked_pages", "if_match") ||
		!fromPath(w, args, "item", r.PathValue("key")) ||
		!ifMatch(w, r, args, "if_match") {
		return
	}
	s.call(w, r, tracker.UpdateWorkItemTool, args)
}

func (s *Service) deleteItem(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "subtree") ||
		!fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, tracker.RemoveWorkItemTool, args)
}

func (s *Service) postRestore(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) || !fromPath(w, args, "item", r.PathValue("key")) {
		return
	}
	s.call(w, r, tracker.RestoreWorkItemTool, args)
}

// putProject and postProjectTags are write_project's two facets: a project's
// POLICY, which is its lead's, and its TAGS, whose declaring is any
// colleague's. Two routes because the tool's two halves are two records on
// two subjects with two authorities — a request that could carry both would
// be answered for one of them.
func (s *Service) putProject(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "fields", "default_assignee", "archived") ||
		!fromPath(w, args, "project", tracker.ProjectKey(r.PathValue("key"))) {
		return
	}
	s.call(w, r, tracker.WriteProjectTool, args)
}

func (s *Service) postProjectTags(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "tags_add", "tags_rename", "tags_archive") ||
		!fromPath(w, args, "project", tracker.ProjectKey(r.PathValue("key"))) {
		return
	}
	s.call(w, r, tracker.WriteProjectTool, args)
}

func (s *Service) postView(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, tracker.SaveWorkViewTool, args)
}

func (s *Service) putCatalogue(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	s.call(w, r, tracker.WriteWorkCatalogueTool, args)
}

// putPriorities is set_priorities, which names whose queue it sets and asks
// the lead relation itself.
func (s *Service) putPriorities(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "items") ||
		!fromPath(w, args, "handle", strings.TrimSpace(r.PathValue("handle"))) {
		return
	}
	s.call(w, r, tracker.SetPrioritiesTool, args)
}

// putInbox and putPins write one person's inbox or pins.
//
// # Yours through the tool, somebody else's through the same writer
//
// `mark_inbox` and `set_pins` write the CALLER's record and take no handle,
// deliberately — see [builtin.MarkInboxFor]. So a request about the caller's
// own record goes through the tool exactly as their assistant's would, and a
// request about SOMEBODY ELSE's — which the route has already decided on the
// record its path names, and which only the owner-or-admin class admits —
// goes through the same parsing and the same writer with the table's answer
// as its authority. The tools are not widened: no seat, and no assistant,
// gains a way to name another person's record.
func (s *Service) putInbox(w http.ResponseWriter, r *http.Request) {
	s.personRecord(w, r, tracker.MarkInboxTool, builtin.MarkInboxFor)
}

func (s *Service) putPins(w http.ResponseWriter, r *http.Request) {
	s.personRecord(w, r, tracker.SetPinsTool, builtin.SetPinsFor)
}

// personRecord is the one body both person routes share.
func (s *Service) personRecord(w http.ResponseWriter, r *http.Request, verb string,
	forOther func(ctx context.Context, deps builtin.WorkDeps, handle string,
		args map[string]any, authority tracker.PersonAuthority) tools.Result) {

	args, ok := readArgs(w, r)
	if !ok {
		return
	}
	handle := strings.TrimSpace(r.PathValue("handle"))
	principal, _ := iam.From(r.Context())
	// EITHER OF THE CALLER'S OWN NAMES IS THEIR OWN RECORD, which the tool
	// writes under the one name it is kept under ([iam.RecordOwner]). A
	// bound person naming their LOGIN here was sent down the other path and
	// wrote a record under that login — decided as theirs, and read back by
	// nothing they look at.
	if iam.NamesSelf(principal, handle) {
		s.call(w, r, verb, args)
		return
	}
	key := operationKey(r)
	work, _ := s.deps(key)
	// THE TABLE'S ANSWER, which the route took on this record before the
	// handler ran — the owner, or the admin path — and the tracker's own
	// rule on top of it: a SEAT never writes a colleague's record, whatever
	// the table said, so an agent's authority is marked as such.
	authority := tracker.PersonAuthority{
		Authorized: true, Agent: principal.Kind == iam.KindSeat,
	}
	answerTool(w, key, forOther(r.Context(), work, handle, args, authority))
}

// ---- the gestures no tool makes ---------------------------------------- //

// postRank places one item between two neighbours in its project's order.
//
// # Why this is not a tool
//
// A rank move is a gesture on a BOARD a person is looking at: it names two
// neighbours by what is on either side of the card being dropped. A seat sees
// no board and has no neighbours to name, so it is given no such verb and the
// order it needs is the priority list instead.
//
// # The neighbours name the project
//
// Both neighbours must be in the item's own project, because the order is the
// project's: a card cannot be placed between two cards on somebody else's
// board. Naming neither is refused rather than read as "the head", because a
// move to nowhere is a request somebody built wrong.
func (s *Service) postRank(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "after", "before") {
		return
	}
	item, ok := s.readTask(w, r, r.PathValue("key"), tracker.DetailWants{})
	if !ok {
		return
	}
	var bounds [2]tracker.Rank
	named := 0
	for i, field := range []string{"after", "before"} {
		ref, _ := args[field].(string)
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		named++
		neighbour, ok := s.readTask(w, r, ref, tracker.DetailWants{})
		if !ok {
			return
		}
		if neighbour.Task.Project != item.Task.Project {
			httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeRefused,
				map[string]string{"detail": fmt.Sprintf("%s is in %s and %s is "+
					"in %s — an item is placed among its own project's items",
					neighbour.Task.Key, neighbour.Task.Project, item.Task.Key,
					item.Task.Project)})
			return
		}
		bounds[i] = neighbour.Task.Rank
	}
	if named == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "name the item this one goes `after`, " +
				"the one it goes `before`, or both"})
		return
	}
	key := operationKey(r)
	actor, ok := s.actor(w, r, key)
	if !ok {
		return
	}
	result, err := s.tracker(actor).MoveTask(r.Context(),
		"rank-"+item.Task.ID+"-"+key, item.Task.Project, item.Task.ID,
		bounds[0], bounds[1])
	if err != nil {
		failErr(w, err, key)
		return
	}
	answer(w, key, result.Outcome, map[string]any{
		"item": item.Task.Key, "outcome": string(result.Outcome),
		"position": positionOf(result.Position),
	})
}

// patchItemComment rewrites one remark on a work item, AS ITS AUTHOR.
//
// DECIDED TWICE, AND THE TWO ARE DIFFERENT QUESTIONS: the table answers "may
// you act on this remark" ([authz.ClassAuthored] — its author, or the
// deployment grant), and the tracker's writer answers "may you rewrite it" —
// only its author, because a remark somebody else can rewrite is one
// attributed to a person who did not make it. An administrator is admitted by
// the first and refused by the second, which is internal/pages' own rule for
// the same gesture.
func (s *Service) patchItemComment(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "body") {
		return
	}
	body, _ := args["body"].(string)
	cid := strings.TrimSpace(r.PathValue("cid"))
	detail, ok := s.readTask(w, r, r.PathValue("key"), tracker.DetailWants{Comment: cid})
	if !ok {
		return
	}
	if len(detail.Comments) == 0 {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "there is no comment " + cid + " on " +
				detail.Task.Key})
		return
	}
	stored := detail.Comments[0]
	if _, ok := s.decide(w, r, authz.ActionWorkCommentEdit, authz.Object{
		Kind: authz.KindTask, Container: detail.Task.Project, Author: stored.Author,
	}); !ok {
		return
	}
	key := operationKey(r)
	actor, ok := s.actor(w, r, key)
	if !ok {
		return
	}
	edited := stored
	edited.Body = strings.TrimSpace(body)
	notify := tracker.Wake{
		Kind: tracker.ChangeCommentEdited, Before: detail.Task, After: detail.Task,
		Comment: &edited, Mentions: edited.Mentions,
	}.Notify(s.workDeps.Leads)
	result, err := s.tracker(actor).EditComment(r.Context(),
		"comment-edit-"+cid+"-"+key, detail.Task.ID, detail.Task.Project, cid,
		body, notify)
	if err != nil {
		failErr(w, err, key)
		return
	}
	answer(w, key, result.Outcome, map[string]any{
		"item": detail.Task.Key, "comment_id": cid, "edited": true,
		"outcome": string(result.Outcome), "position": positionOf(result.Position),
		"version": result.Version,
	})
}

// postPurge destroys an item and every row it produced, on every node.
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
// the one place that says which that is: a key's prefix names where the item
// was FILED, which a move leaves behind as an alias. The route this replaced
// took it as a parameter, and a purge filed under the wrong one blocks writes
// to a project it is not about.
//
// # A reason is required, and a retry reuses the operation
//
// The reason is the only thing that survives: the rows are gone and the
// deletion marker's reason is the whole account of what was at that key. And
// `unknown` is the one outcome to retry — under the SAME key, because a fresh
// one would append a second purge of an item the first may already have
// destroyed.
func (s *Service) postPurge(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	switch {
	case confirm == "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "repeat the item's KEY in ?confirm= — " +
				"a purge removes the item and every row it produced on every " +
				"node, and there is nothing that undoes it"})
		return
	case reason == "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "state why in ?reason= — the rows are " +
				"destroyed and the marker's reason is the only account of them " +
				"that survives"})
		return
	}
	detail, ok := s.readTask(w, r, r.PathValue("key"), tracker.DetailWants{})
	if !ok {
		return
	}
	if !strings.EqualFold(confirm, detail.Task.Key) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeRefused,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q and the "+
				"item is %s — nothing was destroyed", confirm, detail.Task.Key)})
		return
	}
	key := operationKey(r)
	actor, ok := s.actor(w, r, key)
	if !ok {
		return
	}
	result, err := s.tracker(actor).PurgeTask(r.Context(),
		"purge-"+detail.Task.ID+"-"+key, detail.Task.ID, detail.Task.Project, reason)
	if err != nil {
		log.Warn("api_purge_failed", "task", detail.Task.ID, "actor", actor.Handle,
			"error", err)
		failErr(w, err, key)
		return
	}
	// LOGGED AT INFO WITH THE REASON, because this is the one gesture whose
	// subject no longer exists to be inspected afterwards.
	log.Info("task_purged", "task", detail.Task.ID, "key", detail.Task.Key,
		"project", detail.Task.Project, "actor", actor.Handle, "reason", reason,
		"outcome", result.Outcome)
	answer(w, key, result.Outcome, map[string]any{
		"task": detail.Task.ID, "key": detail.Task.Key,
		"project": detail.Task.Project, "outcome": string(result.Outcome),
		"position": positionOf(result.Position),
	})
}

// readTask is a work item read before a decision is taken on it.
func (s *Service) readTask(w http.ResponseWriter, r *http.Request, ref string,
	want tracker.DetailWants) (tracker.TaskDetail, bool) {

	detail, err := s.workDeps.Reader.Task(r.Context(), strings.TrimSpace(ref),
		want, decisionRead)
	if err != nil {
		readFailed(w, err)
		return tracker.TaskDetail{}, false
	}
	return detail, true
}
