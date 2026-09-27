package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
)

// THE OPERATOR'S GESTURES ON THE PLACEMENT MAP, over HTTP for the retention
// gestures' reason.
//
// The map is one record in the coordination store, and on the default topology
// that store is the engine's own embedded broker, which binds no socket: a CLI
// run while the engine is down cannot reach it, and one run while it is up must
// not try. So each gesture goes through a node that already holds it, and
// `crewlet objects out`, `in`, `hold` and `release` are clients of these
// routes.
//
//   - POST /objects/out/{node}?confirm={node}&reason= takes a member out: the
//     map places nothing on it, and its share is copied to the others while it
//     keeps serving what it holds — the first step of taking a data node away.
//   - POST /objects/in/{node}?confirm={node} puts it back — or vouches for a
//     node the map removed for absence: one back on probation is placed on at
//     once, and one not seen since is placed on the next time it is seen,
//     rather than either proving itself stable first.
//   - POST /objects/hold?for={duration}&reason= holds the map, at most a day:
//     no member is removed for absence while it holds.
//   - POST /objects/release ends the hold.
//
// Every one is a read, a pure gesture and a compare-and-set in the engine
// ([engine.ObjectsControl]), answered with whether the map now says what was
// asked — `landed` — and the map as it stands. POSTs, so the anonymous-read
// posture never reaches them: moving a member's share across the fleet is not
// a read.

// ObjectsControl is the object store's gesture seam — see [engine.ObjectsControl].
//
// EXPORTED, like [NodeGate], because the caller has to convert a typed nil to a
// genuine one before handing it over — which [EngineObjects] does — and a nil
// seam leaves the routes UNMOUNTED: a node that runs no object store has no map
// to change, and an operator who cannot move a member must not be told they
// can.
type ObjectsControl interface {
	Out(ctx context.Context, node, by, reason string) (engine.ObjectsGesture, error)
	In(ctx context.Context, node, by string) (engine.ObjectsGesture, error)
	Hold(ctx context.Context, d time.Duration, by, reason string) (engine.ObjectsGesture, error)
	Release(ctx context.Context, by string) (engine.ObjectsGesture, error)
}

// EngineObjects is the gesture routes' reach into a running engine, or nil
// where it runs no object store.
//
// HERE, beside the seam it satisfies, for [EngineFiles]' reason: `crewlet run`
// and the end-to-end suite mount the routes through the same adapter. NIL AS AN
// INTERFACE rather than an interface holding a nil control, which [App] would
// read as a surface and mount.
func EngineObjects(e *engine.Engine) ObjectsControl {
	if e == nil {
		return nil
	}
	if c := e.ObjectsControl(); c != nil {
		return c
	}
	return nil
}

// ObjectsAnswer is what a gesture route answers once the gesture ran: whether
// the stored map now says what was asked, and the map as it stands.
//
// # One typed value, like [GateAnswer]
//
// The routes render through [RenderObjectsGesture] and nothing else; the
// command line's tests serve what it returns, and the dashboard's suite reads
// the golden this package's test writes from it
// (`internal/api/testdata/objects_answer.json`).
type ObjectsAnswer struct {
	// Landed is whether the stored map says what the gesture asked —
	// written by it, or already so. False only when every attempt lost its
	// compare-and-set to another writer: nothing this gesture asked is in
	// the map, and Hint says what to do.
	Landed bool `json:"landed"`

	// Epoch is the map's epoch after the gesture. Taking a member out or
	// putting one back moves it; a hold and its release do not, because
	// neither changes where anything is placed.
	Epoch uint64 `json:"epoch"`

	// Node is the member an out or an in named, and Member that member as
	// the map now describes it — absent when the map does not hold it,
	// which is what an in answers for a node the map had removed: it is
	// placed on again the next time the maintainer sees it.
	Node   string                   `json:"node,omitempty"`
	Member *queries.ObjectMapMember `json:"member,omitempty"`

	// Hold is the hold in force now, absent when there is none.
	Hold *queries.ObjectHold `json:"hold,omitempty"`

	// Hint is the sentence saying what to do next, where there is
	// something to do — in no surface's vocabulary.
	Hint string `json:"hint,omitempty"`
}

// RenderObjectsGesture is the one rendering of a gesture's result — see
// [ObjectsAnswer]. node is the member an out or an in named, empty for a hold
// and a release; now is what the hold is judged against.
func RenderObjectsGesture(g engine.ObjectsGesture, node string, now time.Time) ObjectsAnswer {
	out := ObjectsAnswer{
		Landed: g.Landed, Epoch: g.State.Map.Epoch, Node: node,
		Hold: queries.RenderHold(g.State, now),
	}
	if node != "" {
		if member, ok := queries.RenderMapMember(g.State, node); ok {
			out.Member = &member
		}
	}
	switch {
	case !g.Landed:
		// A LOST RACE IS NOT A REFUSAL AND NOT A FAULT: the map moved
		// under every attempt — the maintainer's tick or another operator
		// — and what is rendered above is what it says now, which is
		// what the next gesture is decided against.
		out.Hint = "the placement map changed under every attempt, so this gesture " +
			"is not in it: read the map as it stands and make the gesture again " +
			"if it still applies"
	case node != "" && out.Member == nil:
		// AN IN THAT VOUCHED FOR A NODE THE MAP HAD REMOVED: it is not a
		// member yet, and a reader shown no member would take the gesture
		// for one that did nothing.
		out.Hint = "not a member of the map now: the map places on it again the " +
			"next time its maintainer sees it present and healthy"
	}
	return out
}

// maxHold is the engine's ceiling on a hold as the refusals spell it, "24h"
// rather than time's "24h0m0s" — read off [upkeep.MaxHold] so the sentence
// cannot outlive a change to the ceiling.
var maxHold = fmt.Sprintf("%gh", upkeep.MaxHold.Hours())

// ObjectsRefusal is a gesture refused with nothing written: the status and the
// body the route answers with.
type ObjectsRefusal struct {
	Status int
	Body   ObjectsRefusalBody
}

// ObjectsRefusalBody is every refusal's body: the code a caller branches on,
// the sentence saying what is wrong, and the one saying what to do.
type ObjectsRefusalBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
}

// RenderObjectsRefusal renders err if it is one of the gestures' refusals,
// reporting false for anything else — a failure the route answers `500
// objects_failed`.
//
// EACH REFUSAL SENDS AN OPERATOR SOMEWHERE DIFFERENT, which is why each has
// its own code rather than one 409 for all: a fleet with no map yet waits for
// a data node, a name the map does not hold is a typo, a node the map removed
// is put back rather than taken out, taking out the last member is a decision
// to revisit, and a store that did not answer is asked again.
func RenderObjectsRefusal(err error) (ObjectsRefusal, bool) {
	refuse := func(status int, code, hint string) (ObjectsRefusal, bool) {
		return ObjectsRefusal{Status: status, Body: ObjectsRefusalBody{
			Error: code, Detail: err.Error(), Hint: hint,
		}}, true
	}
	switch {
	case err == nil:
		return ObjectsRefusal{}, false
	case errors.Is(err, upkeep.ErrNoMap):
		return refuse(http.StatusServiceUnavailable, "no_object_map",
			"no data node has joined the object store yet, so there is no map to "+
				"change: one is written within seconds of the first data node starting")
	case errors.Is(err, upkeep.ErrRemovedMember):
		// BEFORE THE UNKNOWN MEMBER IT WRAPS: taking out a node the map
		// removed and has not seen back names a node the map does know,
		// and the unknown-member hint — name a node the map lists — would
		// send the operator straight back to the one just refused.
		// The detail already says what is so; the hint is what follows
		// from it.
		return refuse(http.StatusConflict, "removed_member",
			"leave it: it rejoins on probation the next time it is seen present "+
				"and healthy, or putting it back vouches for it now")
	case errors.Is(err, upkeep.ErrUnknownMember):
		return refuse(http.StatusNotFound, "unknown_member",
			"name a node the fleet view's object placement lists: a member to take "+
				"out or put back, or a removed node to put back")
	case errors.Is(err, upkeep.ErrNothingPlaceable):
		return refuse(http.StatusConflict, "objects_refused",
			"every write needs a present member to land on: put another member "+
				"back in, or bring an absent one back, before taking this one out")
	case errors.Is(err, upkeep.ErrHoldRange):
		return refuse(http.StatusBadRequest, "invalid_hold",
			"hold for more than nothing and at most "+maxHold+"; a longer "+
				"maintenance is a hold renewed on purpose")
	case errors.Is(err, engine.ErrObjectsNewerMap):
		return refuse(http.StatusConflict, "objects_newer_map",
			"a node running a newer build wrote the map, and this one must not "+
				"rewrite it: make the gesture through a node running that build, or "+
				"once the upgrade has finished")
	case errors.Is(err, engine.ErrObjectsUnavailable):
		return refuse(http.StatusServiceUnavailable, "objects_unavailable",
			"the coordination store did not answer, so whether the map changed is "+
				"unknown — asking again is safe: an out, an in or a release the map "+
				"already says is answered as landed with nothing written, and a hold "+
				"sent again replaces the one in force, ending its length after the "+
				"resend")
	}
	return ObjectsRefusal{}, false
}

// mountObjects registers the gesture routes — only where there is a map to
// change. See [ObjectsControl].
func (a *App) mountObjects(mux *http.ServeMux) {
	if a.objects == nil {
		return
	}
	mux.Handle("POST /objects/out/{node}", a.objectsMember(true))
	mux.Handle("POST /objects/in/{node}", a.objectsMember(false))
	mux.Handle("POST /objects/hold", http.HandlerFunc(a.serveObjectsHold))
	mux.Handle("POST /objects/release", http.HandlerFunc(a.serveObjectsRelease))
}

// objectsOperator is the operator on the request, or false with the refusal
// written: every gesture is recorded on the map under the person who made it.
func objectsOperator(w http.ResponseWriter, r *http.Request) (string, bool) {
	operator, ok := auth.OperatorFrom(r.Context())
	if !ok || operator == "" {
		// THE GUARD ALREADY REFUSED AN UNAUTHENTICATED CALLER, so this is
		// the build with no operator identity on the context at all — and
		// the map records who made each gesture, which is what the screen
		// shows beside a member taken out or a hold.
		writeJSON(w, http.StatusForbidden, ObjectsRefusalBody{
			Error:  "operator_required",
			Detail: "a gesture on the placement map is an operator gesture and this request carries no operator identity",
			Hint:   "send the request with an api.auth token",
		})
		return "", false
	}
	return operator, true
}

// objectsMember answers the out and in routes.
//
// ONE HANDLER FOR BOTH, because they are one gesture with a sign, and both
// confirm the same way.
func (a *App) objectsMember(out bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.PathValue("node")
		// THE CONFIRMATION ECHOES THE NODE ID, the shape every destructive
		// gesture here takes: taking a member out moves its whole share
		// across the fleet, and putting one back moves it again.
		if node == "" || r.URL.Query().Get("confirm") != node {
			writeJSON(w, http.StatusBadRequest, ObjectsRefusalBody{
				Error: "confirm_required",
				Detail: "the node id was not repeated: taking a member out or " +
					"putting it back moves its share of the company's files across the fleet",
				Hint: "repeat the node id in ?confirm=",
			})
			return
		}
		operator, ok := objectsOperator(w, r)
		if !ok {
			return
		}
		reason := strings.TrimSpace(r.URL.Query().Get("reason"))
		var g engine.ObjectsGesture
		var err error
		if out {
			g, err = a.objects.Out(r.Context(), node, operator, reason)
		} else {
			g, err = a.objects.In(r.Context(), node, operator)
		}
		gesture := "in"
		if out {
			gesture = "out"
		}
		a.answerObjects(w, gesture, operator, node, g, err)
	}
}

// serveObjectsHold answers POST /objects/hold.
func (a *App) serveObjectsHold(w http.ResponseWriter, r *http.Request) {
	d, err := time.ParseDuration(r.URL.Query().Get("for"))
	if err != nil {
		// NO DEFAULT LENGTH: a hold pins every gone member in the map,
		// its groups a copy short, and how long that is acceptable is
		// the operator's to say rather than this route's to guess.
		writeJSON(w, http.StatusBadRequest, ObjectsRefusalBody{
			Error:  "invalid_hold",
			Detail: "?for= is not a duration: " + err.Error(),
			Hint:   "name how long to hold the map, as a duration like 30m or 2h — at most " + maxHold,
		})
		return
	}
	operator, ok := objectsOperator(w, r)
	if !ok {
		return
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	g, err := a.objects.Hold(r.Context(), d, operator, reason)
	a.answerObjects(w, "hold", operator, "", g, err)
}

// serveObjectsRelease answers POST /objects/release.
func (a *App) serveObjectsRelease(w http.ResponseWriter, r *http.Request) {
	operator, ok := objectsOperator(w, r)
	if !ok {
		return
	}
	g, err := a.objects.Release(r.Context(), operator)
	a.answerObjects(w, "release", operator, "", g, err)
}

// answerObjects writes a gesture's answer, its refusal, or its failure.
func (a *App) answerObjects(w http.ResponseWriter, gesture, operator, node string,
	g engine.ObjectsGesture, err error) {

	if refusal, ok := RenderObjectsRefusal(err); ok {
		log.Info("objects_gesture_refused", "operator", operator, "gesture", gesture,
			"node", node, "error", refusal.Body.Error)
		writeJSON(w, refusal.Status, refusal.Body)
		return
	}
	if err != nil {
		log.Warn("api_objects_gesture_failed", "gesture", gesture, "node", node,
			"error", err)
		// EVERY FAILURE PAST A WRITE IS objects_unavailable, above: what
		// reaches here failed before the map was written — a map this
		// build could not re-encode — and is this node's to report.
		writeJSON(w, http.StatusInternalServerError, ObjectsRefusalBody{
			Error: "objects_failed", Detail: err.Error(),
			Hint: "the gesture failed before the map was written; this node's log " +
				"has the reason",
		})
		return
	}
	answer := RenderObjectsGesture(g, node, a.now())
	log.Info("objects_gesture", "operator", operator, "gesture", gesture, "node", node,
		"landed", answer.Landed, "epoch", answer.Epoch)
	writeJSON(w, http.StatusOK, answer)
}
