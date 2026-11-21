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

// It replaces the removed `api.auth.disabled`, which authenticated the EMPTY
// credential into full operator authority with no bind check, a total bypass
// on any deployment (a Tier A file still carrying it is refused by name, see
// config's removedKeys). What somebody wanted was a laptop dashboard opening
// without a token. Two refusals, neither enough alone:
//
//   - THE BIND MUST BE LOOPBACK, the only check that physically keeps another
//     machine out. It judges `api.host`, not `api.external_url`: this is about
//     who can open a socket to this process, where the bind is.
//   - THE BINARY MUST BE A DEVELOPMENT BUILD, since a setting reaches
//     production by being copied (compose file, Helm value, forked image).
//     [version.IsDevelopment] asks positively, so anything unrecognised refuses.

// It is a FLAG, not a config field, for the same reason: a field gets copied.
//
// It grants every app, and admin where this node serves it: what a Tier A token
// listing `[admin]` carries, and no more; a posture this build cannot read
// confers nothing.

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
		return nil, fmt.Errorf("-dev-principal only works on a development "+
			"build (this one is %q). Use an `api.auth.tokens` entry instead",
			version.String())
	}
	if b == nil {
		return nil, fmt.Errorf("-dev-principal needs a config file with " +
			"`api.host` and `api.auth.admin`. Pass one with -config")
	}
	if !BindIsLoopback(b.API.Host) {
		return nil, fmt.Errorf("-dev-principal only works on loopback, and "+
			"`api.host: %q` isn't. Set `api.host` to 127.0.0.1, or use an "+
			"`api.auth.tokens` entry", b.API.Host)
	}
	if strings.ContainsAny(login, ":.") {
		return nil, fmt.Errorf("-dev-principal %q can't contain `:` or `.`. "+
			"Use a single word, like a seat handle", login)
	}
	return &DevPrincipal{
		login:  DevPrincipalLoginPrefix + login,
		grants: b.API.Auth.Admin.Effective([]iam.Grant{iam.GrantAdmin}),
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
	// FRESH BY CONSTRUCTION, as every credential with nobody at a keyboard
	// is: a laptop run with no credential at all has nothing it could
	// present to confirm who is there, and a development principal that
	// could never reach a step-up surface could not edit the very company
	// it was started to develop against. The bypass is already gated on an
	// unversioned build and a loopback host; this adds no reach it lacks.
	g.proof.stamp(&p, g.now())
	return &p
}
