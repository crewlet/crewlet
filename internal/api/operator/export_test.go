package operator

// ReceiptOf is the act transport's reading of a tool's answer.
var ReceiptOf = receiptOf

// AuditOutcomeOf is the audit's reading of what became of one call.
var AuditOutcomeOf = auditOutcome

// WorkActor and PageActor are the actors this surface hands the tracker's and
// the knowledge base's tools, and WithKey marks a context with the operation
// one call's transport named — what [Server.Dispatch] does for a [Call.Key].
var (
	WorkActor = workActor
	PageActor = pageActor
	WithKey   = withKey
)

// Parameters is the JSON Schema one catalogue tool takes, as the transports
// serve it NOW, and false for a name the catalogue does not hold.
func (s *Server) Parameters(name string) (map[string]any, bool) {
	cat, up := s.catalogueFor()
	if !up {
		return nil, false
	}
	tool, ok := cat.lookup(name)
	if !ok {
		return nil, false
	}
	return tool.Parameters(), true
}
