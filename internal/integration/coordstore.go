package integration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// CoordStore is the reconcile status, on the FLEET's coordination store.
//
// The one implementation, and the node's own database is deliberately not an
// option: the loop is a worker duty and the status is read by whichever node
// serves the API, which on a split-role fleet is never the same host. See
// [coord.Integrations] for the whole argument.
//
// A single-node company is not a special case here. It runs the in-memory
// coordination twin, which is a real implementation of the same contract
// certified by the same suite, rather than a stub.
type CoordStore struct{ statuses coord.Integrations }

// NewCoordStore wraps the fleet's integration statuses.
func NewCoordStore(statuses coord.Integrations) *CoordStore {
	return &CoordStore{statuses: statuses}
}

var _ Store = (*CoordStore)(nil)

// LoadIntegrations reads every recorded status.
//
// A status this build cannot decode is SKIPPED rather than raised, and the
// difference matters on a rolling upgrade. Unknown fields decode away on
// their own, so what lands here is a value genuinely no longer readable, and
// the honest reading of one is "this surface has no status I can use". The
// loop then treats it as never reconciled, runs a pass, and overwrites it,
// which is the recovery. Raising instead would stop every OTHER surface from
// being reconciled because one row was unreadable.
func (s *CoordStore) LoadIntegrations(ctx context.Context) ([]State, error) {
	raw, err := s.statuses.IntegrationStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("integration: read the reconcile statuses: %w", err)
	}
	out := make([]State, 0, len(raw))
	for kind, value := range raw {
		var state State
		if err := json.Unmarshal(value, &state); err != nil {
			log.WarnContext(ctx, "integration_status_undecodable",
				"integration", kind, "error", err,
				"detail", "this surface is reconciled again and its status rewritten")
			continue
		}
		// THE KEY IS AUTHORITATIVE, not the field inside the document.
		// They agree on everything this engine wrote, and when they do
		// not, the one the store looked the value up by is the one the
		// caller asked about. Trusting the field would let a status
		// written under the wrong key rename another surface's row.
		state.Kind = Kind(kind)
		out = append(out, state)
	}
	return out, nil
}

// SaveIntegration records one surface's status.
func (s *CoordStore) SaveIntegration(ctx context.Context, state State) error {
	if state.Kind == "" {
		return fmt.Errorf("integration: cannot record a status for no surface")
	}
	value, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("integration: encode the %s status: %w", state.Kind, err)
	}
	if err := s.statuses.PutIntegrationStatus(ctx, state.Kind.String(), value); err != nil {
		return fmt.Errorf("integration: record the %s status: %w", state.Kind, err)
	}
	return nil
}

// ForgetIntegration drops a surface's status.
func (s *CoordStore) ForgetIntegration(ctx context.Context, kind Kind) error {
	if err := s.statuses.DeleteIntegrationStatus(ctx, kind.String()); err != nil {
		return fmt.Errorf("integration: forget the %s status: %w", kind, err)
	}
	return nil
}
