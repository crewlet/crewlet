package authz

import "context"

// Chart is the company hierarchy, as much of it as the rules ask about.
//
// CONSUMER-DEFINED AND SEVEN METHODS. It is declared here because this is the
// package that calls it, and it is small because a seam is what a caller
// needs rather than what a provider has: internal/org can answer a dozen
// questions about a chart and these are the seven any authority rule turns
// on: who leads a person, a project, a unit and a page container, whether
// somebody leads ANYBODY, which is the question asked about a record before
// anybody knows whose it is ([Object.Unresolved]), who SITS in a unit,
// which is what a share with a team admits ([ClassShared]), and which grants
// a seat's ROLE carries, which is what admits an agent to a verb whose row
// names one ([rule.seatGrant]). A relation arrives with its first call site —
// a seam method nothing calls is a declaration connected to nothing.
//
// # Every one is THREE-VALUED, and that is the whole reason it exists
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
//
// THE RELATIONS ARE ANSWERED OVER ONE TREE by internal/authz/orgchart — the
// running company's, for every surface asking about the company as it is, and
// the two documents a /config write compares, for the org chart's own write.
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

	// LeadsContainer reports whether actor leads the unit that owns a
	// PAGE CONTAINER, or is the seat whose own container it is.
	//
	// THE THIRD RELATION, and it is not [LeadsProject] either: a unit
	// declares its tracker project and its page container in two separate
	// fields, so asking the project relation with a container key matches
	// only a company whose unit files its work and its pages under the
	// same name — and answers false everywhere else, refusing the lead of
	// that very container with no error to notice. It is the same mistake
	// [LeadsUnit] exists to make impossible, one key class further on.
	LeadsContainer(ctx context.Context, actor, containerKey string) (bool, error)

	// LeadsAnyone reports whether actor leads SOMEBODY — any seat at all
	// that [Chart.Leads] would answer true for.
	//
	// THE QUESTION ASKED BEFORE THE RECORD IS KNOWN, and nothing else asks
	// it: a caller naming somebody else's LOGIN is admitted to that
	// person's record only as the lead of the seat the login resolves to,
	// and the resolution is the identity directory's. Asked first, it is
	// what lets a caller who leads nobody be refused without the directory
	// being consulted at all — so what the directory says, or that it
	// cannot say, is never an answer that differs by login for somebody
	// the answer could not admit. See [Object.Unresolved].
	LeadsAnyone(ctx context.Context, actor string) (bool, error)

	// MemberOf reports whether actor's seat SITS in the unit key names or
	// in any unit below it: who a share with that team admits.
	//
	// MEMBERSHIP, NOT LEADERSHIP, and not [Chart.LeadsUnit] either: a unit's lead
	// seated in the unit above it leads the team and is not in it, so a
	// share with the team does not reach them through this, while every
	// seat placed in the team or in a team beneath it is reached, live, as
	// the chart moves. It cannot be read off `iam.Principal.Position`: a
	// seat principal carries only its own unit, and "anywhere below" needs
	// the tree.
	//
	// A KEY NAMING NO UNIT, or an actor the tree does not seat, answers
	// false: the tree was read and holds no such membership.
	MemberOf(ctx context.Context, actor, unitKey string) (bool, error)

	// SeatGranted reports whether actor's seat's ROLE carries grant in the
	// company document — `role.health_tools`, for [SeatGrantHealth] — which
	// is what admits an agent seat to a verb whose row names the grant
	// ([rule.seatGrant]).
	//
	// A FACT ABOUT THE ROLE, NOT A RELATION: nobody is above or beside
	// anybody here. It is on the chart all the same because it is read off
	// the same tree, at the same moment, as every relation a decision asks,
	// and three-valued for the same reason: a node holding no company must
	// say so rather than tell a granted seat it holds nothing.
	//
	// A SEAT THE TREE DOES NOT HOLD, A HUMAN SEAT, AND A GRANT THIS BUILD
	// DOES NOT KNOW answer false: the tree was read and grants nothing of
	// the kind — a person is decided by the class, and a grant a newer
	// build wrote opens nothing here, as an unknown permission does not.
	SeatGranted(ctx context.Context, actor string, grant SeatGrant) (bool, error)
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

// LeadsContainer reports that this surface holds no chart.
func (NoChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, ErrNoChart
}

// LeadsAnyone reports that this surface holds no chart.
func (NoChart) LeadsAnyone(context.Context, string) (bool, error) {
	return false, ErrNoChart
}

// MemberOf reports that this surface holds no chart.
func (NoChart) MemberOf(context.Context, string, string) (bool, error) {
	return false, ErrNoChart
}

// SeatGranted reports that this surface holds no chart.
func (NoChart) SeatGranted(context.Context, string, SeatGrant) (bool, error) {
	return false, ErrNoChart
}
