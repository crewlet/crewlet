package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
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

// BrokerRefusal is a removal refused with nothing changed: the status and the
// body the route answers with.
type BrokerRefusal struct {
	Status int
	Body   BrokerRefusalBody
}

// BrokerRefusalBody is every refusal's body: the code a caller branches on,
// the sentence saying what is wrong, and the one saying what to do.
type BrokerRefusalBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
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
func RenderBrokerRefusal(err error) (BrokerRefusal, bool) {
	refuse := func(status int, code, hint string) (BrokerRefusal, bool) {
		return BrokerRefusal{Status: status, Body: BrokerRefusalBody{
			Error: code, Detail: err.Error(), Hint: hint,
		}}, true
	}
	var live *engine.BrokerMemberLive
	switch {
	case err == nil:
		return BrokerRefusal{}, false
	case errors.As(err, &live):
		return refuse(http.StatusConflict, "member_live",
			"stop the node first and remove it once its presence lease has lapsed; "+
				"force it only for a member that is wedged but still renewing")
	case errors.Is(err, engine.ErrRemovalOutcomeUnknown):
		return refuse(http.StatusGatewayTimeout, "outcome_unknown",
			"read GET /fleet/broker before asking again: the member carrying it may "+
				"have committed the removal before it went silent")
	case errors.Is(err, engine.ErrExternalBroker):
		return refuse(http.StatusConflict, "external_broker",
			"change the membership with the external cluster's own tools")
	case errors.Is(err, jetstream.ErrNotAMetaPeer):
		return refuse(http.StatusNotFound, "not_a_member",
			"the metadata group counts no voter by that peer id, so there is nothing "+
				"to remove: it was never a member, or a removal already made it so — "+
				"GET /fleet/broker lists every voter it counts")
	case errors.Is(err, jetstream.ErrMembershipChanging):
		return refuse(http.StatusConflict, "membership_changing",
			"ask again once the change in flight has been committed")
	case errors.Is(err, jetstream.ErrNoMetaLeader):
		return refuse(http.StatusServiceUnavailable, "no_leader",
			"a group without a leader can change nothing about itself: bring enough "+
				"members back for a quorum, then ask again — and read the group first, "+
				"since a removal whose answer was lost may already have been committed")
	case errors.Is(err, engine.ErrNoBrokerMember):
		return refuse(http.StatusServiceUnavailable, "no_member",
			"a removal is carried by a live member's system account: start one, or "+
				"ask again once one answers")
	}
	return BrokerRefusal{}, false
}

// mountFleetBroker registers the two removals. The listing is a named read
// route over the `fleet_broker` question, so the socket's query channel and
// REST answer it from one implementation — see rest.go.
func (a *App) mountFleetBroker(mux *http.ServeMux) {
	mux.Handle("POST /fleet/broker/remove/{node}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.serveBrokerRemove(w, r, voterByNode, r.PathValue("node"))
	}))
	mux.Handle("POST /fleet/broker/remove-peer/{peer}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.serveBrokerRemove(w, r, voterByPeer, r.PathValue("peer"))
	}))
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
		writeJSON(w, http.StatusBadRequest, BrokerRefusalBody{
			Error: "confirm_required",
			Detail: "the " + by.what + " was not repeated: removing a member changes " +
				"the quorum every election and every create of the fleet's broker runs on",
			Hint: "repeat the " + by.what + " in ?confirm=",
		})
		return
	}
	if !by.valid(id) {
		writeJSON(w, http.StatusBadRequest, BrokerRefusalBody{
			Error: "voter_invalid", Detail: id + " is not a " + by.what,
			Hint: "name the voter by the node id GET /fleet/broker lists it under, or " +
				"by its peer id through /fleet/broker/remove-peer where no member " +
				"knows its name",
		})
		return
	}
	operator, _ := auth.OperatorFrom(r.Context())
	force := r.URL.Query().Get("force") == "true"
	removal := by.removal(id)
	removal.Force, removal.By = force, operator
	done, err := a.fleetBroker.Remove(r.Context(), removal)
	if refusal, ok := RenderBrokerRefusal(err); ok {
		log.Warn("api_broker_remove_refused", "operator", operator, by.key, id,
			"force", force, "code", refusal.Body.Error, "error", err)
		writeJSON(w, refusal.Status, refusal.Body)
		return
	}
	if err != nil {
		log.Warn("api_broker_remove_failed", "operator", operator, by.key, id, "error", err)
		writeJSON(w, http.StatusInternalServerError, BrokerRefusalBody{
			Error: "broker_remove_failed", Detail: err.Error(),
			Hint: "read GET /fleet/broker before asking again: the removal may have " +
				"been committed although its answer was lost",
		})
		return
	}
	log.Warn("fleet_broker_member_removed_by_operator", "operator", operator,
		"node", done.Node, "peer", done.Peer, "force", force, "by", done.By)
	writeJSON(w, http.StatusOK, done)
}
