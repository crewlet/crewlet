package authz

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// Standing is what one principal holds on one artifact, as [ClassShared]
// resolves it: the role every surface stamps into a write's record, shows a
// reader as their own, and shapes what a reader is sent by.
//
// TWO ROLES, because the admin path is not a relation. Role is what the caller
// may do; Related is what their relations alone grant, which tells an override
// from an ordinary admission ([Standing.Elevated]).
//
// THE ADMIN PATH IS ALL OR NOTHING, PER DECISION. An administrator whose
// relations reach the row stands at exactly the sharer's role; only a decision
// the admin path alone made stands at owner ([Decision.Elevated], recorded).
type Standing struct {
	// Role is the EFFECTIVE role: owner where the admin path alone admitted
	// the caller, and otherwise [Standing.Related]. Empty for a caller with
	// no relation and no admin path, which is nobody the artifact was shared
	// with.
	Role artifacts.Role

	// Related is the best role the caller's RELATIONS grant: owning the
	// artifact, a share naming their seat, a unit they sit in or the whole
	// company, and leading the agent that owns it, after the cap the
	// company's setting puts on an agent ([Object.AgentsManageOthers]).
	// Equal to Role for anybody the admin path does not reach.
	Related artifacts.Role

	// unsettled is why Role may be BELOW the caller's real role: an arm that
	// could have raised it asked the chart, and the chart could not answer.
	// Nil when Role is exact. See [Standing.Err].
	unsettled error
}

// Err reports why [Standing.Role] may be lower than the caller's actual role,
// wrapping [ErrUnsettled] and the chart's error, and nil when it is exact.
//
// AN ADMISSION AND A ROLE ARE TWO QUESTIONS. A viewer's seat share admits them
// to read whether or not the chart can say they also sit in a unit shared as
// editor; a role stamped into a data write then would have the applier refuse a
// write the caller may make. A surface needing the role asks this and answers
// unknown on an error, as [StandingOn] does.
//
// An administrator is never unsettled: the admin path's Role is exact (owner)
// and recorded; only Related may be a lower bound, so an override may be
// reported where a share would have made it ordinary, which an audit can live with.
func (s Standing) Err() error { return s.unsettled }

// Elevated reports whether a verb needing need is reached ONLY through the
// admin path: the effective role includes it and the caller's relations do
// not. It is [Decision.Elevated] for a requirement that is not a row's, such
// as a datastore collection's own write role.
func (s Standing) Elevated(need artifacts.Role) bool {
	return s.Role.Includes(need) && !s.Related.Includes(need)
}

// StandingOn is principal p's standing on artifact o, decided as reading it is
// ([ActionArtifactRead]): the EFFECTIVE-ROLE RESOLVER every surface that
// stamps a write, shows a reader their role or shapes a push by role asks.
//
// THREE-VALUED: a zero Standing with a nil error is a caller the artifact is
// not shared with; an error is a node that could not tell, because the read
// could not be decided or the role is unsettled ([Standing.Err]).
//
// ONE RESOLVER, THE TABLE'S OWN: [Decide] over the read row, so a stamped role
// is the role every row compares. A verb only the admin path admitted
// ([Decision.Elevated]) is acted on with that decision's standing (owner), not
// this one.
func StandingOn(ctx context.Context, p iam.Principal, o Object, chart Chart,
	now time.Time) (Standing, error) {

	d := Decide(ctx, p, ActionArtifactRead, o, chart, now)
	switch {
	case d.Unknown():
		return Standing{}, d.Err
	case !d.Allowed:
		return Standing{}, nil
	}
	if err := d.Standing.Err(); err != nil {
		return Standing{}, err
	}
	return d.Standing, nil
}

// decideShared is [ClassShared]: the caller's role on one artifact against the
// least role the row admits.
//
// # The order of the arms is the contract
//
// Unread object, then the relations, then the admin path, then unknown, then
// the refusals; each arm's reason is written where it stands below.
func decideShared(ctx context.Context, p iam.Principal, r rule, o Object,
	chart Chart) Decision {

	admin := p.Can(adminGrant)
	// AN OBJECT NOBODY HAS READ is the admin path's, or "read it and decide
	// again" ([ErrUnresolved]): the shares are on the stored row, so a gate that
	// formed the object from a tool's arguments cannot know them. An object
	// naming neither an owner nor a share is refused [ReasonUnnamed] beside the
	// admin path.
	if o.Unresolved {
		if admin {
			return overridden(relation{})
		}
		return Decision{Reason: ReasonNotShared, Err: ErrUnresolved}
	}
	if o.Owner == "" && len(o.Shares) == 0 {
		if admin {
			return overridden(relation{})
		}
		return consulting(Decision{Reason: ReasonUnnamed}, adminGrant)
	}
	// ALL THE RELATIONS ([relate]) are weighed for the role, not merely an
	// admission: the best relation decides what a write is stamped with.
	rel := relate(ctx, p, o, chart)
	switch {
	case rel.role.Includes(r.minRole) && (!admin || rel.standing(false).Err() == nil):
		return consulting(Decision{Allowed: true, Reason: rel.via,
			Standing: rel.standing(false)}, adminGrant)
	case admin:
		// AFTER THE RELATIONS, so an administrator whose share reaches the row
		// is admitted AT THE SHARER'S ROLE and no override is recorded that did
		// not happen; BEFORE ANY CHART FAILURE, so a node behind its chart never
		// tells an administrator it cannot tell.
		return overridden(rel)
	case rel.err != nil && rel.pending.Includes(r.minRole):
		// UNKNOWN only where an arm the chart could not answer could have
		// admitted: an unreadable unit share of viewer changes nothing about a
		// publish, and a 503 would send somebody back for the same refusal.
		return Decision{Reason: ReasonNotShared, Err: rel.err}
	case rel.role == "":
		// No relation is NOT FOUND ([Decision.NotFound]), saying nothing about
		// whether the artifact exists.
		return consulting(Decision{Reason: ReasonNotShared,
			Standing: rel.standing(false)}, adminGrant)
	case rel.capped && !artifacts.RoleContributor.Includes(r.minRole):
		// An agent kept off somebody else's work is refused as a seat, naming
		// nothing, as no share would change it.
		return Decision{Reason: ReasonSeatRefused, Standing: rel.standing(false)}
	}
	return consulting(Decision{Reason: ReasonRole, Role: r.minRole,
		Standing: rel.standing(false)}, adminGrant)
}

// overridden is the admin path's admission over what the caller's relations
// grant: owner, made by the permission and not by any relation, so ELEVATED.
func overridden(rel relation) Decision {
	return consulting(Decision{Allowed: true, Reason: ReasonGrant,
		Standing: rel.standing(true), Elevated: true}, adminGrant)
}

// relation is what the caller's relations to one artifact grant, before the
// admin path.
type relation struct {
	// role is the best role an arm that ANSWERED grants, after the cap.
	role artifacts.Role
	// via is the arm that granted role: [ReasonSelf], [ReasonShared] or
	// [ReasonLead]. Empty while role is.
	via Reason
	// pending is the best role an arm the chart could NOT answer might have
	// granted, after the cap, and err is the first such arm's error.
	pending artifacts.Role
	err     error
	// capped says the caller is an agent the company keeps to contributor on
	// somebody else's artifact ([Object.AgentsManageOthers]).
	capped bool
}

// standing is the [Standing] rel describes, with or without the admin path.
func (rel relation) standing(admin bool) Standing {
	s := Standing{Role: rel.role, Related: rel.role}
	if rel.err != nil && rel.pending.Rank() > rel.role.Rank() {
		s.unsettled = fmt.Errorf("%w: %w", ErrUnsettled, rel.err)
	}
	if admin {
		s.Role, s.unsettled = artifacts.RoleOwner, nil
	}
	return s
}

// relate resolves every relation the caller holds to the artifact: the best
// role, which arm granted it, and what an arm the chart could not answer might
// have added. The owner holds every role, by login or seat.
//
// THE ARMS THAT NEED NO CHART COME FIRST (a share naming the caller's seat, a
// share with the whole company), then the arms that ask it (leading the agent
// that owns the artifact, only for an agent owner ([Object.OwnerIsAgent]), then
// each unit share from the highest role down). An arm is asked only where it
// could still RAISE the role: a question whose answer changes nothing is one
// more way to answer unknown. A share this build cannot read (an audience kind
// a newer peer wrote, a role off the ladder) grants nothing.
func relate(ctx context.Context, p iam.Principal, o Object, chart Chart) relation {
	if ownedBy(p, o) {
		return relation{role: artifacts.RoleOwner, via: ReasonSelf}
	}
	var rel relation
	ceiling := artifacts.RoleOwner
	// AN AGENT IS CAPPED AT CONTRIBUTOR on an artifact it does not own unless
	// the company lifts the cap ([Object.AgentsManageOthers]). The cap applies
	// to every arm before it is asked, so a capped agent's chart questions stop
	// early.
	if p.Kind == iam.KindSeat && !o.AgentsManageOthers {
		ceiling, rel.capped = artifacts.RoleContributor, true
	}
	cut := func(r artifacts.Role) artifacts.Role {
		if r.Rank() > ceiling.Rank() {
			return ceiling
		}
		return r
	}
	raise := func(r artifacts.Role, via Reason) {
		if r = cut(r); r.Rank() > rel.role.Rank() {
			rel.role, rel.via = r, via
		}
	}
	miss := func(r artifacts.Role, err error) {
		if r = cut(r); r.Rank() > rel.pending.Rank() {
			rel.pending = r
		}
		if rel.err == nil {
			rel.err = err
		}
	}
	raises := func(r artifacts.Role) bool { return cut(r).Rank() > rel.role.Rank() }

	var units []artifacts.Share
	for _, s := range o.Shares {
		switch s.Audience.Kind {
		case artifacts.AudienceSeat:
			if p.Seat != "" && s.Audience.ID == p.Seat {
				raise(s.Role, ReasonShared)
			}
		case artifacts.AudienceCompany:
			raise(s.Role, ReasonShared)
		case artifacts.AudienceUnit:
			units = append(units, s)
		}
	}

	actor := actorOf(p)
	if o.OwnerIsAgent && raises(artifacts.RoleOwner) {
		switch led, err := chartAsks(chart, actor, func(c Chart) (bool, error) {
			return c.Leads(ctx, actor, o.Owner)
		}); {
		case err != nil:
			miss(artifacts.RoleOwner, err)
		case led:
			raise(artifacts.RoleOwner, ReasonLead)
		}
	}
	slices.SortStableFunc(units, func(a, b artifacts.Share) int {
		return cmp.Compare(b.Role.Rank(), a.Role.Rank())
	})
	for _, s := range units {
		if !raises(s.Role) {
			// SORTED FROM THE HIGHEST ROLE DOWN, so nothing after this
			// one could raise it either.
			break
		}
		switch member, err := chartAsks(chart, actor, func(c Chart) (bool, error) {
			return c.MemberOf(ctx, actor, s.Audience.ID)
		}); {
		case err != nil:
			miss(s.Role, err)
		case member:
			raise(s.Role, ReasonShared)
		}
	}
	return rel
}

// chartAsks asks one relation of the chart for actor, with the two answers
// every relation helper here gives before the chart is consulted: a caller the
// chart cannot know (no seat and no login) holds no relation, and a surface
// with no chart cannot tell ([ErrNoChart]).
func chartAsks(chart Chart, actor string, ask func(Chart) (bool, error)) (bool, error) {
	if actor == "" {
		return false, nil
	}
	if chart == nil {
		return false, ErrNoChart
	}
	return ask(chart)
}
