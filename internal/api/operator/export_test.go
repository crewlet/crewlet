package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/tools"
)

// Dispatch is the catalogue's own dispatch — the path every transport takes
// into a tool — so a test can hold a transport's answer against it.
func (s *Server) Dispatch(ctx context.Context, name string,
	args map[string]any) (tools.Result, bool, error) {

	return s.catalogue.call(ctx, name, args)
}

// ReceiptOf is the act transport's reading of a tool's answer.
var ReceiptOf = receiptOf

// AuditOutcomeOf is the audit's reading of what became of one call.
var AuditOutcomeOf = auditOutcome

// Parameters is the JSON Schema one catalogue tool takes, as the transports
// serve it, and false for a name the catalogue does not hold.
func (s *Server) Parameters(name string) (map[string]any, bool) {
	tool, ok := s.catalogue.lookup(name)
	if !ok {
		return nil, false
	}
	return tool.Parameters(), true
}

// Interrupted and Refuse are the act transport's two 503 answers: a call the
// context ended, and a tool's own refusal.
var (
	Interrupted = interrupted
	Refuse      = refuse
)
