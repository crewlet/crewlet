package issuereport

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/panics"
)

// beforeAuthentication are the packages that parse input from outside the
// company's principals before that input is authenticated: a defect in one may
// be raised at will by whoever can reach the node, so a report placed in one
// follows SECURITY.md. A package beneath one is held the same way. The route
// table's walk in internal/api fails on an unauthenticated route whose handler
// lives anywhere else.
var beforeAuthentication = []Component{
	// The guard, which reads every credential a request presents.
	"internal/api/auth",
	// Sign-in, invitations and password resets.
	"internal/api/authapi",
	// A box's tool calls, read before their run token is checked.
	"internal/api/mcpbridge",
	// Vendor deliveries and app landings, read to check their signatures.
	"internal/api/webhooks",
	// A box's git and code reads, and the receive path a push takes.
	"internal/gitlet/host",
	// An exchange another node received and relayed here.
	"internal/httprelay",
	// A key set an issuer publishes.
	"internal/jwks",
	// A per-run endpoint's token.
	"internal/runtoken",
	// Signed requests and the answers to them.
	"internal/sigv4",
}

// executesToolCalls are the packages that run a tool call on the arguments a
// model chose. A fatal crash there cannot be contained the way a panic is, and
// anybody who can put text in front of a seat may be able to steer one (ADR-0040),
// so a crash with a frame in one is never filed publicly. Every package that
// declares a tool is here: the suite walks the source for them.
var executesToolCalls = []Component{
	// The tool loop and the surface every call goes through.
	"internal/agent/toolloop",
	"internal/tools",
	// The bridge and the session a harness's calls arrive by.
	"internal/api/mcpbridge",
	"internal/toolsession",
	// The engine's own tools, the discovery tools the runner offers, a
	// structured answer, a delegation and a tool server's.
	"internal/agent/builtin",
	"internal/agent/runner",
	"internal/agent/structured",
	"internal/agent/subagent",
	"internal/mcp",
}

// BeforeAuthentication reports whether pkg parses input nobody has authenticated
// yet: one of [beforeAuthentication], or a package beneath one.
func BeforeAuthentication(pkg Component) bool { return within(pkg, beforeAuthentication) }

// ExecutesToolCalls reports whether pkg runs a tool call on a model's arguments:
// one of [executesToolCalls], or a package beneath one.
func ExecutesToolCalls(pkg Component) bool { return within(pkg, executesToolCalls) }

func within(pkg Component, set []Component) bool {
	return slices.ContainsFunc(set, func(held Component) bool {
		return pkg == held || strings.HasPrefix(string(pkg), string(held)+"/")
	})
}

// exposedPanic says a panic's report must carry its site alone: it served input
// nobody had authenticated, or its site or a frame it would send is in such a
// package. Frames would show an outside party the path into the defect.
func exposedPanic(p *panics.Panic, at Location, frames []Frame) bool {
	if p.Unauthenticated || (at.Site != "" && BeforeAuthentication(at.Site.Package())) {
		return true
	}
	return slices.ContainsFunc(frames, func(f Frame) bool { return BeforeAuthentication(f.Function.Package()) })
}
