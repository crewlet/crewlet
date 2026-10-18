package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FLEET'S BROKER MEMBERSHIP, over HTTP for the retention gestures' reason:
// the metadata group is read and changed from inside a member, whose broker
// binds no socket anything outside the fleet could reach, so `crewlet fleet
// broker list` and `remove` are clients of these routes.
//
//   - GET /fleet/broker answers what every live node advertises about its
//     broker, the metadata group as a member reports it, and every place the
//     two disagree — a member gone for good that the group still counts
//     first among them. Any node answers it, asking a member where it is not
//     one ([engine.FleetBroker.List]). It is the `fleet_broker` question, as
//     a named read route: the dashboard asks it over the socket.
//   - POST /fleet/broker/remove/{node}?confirm={node}[&force=true] removes a
//     member from the metadata group through a live member's system account,
//     by the peer id its node id hashes to — which reaches it whether or not
//     any member still remembers its name. Refused while the named node holds
//     a live presence lease as a member (or without saying what its broker
//     is), unless forced: a running member removed from the group rejoins it
//     as a voter at its next restart.
//   - POST /fleet/broker/remove-peer/{peer}?confirm={peer}[&force=true] is the
//     same removal naming the voter by the raft peer id GET /fleet/broker
//     shows, for a voter whose name no member has heard — which has no node
//     id to give.

// FleetBrokerControl is the broker-membership seam — see [engine.FleetBroker].
// Every engine has one, so it is required like the capacity seam.
type FleetBrokerControl interface {
	List(ctx context.Context) (engine.BrokerView, error)
	Remove(ctx context.Context, req engine.BrokerRemoval) (engine.BrokerRemoved, error)
}

// BrokerRefusal is a removal refused with nothing changed: the status, the
// code and the facts the route answers with.
//
// IN THE ENGINE'S ENVELOPE, as [GateRefusal] is and for its reason: the code
// is on httpjson's table and carries its sentence as `message`, `detail` says
// what is wrong and `hint` what to do, and a 503 carries the Retry-After its
// cause implies. It was a body of its own — `{error, detail, hint}` with no
// sentence — on the one surface that changes the quorum every node runs on.
type BrokerRefusal struct {
	Status int
	Code   httpjson.Code
	Fields httpjson.Detail

	// RetryAfter is the Retry-After in whole seconds, on a 503 whose cause
	// waiting can clear; zero writes none.
	RetryAfter int
}

// Envelope is the refusal's body as the route sends it — [httpjson.Envelope]'s.
// A server standing in for a node answers with [BrokerRefusal.Write] rather
// than encoding this itself, or its 503 goes out with no Retry-After.
func (b BrokerRefusal) Envelope() map[string]any { return httpjson.Envelope(b.Code, b.Fields) }

// Write answers the refusal: a 503 through [httpjson.UnavailableWith], which
// sets the header and the envelope together, and anything else through
// [httpjson.FailWithFields].
func (b BrokerRefusal) Write(w http.ResponseWriter) {
	if b.Status == http.StatusServiceUnavailable {
		httpjson.UnavailableWith(w, b.Code, b.RetryAfter, b.Fields)
		return
	}
	httpjson.FailWithFields(w, b.Status, b.Code, b.Fields)
}

// RenderBrokerRefusal renders err if it is one of the removal's refusals,
// reporting false for anything else — a failure the route answers
// `500 broker_remove_failed`.
//
// EACH SENDS AN OPERATOR SOMEWHERE DIFFERENT, which is why each has its own
// code: a member still running is stopped first, a voter the group does not
// count is a typo or a removal already made, a change in flight is waited out,
// a group with no leader is brought back to a quorum, a carrier that went
// silent is read about before asking again, and an external cluster is its
// own operator's.
//
// THE TWO 503s WAIT AN ELECTION ([statelog.ElectionRetryHint]): a group with
// no leader answers again no sooner than it elects one, and a removal with no
// member to carry it waits on a member's broker coming back into that group.
func RenderBrokerRefusal(err error) (BrokerRefusal, bool) {
	refuse := func(status int, code httpjson.Code, hint string) (BrokerRefusal, bool) {
		out := BrokerRefusal{Status: status, Code: code,
			Fields: httpjson.Detail{"detail": err.Error(), "hint": hint}}
		if status == http.StatusServiceUnavailable {
			out.RetryAfter = httpjson.RetrySeconds(statelog.ElectionRetryHint)
		}
		return out, true
	}
	var live *engine.BrokerMemberLive
	switch {
	case err == nil:
		return BrokerRefusal{}, false
	case errors.As(err, &live):
		return refuse(http.StatusConflict, httpjson.CodeMemberLive,
			"stop the node first and remove it once its presence lease has lapsed; "+
				"force it only for a member that is wedged but still renewing")
	case errors.Is(err, engine.ErrRemovalOutcomeUnknown):
		return refuse(http.StatusGatewayTimeout, httpjson.CodeRemovalOutcomeUnknown,
			"read GET /fleet/broker before asking again: the member carrying it may "+
				"have committed the removal before it went silent")
	case errors.Is(err, engine.ErrExternalBroker):
		return refuse(http.StatusConflict, httpjson.CodeExternalBroker,
			"change the membership with the external cluster's own tools")
	case errors.Is(err, jetstream.ErrNotAMetaPeer):
		return refuse(http.StatusNotFound, httpjson.CodeNotAMember,
			"the metadata group counts no voter by that peer id, so there is nothing "+
				"to remove: it was never a member, or a removal already made it so — "+
				"GET /fleet/broker lists every voter it counts")
	case errors.Is(err, jetstream.ErrMembershipChanging):
		return refuse(http.StatusConflict, httpjson.CodeMembershipChanging,
			"ask again once the change in flight has been committed")
	case errors.Is(err, jetstream.ErrNoMetaLeader):
		return refuse(http.StatusServiceUnavailable, httpjson.CodeNoLeader,
			"a group without a leader can change nothing about itself: bring enough "+
				"members back for a quorum, then ask again — and read the group first, "+
				"since a removal whose answer was lost may already have been committed")
	case errors.Is(err, engine.ErrNoBrokerMember):
		return refuse(http.StatusServiceUnavailable, httpjson.CodeNoBrokerMember,
			"a removal is carried by a live member's system account: start one, or "+
				"ask again once one answers")
	}
	return BrokerRefusal{}, false
}

// mountFleetBroker registers the two removals, through the mount
// [App.mountDeployment] hands it — which is what decides the grant: a removal
// changes the quorum every election runs on, so it is `admin`'s and asks for a
// recent proof. The listing is a named read route over the `fleet_broker`
// question, so the socket's query channel and REST answer it from one
// implementation — see rest.go.
func (a *App) mountFleetBroker(mount func(string, authz.Effect, http.HandlerFunc)) {
	// A MEMBER TAKEN OUT OF THE GROUP is gone from its quorum for good.
	mount("POST /fleet/broker/remove/{node}", authz.EffectDestroys,
		func(w http.ResponseWriter, r *http.Request) {
			a.serveBrokerRemove(w, r, voterByNode, r.PathValue("node"))
		})
	mount("POST /fleet/broker/remove-peer/{peer}", authz.EffectDestroys,
		func(w http.ResponseWriter, r *http.Request) {
			a.serveBrokerRemove(w, r, voterByPeer, r.PathValue("peer"))
		})
}

// voterName is how a removal route names the voter: the rule its path
// segment is held to, and the removal it becomes.
type voterName struct {
	// what is the identifier's name in a sentence, and key its name in a
	// log line.
	what, key string
	// valid is its spelling.
	valid func(string) bool
	// removal names the voter to the engine.
	removal func(id string) engine.BrokerRemoval
}

var (
	voterByNode = voterName{what: "node id", key: "node", valid: config.ValidNodeID,
		removal: func(id string) engine.BrokerRemoval { return engine.BrokerRemoval{Node: id} }}
	voterByPeer = voterName{what: "peer id", key: "peer", valid: jetstream.ValidPeerID,
		removal: func(id string) engine.BrokerRemoval { return engine.BrokerRemoval{Peer: id} }}
)

// serveBrokerRemove answers both removal routes, the voter named by id as
// the route's voterName reads it.
func (a *App) serveBrokerRemove(w http.ResponseWriter, r *http.Request, by voterName, id string) {
	// THE CONFIRMATION ECHOES THE ID, the shape every destructive gesture
	// here takes: a removed member is no longer counted in any election,
	// which is a change to the quorum every node runs on.
	if id == "" || r.URL.Query().Get("confirm") != id {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeConfirmRequired,
			httpjson.Detail{
				"detail": "the " + by.what + " was not repeated: removing a member " +
					"changes the quorum every election and every create of the " +
					"fleet's broker runs on",
				"hint": "repeat the " + by.what + " in ?confirm=",
			})
		return
	}
	if !by.valid(id) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeVoterInvalid,
			httpjson.Detail{
				"detail": id + " is not a " + by.what,
				"hint": "name the voter by the node id GET /fleet/broker lists it " +
					"under, or by its peer id through /fleet/broker/remove-peer " +
					"where no member knows its name",
			})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	// WHO ASKED, as every record on this surface names a caller: the author
	// and the credential they pressed it through.
	actor := iam.ActorFor(caller)
	force := r.URL.Query().Get("force") == "true"
	removal := by.removal(id)
	removal.Force, removal.By = force, actor
	done, err := a.fleetBroker.Remove(r.Context(), removal)
	if refusal, ok := RenderBrokerRefusal(err); ok {
		log.Warn("api_broker_remove_refused", "by", actor.Name, "operator", actor.OperatorID,
			by.key, id, "force", force, "code", refusal.Code, "error", err)
		refusal.Write(w)
		return
	}
	if err != nil {
		log.Warn("api_broker_remove_failed", "by", actor.Name, "operator", actor.OperatorID,
			by.key, id, "error", err)
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeBrokerRemoveFailed,
			httpjson.Detail{
				"detail": err.Error(),
				"hint": "read GET /fleet/broker before asking again: the removal may " +
					"have been committed although its answer was lost",
			})
		return
	}
	log.Warn("fleet_broker_member_removed_by_operator", "by", actor.Name,
		"operator", actor.OperatorID, "node", done.Node, "peer", done.Peer,
		"force", force, "carried_by", done.By)
	writeJSON(w, http.StatusOK, done)
}
