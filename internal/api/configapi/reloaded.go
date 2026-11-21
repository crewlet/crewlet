package configapi

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
)

// Reloaded is what a re-activation a credential write made did, as the answer
// to that write reports it: the revision and epoch it activated, or the code
// and sentence of why it did not, beside a value that is sealed whatever
// happened (ADR-0070).
//
// ONE SHAPE FOR EVERY SURFACE THAT SEALS AND THEN RELOADS (`/secrets` routes,
// the model credential route): crewlet-operator, `crewlet secrets set` and the
// dashboard read the same `reload` member off all of them.
//
// A FAILED RELOAD IS NOT A FAILED WRITE: the value is sealed first, the status
// stays 200, and re-activating by hand (`POST /config/reload`) is what is
// left. The codes are the config surface's own.
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
	var tooLarge *DocumentTooLargeError
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
	case errors.As(err, &tooLarge):
		// REFUSED FOR ITS SIZE, before anything was stored: the active
		// company, sealed into the activation pointer beside this
		// re-activation's own summary, makes a pointer past what one key of
		// the coordination store holds. A definite answer, never a fault this
		// node's log alone could explain.
		return &Reloaded{Error: httpjson.CodeBodyTooLarge,
			Detail: fmt.Sprintf("the active company, sealed into the activation pointer beside "+
				"this re-activation's summary, makes a pointer of %d bytes, past the %d one key "+
				"of the coordination store holds, so it was not re-activated; the value is "+
				"sealed: make the company document smaller, then POST /config/reload "+
				"re-activates it", tooLarge.PointerBytes, coord.MaxValueBytes)}
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
