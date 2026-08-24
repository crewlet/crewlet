package plane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The Plane half of provisioning: a service account per agent seat, project
// memberships, a token minted into the config's own `${VAR}`, and the
// workspace webhook — whose secret Plane GENERATES and this run captures.
//
// # The capability preflight, and why it comes first
//
// Service accounts and token lifecycle are fork features. On stock Plane
// Community the endpoints are simply not there, and a run that discovered
// that halfway would have created some accounts, minted some tokens and
// stopped — leaving an operator to work out which. So the run PROBES first
// and refuses before it touches anything, naming what is missing.
//
// The probe is a METHOD PROBE rather than a version check: a fork's version
// string says nothing reliable about which endpoints it carries, and an
// operator running a patched instance is exactly who a version gate refuses
// wrongly.

// SeatEnv is the mcp_env server whose credentials belong to Plane, and
// SeatKey is the variable inside it holding a seat's own API key.
const (
	SeatEnv = "plane"
	SeatKey = "PLANE_API_KEY"
)

// EngineHandle is the account the ENGINE itself authenticates as.
//
// Not a seat: it reads subscriber lists, resolves project identifiers and
// enriches webhook payloads, on behalf of whichever agent a delivery is
// routed to. Without it the integration still works and routing degrades to
// payload-derived targets alone — which is a company where a comment
// mentioning three people wakes only the one the payload happened to name.
//
// It is provisioned like a seat and reported separately, because an
// operator reading "1 account created" for a two-seat company needs to know
// which one did not happen.
const EngineHandle = "engine"

// PlanFor walks the org for seats that need a Plane account.
func PlanFor(o *org.Organization, cfg *config.Plane) (*provision.Plan, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, errors.New("plane: the company config does not enable plane")
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, errors.New(
			"plane: integrations.plane.workspace is unset — every resource " +
				"path is workspace-scoped, so nothing can be addressed without it")
	}
	plan := &provision.Plan{}
	claimed := map[string]string{}
	for seat := range o.AllRoles() {
		if !seat.IsAgent() {
			continue
		}
		handle := seat.Handle()
		value := strings.TrimSpace(seat.MCPEnv[SeatEnv][SeatKey])
		if value == "" {
			continue
		}
		name, ok := provision.SoleVar(value)
		if !ok {
			plan.Note("%s: mcp_env.plane.%s is %s rather than a whole ${VAR} "+
				"reference, so there is nowhere to mint a token — point it "+
				"at a variable, or manage this seat's credential by hand",
				handle, SeatKey, describeShape(value))
			continue
		}
		if owner, taken := claimed[name]; taken {
			// ONE VARIABLE CANNOT RECORD TWO IDENTITIES. The second seat
			// would overwrite the first, and the first agent would then
			// authenticate as the second — every action it took
			// attributed to the wrong seat, with nothing anywhere
			// reporting a problem.
			return nil, fmt.Errorf(
				"plane: %s and %s both mint into ${%s}; one variable cannot "+
					"hold two seats' credentials — give each seat its own",
				owner, handle, name)
		}
		claimed[name] = handle
		plan.Add(provision.Seat{Handle: handle, Role: seat.Name, TokenVar: name})
	}
	// THE ENGINE'S OWN ACCOUNT IS PART OF THE PLAN, so a --dry-run shows
	// it and the same walk decides it — a second derivation in the run
	// itself is how a plan and a run come to disagree about what happens.
	if name, ok := provision.SoleVar(cfg.Token); ok {
		if owner, taken := claimed[name]; taken {
			return nil, fmt.Errorf(
				"plane: integrations.plane.token and the seat %s both mint "+
					"into ${%s}; the engine would authenticate as that seat, "+
					"attributing every routing read to it", owner, name)
		}
		plan.Add(provision.Seat{
			Handle: EngineHandle, Role: "Crewlet engine", TokenVar: name,
		})
	} else if strings.TrimSpace(cfg.Token) == "" {
		plan.Note("integrations.plane.token is unset, so the engine has no " +
			"read credential of its own: routing falls back to the targets a " +
			"payload happens to name, and the project cache can only learn " +
			"from payloads. Point it at a ${VAR} to have this run mint one")
	} else {
		plan.Note("integrations.plane.token is %s rather than a whole ${VAR} "+
			"reference, so the engine's own credential cannot be minted here",
			describeShape(cfg.Token))
	}
	return plan, nil
}

func describeShape(value string) string {
	if len(provision.ReferencedVars(value)) == 0 {
		return "a literal"
	}
	return "a reference embedded in other text"
}

// AccountUsername is a seat's service-account name.
func AccountUsername(p *config.PlaneProvisioning, handle string) string {
	prefix := "crewlet-"
	if p != nil && strings.TrimSpace(p.UsernamePrefix) != "" {
		prefix = strings.TrimSpace(p.UsernamePrefix)
	}
	return strings.ToLower(prefix + handle)
}

// AccountRole is the workspace role a seat's account is created with.
//
// MEMBER unless the config says otherwise, never admin. The create endpoint
// itself defaults to admin, so a company that names no role would otherwise
// get a workspace full of administrators — and the seats this provisions are
// the ones an agent authenticates as.
func AccountRole(p *config.PlaneProvisioning, handle string) int {
	if handle == EngineHandle {
		// PINNED, and not to the configured default: the engine reads
		// subscriber lists and project members, which a guest cannot,
		// and it writes nothing, which is what makes admin wrong. A
		// company that set `role: guest` for its agents would otherwise
		// get an engine that silently resolves nobody.
		return RoleMember
	}
	role := config.PlaneMember
	if p != nil {
		if p.Role != "" {
			role = p.Role
		}
		if override, ok := p.Roles[handle]; ok && override != "" {
			role = override
		}
	}
	switch role {
	case config.PlaneAdmin:
		return RoleAdmin
	case config.PlaneGuest:
		return RoleGuest
	default:
		return RoleMember
	}
}

// ---- the capability preflight ------------------------------------------ //

// zeroUUID addresses a token collection that cannot exist.
//
// The token-lifecycle probe needs a URL that RESOLVES without naming a real
// account, because the alternative — probing against a seat's account —
// would make the answer depend on which seat happened to be first and would
// touch a live credential to ask a question about the instance.
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// Capabilities is what this instance can actually do.
//
// Established BEFORE anything is created, and every field is a separate
// question because the remedies differ: a missing endpoint means the wrong
// distribution, while a refused one means the wrong role.
type Capabilities struct {
	// Workspace says the configured slug resolves for this credential.
	// Its own question because a mistyped slug and a permission problem
	// produce the same 403 everywhere else.
	Workspace bool

	// Admin says this credential is a workspace administrator. Every
	// mutation below needs it regardless of whether the route exists.
	Admin bool

	// ServiceAccounts is the account endpoint.
	ServiceAccounts bool

	// TokenLifecycle is minting and revoking an account's API tokens.
	//
	// Its absence is DEGRADED rather than fatal: an account's create
	// response carries its first token, so a run can still stand up new
	// seats — it just cannot rotate the credential of a seat that already
	// has one.
	TokenLifecycle bool

	// Webhooks is the workspace webhook endpoint.
	Webhooks bool
}

// Fatal names what stops the run, or nothing.
func (c Capabilities) Fatal() []string {
	var out []string
	if !c.Workspace {
		out = append(out, "the configured workspace slug does not resolve for "+
			"this credential — check integrations.plane.workspace")
	}
	if !c.Admin {
		out = append(out, "this credential is not a workspace administrator; "+
			"accounts, memberships and webhooks all require one")
	}
	if !c.ServiceAccounts {
		out = append(out, "this instance has no service-account API, which "+
			"stock Plane Community does not ship — agent seats cannot be "+
			"created on it")
	}
	if !c.Webhooks {
		out = append(out, "this instance has no workspace webhook API, so no "+
			"agent could be woken by anything happening in it")
	}
	return out
}

// Degraded names what the run will not be able to do, or nothing.
func (c Capabilities) Degraded() []string {
	if c.TokenLifecycle {
		return nil
	}
	return []string{"this instance has no token-lifecycle API: a NEW seat's " +
		"account still carries a token from its creation, but a seat that " +
		"already has an account cannot have its credential rotated"}
}

// Probe asks the instance what it supports, without changing anything.
//
// # Every probe is a request the route is expected to REFUSE
//
// A capability check that created something to find out whether it could
// would leave that something behind on the instance of an operator who was
// then told the run could not proceed. So each probe is a method the route
// does not allow, or a plain read, and the STATUS is the answer.
//
// # A transport failure is not an answer
//
// A dropped connection returns an error rather than a capability set. The
// alternative — reading it as absence — would tell an operator their fork
// lacks a feature because their network blinked.
func (c *Client) Probe(ctx context.Context) (Capabilities, error) {
	var caps Capabilities

	// FIRST, and separately: a credential that does not authenticate makes
	// every later 403 unreadable, since it cannot be told from the
	// permission refusal the probes are looking for.
	if _, err := c.Me(ctx); err != nil {
		return caps, fmt.Errorf("plane: this credential does not authenticate "+
			"against %s: %w", c.base, err)
	}

	// The cheapest workspace-scoped route that does NOT require admin: any
	// member gets 200, and a slug that does not exist cannot satisfy the
	// permission class whatever the credential's real rights are.
	status, err := c.probe(ctx, http.MethodGet, c.ws("/projects/"))
	if err != nil {
		return caps, err
	}
	caps.Workspace = status == http.StatusOK

	// Admin-only, so a 200 here IS the administrator check. It is also the
	// enumeration the reconcile reads, which is why it is a plain GET.
	if status, err = c.probe(ctx, http.MethodGet, c.ws("/members/")); err != nil {
		return caps, err
	}
	caps.Admin = status == http.StatusOK

	// The fork registers this route POST-only, so a GET that resolves is
	// refused for its METHOD — which is a clean presence signal that
	// creates nothing.
	if status, err = c.probe(ctx, http.MethodGet, c.ws("/service-accounts/")); err != nil {
		return caps, err
	}
	caps.ServiceAccounts = present(status)

	// PATCH is allowed by no token route, and the zero UUID names no
	// account — so this asks whether the route family is registered
	// without touching any account's credentials.
	if status, err = c.probe(ctx, http.MethodPatch,
		c.ws("/service-accounts/"+zeroUUID+"/tokens/")); err != nil {
		return caps, err
	}
	caps.TokenLifecycle = present(status)

	if status, err = c.probe(ctx, http.MethodGet, c.ws("/webhooks/")); err != nil {
		return caps, err
	}
	caps.Webhooks = present(status)

	return caps, nil
}

// probe issues one request and reports its status.
//
// A refusal is the RESULT rather than an error; only a request that never
// landed is an error, because only that leaves the question unanswered.
func (c *Client) probe(ctx context.Context, method, path string) (int, error) {
	err := c.send(ctx, method, path, nil, nil)
	if err == nil {
		return http.StatusOK, nil
	}
	if status := Status(err); status != 0 {
		return status, nil
	}
	return 0, fmt.Errorf("plane: probing %s %s: %w", method, path, err)
}

// present reports a route that exists, from the status of a request
// deliberately made wrong.
//
// 404 IS THE ONLY ABSENCE. A 405 is the route rejecting the method, a 403 is
// its permission class refusing this credential, a 400 is its serializer
// disliking the body — every one of those is the framework having RESOLVED
// the URL, which is the question being asked. Reading 403 as absence is the
// mistake that costs: it would tell an operator running the fork that their
// instance lacks a feature it has, when the fix is one role change that the
// separate administrator check already names.
func present(status int) bool {
	return status != http.StatusNotFound
}
