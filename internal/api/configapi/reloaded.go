package configapi

import (
	"errors"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// Reloaded is what a re-activation a credential write made did, as the answer
// to that write reports it: the revision and epoch it activated, or the code
// and sentence of why it did not, beside a value that is sealed whatever
// happened (ADR-0070).
//
// ONE SHAPE FOR EVERY SURFACE THAT SEALS AND THEN RELOADS (`PUT /secrets`,
// `PUT /secrets/{name}`, `DELETE /secrets/{name}` and the model credential
// route), because crewlet-operator, `crewlet secrets set` and the dashboard
// read the same `reload` member off all of them, and a field spelled one way
// on one route and another on the next is a reader that is right on one.
//
// A FAILED RELOAD IS NOT A FAILED WRITE. The value is sealed before the
// reload is asked for, so the write's own status stays 200 and this says what
// the person still has to do: re-activate by hand (`POST /config/reload`) once
// the cause is fixed. The codes are the ones the config surface answers the
// same failure with, so a reader branches on one vocabulary.
type Reloaded struct {
	// RevisionID and Epoch are the revision the reload stored and
	// activated, set when it landed.
	RevisionID string `json:"revision_id,omitempty"`
	Epoch      int64  `json:"epoch,omitempty"`
	// Error and Detail say why it did not land, set when it did not.
	Error  httpjson.Code `json:"error,omitempty"`
	Detail string        `json:"detail,omitempty"`
	// Deferred is a write whose caller re-activates at its own end
	// (`PUT /secrets` with `defer_reload`): the value is sealed and NOT
	// active yet. Nothing that is running reads it until a re-activation,
	// the caller's own or any later one.
	Deferred bool `json:"deferred,omitempty"`
	// StoredRevisionID and Node name a revision the reload stored whose
	// activation was lost or went unanswered: a row of that node's history
	// alone ([KeptRevision]).
	StoredRevisionID string `json:"stored_revision_id,omitempty"`
	Node             string `json:"node,omitempty"`
}

// ReloadOutcome renders what [Service.Reload] answered.
func ReloadOutcome(applied Applied, err error) *Reloaded {
	if err == nil {
		return &Reloaded{RevisionID: applied.RevisionID, Epoch: applied.Epoch}
	}
	var raced *RacedError
	var unknown *ActivationUnknownError
	var invalid *ValidationError
	switch {
	case errors.As(err, &raced):
		return &Reloaded{Error: httpjson.CodeRevisionAdvanced, StoredRevisionID: raced.Stored,
			Node: raced.Node,
			Detail: "another configuration write activated first, so the revision that reads " +
				"this value was not re-activated by this request; the value is sealed, and " +
				"POST /config/reload re-activates the company that runs now"}
	case errors.As(err, &unknown):
		return &Reloaded{Error: httpjson.CodeUnavailable, StoredRevisionID: unknown.Stored,
			Node: unknown.Node,
			Detail: "the re-activation was stored and the coordination store did not answer " +
				"it, so whether the fleet runs it is unknown; GET /config names the active " +
				"revision once the store answers again"}
	case errors.As(err, &invalid):
		return &Reloaded{Error: httpjson.CodeValidationError,
			Detail: "the active company does not pass this build's validation, so it cannot " +
				"be re-activated as it stands: " + invalid.Err.Error()}
	case errors.Is(err, ErrNoActiveRevision):
		return &Reloaded{Error: httpjson.CodeNoActiveRevision,
			Detail: "the fleet runs no company to re-activate"}
	default:
		log.Error("config_reload_after_secret_failed", "error", err)
		return &Reloaded{Error: httpjson.CodeInternalError,
			Detail: "the re-activation failed; this node's log has the reason, and POST " +
				"/config/reload tries it again"}
	}
}
