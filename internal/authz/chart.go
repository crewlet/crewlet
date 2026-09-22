package authz

import "context"

// Chart is the company hierarchy, as much of it as the rules ask about.
//
// CONSUMER-DEFINED AND THREE METHODS. It is declared here because this is the
// package that calls it, and it is small because a seam is what a caller
// needs rather than what a provider has: internal/org can answer a dozen
// questions about a chart and these are the three any authority rule turns
// on. UnitMember arrives with the chat surface, where it gets its first call
// site — a seam method nothing calls is a declaration connected to nothing.
//
// # Both are THREE-VALUED, and that is the whole reason it exists
//
// The seam this replaces was `func(ctx, actor, project string) bool`, and its
// implementation opened with `if c == nil || c.Org == nil { return false }`.
// A node holds no company while it is booting, while it is applying a
// revision, and for as long as it is behind the log — and every one of those
// answered "you do not lead this project" about a person who leads it. A lead
// locked out of their own project's policy by a node that was merely lagging
// reads as a permissions bug, and the node reports itself healthy throughout.
//
// So an implementation returns an error for "I cannot tell" and NEVER false.
// [Decide] turns that into [Decision.Err], which is UNKNOWN rather than a
// refusal, and a surface answers 503 rather than 403.
type Chart interface {
	// Leads reports whether actor is above subject in the chart — the
	// management chain, or the effective lead of a unit subject sits in.
	// Both logins are seat handles as the chart addresses them TODAY;
	// resolving a retired one is the caller's to do before asking.
	Leads(ctx context.Context, actor, subject string) (bool, error)

	// LeadsProject reports whether actor leads the unit that owns a
	// project, or is the seat whose own project it is.
	LeadsProject(ctx context.Context, actor, projectKey string) (bool, error)

	// LeadsUnit reports whether actor leads the unit key names, directly
	// or from anywhere above it.
	//
	// IT IS NOT [LeadsProject] WITH A DIFFERENT ARGUMENT, and reaching for
	// that is the mistake this method exists to make impossible: a project
	// is a TRACKER key a unit may declare, so asking LeadsProject with a
	// unit key matches only a unit that happens to file its work under a
	// project of the same name. On every other company it answers false —
	// refusing the lead of that very unit, silently, with no error to
	// notice.
	LeadsUnit(ctx context.Context, actor, unitKey string) (bool, error)
}

// NoChart is a chart that can answer nothing, and says so.
//
// FOR A SURFACE THAT HAS NONE — a command-line tool, a node with no company
// yet — and it is deliberately not a chart that answers false. Wiring nothing
// must degrade to "this node cannot tell", which refuses loudly, rather than
// to "nobody leads anything", which refuses silently and looks like a policy.
// A test fixture that wants the second writes it down explicitly.
type NoChart struct{}

// Leads reports that this surface holds no chart.
func (NoChart) Leads(context.Context, string, string) (bool, error) {
	return false, ErrNoChart
}

// LeadsProject reports that this surface holds no chart.
func (NoChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, ErrNoChart
}

// LeadsUnit reports that this surface holds no chart.
func (NoChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, ErrNoChart
}
