package configapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// A stored MCP value's pointer is an ordinary document write, applied to the
// revision as it stands when written ([config.Company.PointMCPValue]).

// MCPValueMovedError is a key that, in the document as it stands, would read
// another variable than the one the value was sealed under.
type MCPValueMovedError struct {
	Sealed, Now string
}

func (e *MCPValueMovedError) Error() string {
	return fmt.Sprintf("configapi: the document changed while the value was tested: it is stored as %s, "+
		"and the key would now read %s", e.Sealed, e.Now)
}

// CheckMCPValue is a dry run of slot's pointer, held to every rule a write is.
func (s *Service) CheckMCPValue(ctx context.Context, slot config.MCPValueSlot, by iam.Actor) (*config.Company, error) {
	d := mcpValueDraft(slot)
	d.by = by
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return nil, err
	}
	return prepared.company, nil
}

// PointMCPValue writes slot's pointer as one revision and activates it.
func (s *Service) PointMCPValue(ctx context.Context, slot config.MCPValueSlot, summary string,
	by iam.Actor) (Applied, error) {

	d := mcpValueDraft(slot)
	d.by = by
	prepared, err := s.prepare(ctx, d)
	if err != nil {
		return Applied{}, err
	}
	return s.commit(ctx, prepared, summary)
}

// mcpValueDraft points slot's key in the active document, re-reading the slot.
func mcpValueDraft(slot config.MCPValueSlot) draft {
	return draft{
		requireActive: true,
		rules:         authoredRules, authored: true,
		build: func(b base) (*config.Company, []byte, error) {
			next, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			fresh, err := next.MCPValueSlot(slot.Server, slot.Seat, slot.Key)
			if err != nil {
				return nil, nil, err
			}
			if fresh.Variable != slot.Variable {
				return nil, nil, &MCPValueMovedError{Sealed: slot.Variable, Now: fresh.Variable}
			}
			if err = next.PointMCPValue(fresh); err != nil {
				return nil, nil, err
			}
			document, err := json.Marshal(next)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
			}
			// WHAT THIS BUILD CANNOT REPRESENT STAYS, as on every write of
			// the whole document: see unknown.go.
			if document, err = carryUnknown(b.document, document); err != nil {
				return nil, nil, err
			}
			return next, document, nil
		},
	}
}
