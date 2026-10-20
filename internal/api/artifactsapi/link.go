package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// LINKING AN ARTIFACT THAT ALREADY EXISTS TO A TRACKER WORK ITEM, so the item's
// Artifacts panel lists it exactly as it lists one created or published for the
// item: a design made first, and a task filed for it afterwards, end up on one
// card. The domain records the link as a history row filed under the item
// ([artifactsdomain.OpLinkWorkItem]); this surface decides who may make it.

// WorkItems is the company's own tracker as a link reads it: one item, by key
// or by id, through the estate router, so a node holding no data reads it
// through a data node.
type WorkItems interface {
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		fresh statelog.Freshness) (tracker.TaskDetail, error)
}

// ErrTrackerNotUp is a node not yet handed a company, so the company's own
// tracker is not running here: it comes up with the first revision.
var ErrTrackerNotUp = errors.New("artifactsapi: the company's own tracker is not running here yet")

// ErrNoTracker is a company whose work items are not kept by the engine's own
// tracker, so there is no item here an artifact could be linked to.
var ErrNoTracker = errors.New("artifactsapi: this company keeps no work items in the engine's tracker")

// linkBody names the work item an artifact is linked to: its key (`ENG-12`),
// or its id.
type linkBody struct {
	WorkItem string `json:"work_item"`
}

// linkWorkItem links the path's artifact to a work item (`POST
// /artifacts/{id}/work-items`), answering the item's key as linked.
//
// # Two decisions, each the one its own surface takes
//
// The ARTIFACT is admitted as a read, through [Gate.Read], which is how
// `get_artifact` admits one: any role on it, viewer and above, a caller with no
// relation answered exactly as for an artifact nothing holds, and the admin
// path's override recorded before anything about the artifact is answered. A
// link changes nothing a viewer could not already see; it says which work the
// artifact belongs to.
//
// The ITEM is admitted as a write, `update_work_item` on a task, decided by the
// authority table's own row ([authz.Decide]) exactly as `PATCH
// /work/items/{key}` and the tool decide it: putting something on an item's
// card is the item's writers' to do, whatever the caller's role on the
// artifact.
//
// # The item must be one the tracker holds, and live
//
// Another log, read here before the record is formed: an item nobody filed is
// refused, and so is a removed one, which the tracker holds frozen until it is
// restored. The key the tracker answers is the one linked, so an id or a former
// key links under the item's current key, the one its panel lists by.
//
// A link the artifact's history already makes answers success and writes
// nothing (the domain's rule), so a retry under a fresh key is harmless.
func (s *Service) linkWorkItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, ok := caller(w, r, authz.ActionArtifactRead)
	if !ok {
		return
	}
	a, err := s.gate.Read(r.Context(), p, id, 0)
	if err != nil {
		refuseAdmission(w, r, authz.ActionArtifactRead, err)
		return
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return
	}
	var body linkBody
	if !decode(w, r, &body, false) {
		return
	}
	ref, err := checkWorkItem(body.WorkItem)
	if err != nil {
		invalid(w, err.Error())
		return
	}
	if ref == "" {
		invalid(w, "name the work item to link the artifact to, by its key")
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	if !s.mayUpdateWorkItem(w, r, p) {
		return
	}
	item, ok := s.liveWorkItem(w, r, ref)
	if !ok {
		return
	}
	c, err := a.call(originOf(p), s.gate.limits(), key, item.Key)
	if err != nil {
		unavailable(w, err, "this node could not settle your role on the artifact")
		return
	}
	out, err := s.writes.LinkWorkItem(r.Context(), c, a.Head.ID, item.Key)
	if err != nil {
		writeFailed(w, r, key, "link the artifact to the work item", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"artifact": a.Head.ID.String(), "work_item": item.Key})
}

// mayUpdateWorkItem decides `update_work_item` for p on a task, rendering a
// refusal or an undecidable answer itself. It reports whether p may.
func (s *Service) mayUpdateWorkItem(w http.ResponseWriter, r *http.Request, p iam.Principal) bool {
	d := authz.Decide(r.Context(), p, authz.ActionWorkUpdate, authz.Object{Kind: authz.KindTask},
		s.gate.Chart, s.gate.now())
	switch {
	case d.Unknown():
		unavailable(w, d.Err, "this node could not decide whether you may change work items")
		return false
	case !d.Allowed:
		refuseDecision(w, r, authz.ActionWorkUpdate, d)
		return false
	}
	return true
}

// liveWorkItem is the work item ref names, read from the company's own
// tracker, refusing an item nobody filed and a removed one. It renders every
// refusal itself and reports whether the link goes on.
func (s *Service) liveWorkItem(w http.ResponseWriter, r *http.Request, ref string) (tracker.Task, bool) {
	items, err := s.workItems()
	switch {
	case errors.Is(err, ErrTrackerNotUp):
		httpjson.NoActiveRevision(w, httpjson.Detail{"detail": httpjson.NativeHalvesNotUp})
		return tracker.Task{}, false
	case errors.Is(err, ErrNoTracker):
		invalid(w, "this company's work items are not kept in Crewlet's tracker, so there is "+
			"no work item here to link the artifact to")
		return tracker.Task{}, false
	case err != nil:
		unavailable(w, err, "this node could not reach the company's tracker; try again")
		return tracker.Task{}, false
	}
	detail, err := items.Task(r.Context(), ref, tracker.DetailWants{}, decisionRead)
	switch {
	case errors.Is(err, tracker.ErrNoTask):
		invalid(w, fmt.Sprintf("there is no work item %s", ref))
		return tracker.Task{}, false
	case err != nil:
		log.WarnContext(r.Context(), "artifacts_work_item_unread", "work_item", ref, "error", err.Error())
		unavailable(w, err, "the tracker could not be read to check the work item; try again")
		return tracker.Task{}, false
	case detail.Task.Removed != nil:
		invalid(w, fmt.Sprintf("work item %s was removed; restore it before linking an artifact to it",
			detail.Task.Key))
		return tracker.Task{}, false
	}
	return detail.Task, true
}
