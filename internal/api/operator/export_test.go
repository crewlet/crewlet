package operator

// ReceiptOf is the act transport's reading of a tool's answer.
var ReceiptOf = receiptOf

// DecodeAct is the act transport's reading of a request body: the tool's
// arguments, or what the body got wrong, in the words this node logs.
var DecodeAct = decodeAct

// AuditOutcomeOf is the audit's reading of what became of one call.
var AuditOutcomeOf = auditOutcome

// WorkActor, PageActor and ChatActor are the actors this surface hands the
// tracker's, the knowledge base's and chat's tools, and WithKey marks a
// context with the operation one call's transport named — what
// [Server.Dispatch] does for a [Call.Key].
var (
	WorkActor = workActor
	PageActor = pageActor
	ChatActor = chatActor
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
