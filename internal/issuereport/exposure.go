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
	"internal/code/host",
	// An exchange another node received and relayed here.
	"internal/httprelay",
	// A key set an issuer publishes.
	"internal/jwks",
	// A per-run endpoint's token.
	"internal/runtoken",
	// Signed requests and the answers to them.
	"internal/sigv4",
}

// BeforeAuthentication reports whether pkg parses input nobody has authenticated
// yet: one of [beforeAuthentication], or a package beneath one.
func BeforeAuthentication(pkg Component) bool {
	return slices.ContainsFunc(beforeAuthentication, func(held Component) bool {
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
