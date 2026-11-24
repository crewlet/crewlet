package configapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// Removing a fleet: `DELETE /config/llm-fleets/{id}`, the model removal's
// skeleton (removal.go). Every seat job and helper set to `fleet:<id>` is
// repointed in the same write; the members stay under providers.llm.

// ErrDefaultFleet refuses a removal of default-fleet, which every company has.
var ErrDefaultFleet = errors.New("configapi: the default fleet can't be deleted")

// fleetRemoval is `DELETE /config/llm-fleets/{id}`.
var fleetRemoval = removalRoute{
	kind: EntityLLMFleets, what: "a fleet", noun: "fleet",
	read: func(_ *Service, id string, text []byte) (removing, string, error) {
		req, summary, err := readRemoval(text)
		return fleetRemove{id: id, req: req}, summary, err
	},
	bodyHint: "name every field that runs on the fleet by its seat and job, Ask Crewlet's " +
		"chat or the Advisor's findings, with the fleet (fleet:<key>) or model it moves to " +
		"(null to leave it unset)",
	summaryHint: "name the fleet removed and where its seats move",
	logAs:       "fleet",
}

// fleetRemove is a fleet's removal, read: its key and the repoint of every
// seat job and helper set to it.
type fleetRemove struct {
	id  string
	req RemovalRequest
}

func (m fleetRemove) deleting() []string { return m.req.DeleteSecrets }

func (m fleetRemove) draft(expect string) draft {
	return draft{
		expect: expect, requireActive: true,
		rules: authoredRules, authored: true,
		build: func(b base) (*config.Company, []byte, error) {
			next, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			if m.id == config.DefaultFleetKey {
				return nil, nil, ErrDefaultFleet
			}
			if _, ok := next.Providers.LLMFleets[m.id]; !ok {
				return nil, nil, &EntityError{Err: ErrNoSuchEntity}
			}
			if refused := repoint(next, config.FleetRef(m.id), m.req); refused != nil {
				return nil, nil, refused
			}
			delete(next.Providers.LLMFleets, m.id)
			document, err := json.Marshal(next)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
			}
			if document, err = carryUnknown(b.document, document); err != nil {
				return nil, nil, err
			}
			return next, document, nil
		},
	}
}

func (fleetRemove) refuse(w http.ResponseWriter, _ *http.Request, err error) bool {
	var refused *RemovalError
	switch {
	case errors.Is(err, ErrDefaultFleet):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotRemovable,
			map[string]string{"hint": "The default fleet can't be deleted"})
	case errors.As(err, &refused):
		refuseRemoval(w, refused)
	default:
		return false
	}
	return true
}

// readsAfter is everything prior reads: a fleet names no variable.
func (fleetRemove) readsAfter(prior *config.Company) ([]config.Reference, error) {
	return prior.VariablesRead(), nil
}

func (fleetRemove) checked(context.Context, *prepared, RemovalSecrets) (map[string]any, error) {
	return map[string]any{}, nil
}

func (fleetRemove) written(context.Context, *prepared, iam.Actor) map[string]any { return nil }

// fleetReferences serves `GET /config/llm-fleets/{id}/references`: every seat
// job and helper set to the fleet, as its removal answers them.
func (s *Service) fleetReferences(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	company, revision, err := s.activeCompany(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		refuseNoCompany(w)
		return
	case err != nil:
		s.fail(w, r, "read the active revision", err)
		return
	}
	if _, held := company.Providers.Fleet(id); !held {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{"hint": NotFound(EntityLLMFleets, id)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revision.ID, "references": referencesOf(company, config.FleetRef(id)),
	})
}
