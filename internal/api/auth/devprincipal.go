package auth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/version"
)

// THE DEVELOPMENT PRINCIPAL, and what it is allowed to replace.
//
// # What it replaces
//
// `api.auth.disabled` authenticated the EMPTY credential into full operator
// authority, with no check on the bind address anywhere — so one unset
// environment variable was a total bypass of every gate this package has, on
// any deployment, reachable by anybody who could reach the port. It is retired
// (see config.retiredBootstrapFields), and this is the thing somebody actually
// wanted when they reached for it: a laptop where the dashboard opens without
// a token while its author is writing a feature.
//
// # The two refusals, and why neither alone is enough
//
//   - THE BIND MUST BE LOOPBACK. That is the only check that physically
//     prevents another machine reaching this, which is the whole difference
//     from `disabled`. It judges `api.host` rather than `api.external_url`,
//     deliberately and unlike `api.auth.local`'s insecure rule: that one is
//     about whether a cookie crosses plaintext through a proxy, where the
//     external address is the truth, and this one is about who can open a
//     socket to this process, where the bind is.
//   - THE BINARY MUST BE A DEVELOPMENT BUILD. A bind is a setting, and a
//     setting reaches production by being copied — a compose file, a Helm
//     value, a base image somebody forked. [version.IsDevelopment] asks
//     positively rather than testing for a release shape, so anything this
//     build cannot recognise as a development build refuses the flag, and no
//     copied setting can carry this into a deployment that pulled a tag.
//
// It is a FLAG rather than a config field for the same reason: a field gets
// copied into an image, and the copy is the failure.
//
// # What it grants
//
// The deployment's own ceiling, `api.auth.max_grants`, and no more — which is
// what a Tier A token gets. `disabled` granted everything unconditionally,
// which meant a laptop's convenience and the deployment's authority were the
// same thing; here an operator who has narrowed their ceiling has narrowed
// this too.

// DevPrincipalLoginPrefix is the class segment the development principal's
// login carries.
//
// THE MACHINE GRAMMAR, like a Tier A token's, because that is what it is: a
// credential belonging to this process rather than a person who enrolled. A
// distinct prefix from `token:` so an audit row says which it was — the whole
// point of writing one is that a reader can tell a development bypass from a
// break-glass credential months later.
const DevPrincipalLoginPrefix = "dev:"

// DevPrincipalNamespace is the UUIDv5 namespace the development principal's id
// is derived under, for [TokenNamespace]'s reason: a stable id across restarts
// so the audit rows one laptop writes resolve to one principal.
//
// ITS OWN NAMESPACE rather than TokenNamespace's, so a development principal
// and a Tier A token sharing a name are still two different ids. They are two
// different things, and one id for both would let a laptop's rows and a
// pipeline's merge in the audit feed.
var DevPrincipalNamespace = uuid.MustParse("c4a7b8e2-5d31-5f96-9b02-7e1a3c5d8f44")

// DevPrincipal is the identity a `-dev-principal` run resolves every
// unauthenticated request to.
type DevPrincipal struct {
	login  string
	grants []iam.Grant
}

// NewDevPrincipal builds it, or refuses with the reason.
//
// REFUSED AT CONSTRUCTION rather than per request, because this is a boot-time
// decision about the whole process and a per-request check would be a rule
// somebody could reach a surface around. The engine fails to start rather than
// starting without the flag it was given, which is the honest answer to being
// asked for something this build will not do.
func NewDevPrincipal(login string, b *config.Bootstrap) (*DevPrincipal, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, nil
	}
	if !version.IsDevelopment() {
		return nil, fmt.Errorf("-dev-principal is refused by a binary that is "+
			"not a development build (this one reports version %q): it "+
			"authenticates every request with no credential at all, and a "+
			"setting that reaches production does so by being copied into an "+
			"image. Build from source to use it, or create an "+
			"`api.auth.tokens` entry, which is listable, revocable and "+
			"audited", version.String())
	}
	if b == nil {
		return nil, fmt.Errorf("-dev-principal needs a configuration to read " +
			"`api.host` and `api.auth.max_grants` from: without one it cannot " +
			"establish that this node binds loopback, and it is refused rather " +
			"than assumed")
	}
	if !BindIsLoopback(b.API.Host) {
		return nil, fmt.Errorf("-dev-principal is refused on `api.host: %q`, "+
			"which other machines can reach: it authenticates every request "+
			"with no credential at all, so the bind is the only thing that "+
			"keeps it to this machine. Set `api.host` to 127.0.0.1, or create "+
			"an `api.auth.tokens` entry for anything reachable from elsewhere",
			b.API.Host)
	}
	if strings.ContainsAny(login, ":.") {
		return nil, fmt.Errorf("-dev-principal %q may not contain `:` or `.`: "+
			"internal/iam keeps three identity namespaces apart by the SHAPE "+
			"of the name, and a login carrying either separator would collide "+
			"with a machine handle or a person's dotted login", login)
	}
	return &DevPrincipal{
		login:  DevPrincipalLoginPrefix + login,
		grants: b.API.Auth.MaxGrants,
	}, nil
}

// principal is the identity it resolves to.
func (d *DevPrincipal) principal() iam.Principal {
	return iam.Principal{
		ID:     uuid.NewSHA1(DevPrincipalNamespace, []byte(d.login)),
		Login:  d.login,
		Kind:   iam.KindMachine,
		Stage:  iam.StageActive,
		Grants: d.grants,
	}
}

// WithDevPrincipal installs it, and returns the guard for chaining.
//
// A NIL INSTALLS NOTHING, which is what an unset flag produces, so the caller
// wires this unconditionally and the guard is unchanged when nobody asked.
func (g *Guard) WithDevPrincipal(d *DevPrincipal) *Guard {
	g.dev = d
	return g
}

// devResolution is the development principal's answer for a request that
// presented no credential, or nil when there is none.
//
// ONLY FOR AN ABSENT CREDENTIAL. A token that is present and WRONG still
// resolves anonymous, because the person typed something and being told it
// worked would hide exactly the typo they are about to spend an afternoon on
// — and because it is the one arm where the caller has made a claim this node
// has checked and refused.
func (g *Guard) devResolution(r *http.Request) *iam.Principal {
	if g.dev == nil || g.Credential(r) != "" {
		return nil
	}
	p := g.dev.principal()
	return &p
}
