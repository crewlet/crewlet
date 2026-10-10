package auth

import (
	"context"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// Reach is how far into the company a caller may see and act (ADR-0031).
//
// ORDERED, and every surface names the reach it needs: each question, each
// mounted route, each push kind and each operator tool. A caller is served
// what their reach COVERS ([Reach.Covers]) and nothing past it.
//
// ONE WITHHOLDING RULE decides the line between the two keyed reaches:
// members read what the company PUBLISHED — its work, its pages, its chart,
// who is working on what — and admins also read what the machine PROCESSED
// (prompts, responses, tool arguments, diaries, events, traces, spend) and how
// it is RUN (nodes, keys, secrets, configuration). A surface that cannot say
// which side it is on has not been classified yet, and the zero Reach is
// refused wherever one is declared rather than read as either.
type Reach string

const (
	// ReachOpen is anyone who can reach the port: the probes, the dashboard
	// shell and its assets, the question "who am I", and the routes outside
	// parties call, which authenticate by their own signature or per-run
	// token rather than by a key.
	ReachOpen Reach = "open"
	// ReachPublic is the company's public face — its name, mission and
	// chart — which a caller with no key reaches unless Tier A says
	// `anonymous: none`.
	ReachPublic Reach = "public"
	// ReachMember is what the company published, for a member key and
	// above.
	ReachMember Reach = "member"
	// ReachAdmin is what the machine processed and how it is run, for an
	// admin key only.
	ReachAdmin Reach = "admin"
)

// Reaches is every reach, lowest first — which is also the order [Reach.Covers]
// compares by.
var Reaches = []Reach{ReachOpen, ReachPublic, ReachMember, ReachAdmin}

// Valid reports whether r is a reach this build knows.
func (r Reach) Valid() bool { return slices.Contains(Reaches, r) }

// Covers reports whether a caller holding r may be served something that needs
// need. An unknown reach covers nothing and is covered by nothing, so a value
// that is not one of [Reaches] can never open a surface.
func (r Reach) Covers(need Reach) bool {
	have, want := slices.Index(Reaches, r), slices.Index(Reaches, need)
	return have >= 0 && want >= 0 && have >= want
}

// ReachOfRole is the reach a key of this role carries. A role this build does
// not know carries [ReachOpen], never a keyed reach: validation refuses one,
// and a struct built around validation must not open the company.
func ReachOfRole(role config.TokenRole) Reach {
	switch role {
	case config.RoleAdmin:
		return ReachAdmin
	case config.RoleMember:
		return ReachMember
	default:
		return ReachOpen
	}
}

// ReachOfAnonymous is the reach a caller presenting no key carries under this
// posture. An unknown posture is [ReachOpen], for the reason [ReachOfRole]
// gives.
func ReachOfAnonymous(posture config.AnonymousAccess) Reach {
	if posture == config.AnonymousPublic {
		return ReachPublic
	}
	return ReachOpen
}

// Principal is who a request is, as the guard resolved it.
//
// EVERY REQUEST HAS ONE, anonymous included, so no handler has to tell "no
// principal attached" from "nobody": an anonymous caller is a Principal with
// no ID whose Reach is the anonymous posture's.
type Principal struct {
	// ID is the accepted key's label (`api.auth.tokens[].id`), recorded as
	// the author of every write made with it — or "" for a caller who
	// presented none, and [AnonymousOperator] while the guard is disabled.
	ID string
	// Role is the key's role, "" for an anonymous caller.
	Role config.TokenRole
	// Reach is what this caller may be served, decided once by the guard so
	// that every surface compares against the same value.
	Reach Reach
}

// Authenticated reports whether the caller presented a key the guard accepted
// (or the guard is disabled, which accepts every caller as an admin).
func (p Principal) Authenticated() bool { return p.ID != "" }

// IsAdmin reports whether the caller reaches the admin surfaces.
func (p Principal) IsAdmin() bool { return p.Reach.Covers(ReachAdmin) }

// principalKey carries the resolved principal down the handler chain.
type principalKey struct{}

// PrincipalFrom returns the principal the guard attached. A context the guard
// never saw carries the zero Principal: no ID, and a Reach that covers
// nothing — so a surface reached around the guard is refused rather than
// opened.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithPrincipal attaches a principal, for a surface that authenticates outside
// the middleware: the WebSocket handshake, and a socket frame that presents a
// key of its own for one question.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Router is what a surface mounts its routes on, NAMING THE REACH EACH NEEDS:
// the two registration verbs of [net/http.ServeMux], each with the reach the
// route serves.
//
// AN INTERFACE rather than the mux itself so the API can KEEP every pattern a
// surface mounts and the reach it declared: both of a node's listeners serve
// one route table behind a partition that is a predicate over paths, and the
// route gate walks that table whole. Declared HERE, beside [Reach], because
// every surface's Routes method names it and two interfaces with one method
// set are still two types there.
//
// There is no default and no unguarded overload: a route that cannot say who
// may reach it does not compile, which is the whole of what this interface is
// for. The table behind it ([Guard.Require]) refuses a caller below the reach
// before the handler runs, so a handler never re-checks it — and a handler
// that answers DIFFERENTLY per caller reads [PrincipalFrom] for that, never
// for whether to answer at all.
type Router interface {
	Handle(pattern string, reach Reach, handler http.Handler)
	HandleFunc(pattern string, reach Reach, handler func(http.ResponseWriter, *http.Request))
}
