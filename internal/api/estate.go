package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE OPERATOR'S GESTURES ON THE ESTATE MAP, over HTTP for the object map's
// reason: the map is one record in the coordination store, which on the
// default topology is the engine's own embedded broker and binds no socket, so
// `crewlet estate out`, `in`, `hold`, `release` and `move` are clients of these
// routes.
//
//   - POST /estate/out/{node}?confirm={node}&reason= takes a data node out of
//     every partition's target: what it holds is rebuilt on the others while
//     it keeps serving, and then released.
//   - POST /estate/in/{node}?confirm={node} puts it back — or vouches for a
//     node the map removed for absence.
//   - POST /estate/hold?for={duration}&confirm={generation}&reason= holds the
//     map, at most a day: no member is removed for absence while it holds.
//   - POST /estate/release?confirm={generation} ends the hold.
//   - POST /estate/move/{partition}?from={node}&confirm={node}&reason= moves one
//     partition's copy off one node: it is rebuilt on the member the
//     partition's ranking offers next, and then released.
//   - POST /estate/move/{partition}/cancel?from={node}&confirm={node} lifts it.
//
// EVERY GESTURE IS CONFIRMED. The four that move a node's copies repeat the
// node, the shape every gesture here that moves data takes, and the route
// checks it before anything is asked. A hold and a release name no node and
// act on the whole map, so they repeat the map's GENERATION, which GET /estate
// names — what says the operator looked at this fleet's map rather than
// another's reached through the wrong node. That one is NOT checked here but
// passed through as typed and judged inside the engine's compare-and-set
// ([engine.ErrEstateUnconfirmed], [engine.ErrEstateOtherMap]): a generation is
// something only a map has, so where there is none — every fleet at layout 0
// — the answer has to be the fleet's own, which no confirmation could ever
// reach if the route asked for one first.
//
// Each is a read, a pure gesture and a compare-and-set in the engine
// ([engine.EstateControl]), answered with whether the map now says what was
// asked — `landed` — and what it says. Under layout 0 there is no map, and
// every gesture is refused `estate_whole` in the words every surface gives that
// layout. POSTs, so the anonymous-read posture never reaches them, and refused
// during a shutdown drain like every other write.

// EstateControl is the estate map's seam — see [engine.EstateControl]: its
// gestures, and the read the estate question renders.
//
// ONE SEAM FOR THE READ AND THE GESTURES, the fleet broker's rule: one wiring
// serves the question and the routes, so a node cannot answer GET /estate from
// one map and apply a gesture to another.
//
// EXPORTED for [ObjectsControl]'s reason: the caller converts a typed nil to a
// genuine one ([EngineEstate]), and a nil seam leaves the routes UNMOUNTED and
// the question unregistered — a node with no coordination store has no map.
type EstateControl interface {
	queries.EstateReader

	Out(ctx context.Context, node, by, reason string) (engine.EstateGesture, error)
	In(ctx context.Context, node, by string) (engine.EstateGesture, error)
	Hold(ctx context.Context, confirm string, d time.Duration, by, reason string) (engine.EstateGesture, error)
	Release(ctx context.Context, confirm, by string) (engine.EstateGesture, error)
	Move(ctx context.Context, p statelog.PartitionID, node, by, reason string) (engine.EstateGesture, error)
	CancelMove(ctx context.Context, p statelog.PartitionID, node, by string) (engine.EstateGesture, error)
}

// EngineEstate is the gesture routes' reach into a running engine, or nil where
// it has no coordination store. NIL AS AN INTERFACE rather than an interface
// holding a nil control, which [App] would read as a surface and mount.
func EngineEstate(e *engine.Engine) EstateControl {
	if e == nil {
		return nil
	}
	if c := e.EstateControl(); c != nil {
		return c
	}
	return nil
}

// EstateAnswer is what a gesture route answers once the gesture ran: whether
// the stored map now says what was asked, and what it says about it.
//
// # One typed value, like [ObjectsAnswer]
//
// The routes render through [RenderEstateGesture] and nothing else; the
// command line's tests serve what it returns, and the dashboard's suite reads
// the golden this package's test writes from it
// (`internal/api/testdata/estate_answer.json`).
type EstateAnswer struct {
	// Landed is whether the stored map says what the gesture asked —
	// written by it, or already so. False only when every attempt lost its
	// compare-and-set to another writer, and Hint says what to do.
	Landed bool `json:"landed"`

	// Epoch is the map's epoch after the gesture. NO GESTURE MOVES IT: the
	// epoch counts the holder table, and a gesture changes the targets —
	// the maintainer's next tick moves the holders toward them.
	Epoch uint64 `json:"epoch"`

	// Generation is the map's lineage, which a hold and a release are
	// confirmed by.
	Generation uuid.UUID `json:"generation"`

	// Node is the node an out, an in, a move or a cancel named, and Member
	// that member as the map now describes it — absent when the map does
	// not hold it, which is what an in answers for a node the map had
	// removed.
	Node   string             `json:"node,omitempty"`
	Member *queries.MapMember `json:"member,omitempty"`

	// Partition is the partition a move or a cancel named; Move the move
	// of it off Node in force now, absent after a cancel; and Target the
	// nodes it should be held by now — where a moved copy is rebuilt.
	Partition string              `json:"partition,omitempty"`
	Move      *queries.EstateMove `json:"move,omitempty"`
	Target    []string            `json:"target,omitempty"`

	// Hold is the hold in force now, absent when there is none.
	Hold *queries.MapHold `json:"hold,omitempty"`

	// Hint is the sentence saying what to do next, where there is
	// something to do — in no surface's vocabulary.
	Hint string `json:"hint,omitempty"`
}

// RenderEstateGesture is the one rendering of a gesture's result — see
// [EstateAnswer]. node is the node an out, an in, a move or a cancel named,
// and partition the partition a move or a cancel named — empty where the
// gesture named none; now is what the hold is judged against.
func RenderEstateGesture(g engine.EstateGesture, node, partition string, now time.Time) EstateAnswer {
	m := g.State.Map
	out := EstateAnswer{
		Landed: g.Landed, Epoch: m.Epoch, Generation: m.Generation, Node: node,
		Partition: partition, Hold: queries.RenderHold(g.State.State, now),
	}
	if node != "" {
		if member, ok := queries.RenderMember(g.State.State, m.Draw(), node); ok {
			out.Member = &member
		}
	}
	if partition != "" {
		if p, err := statelog.ParsePartitionID(partition); err == nil {
			out.Move = queries.RenderEstateMove(m, p, node)
			out.Target = m.Target(p)
		}
	}
	switch {
	case !g.Landed:
		// A LOST RACE IS NOT A REFUSAL AND NOT A FAULT: the map moved
		// under every attempt, and what is rendered above is what it says
		// now, which is what the next gesture is decided against.
		out.Hint = "the estate map changed under every attempt, so this gesture is " +
			"not in it: read the map as it stands and make the gesture again if it " +
			"still applies"
	case node != "" && partition == "" && out.Member == nil:
		// AN IN THAT VOUCHED FOR A NODE THE MAP HAD REMOVED: it is not a
		// member yet, and a reader shown no member would take the gesture
		// for one that did nothing.
		out.Hint = "not a member of the map now: the map places partitions on it " +
			"again the next time its maintainer sees it present and healthy"
	}
	return out
}

// EstateRefusal is a gesture refused with nothing written: the status and the
// body the route answers with.
type EstateRefusal struct {
	Status int
	Body   EstateRefusalBody
}

// EstateRefusalBody is every refusal's body: the code a caller branches on,
// the sentence saying what is wrong, and the one saying what to do.
type EstateRefusalBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
}

// RenderEstateRefusal renders err if it is one of the gestures' refusals,
// reporting false for anything else — a failure the route answers `500
// estate_failed`.
//
// EACH SENDS AN OPERATOR SOMEWHERE DIFFERENT, which is why each has its own
// code: a fleet at layout 0 has nothing to move and never will, a partitioned
// fleet with no map yet waits for one, a name the map does not hold is a typo,
// a move with nowhere to rebuild the copy needs a data node first, a node the
// map removed is put back rather than taken out, a hold or a release confirmed
// by no generation at all is made again with the one the map names, one
// confirmed for another map was sent to the wrong fleet, and a store that did
// not answer is asked again.
func RenderEstateRefusal(err error) (EstateRefusal, bool) {
	refuse := func(status int, code, hint string) (EstateRefusal, bool) {
		return EstateRefusal{Status: status, Body: EstateRefusalBody{
			Error: code, Detail: err.Error(), Hint: hint,
		}}, true
	}
	switch {
	case err == nil:
		return EstateRefusal{}, false
	case errors.Is(err, engine.ErrEstateWhole):
		// BEFORE THE NO-MAP REFUSAL IT WRAPS: at layout 0 there is no
		// map to wait for, and the no-map hint — wait for the duty to
		// write one — would send the operator to wait for ever.
		return refuse(http.StatusConflict, "estate_whole",
			"nothing to move: a data node is added by starting it and taken away by "+
				"stopping it, and every other data node goes on holding the whole estate")
	case errors.Is(err, partmap.ErrNoMap):
		return refuse(http.StatusServiceUnavailable, "no_estate_map",
			"the estate-map duty writes the first map within a tick of the layout's logs "+
				"existing and a data node holding a live estate lease: ask again then")
	case errors.Is(err, partmap.ErrUnknownPartition):
		return refuse(http.StatusNotFound, "unknown_partition",
			"name a partition the estate map lists: a space and a three-digit index, "+
				"like tracker.007")
	case errors.Is(err, partmap.ErrNotAHolder):
		return refuse(http.StatusConflict, "not_a_holder",
			"name a node the partition's holders list: a move takes a copy off a node "+
				"that has one")
	case errors.Is(err, partmap.ErrNowhereToMove):
		return refuse(http.StatusConflict, "nowhere_to_move",
			"add a data node first, so there is a member to rebuild the copy on — or "+
				"lower estate.replicas if the company means to keep fewer copies")
	case errors.Is(err, membership.ErrRemovedMember):
		// BEFORE THE UNKNOWN MEMBER IT WRAPS, for the object map's reason:
		// the map does know this node, and the unknown-member hint would
		// send the operator straight back to the one just refused.
		return refuse(http.StatusConflict, "removed_member",
			"leave it: it rejoins on probation the next time it is seen present and "+
				"healthy, or putting it back vouches for it now")
	case errors.Is(err, membership.ErrUnknownMember):
		return refuse(http.StatusNotFound, "unknown_member",
			"name a node the estate map lists: a member to take out or put back, or a "+
				"removed node to put back")
	case errors.Is(err, membership.ErrNothingPlaceable):
		return refuse(http.StatusConflict, "estate_refused",
			"every partition needs a present member to hold a copy: put another member "+
				"back in, or bring an absent one back, before taking this one out")
	case errors.Is(err, membership.ErrHoldRange):
		return refuse(http.StatusBadRequest, "invalid_hold",
			"hold for more than nothing and at most "+maxHold+"; a longer maintenance is "+
				"a hold renewed on purpose")
	case errors.Is(err, engine.ErrEstateUnconfirmed):
		return refuse(http.StatusBadRequest, "confirm_required",
			"repeat the generation GET /estate names in ?confirm=")
	case errors.Is(err, engine.ErrEstateOtherMap):
		return refuse(http.StatusConflict, "other_estate_map",
			"read the estate map through the node you mean to change and confirm with the "+
				"generation it names: this one is another fleet's, or was written again "+
				"since it was read")
	case errors.Is(err, engine.ErrEstateNewerMap):
		return refuse(http.StatusConflict, "estate_newer_map",
			"a node running a newer build wrote the map, and this one must not rewrite "+
				"it: make the gesture through a node running that build, or once the "+
				"upgrade has finished")
	case errors.Is(err, engine.ErrEstateUnavailable):
		return refuse(http.StatusServiceUnavailable, "estate_unavailable",
			"the coordination store did not answer, so whether the map changed is "+
				"unknown — asking again is safe: an out, an in, a release, a move or a "+
				"cancel the map already says is answered as landed with nothing written, "+
				"and a hold sent again replaces the one in force, ending its length "+
				"after the resend")
	}
	return EstateRefusal{}, false
}

// mountEstate registers the gesture routes — where there is a map to change.
// The read is a named route over the estate question (rest.go).
func (a *App) mountEstate(mux *http.ServeMux) {
	if a.estate == nil {
		return
	}
	mux.Handle("POST /estate/out/{node}", a.estateMember(true))
	mux.Handle("POST /estate/in/{node}", a.estateMember(false))
	mux.Handle("POST /estate/hold", http.HandlerFunc(a.serveEstateHold))
	mux.Handle("POST /estate/release", http.HandlerFunc(a.serveEstateRelease))
	mux.Handle("POST /estate/move/{partition}", a.estateMove(false))
	mux.Handle("POST /estate/move/{partition}/cancel", a.estateMove(true))
}

// refuseEstate writes a refusal the route made before any gesture ran.
func refuseEstate(w http.ResponseWriter, status int, code, detail, hint string) {
	writeJSON(w, status, EstateRefusalBody{Error: code, Detail: detail, Hint: hint})
}

// estateOperator is the operator on the request, or false with the refusal
// written: every gesture is recorded on the map under the person who made it.
func estateOperator(w http.ResponseWriter, r *http.Request) (string, bool) {
	operator, ok := auth.OperatorFrom(r.Context())
	if !ok || operator == "" {
		// THE GUARD ALREADY REFUSED AN UNAUTHENTICATED CALLER, so this is
		// the build with no operator identity on the context at all — and
		// the map records who made each gesture.
		refuseEstate(w, http.StatusForbidden, "operator_required",
			"a gesture on the estate map is an operator gesture and this request carries "+
				"no operator identity",
			"send the request with an api.auth token")
		return "", false
	}
	return operator, true
}

// estateMember answers the out and in routes: one gesture with a sign, both
// confirmed by repeating the node.
func (a *App) estateMember(out bool) http.HandlerFunc {
	gesture := "in"
	if out {
		gesture = "out"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")
		if node == "" || r.URL.Query().Get("confirm") != node {
			refuseEstate(w, http.StatusBadRequest, "confirm_required",
				"the node id was not repeated: taking a node out of the estate map or "+
					"putting it back moves its copies of the company's partitions across "+
					"the fleet",
				"repeat the node id in ?confirm=")
			return
		}
		operator, ok := estateOperator(w, r)
		if !ok {
			return
		}
		var g engine.EstateGesture
		var err error
		if out {
			g, err = a.estate.Out(r.Context(), node, operator, reasonOf(r))
		} else {
			g, err = a.estate.In(r.Context(), node, operator)
		}
		a.answerEstate(w, gesture, operator, node, "", g, err)
	}
}

// estateMove answers a move and its cancel: both name a partition and the node
// its copy moves off, confirmed by repeating the node.
func (a *App) estateMove(cancel bool) http.HandlerFunc {
	gesture := "move"
	if cancel {
		gesture = "cancel_move"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.PathValue("partition")
		p, err := statelog.ParsePartitionID(raw)
		if err != nil {
			refuseEstate(w, http.StatusBadRequest, "partition_invalid",
				raw+" is not a partition: "+err.Error(),
				"name a partition as a space and a three-digit index, like tracker.007")
			return
		}
		node := r.URL.Query().Get("from")
		if node == "" || r.URL.Query().Get("confirm") != node {
			refuseEstate(w, http.StatusBadRequest, "confirm_required",
				"the node the copy moves off was not named in ?from= and repeated in "+
					"?confirm=: a move rebuilds the partition's copy on another member and "+
					"then releases the one on that node",
				"name the node in ?from= and repeat it in ?confirm=")
			return
		}
		operator, ok := estateOperator(w, r)
		if !ok {
			return
		}
		var g engine.EstateGesture
		if cancel {
			g, err = a.estate.CancelMove(r.Context(), p, node, operator)
		} else {
			g, err = a.estate.Move(r.Context(), p, node, operator, reasonOf(r))
		}
		a.answerEstate(w, gesture, operator, node, p.String(), g, err)
	}
}

// serveEstateHold answers POST /estate/hold.
func (a *App) serveEstateHold(w http.ResponseWriter, r *http.Request) {
	d, err := time.ParseDuration(r.URL.Query().Get("for"))
	if err != nil {
		// NO DEFAULT LENGTH, for the object map's reason: a hold keeps
		// every gone member's partitions a copy short, and how long that
		// is acceptable is the operator's to say.
		refuseEstate(w, http.StatusBadRequest, "invalid_hold",
			"?for= is not a duration: "+err.Error(),
			"name how long to hold the map, as a duration like 30m or 2h — at most "+maxHold)
		return
	}
	operator, ok := estateOperator(w, r)
	if !ok {
		return
	}
	// ?confirm= AS TYPED: the engine judges it against the stored map —
	// see the file's doc.
	g, err := a.estate.Hold(r.Context(), r.URL.Query().Get("confirm"), d, operator, reasonOf(r))
	a.answerEstate(w, "hold", operator, "", "", g, err)
}

// serveEstateRelease answers POST /estate/release.
func (a *App) serveEstateRelease(w http.ResponseWriter, r *http.Request) {
	operator, ok := estateOperator(w, r)
	if !ok {
		return
	}
	g, err := a.estate.Release(r.Context(), r.URL.Query().Get("confirm"), operator)
	a.answerEstate(w, "release", operator, "", "", g, err)
}

// reasonOf is the request's ?reason=, trimmed.
func reasonOf(r *http.Request) string { return strings.TrimSpace(r.URL.Query().Get("reason")) }

// answerEstate writes a gesture's answer, its refusal, or its failure.
func (a *App) answerEstate(w http.ResponseWriter, gesture, operator, node, partition string,
	g engine.EstateGesture, err error) {

	if refusal, ok := RenderEstateRefusal(err); ok {
		log.Info("estate_gesture_refused", "operator", operator, "gesture", gesture,
			"node", node, "partition", partition, "error", refusal.Body.Error)
		writeJSON(w, refusal.Status, refusal.Body)
		return
	}
	if err != nil {
		log.Warn("api_estate_gesture_failed", "gesture", gesture, "node", node,
			"partition", partition, "error", err)
		// EVERY FAILURE PAST A WRITE IS estate_unavailable, above: what
		// reaches here failed before the map was written — a map this
		// build could not re-encode — and is this node's to report.
		refuseEstate(w, http.StatusInternalServerError, "estate_failed", err.Error(),
			"the gesture failed before the map was written; this node's log has the reason")
		return
	}
	answer := RenderEstateGesture(g, node, partition, a.now())
	log.Info("estate_gesture", "operator", operator, "gesture", gesture, "node", node,
		"partition", partition, "landed", answer.Landed, "epoch", answer.Epoch)
	writeJSON(w, http.StatusOK, answer)
}
