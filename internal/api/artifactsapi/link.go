package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// LINKING AN ARTIFACT THAT ALREADY EXISTS TO A TRACKER WORK ITEM, so the item's
// Artifacts panel lists it exactly as it lists one created or published for the
// item: a design made first, and a task filed for it afterwards, end up on one
// card. The domain records the link as a history row filed under the item
// ([artifactsdomain.OpLinkWorkItem]); this surface decides who may make it.

// WorkItems is Worklet as a link reads it: one item, by key or by id, through
// the estate router, so a node holding no data reads it through a data node.
type WorkItems interface {
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
}

// ErrWorkletNotUp is a node not yet handed a company, so Worklet is not
// running here: it comes up with the first revision.
var ErrWorkletNotUp = errors.New("artifactsapi: Worklet is not running here yet")

// ErrNoWorklet is a company that does not run Worklet, so there is no item
// here an artifact could be linked to.
var ErrNoWorklet = errors.New("artifactsapi: this company does not run Worklet")

// linkBody names the work item an artifact is linked to: its key (`ENG-12`),
// or its id, and where the caller's own create of that item landed.
type linkBody struct {
	WorkItem string `json:"work_item"`

	// After is the tracker log position the caller's create of the item
	// answered with (`<stream>@<generation>:<sequence>`, the act's
	// `position`), empty when the caller filed nothing. The item is read
	// from no earlier than it ([linkFreshness]).
	After string `json:"after"`
}

// linkWorkItem links the path's artifact to a work item (`POST
// /artifacts/{id}/work-items`), answering the item's key as linked.
//
// The ARTIFACT is admitted as a read ([Gate.Read]), the ITEM as a write
// (`update_work_item` on a task, [authz.Decide]). The item is read from the
// tracker before the record is formed: one nobody filed or removed is refused;
// an id or former key links under its current key. The dashboard files a task
// and links it in one gesture, and on a fleet the link can reach a node that
// has not applied the create. So the caller hands back the create's position
// as `after`, read through the tracker's read-your-writes floor
// ([statelog.Freshness]'s Floors): it waits up to the read budget, else is
// refused `behind`. A repeat link writes nothing.
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
	fresh, ok := linkFreshness(w, body.After)
	if !ok {
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	if !s.mayUpdateWorkItem(w, r, p) {
		return
	}
	item, ok := s.liveWorkItem(w, r, ref, fresh)
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
		operator.RefuseDecision(w, r, authz.ActionWorkUpdate, d)
		return false
	}
	return true
}

// linkFreshness is the read a link resolves its item at: the level every
// decision here reads at ([decisionRead]), floored at after where the caller
// named a position. It renders a refusal of after itself and reports whether
// the link goes on.
//
// THE FLOOR IS THE TRACKER'S OWN, not a wait of this surface's: a position is
// parsed by the grammar every read's `min_position` is ([statelog.ParsePosition])
// and honoured by the tracker's reader, which also refuses one on another log
// ([statelog.ErrForeignPosition]) before it waits for anything, because only
// the reader knows which log it reads.
func linkFreshness(w http.ResponseWriter, after string) (statelog.Freshness, bool) {
	fresh := decisionRead
	raw := strings.TrimSpace(after)
	if raw == "" {
		return fresh, true
	}
	at, err := statelog.ParsePosition(raw)
	if err != nil {
		invalid(w, "after is not a log position: send the position the work item's create "+
			"answered with, in the form <stream>@<generation>:<sequence>")
		return statelog.Freshness{}, false
	}
	fresh.Floors = []statelog.Position{at}
	return fresh, true
}

// liveWorkItem is the work item ref names, read from Worklet at fresh,
// refusing an item nobody filed and a removed one. It renders every refusal
// itself and reports whether the link goes on.
//
// A READ THE TRACKER REFUSED (a floor this node did not reach within the read
// budget, or any other state-log refusal) is answered as the tracker's own
// reads answer it over REST ([stream.UnavailableOf]): `503 unavailable`
// carrying the refusal's code and words, and the Retry-After its own hint
// derives, absent where waiting cannot clear it. A fixed sentence here told a
// caller whose create had not landed yet nothing about why, or when to send
// the link again.
func (s *Service) liveWorkItem(w http.ResponseWriter, r *http.Request, ref string,
	fresh statelog.Freshness) (worklet.Task, bool) {
	items, err := s.workItems()
	switch {
	case errors.Is(err, ErrWorkletNotUp):
		httpjson.NoActiveRevision(w, httpjson.Detail{"detail": httpjson.NativeHalvesNotUp})
		return worklet.Task{}, false
	case errors.Is(err, ErrNoWorklet):
		invalid(w, "this company does not run Worklet, so there is no work item "+
			"here to link the artifact to")
		return worklet.Task{}, false
	case err != nil:
		unavailable(w, err, "this node could not reach Worklet; try again")
		return worklet.Task{}, false
	}
	detail, err := items.Task(r.Context(), ref, worklet.DetailWants{}, fresh)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		invalid(w, fmt.Sprintf("there is no work item %s", ref))
		return worklet.Task{}, false
	case errors.Is(err, statelog.ErrForeignPosition):
		invalid(w, fmt.Sprintf("after names a position on another log; a work item's create "+
			"answers with a position on Worklet's log, %s, and that is the one to send",
			worklet.Domain{}.Stream().Name))
		return worklet.Task{}, false
	case errors.As(err, new(*statelog.Refused)):
		operator.LogCondition(r.Context(), log, "artifacts_work_item_unread", err,
			"work_item", ref)
		u := stream.UnavailableOf(err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, u.RetryAfter, u.Fields())
		return worklet.Task{}, false
	case err != nil:
		log.WarnContext(r.Context(), "artifacts_work_item_unread", "work_item", ref, "error", err.Error())
		unavailable(w, err, "Worklet could not be read to check the work item; try again")
		return worklet.Task{}, false
	case detail.Task.Removed != nil:
		invalid(w, fmt.Sprintf("work item %s was removed; restore it before linking an artifact to it",
			detail.Task.Key))
		return worklet.Task{}, false
	}
	return detail.Task, true
}
