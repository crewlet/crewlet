package integration

import (
	"context"
	"encoding/json"
	"errors"
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
// A single-node company is not a special case here. Its fleet store is the
// same KV, on the node's own embedded broker, whatever `coordination.type`
// says about its leases (see engine.attachCoordination).
type CoordStore struct{ statuses coord.Integrations }

// NewCoordStore wraps the fleet's integration statuses.
//
// # It REFUSES a nil backend rather than wrapping one
//
// A constructor that took whatever it was handed would return a perfectly
// non-nil *CoordStore holding a nil interface, which satisfies [Store], which
// [New] then accepts because its own nil check sees a value. The failure lands
// on the first tick, inside a detached goroutine, as a nil dereference that
// takes the process down. That is exactly what happened when this was once
// wired to a nil fleet store, so the refusal is here rather than at each call
// site: a caller cannot forget a check the type will not let them skip.
func NewCoordStore(statuses coord.Integrations) (*CoordStore, error) {
	if statuses == nil {
		return nil, errors.New(
			"integration: NewCoordStore needs the fleet's coordination " +
				"backend (statuses is nil), which is where every node records " +
				"what a pass finds")
	}
	return &CoordStore{statuses: statuses}, nil
}

var _ Store = (*CoordStore)(nil)

// LoadIntegrations reads every recorded status, never a findings record: a
// pass over the status class, at the read term (ADR-0175).
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
		if state, ok := decodeStatus(ctx, Kind(kind), value); ok {
			out = append(out, state)
		}
	}
	return out, nil
}

// LoadIntegrationsWithFindings reads every recorded status, each with the
// findings its record names attached: the status pass, then one pass over
// the findings, for a reader that shows them (the dashboard, the Advisor).
func (s *CoordStore) LoadIntegrationsWithFindings(ctx context.Context) ([]State, error) {
	states, err := s.LoadIntegrations(ctx)
	if err != nil {
		return nil, err
	}
	records, err := s.statuses.IntegrationFindings(ctx)
	if err != nil {
		// THE STATUSES STAND: a findings pass that failed is no evidence
		// about any phase, so each status naming findings reads them unread.
		log.WarnContext(ctx, "integration_findings_unread", "error", err,
			"detail", "every surface's status is answered; its findings read as unread until the next read")
		for i := range states {
			states[i].FindingsUnread = states[i].FindingsRecord.Named()
		}
		return states, nil
	}
	for i, state := range states {
		raw, found := records[state.Kind.String()]
		states[i] = attachFindings(ctx, state, raw, found)
	}
	return states, nil
}

// LoadIntegration reads one surface's status by its key, never its findings.
//
// An undecodable row answers NOT FOUND rather than raising, exactly as
// [CoordStore.LoadIntegrations] skips one, and for the same reason: the honest
// reading of a value this build cannot decode is "there is no status here I can
// use", and the recovery is the pass that follows overwriting it.
func (s *CoordStore) LoadIntegration(ctx context.Context, kind Kind) (State, bool, error) {
	value, found, err := s.statuses.IntegrationStatus(ctx, kind.String())
	if err != nil {
		return State{}, false, fmt.Errorf(
			"integration: read the %s reconcile status: %w", kind, err)
	}
	if !found {
		return State{}, false, nil
	}
	state, ok := decodeStatus(ctx, kind, value)
	return state, ok, nil
}

// LoadFindings is state with the findings its record names attached, read by
// their key in the time their size takes to cross; state as it is where it
// names none.
func (s *CoordStore) LoadFindings(ctx context.Context, state State) (State, error) {
	if !state.FindingsRecord.Named() {
		return state, nil
	}
	raw, found, err := s.statuses.IntegrationFindingsOf(ctx, state.Kind.String())
	if err != nil {
		return state, fmt.Errorf("integration: read the %s reconcile findings: %w", state.Kind, err)
	}
	return attachFindings(ctx, state, raw, found), nil
}

// decodeStatus is a stored status, false for one this build cannot decode.
// THE KEY IS AUTHORITATIVE, not the field inside the document: trusting the
// field would let a status written under the wrong key rename another
// surface's row.
func decodeStatus(ctx context.Context, kind Kind, value []byte) (State, bool) {
	var state State
	if err := json.Unmarshal(value, &state); err != nil {
		log.WarnContext(ctx, "integration_status_undecodable",
			"integration", kind.String(), "error", err,
			"detail", "this surface is reconciled again and its status rewritten")
		return State{}, false
	}
	state.Kind = kind
	return state, true
}

// attachFindings is state with raw, the findings record read for it, attached
// where it is the one its status names, and read as unread where it is not:
// missing, or written by a pass between the reader's two reads.
func attachFindings(ctx context.Context, state State, raw []byte, found bool) State {
	rec := state.FindingsRecord
	if !rec.Named() {
		return state
	}
	if !found || len(raw) != rec.Bytes || digest(raw) != rec.SHA256 {
		state.FindingsUnread = true
		return state
	}
	var findings []Finding
	if err := json.Unmarshal(raw, &findings); err != nil {
		log.WarnContext(ctx, "integration_findings_undecodable",
			"integration", state.Kind.String(), "error", err,
			"detail", "its findings read as unread until a pass writes them again")
		state.FindingsUnread = true
		return state
	}
	state.Findings, state.FindingsUnread = findings, false
	return state
}

// SaveIntegration records one surface's status alone, naming the findings
// record it was read with ([CoordStore.LoadIntegration]). A fold's findings
// go through [CoordStore.RecordPass], so a state carrying some is refused
// here: its status would name a record holding other findings.
func (s *CoordStore) SaveIntegration(ctx context.Context, state State) error {
	if state.Kind == "" {
		return fmt.Errorf("integration: cannot record a status for no surface")
	}
	if state.Findings != nil {
		return fmt.Errorf("integration: the %s status carries a pass's findings, which "+
			"RecordPass records with it; SaveIntegration writes a status alone", state.Kind)
	}
	value, err := s.encode(ctx, state)
	if err != nil {
		return err
	}
	if err := s.statuses.PutIntegrationStatus(ctx, state.Kind.String(), value); err != nil {
		return fmt.Errorf("integration: record the %s status: %w", state.Kind, err)
	}
	return nil
}

// RecordPass records what a fold concluded ([Observe], [ObserveTeardown],
// [AskTeardown]): its findings as their own record, then its status naming
// them by size and digest. A fold that found nothing removes the surface's
// record whatever its status named, so none is left that no status names.
func (s *CoordStore) RecordPass(ctx context.Context, state State) error {
	if state.Kind == "" {
		return fmt.Errorf("integration: cannot record a status for no surface")
	}
	rec := &state.FindingsRecord
	rec.Bytes, rec.SHA256, rec.Count = 0, "", 0
	var findings []byte
	if len(state.Findings) > 0 {
		raw, err := encodeFindings(state.Findings)
		if err != nil {
			return fmt.Errorf("integration: encode the %s findings: %w", state.Kind, err)
		}
		findings = raw
		rec.Bytes, rec.SHA256, rec.Count = len(raw), digest(raw), len(state.Findings)
	}
	state.Findings = nil
	value, err := s.encode(ctx, state)
	if err != nil {
		return err
	}
	if err = s.statuses.RecordIntegrationPass(ctx, state.Kind.String(), value, findings); err != nil {
		return fmt.Errorf("integration: record the %s status: %w", state.Kind, err)
	}
	return nil
}

// encode is state's status as stored ([encodeStatus]), logging WHOLE the text
// a cut to fit shortened, as [LogWhole] does for the rest.
func (s *CoordStore) encode(ctx context.Context, state State) ([]byte, error) {
	value, cut, err := encodeStatus(state)
	if err != nil {
		return nil, err
	}
	if cut {
		log.InfoContext(ctx, "integration_status_shortened", "integration", state.Kind.String(),
			"error", state.LastError, "detail", state.Report.Detail, "action_url", state.Report.ActionURL,
			"bound", coord.MaxIntegrationStatusBytes)
	}
	return value, nil
}

// ForgetIntegration drops a surface's status and its findings.
func (s *CoordStore) ForgetIntegration(ctx context.Context, kind Kind) error {
	if err := s.statuses.DeleteIntegrationStatus(ctx, kind.String()); err != nil {
		return fmt.Errorf("integration: forget the %s status: %w", kind, err)
	}
	return nil
}
