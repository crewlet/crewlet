package config

import (
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// ---- api ------------------------------------------------------------- //

// DefaultAPIHost binds every interface, which is what a container needs.
const DefaultAPIHost = "0.0.0.0"

// DefaultAPIPort is the port this project's own examples, quickstart, image
// and documentation all name.
//
// ONE NUMBER, because there were two. The examples, the quickstart and
// `docs/getting-started/configuration.md` bind 8000; the Dockerfile's EXPOSE,
// the control-plane page, the API reference and the Slack walkthrough said
// 8080 — so a founder who followed the quickstart and then pasted a curl from
// the reference got a connection refused, and the container published a port
// nothing was listening on. 8000 wins because it is what the two things a
// reader actually RUNS carry, and because it is not the port every other
// service on a developer's laptop has already taken.
//
// It is NOT a default applied to [API.Port]: zero there means "serve no HTTP
// at all", which is a real posture a default would take away. This constant is
// what the flag, the image and every page quote.
const DefaultAPIPort = 8000

// API is the Tier A HTTP surface: where it binds, where a browser reaches it,
// whose forwarded headers it believes, and how anyone proves who they are.
type API struct {
	// Host is the bind address.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the HTTP surface."`

	// Port is the bind port of this node's ONE listener, and node.roles
	// decides what it carries. With the ingress role it is the whole HTTP
	// surface: the dashboard, the REST API, the webhooks and the probes,
	// less the routes [API.Public] moves to a listener of their own.
	// Without it, the probes alone (/health and /ready), because an
	// orchestrator runs every node and must be able to probe each, while
	// the API is placed only where traffic is meant to arrive. ONE PORT
	// rather than a second setting for the probes: the probes are already
	// on this listener on an ingress node, so a probe port beside it would
	// be a second address for the same two routes there, and one Tier A
	// file keeps working for both shapes. 0 binds nothing at all, on any
	// node.
	//
	// THE BRIDGE IS THE EXECUTOR'S ALONE. A seat's executor is its CLI's own
	// run in a box, which may be another machine, so it reaches the seat's
	// tools over HTTP, on an ingress node's listener; every other phase runs
	// its CLI beside the engine and reaches them through a unix socket in the
	// engine's own session directory (internal/toolsession), so those need no
	// port at all. A direct box on a node that serves the bridge route itself
	// — the ingress role and a bound listener — dials it here on loopback;
	// every other box dials CREWLET_MCP_BRIDGE_URL, the fleet's address, and
	// the ingress node that answers relays each call to the node holding the
	// seat over the broker (internal/api/mcpbridge). So a seats node without
	// the ingress role serves its seats' tools with this at 0.
	//
	// So a port is not what makes a node SERVE the API — a port and the
	// ingress role together are ([Bootstrap.ServesAPI]), and only that pair
	// asks for the authentication settings below.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port of this node's HTTP listener: the whole API with the ingress role (less the routes api.public moves to its own listener), only /health and /ready without it. 0 binds nothing."`

	// ExternalURL is where a BROWSER reaches this deployment — a person, a
	// session cookie and every link a person follows — and, while no
	// public listener is set, where a vendor and a sandbox box reach it
	// too. REQUIRED once this node serves the API ([Bootstrap.ServesAPI]).
	//
	// # What reads it
	//
	//   - the session cookie's `Secure` flag and its `__Host-` prefix,
	//     which follow this scheme. The engine sits behind a
	//     TLS-terminating proxy and therefore cannot read `r.TLS` — the
	//     request it sees is plain http on a loopback socket — so there
	//     is nothing else to derive them from;
	//   - the origin a non-GET request and a socket handshake are checked
	//     against;
	//   - every link a PERSON follows: a dashboard address in a
	//     notification, a page-change link, a skill's `${crewlet_base_url}`;
	//   - the base every vendor webhook and app manifest is built on —
	//     UNLESS [API.Public] opens a listener of its own, in which case
	//     those routes are served only there and [APIPublic.ExternalURL] is
	//     their base ([API.WebhookBase]). A box's per-run sandbox endpoints
	//     are built on neither: they come from CREWLET_MCP_BRIDGE_URL and
	//     CREWLET_SANDBOX_OTEL_RECEIVER_URL, which name the same address.
	//
	// # Two sockets, two addresses, and no fallback between them
	//
	// It replaced the Tier B `integrations.public_base_url` and
	// `integrations.dashboard_base_url`. One address cannot be right for
	// both sides once the public routes have a socket of their own — the
	// person reaches the admin listener and the vendor reaches the public
	// one — so each socket names its own, and neither stands in for the
	// other where they differ: a vendor handed this address once
	// api.public is set would be handed a socket that answers 404 for the
	// route it was given.
	//
	// # Why it is Tier A
	//
	// The old fields were Tier B, so each was a `${VAR}` a node resolved at
	// the surface that used it — and a node that could not resolve one
	// built a manifest from the literal seven characters. The cookie and
	// the origin check cannot work that way: they are decided before a
	// company document is loaded, and they are part of what authenticates
	// the request that would go on to load one. A value the authentication
	// path depends on belongs in the tier that holds the keyring.
	//
	// Written as scheme://host[:port], with no query or fragment: it is a
	// BASE, and every URL built from it is this value with a path appended.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" desc:"Where a browser reaches this deployment, e.g. https://crewlet.example.com, and where a vendor does too unless api.public names its own. Required once this node serves the API: a port and the ingress role."`

	// TrustedProxies are the peers whose `X-Forwarded-For` and
	// `X-Forwarded-Proto` this deployment believes, as CIDR blocks.
	//
	// A CIDR LIST AND NEVER A BOOL. The question a forwarded header poses
	// is not "does this deployment sit behind a proxy" — it is "is THIS
	// peer the proxy", and a bool answers the first while the throttle,
	// the audit trail and every per-source rule need the second. A bool
	// set true trusts a header anybody can send, which hands an attacker
	// the ability to choose their own rate-limit bucket and their own
	// audit row; set false behind a real proxy it buckets the entire
	// internet under one address, which throttles every honest person the
	// moment one attacker arrives.
	//
	// Empty is the default and means the peer address is the client
	// address, which is correct for a node reached directly.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty" json:"trusted_proxies,omitempty" desc:"CIDR blocks whose X-Forwarded-For is honoured. Empty trusts no forwarded header."`

	// Public is the optional dedicated listener for the routes outside
	// parties call. See [APIPublic].
	Public APIPublic `yaml:"public,omitempty" json:"public,omitzero"`

	// Artifacts is the optional listener that serves artifact bytes and
	// nothing else. See [APIArtifacts].
	Artifacts APIArtifacts `yaml:"artifacts,omitempty" json:"artifacts,omitzero"`

	Auth APIAuth `yaml:"auth,omitempty" json:"auth"`
}

// ServesAPI reports whether this node serves the API surface: it binds a port
// AND holds the ingress role.
//
// The one predicate the authentication rules turn on, in one place: a node
// that serves the API needs an external URL, an admin posture and a
// credential, and one that does not needs none of them. A PORT ALONE IS NOT IT
// — a node without ingress binds api.port for its probes (and a seats node's
// tool bridge) and serves no route anybody signs in to, so asking it for a
// posture and a token would be ceremony every worker pays for a surface it
// never opens. The keyring is not among them: every node needs it, serving or not
// ([Bootstrap.validateTopology]).
//
// THE BOOTSTRAP'S RATHER THAN THE API BLOCK'S, because half of the answer is
// the node block's roles, which the API block cannot see.
func (b *Bootstrap) ServesAPI() bool {
	return b.API.Port != 0 && b.Profile("").RunsIngress()
}

// ExternalBase is [API.ExternalURL] without its trailing slash, which is the
// form every URL is built on.
//
// NO RESOLVER ARGUMENT, unlike the Tier B field this replaced. Tier A expands
// its `${VAR}` references before decoding, so this value is an address by the
// time anything can read it — there is no state in which a caller holds the
// literal reference and has to decide what to do with it.
func (a API) ExternalBase() string {
	return strings.TrimRight(strings.TrimSpace(a.ExternalURL), "/")
}

// WebhookBase is where a VENDOR reaches this deployment: the base every
// webhook, app manifest and app redirect URL is built on. A box's per-run
// sandbox endpoints are not: a box is handed CREWLET_MCP_BRIDGE_URL and
// CREWLET_SANDBOX_OTEL_RECEIVER_URL, which the operator points at this same
// address, and a `direct` box on an ingress node dials its listener on
// loopback.
//
// THE PUBLIC LISTENER'S OWN ADDRESS WHEN IT HAS ONE ([APIPublic.ExternalURL]),
// and [API.ExternalBase] otherwise. No fallback between the two: once
// api.public is set those routes are served ONLY on that socket, so the person's
// address answers 404 for every one of them, and an empty answer — which a
// caller reports as "no public address" — is the truthful one where the public
// address is unset. The ONE accessor, so no caller branches on the listener
// itself and two callers cannot answer the question differently.
func (a API) WebhookBase() string {
	if a.Public.Enabled() {
		return a.Public.ExternalBase()
	}
	return a.ExternalBase()
}

// WebhookBaseKey is the Tier A key [API.WebhookBase] is read from, for a
// screen or a refusal that tells an operator which setting to change: the
// public listener's address once it is set, and the person's otherwise.
//
// BESIDE [API.WebhookBase] AND BRANCHING EXACTLY AS IT DOES, for its reason:
// a caller naming the key for itself would be a second reading of which
// listener is in force, and the one that drifts sends an operator to edit a
// setting nothing reads.
func (a API) WebhookBaseKey() string {
	if a.Public.Enabled() {
		return "api.public.external_url"
	}
	return "api.external_url"
}

// Addr is the API listener's bind address, host and port.
func (a *API) Addr() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// APIPublic is a second listener that serves the routes outside parties call
// without an operator credential, and nothing else: the vendor webhooks and the
// vendor app landings under /webhooks/, and the per-run sandbox endpoints
// /otlp/{token} and /mcp/{token}. Once it is set those routes are served ONLY
// here, and api.port answers 404 for them, so the address an integration or a
// remote sandbox must reach is a different socket from the one that serves the
// admin API, the dashboard and the probes.
//
// # Why a listener rather than a path filter in a proxy
//
// Without it, a deployment that must accept vendor webhooks from the internet
// but keep /config, /secrets and the read surface private has one socket to
// expose and must filter it by path in front. That filter is an allow-list
// somebody else maintains: one route it forgets, or one prefix it matches too
// widely, puts the admin API on the internet, and nothing here can see it.
// Two sockets make the split a fact about the network — the public one is
// published, the other is not — and the engine itself decides which routes
// each answers, from one predicate in the API's auth package.
//
// # Why the sandbox endpoints are public and the probes are not
//
// The sandbox endpoints are reached from INSIDE a box, which on E2B's cloud
// is somebody else's network: a box can reach only what the deployment
// publishes. They hold no operator credential by design — the signed per-run
// token in the path is theirs — so they are exactly the routes this listener
// is for, and leaving them on api.port would force that port public for any
// company running remote sandboxes.
//
// The probes stay on api.port. /health describes the node — its seats, its
// version, its posture — for whoever operates it, and an orchestrator or a
// load balancer can probe a port other than the one it routes traffic to.
//
// # Never the sign-in routes
//
// /auth is a person's surface and is judged against [API.ExternalURL]'s origin
// and cookie scope, so it is never among the routes this listener serves.
//
// # On a node without the ingress role
//
// Such a node serves its probes on api.port and nothing else, and binds no
// listener here whatever this block says, so one file still works for every
// role. Its seats' tool bridge is not a route on it: a box dials the ingress
// nodes, which relay each call to the node holding the seat over the broker
// (internal/api/mcpbridge).
type APIPublic struct {
	// Host is the bind address. Empty binds every interface, which is the
	// point of a listener meant to be reached from outside.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the public listener. Empty binds every interface."`

	// Port is the bind port. 0 (the default) opens no public listener, and
	// every route stays on api.port.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port for the dedicated public listener serving only /webhooks/, /otlp/ and /mcp/; 0 keeps every route on api.port."`

	// ExternalURL is where a VENDOR and a sandbox box reach this listener:
	// the base of every webhook, app manifest and app redirect once the
	// listener is set ([API.WebhookBase]), and the address a remote box's
	// CREWLET_MCP_BRIDGE_URL and CREWLET_SANDBOX_OTEL_RECEIVER_URL name,
	// which are what its per-run endpoints are built on.
	// REQUIRED once the listener is set on an ingress node, because that
	// node is the one building those addresses and handing them to vendors.
	//
	// TIER A, beside [API.ExternalURL] and for its reason: an address a
	// manifest and a webhook registration are built from is a fact about
	// where THIS deployment's sockets are published, which is the
	// operator's, and it is an address by the time anything reads it.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" desc:"Where vendors and remote sandboxes reach this listener, e.g. https://hooks.example.com. Required once port is set on an ingress node."`
}

// IsZero lets an unset public block drop out of a JSON round trip.
func (p APIPublic) IsZero() bool { return p.Host == "" && p.Port == 0 && p.ExternalURL == "" }

// Enabled reports whether the public routes have a listener of their own.
func (p APIPublic) Enabled() bool { return p.Port != 0 }

// Addr is the listener's bind address, host and port.
func (p APIPublic) Addr() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.Port)) }

// ExternalBase is [APIPublic.ExternalURL] without its trailing slash.
func (p APIPublic) ExternalBase() string {
	return strings.TrimRight(strings.TrimSpace(p.ExternalURL), "/")
}

func (p *APIPublic) validate(path Path, ingress bool) error {
	var probs problems
	if p.Port < 0 || p.Port > 65535 {
		probs.add(at(path, "port"), ErrOutOfRange,
			"must be 0 (no public listener) or a port 1..65535, got %d", p.Port)
	}
	if p.Port == 0 && (p.Host != "" || strings.TrimSpace(p.ExternalURL) != "") {
		probs.add(at(path, "port"), ErrMissing,
			"is required when host or external_url is set. Without a port, "+
				"the public listener doesn't open")
	}
	raw := strings.TrimSpace(p.ExternalURL)
	if raw == "" {
		if p.Enabled() && ingress {
			probs.add(at(path, "external_url"), ErrMissing,
				"required when the public listener is set. Set it to the "+
					"address vendors reach, e.g. https://hooks.example.com")
		}
		return probs.err()
	}
	probs.wrap(checkExternalURL(at(path, "external_url"), raw,
		"webhook and sandbox URLs are built on it, and vendors refuse one "+
			"without a scheme",
		"vendors and sandboxes use to reach this listener"))
	return probs.err()
}

// externalLoopback reports whether the external URL names a loopback host.
//
// IT IS THE EXTERNAL URL AND NOT THE BIND ADDRESS that decides the insecure
// postures below, and the difference is the whole point: a hardened production
// node binds 127.0.0.1 behind its proxy, so a judgement made on [API.Host]
// would permit `http` and an optional second factor in exactly the deployment
// that must refuse them. What matters is where a BROWSER reaches this.
func (a API) externalLoopback() bool {
	parsed, err := url.Parse(a.ExternalBase())
	if err != nil {
		return false
	}
	// A host name is compared as a browser compares it, case aside:
	// `LOCALHOST` is the browser's localhost, and read as written it was
	// held to the rules for a routable address it never is.
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validate checks the API block. serving is [Bootstrap.ServesAPI] and ingress
// whether the node holds the ingress role: both are the node block's to say,
// which this block cannot see.
func (a *API) validate(path Path, serving, ingress bool) error {
	var p problems
	// Unbounded, a port of 70000 passes validation and fails at bind,
	// long after `crewlet validate` said the config was good.
	if a.Port < 0 || a.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange,
			"must be 0 (no HTTP surface) or a port 1..65535, got %d", a.Port)
	}
	p.wrap(a.validateExternalURL(path, serving))
	for i, block := range a.TrustedProxies {
		p.wrap(checkTrustedProxy(idx(at(path, "trusted_proxies"), i), block))
	}
	p.wrap(a.Public.validate(at(path, "public"), ingress))
	p.wrap(a.Artifacts.validate(at(path, "artifacts"), a, ingress))
	p.wrap(a.Auth.validate(at(path, "auth"), *a, serving))
	return p.err()
}

// validateExternalURL is the required-once-served rule and the shape rules
// under it.
func (a *API) validateExternalURL(path Path, serving bool) error {
	var p problems
	raw := strings.TrimSpace(a.ExternalURL)
	if raw == "" {
		if serving {
			p.add(at(path, "external_url"), ErrMissing,
				"required when this node serves the API (`api.port` is set and "+
					"`node.roles` has ingress). Set it to the address people use "+
					"for the dashboard, e.g. https://crewlet.example.com or "+
					"http://localhost:%d",
				DefaultAPIPort)
		}
		return p.err()
	}
	p.wrap(checkExternalURL(at(path, "external_url"), raw,
		"the scheme sets cookie security and every link, so it can't be guessed",
		"people use to reach this deployment in a browser"))
	if parsed, err := url.Parse(raw); err == nil && hasHTTPScheme(raw) {
		p.wrap(checkBrowserHost(at(path, "external_url"), raw, parsed.Hostname()))
	}
	return p.err()
}

// checkExternalURL is the shape every external address is held to: an http or
// https scheme, a host, and no query or fragment. why says what the scheme
// decides for this field and reachedBy who reaches the address, so each
// refusal speaks about its own field.
func checkExternalURL(path Path, raw, why, reachedBy string) error {
	var p problems
	if !hasHTTPScheme(raw) {
		p.add(path, ErrShape, "%q must start with http:// or https://: %s", raw, why)
		return p.err()
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		p.add(path, ErrShape, "%q is not a URL: %v", raw, err)
		return p.err()
	}
	if parsed.Hostname() == "" {
		p.add(path, ErrShape, "%q has no host. It must be the address %s", raw, reachedBy)
	}
	p.wrap(checkPort(path, raw, parsed.Port()))
	// A QUERY OR A FRAGMENT IS REFUSED, AND A PATH IS NOT. Every consumer
	// concatenates onto this value, so `https://x/crewlet/` yields
	// `https://x/crewlet/webhooks/github` — which may be exactly what a
	// path-routing proxy needs — while a query or a fragment yields an
	// address no proxy can route and no vendor will accept. Refusing or
	// trimming the path would break the deployment it is correct for, so
	// only the two that can never be right are refused, and the trailing
	// slash is the one thing normalised.
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		p.add(path, ErrShape,
			"%q has a query or fragment. Use only the scheme, host, port and "+
				"path", raw)
	}
	return p.err()
}

// checkTrustedProxy refuses an entry that cannot mean what it says.
//
// # Why a bare address is refused rather than widened to a /32
//
// Because the two readings of `10.0.0.7` are a host and a typo for a block,
// and only one of them is safe to guess. Guessing the host is harmless when
// the writer meant the host and silently trusts one machine when they meant a
// subnet; guessing the block is a catastrophe in the other direction. So
// neither is guessed: the message names the /32 spelling and the operator
// writes what they meant.
func checkTrustedProxy(path Path, block string) error {
	var p problems
	entry := strings.TrimSpace(block)
	switch {
	case entry == "":
		p.add(path, ErrMissing,
			"an empty entry matches nothing: remove it, or name a block as "+
				"10.0.0.0/8")
	case !strings.Contains(entry, "/"):
		if ip := net.ParseIP(entry); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			p.add(path, ErrShape,
				"%q is an address, not a block. To trust just that host, "+
					"write %s/%d", entry, entry, bits)
			break
		}
		p.add(path, ErrShape,
			"%q is not a CIDR block: write it as an address and a prefix "+
				"length, e.g. 10.0.0.0/8", entry)
	default:
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			p.add(path, ErrShape,
				"%q is not a CIDR block: %v. Write it as an address and a "+
					"prefix length, e.g. 10.0.0.0/8", entry, err)
			break
		}
		// EVERYTHING IS NOT A PROXY. A zero-length prefix says every
		// peer's forwarded header is believed, which is the same as
		// having no client address at all: anyone may state their own,
		// so the throttle bucket, the audit row and every per-source
		// rule are chosen by the caller. It is refused rather than
		// warned about because there is no deployment it is right for
		// — a proxy in front of everything is still a finite set of
		// addresses.
		if prefix.Bits() == 0 {
			p.add(path, ErrShape,
				"%q trusts every caller, so anyone could fake their address. "+
					"List your proxies' address block instead, e.g. 10.0.0.0/8",
				entry)
		}
		if prefix.Addr() != prefix.Masked().Addr() {
			p.add(path, ErrShape,
				"%q has host bits set below its prefix length, so it is not "+
					"the block it reads as. Write %s", entry, prefix.Masked())
		}
	}
	return p.err()
}

// APIAuth is how anyone — a person, a machine, this deployment's own operator
// — proves who they are to the HTTP surface.
//
// # What is not here any more, and why neither could be repaired
//
// `allow_anonymous_read` served every GET without a credential and defaulted
// TRUE. Two things were wrong with it and only deleting it fixes either. The
// exposure: the read surface carries LLM transcripts, diary entries, the whole
// event stream and the roster, so the default posture published the company's
// working memory to anyone who could reach the port. The shape: it was a bool
// whose safe value was its zero, declared `omitempty`, so `false` did not
// survive an export round trip — a deployment that had closed it re-opened
// itself the first time its config went through `PUT /config`. A deliberately
// public reader is now a named token holding the apps it reads and nothing else,
// which is listable, revocable without a restart, and present in the audit log.
//
// `disabled` authenticated the EMPTY credential into full operator authority,
// with no check on the bind address anywhere. `crewlet run -dev-principal`
// replaces it: a flag rather than a field, because a field gets copied into an
// image, and refused unless `api.host` BINDS loopback and the binary is a
// development build. The bind rather than `api.external_url`, deliberately and
// unlike [APIAuth.AcceptInsecure]'s rule: that one is about whether a cookie
// crosses plaintext through a proxy, where the external address is the truth,
// and this is about who can open a socket to this process, where the bind
// is.
//
// `oidc` signed people in through an identity provider, and sign-in through a
// third party is not something this engine does any more: a person proves who
// they are with a password and a second factor held here.
//
// # Password sign-in is always on, so there is no backend to choose
//
// There was a `backend:` (`local` or `none`) and a `local:` block beneath it.
// With third-party sign-in gone, `local` was the only backend that did
// anything and `none` only switched password sign-in off — which a deployment
// with nobody invited already is, since nobody holds a password to present.
// So the selector went, and what the block held — whether a second factor is
// required, the acknowledgement of an insecure posture, the password floor —
// is stated here directly.
//
// `bootstrap` opened a second way in for the first person — a one-time code a
// node wrote beside its store — beside the one every serving node already
// requires. The Tier A token below IS the credential a company has before it
// has anybody, so the first person is invited under it exactly as everybody
// after them is, and the founder route, its race between two codes and its
// setting went.
//
// All four are refused if they appear, as every key this build does not define
// is: the loader decodes Tier A with unknown keys refused, so a file still
// carrying one fails rather than running a posture it no longer describes.
type APIAuth struct {
	// Admin is whether THIS NODE grants admin to those who hold it
	// (`serve`), or clamps admin away and keeps every app (`refuse`).
	// REQUIRED once the API is served, and its zero is refused rather than
	// read as either.
	//
	// # Why a node decides this at all, when every permission is declared
	//
	// Because the declarations are made in a tier this one does not trust.
	// A person's permissions live in the replicated store and are written by
	// whoever holds admin; admin is host access, and the operator who owns
	// the machines may want some of them — a node published to the people
	// who use the company, say — never to serve it whatever a row says.
	// Tier A states that and never reads Tier B, which is the same direction
	// every other Tier A posture runs in.
	//
	// ADMIN AND NOTHING ELSE, because an app is the company's ordinary work:
	// a node that refused the tracker would refuse its own colleagues their
	// board, and a ceiling over a list of permissions was a set an operator
	// had to read to answer the one question they ever asked of it. The
	// clamp keeps every app — [iam.AdminPosture.Effective] expands before it
	// clamps — so an administrator signed in on a refusing node keeps the
	// tracker and the knowledge base.
	//
	// It is applied at DECISION time, per node, per request: nothing is
	// written when it changes, so changing it takes effect on the next
	// request that node serves after a restart and needs no config apply
	// and no migration. A fleet whose nodes disagree is therefore a LEGAL
	// state — and one nobody could see, which is why each node publishes
	// its own posture on its presence lease.
	//
	// REQUIRED RATHER THAN DEFAULTED, because either default would be a
	// decision nobody made: `serve` publishes host access on a node an
	// operator meant for colleagues, and `refuse` locks every administrator
	// out of a node they meant to sign in on.
	Admin iam.AdminPosture `yaml:"admin,omitempty" json:"admin,omitempty" js:"enum=serve|refuse" desc:"Whether this node grants admin to those who hold it (serve), or clamps admin away and keeps every app (refuse). Required once the API is served."`

	// Session bounds how long a proof of identity lasts.
	Session APISession `yaml:"session,omitempty" json:"session,omitzero"`

	// Audit is how long the identity estate keeps what happened.
	Audit APIAudit `yaml:"audit,omitempty" json:"audit,omitzero"`

	// TOTP is whether a second factor is REQUIRED of a person who signs in
	// with a password, or merely offered.
	//
	// UNSET IS REQUIRED, the secure value, so a deployment that says
	// nothing gets a second factor and `optional` is the explicit — and,
	// off loopback, acknowledged and logged — weakening. It used to have no
	// default and refuse its zero, which made sense only while the block it
	// sat in was itself optional and its absence meant nobody signed in at
	// all; with password sign-in always on, a refusal of the zero would make
	// every deployment write the one line that should be the default. See
	// [APIAuth.SecondFactor], which reads the zero as required.
	TOTP iam.SecondFactor `yaml:"totp,omitempty" json:"totp,omitempty" js:"enum=required|optional" desc:"Whether a second factor is required of a person signing in with a password (default required). optional off loopback needs accept_insecure."`

	// AcceptInsecure is the deliberate acknowledgement that this
	// deployment is reachable off loopback with a posture that should not
	// be. It is what lets `totp: optional` stand on an address a browser
	// reaches over the network, which [APIAuth.validatePasswords] refuses
	// without it, and it is logged at WARN on every start for the whole
	// life of the deployment.
	//
	// NOT what lets an `http://` external URL stand: plain http off
	// loopback is a warning with or without it ([API.warnings]), never a
	// refusal, so there is nothing for an acknowledgement to lift there.
	//
	// A FIELD RATHER THAN A FLAG, unlike `-dev-principal`, and the
	// difference is what each one opens: the flag authenticates every
	// request as somebody, which must never survive being copied into an
	// image; this weakens a requirement while every credential still has
	// to be presented, which a staging deployment may genuinely want to
	// carry in its own config.
	AcceptInsecure bool `yaml:"accept_insecure,omitempty" json:"accept_insecure,omitempty" desc:"Acknowledge an insecure posture on a routable address. Logged at WARN on every start."`

	// MinPasswordLength raises the floor above [iam.MinPasswordChars].
	// Zero takes that floor; anything below it is refused rather than
	// clamped, because a file asking for eight is a file whose writer
	// believes eight is enough.
	MinPasswordLength int `yaml:"min_password_length,omitempty" json:"min_password_length,omitempty" js:"min=0;max=256" desc:"Minimum password length. 0 takes the engine's floor of 12."`

	// Tokens are this deployment's own machine credentials: break-glass,
	// the operator CLI, a CI pipeline. At least one is REQUIRED once the
	// API is served — see [APIAuth.validate].
	Tokens []APIToken `yaml:"tokens,omitempty" json:"tokens,omitempty" desc:"The deployment's machine credentials. At least one is required once the API is served."`

	// AllowedOrigins are the browser origins CORS permits. Empty means
	// SAME-ORIGIN ONLY: the dashboard is served by this process, so it
	// needs no entry. The previous default was "*", which let any site a
	// logged-in operator visited read every unauthenticated endpoint.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty" desc:"Additional browser origins this deployment is reached on. Empty = same-origin only."`

	// CompanyWriters, when set, are the ids of the Tier A tokens that alone
	// may CHANGE the company document, and no other credential may. Empty
	// (the default) leaves every write to the authority table — an
	// administrator, or a lead inside their own subtree — which is the
	// posture of a company edited by people.
	//
	// It exists for a document something ELSE is the source of: a GitOps
	// pipeline, or a Kubernetes operator rendering it from custom
	// resources. Such a system overwrites the active revision with its own
	// at every reconcile, so an edit a person made here survives only until
	// then — and nothing told them. Listing the system's token turns that
	// silent loss into a refusal at the moment of the edit, naming who
	// manages the document. Reads are untouched, and so is everything that
	// is not the document: a secret write, a reload of the revision already
	// active.
	//
	// A TOKEN ID LIST, NOT A ROLE OR A FLAG: a Tier A token acts under the
	// login `token:<id>`, which every revision it writes records as both its
	// author and the credential it came through, so "who may write" and
	// "who wrote" are the same vocabulary. Each entry is held to the token
	// id grammar ([iam.ValidTokenID]). [APIAuth.MayWriteCompany] is the one
	// reading of it; see ADR-0030 for what counts as changing the document.
	CompanyWriters []string `yaml:"company_writers,omitempty" json:"company_writers,omitempty" desc:"Ids of the Tier A tokens that alone may change the company document. Empty = whoever the authority table admits."`
}

// CompanyManaged reports whether the company document is managed: some Tier A
// token ids are named as its only writers.
func (a *APIAuth) CompanyManaged() bool { return len(a.CompanyWriters) > 0 }

// MayWriteCompany reports whether a write made as by may change the company
// document.
//
// THE ONE READING of [APIAuth.CompanyWriters], for every surface that asks:
// the config write path that enforces it and the viewer answer that tells the
// dashboard whether to offer an edit.
//
// THE CREDENTIAL, NEVER THE AUTHOR. by's [iam.Actor.OperatorID] is the
// credential the write came through, and a Tier A bearer's is its own login,
// `token:<id>`: a listed id is admitted by that and by nothing else. A person's
// session (`session:<lineage>`), a machine token acting as its owner
// (`pat:<id>`) and a session exchanged from a Tier A token each name another
// credential, so every one of them — a lead writing their own subtree
// included — is refused while the document is managed, which is the managed
// posture rather than an exception to it: a person's edit is exactly the one
// the managing system's next reconcile overwrites.
//
// THE ENGINE ITSELF IS ADMITTED ([iam.ActorSystem]): its own writes — a chart
// apply, a republish of the revision already active — are not an edit
// somebody made here for the managing system to overwrite.
func (a *APIAuth) MayWriteCompany(by iam.Actor) bool {
	if !a.CompanyManaged() || by.Kind == iam.ActorSystem {
		return true
	}
	for _, id := range a.CompanyWriters {
		if by.OperatorID == iam.TokenLogin(id) {
			return true
		}
	}
	return false
}

// SecondFactor is whether a second factor is required here, with the default
// applied: `optional` where the file says so, and `required` everywhere else.
//
// AN ALLOWLIST OF ONE, for [iam.Stage.MayAct]'s reason turned round: a value
// this build does not know reads as REQUIRED, because the safe direction for
// "must you prove more" is yes. It is the ONE reading of the setting — the
// sign-in that enforces it and the posture read that reports it both ask it.
func (a APIAuth) SecondFactor() iam.SecondFactor {
	if a.TOTP == iam.SecondFactorOptional {
		return iam.SecondFactorOptional
	}
	return iam.SecondFactorRequired
}

// Passwords is the effective minimum password length.
func (a APIAuth) Passwords() int {
	if a.MinPasswordLength == 0 {
		return iam.MinPasswordChars
	}
	return a.MinPasswordLength
}

// APISession bounds how long a proof of identity lasts.
//
// EVERY FIELD HERE IS AN UPPER BOUND, never a schedule. The IDLE deadline —
// twelve hours from the last use — is deliberately not here: it is a constant
// in internal/iam/session, because it is a property of how long a browser tab
// may sit unattended rather than of this deployment's policy, and a company
// that wants a shorter one wants a shorter `absolute`.
type APISession struct {
	// AbsoluteRaw is the longest a session may live from sign-in,
	// whatever it is used for. Default 168h.
	//
	// SEVEN DAYS is the working week, and it is the same horizon the
	// invitation expiry and the sweep's grace already use — one number
	// for "how long is this company willing to remember something".
	//
	// THE FLOOR IS ONE HOUR because the default step-up window is one
	// hour: below that a person re-proves their identity and is signed
	// out inside the same window, which makes the step-up unreachable.
	// THE CEILING IS THIRTY DAYS because the deadline is inside the
	// SIGNED bearer and therefore cannot be shortened after issue —
	// lowering this setting does not touch a session already minted, and
	// only a revocation epoch bump ends one early. A month is how long a
	// deployment can stand not being able to shorten.
	AbsoluteRaw string `yaml:"absolute,omitempty" json:"absolute,omitempty" desc:"Longest a session may live from sign-in (default 168h, 1h..720h)."`

	// StepUpRaw is how long after proving identity a step-up gesture is
	// allowed without proving it again. Default 1h.
	//
	// ONE WINDOW OVER EVERY SENSITIVE GESTURE — revealing a secret,
	// changing somebody's authority or how they prove who they are, ending
	// every session in the company, writing the configuration or the
	// credential store — as GitHub's sudo mode applies one window
	// to all of its sensitive actions. An hour keeps the stricter end of
	// that practice. A second, shorter window for some of the gestures was
	// tried and went: it sent an administrator who had proved half an hour
	// earlier to prove again between one screen and the next, while what
	// actually keeps those gestures from a stolen credential is not a
	// proof's age — internal/authz refuses them to any request a machine
	// token presented, and internal/iamdomain refuses an administrative
	// record written through one.
	//
	// THE FLOOR IS FIVE MINUTES because below it an administrator
	// re-proves between one screen and the next, and THE CEILING A DAY
	// because past it a step-up is not one.
	StepUpRaw string `yaml:"step_up,omitempty" json:"step_up,omitempty" desc:"How long a proof of identity authorises a step-up gesture (default 1h, 5m..24h)."`
}

// Session defaults, and the bounds around them.
const (
	DefaultSessionAbsolute = 168 * time.Hour
	SessionAbsoluteFloor   = time.Hour
	SessionAbsoluteCeiling = 720 * time.Hour

	DefaultSessionStepUp = time.Hour
	SessionStepUpFloor   = 5 * time.Minute
	SessionStepUpCeiling = 24 * time.Hour
)

// Absolute is the longest a session may live, with the default applied.
func (s APISession) Absolute() time.Duration {
	return durationOr(s.AbsoluteRaw, DefaultSessionAbsolute)
}

// StepUp is the step-up window, with the default applied.
func (s APISession) StepUp() time.Duration {
	return durationOr(s.StepUpRaw, DefaultSessionStepUp)
}

// IsZero lets an unset block drop out of a JSON round trip.
func (s APISession) IsZero() bool {
	return s.AbsoluteRaw == "" && s.StepUpRaw == ""
}

func (s APISession) validate(path Path) error {
	var p problems
	check := func(name, raw string, lo, hi time.Duration, why string) {
		checkDurationRange(&p, at(path, name), raw, lo, hi, why)
	}
	check("absolute", s.AbsoluteRaw, SessionAbsoluteFloor, SessionAbsoluteCeiling,
		"under an hour ends sessions too soon, and over a month keeps old "+
			"sessions alive after you change it")
	check("step_up", s.StepUpRaw, SessionStepUpFloor, SessionStepUpCeiling,
		"under five minutes asks people to confirm it's them too often, and "+
			"over a day doesn't protect anything")
	return p.err()
}

// APIAudit is how long the identity estate keeps what happened.
//
// TWO HORIZONS AND NOT ONE, because the two answer different questions and a
// single number would have to satisfy the longer. A change row says who was
// granted what, by whom — the record a compliance review reads years later. A
// session row says who signed in from where, which is what an incident reads
// weeks later and what accumulates at a completely different rate: a company
// of fifty people writes a handful of change rows a month and thousands of
// session rows.
type APIAudit struct {
	// ChangesRaw is how long `iam_history` keeps a grant, status,
	// credential, link, generation or removal row. Default 9600h (400
	// days), which is a year plus the month it takes to notice one ended.
	ChangesRaw string `yaml:"changes,omitempty" json:"changes,omitempty" desc:"How long a grant, status or credential change is kept (default 9600h = 400d, 2160h..24000h)."`

	// SessionsRaw is how long a session's start, step-up and end are
	// kept. Default 2160h (90 days), which is the window an intrusion is
	// reconstructed over.
	SessionsRaw string `yaml:"sessions,omitempty" json:"sessions,omitempty" desc:"How long a sign-in, step-up and sign-out is kept (default 2160h = 90d, 168h..9600h)."`
}

// Audit defaults, and the bounds around them. Written in hours because
// time.ParseDuration has no day unit, and the comment beside each says what
// the number is in days.
const (
	DefaultAuditChanges = 9600 * time.Hour // 400d
	AuditChangesFloor   = 2160 * time.Hour // 90d
	AuditChangesCeiling = 24000 * time.Hour

	DefaultAuditSessions = 2160 * time.Hour // 90d
	AuditSessionsFloor   = 168 * time.Hour  // 7d
	AuditSessionsCeiling = 9600 * time.Hour // 400d
)

// Changes is the change horizon, with the default applied.
func (a APIAudit) Changes() time.Duration {
	return durationOr(a.ChangesRaw, DefaultAuditChanges)
}

// Sessions is the authentication-trail horizon, with the default applied.
func (a APIAudit) Sessions() time.Duration {
	return durationOr(a.SessionsRaw, DefaultAuditSessions)
}

// IsZero lets an unset block drop out of a JSON round trip.
func (a APIAudit) IsZero() bool { return a.ChangesRaw == "" && a.SessionsRaw == "" }

func (a APIAudit) validate(path Path) error {
	var p problems
	checkDurationRange(&p, at(path, "changes"), a.ChangesRaw,
		AuditChangesFloor, AuditChangesCeiling,
		"keep permission changes at least 90 days so a quarterly review can "+
			"see them")
	checkDurationRange(&p, at(path, "sessions"), a.SessionsRaw,
		AuditSessionsFloor, AuditSessionsCeiling,
		"keep sign-ins at least a week so incidents can be investigated")
	// THE ORDER BETWEEN THEM IS THE RULE. A session row from three
	// hundred days ago answers "who signed in"; what it does not answer
	// is what that person was allowed to do, which is a change row. Keep
	// the sessions longer than the changes and an investigation finds a
	// sign-in it cannot interpret.
	if a.Sessions() > a.Changes() {
		p.add(at(path, "sessions"), ErrConflict,
			"%s can't be longer than `changes` (%s). Raise `changes` or lower "+
				"this", a.Sessions(), a.Changes())
	}
	return p.err()
}

// MaxPasswordLength is the ceiling under [APIAuth.MinPasswordLength].
//
// A MINIMUM THAT NOBODY CAN SATISFY IS A DEPLOYMENT NOBODY CAN ENROL IN, and
// 256 is already far past any memorable passphrase — so a larger number here
// is a unit mistake rather than a policy.
const MaxPasswordLength = 256

// validatePasswords is what a person signing in with a password is asked: the
// second factor and the password floor.
func (a *APIAuth) validatePasswords(path Path, api API, serving bool) error {
	var p problems
	if a.TOTP != "" && !a.TOTP.Valid() {
		p.add(at(path, "totp"), ErrUnknownValue,
			"%q (want %s or %s; unset is %s)", a.TOTP, iam.SecondFactorRequired,
			iam.SecondFactorOptional, iam.SecondFactorRequired)
	}
	// AN OPTIONAL SECOND FACTOR OFF LOOPBACK IS A FAULT unless somebody
	// said so out loud. The judgement is on where a BROWSER reaches this
	// deployment, not on the bind address: a hardened node binds loopback
	// behind its proxy, so a bind check would permit the insecure posture
	// in exactly the deployment that must refuse it.
	if a.TOTP == iam.SecondFactorOptional && serving &&
		!api.externalLoopback() && !a.AcceptInsecure {
		p.add(at(path, "totp"), ErrConflict,
			"`optional` allows password-only sign-in at %s. Remove the line "+
				"to require two-step verification, or set `accept_insecure: true`",
			api.ExternalBase())
	}
	if n := a.MinPasswordLength; n != 0 {
		switch {
		case n < iam.MinPasswordChars:
			p.add(at(path, "min_password_length"), ErrOutOfRange,
				"%d is below the minimum of %d. Remove the line to use the "+
					"minimum", n, iam.MinPasswordChars)
		case n > MaxPasswordLength:
			p.add(at(path, "min_password_length"), ErrOutOfRange,
				"%d is over the maximum of %d. Nobody could set a password that "+
					"long", n, MaxPasswordLength)
		}
	}
	return p.err()
}

// APIToken is one of the DEPLOYMENT's own machine credentials.
//
// Not a person and never a stand-in for one: a Tier A token is what
// break-glass, the operator CLI and a CI pipeline present. Writes made with it
// land as `token:<id>` with the operator kind, which is what keeps them
// distinguishable in the audit trail from anything a person did.
type APIToken struct {
	// ID names the token — "founder", "ops", "ci-pipeline" — and the token
	// acts under the login `token:<id>`, which is what every trail a write
	// made with it lands in records as the author: a work item's and a
	// page's actor, a revision's `created_by`, a stored secret's
	// `updated_by`.
	ID string `yaml:"id" json:"id" js:"required;pattern=^[a-z0-9]+(-[a-z0-9]+)*(:[a-z0-9]+(-[a-z0-9]+)*)*$;maxlen=58" desc:"Short label; the token acts under the login token:<id>, which is recorded as the author of its writes. Lowercase letters, digits and hyphens, optionally joined by colons, at most 58 characters so the login stays within 64."`

	// Token is the value, or a ${VAR} reference to it. Resolved once at
	// startup and never stored.
	Token string `secret:"true" yaml:"token" json:"token" js:"required" desc:"Token value or ${VAR} reference."`

	// Grants is what this credential may do: `admin`, `worklet`, `doclet`,
	// `artifacts`. REQUIRED and non-empty: a credential's blast radius is
	// stated at the pin, never inferred. The founder's break-glass token is
	// `[admin]`; a CI pipeline that files work is `[worklet]`.
	//
	// THIS IS THE CHANGE THAT MAKES A TOKEN SAFE TO HAVE. Before it, one
	// token was the whole operator surface — the config document, the
	// secret store, the fleet, every transcript — so the CI pipeline that
	// needed to file a work item held the credential that could read
	// every key the company owns.
	//
	// STRICT, unlike a replicated row: a name this build does not know is
	// refused here, naming the set, because a file an operator wrote is a
	// typo rather than a newer peer's record.
	Grants []iam.Grant `yaml:"grants,omitempty" json:"grants,omitempty" desc:"What this credential may do: admin, worklet, doclet, artifacts. Required and non-empty."`
}

// validate checks the auth block; serving is [Bootstrap.ServesAPI].
func (a *APIAuth) validate(path Path, api API, serving bool) error {
	var p problems

	p.wrap(a.validateAdmin(path, serving))
	p.wrap(a.Session.validate(at(path, "session")))
	p.wrap(a.Audit.validate(at(path, "audit")))
	p.wrap(a.validatePasswords(path, api, serving))
	p.wrap(a.validateTokens(path, serving))
	p.wrap(a.validateWriters(path))

	for i, origin := range a.AllowedOrigins {
		p.wrap(checkOrigin(idx(at(path, "allowed_origins"), i), origin))
	}
	return p.err()
}

// validateWriters holds each company_writers entry to the token id grammar.
//
// A WRITER IS A TOKEN ID, and held to the rules a token id is: an entry no id
// could ever equal is not a stricter list, it is a typo that locks out the
// system it was meant to name. Whether it names a token THIS file configures is
// a warning instead ([APIAuth.writerWarnings]): a list naming none is a frozen
// document, which is a posture, and a token may be listed ahead of its issue.
func (a *APIAuth) validateWriters(path Path) error {
	var p problems
	writers := make(map[string]struct{}, len(a.CompanyWriters))
	for i, id := range a.CompanyWriters {
		wp := idx(at(path, "company_writers"), i)
		switch {
		case strings.TrimSpace(id) == "":
			p.add(wp, ErrMissing, "this entry is empty. Remove it, or put the "+
				"id of the API token the managing system uses")
		case !iam.ValidTokenID(id):
			p.add(wp, ErrUnknownValue,
				"%q isn't a valid token id. Use lowercase letters, digits and "+
					"hyphens, optionally joined by colons, up to %d characters", id,
				iam.MaxLogin-len(iam.TokenLoginPrefix))
		}
		if _, dup := writers[id]; dup && id != "" {
			p.add(wp, ErrConflict, "writer %q is listed twice", id)
		}
		writers[id] = struct{}{}
	}
	return p.err()
}

// validateAdmin is the required-once-served rule on `api.auth.admin`.
func (a *APIAuth) validateAdmin(path Path, serving bool) error {
	var p problems
	switch {
	case a.Admin == "":
		if serving {
			p.add(at(path, "admin"), ErrMissing,
				"required when this node serves the API. Set `serve` to allow "+
					"admin on this node, or `refuse` to block it (apps still work)")
		}
	case !a.Admin.Valid():
		p.add(at(path, "admin"), ErrUnknownValue,
			"%q isn't a valid value. Use one of %v", a.Admin, iam.AdminPostures)
	}
	return p.err()
}

// validateTokens is the token list: the label, the value, the entropy floor,
// the grants and the required-once-served rule.
func (a *APIAuth) validateTokens(path Path, serving bool) error {
	var p problems
	seen := make(map[string]struct{}, len(a.Tokens))
	for i, t := range a.Tokens {
		tp := idx(at(path, "tokens"), i)
		if t.ID == "" {
			p.add(at(tp, "id"), ErrMissing,
				"every token needs an id. Changes made with the token are "+
					"recorded as `token:<id>`")
		}
		if _, dup := seen[t.ID]; dup && t.ID != "" {
			// Two tokens sharing a label make the audit trail
			// unreadable: every write says `token:founder` and no one
			// can tell which credential made it, which is the whole
			// reason the label exists.
			p.add(at(tp, "id"), ErrConflict, "duplicate token id %q", t.ID)
		}
		// A TOKEN IS A MACHINE, so the login it acts under is held to the
		// machine grammar — see [iam.ValidTokenID]. An id outside it
		// composes an author name outside every grammar the namespaces
		// are kept apart by, and a login no directory row can hold, so
		// the token could never be bound to a seat and the refusal
		// would arrive only when somebody tried.
		if t.ID != "" && !iam.ValidTokenID(t.ID) {
			p.add(at(tp, "id"), ErrUnknownValue,
				"token id %q makes the login %q, which isn't valid. Use "+
					"lowercase letters, digits, hyphens and colons (like "+
					"ci:release), up to %d characters (logins can be %d)",
				t.ID, iam.TokenLogin(t.ID),
				iam.MaxLogin-len(iam.TokenLoginPrefix), iam.MaxLogin)
		}
		seen[t.ID] = struct{}{}

		// THE FLOOR IS CHECKED ON THE RESOLVED VALUE, which is the only
		// value there is: Tier A expands its ${VAR} references before
		// decoding, so a reference to an unset variable arrives here as
		// the empty string and a reference to a four-character one
		// arrives as four characters. That is what stops a reference
		// being the way around this rule — which is exactly how a
		// 16-byte webhook key once got in.
		switch {
		case t.Token == "":
			p.add(at(tp, "token"), ErrMissing,
				"token is empty. If it's a ${VAR}, that variable isn't set in "+
					"this node's environment")
		default:
			if err := secrets.CheckOperatorToken(t.Token); err != nil {
				// THE VALUE NEVER REACHES THE MESSAGE. This
				// refusal is printed by `crewlet validate`,
				// answered over /config and written to a log.
				p.add(at(tp, "token"), ErrUnknownValue, "%s", err.Error())
			}
			// A VALUE SHAPED AS AN ASSISTANT GRANT could never
			// authenticate as itself: the guard reads that shape as
			// a grant before it compares any Tier A token, and
			// refuses every one presented over the wire.
			if strings.HasPrefix(t.Token, iam.AssistBearerPrefix) {
				p.add(at(tp, "token"), ErrUnknownValue,
					"a token can't start with %q, which the engine reserves "+
						"for the Dashboard Assistant's grants. Generate one "+
						"with `crewlet secrets keygen`", iam.AssistBearerPrefix)
			}
		}

		if len(t.Grants) == 0 {
			p.add(at(tp, "grants"), ErrMissing,
				"required. List the permissions this token gets, from %v",
				iam.AllGrants)
		}
		p.wrap(checkGrants(at(tp, "grants"), t.Grants, a.Admin))
	}

	// A DEPLOYMENT WITH NO TIER A TOKEN IS A FAULT, and a fault rather
	// than a warning because each way it bites is a permanent lock-out of
	// your own company:
	//
	//   - a fresh deployment's identity estate is EMPTY: somebody has to
	//     create the company and put its first person on their human seat,
	//     and the token is the only credential that exists to do it with;
	//   - on a running one, an administrator who is throttled, who lost
	//     their second factor, or whose password is refused has nothing
	//     else to present.
	if serving && len(a.Tokens) == 0 {
		p.add(at(path, "tokens"), ErrMissing,
			"at least one token is required when this node serves the API. "+
				"You need it to invite the first person. Generate one with "+
				"`crewlet secrets keygen` and point `token:` at a ${VAR}")
	}
	return p.err()
}

// checkGrants refuses a permission this build does not know, and admin on a
// node that refuses it.
//
// ADMIN ON A REFUSING NODE IS A CONFLICT, NOT A NO-OP. The token would be
// clamped to its apps on every request this node serves, so a file listing it
// says one thing and the node does another — and the break-glass token an
// operator reaches for on the day nobody can sign in would turn out to run
// nothing. Read through [iam.Expand] as every comparison of permissions is,
// though a refusing posture only ever withholds admin itself. A posture that
// is unset or unknown is its own rule's fault, already reported, and clamps
// nothing here.
func checkGrants(path Path, grants []iam.Grant, admin iam.AdminPosture) error {
	var p problems
	for i, g := range grants {
		gp := idx(path, i)
		if !g.Valid() {
			p.add(gp, ErrUnknownValue,
				"%q isn't a permission. Use one of %v", g, iam.AllGrants)
		}
	}
	if admin == iam.AdminRefuse && slices.Contains(iam.Expand(grants), iam.GrantAdmin) {
		for i, g := range grants {
			if g == iam.GrantAdmin {
				p.add(idx(path, i), ErrConflict,
					"`api.auth.admin` is refuse, so this token can't use admin "+
						"on this node. Set `api.auth.admin: serve`, or remove "+
						"admin from the token")
			}
		}
	}
	return p.err()
}

// checkDurationRange is the shared shape-and-bounds check for a `…Raw`
// duration field. An empty value is the default and is not checked.
func checkDurationRange(p *problems, path Path, raw string, lo, hi time.Duration, why string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.add(path, ErrShape,
			"%q isn't a duration. Write it like 30m, 24h or 168h (7 days)", raw)
		return
	}
	if d < lo || d > hi {
		p.add(path, ErrOutOfRange, "%s is outside %s..%s: %s", raw, lo, hi, why)
	}
}

// checkOrigin refuses an entry that cannot name the origin it reads as.
//
// # Why each of these is refused rather than accepted and ignored
//
// An allowance is compared against the browser's `Origin` header, which is
// always `scheme://host[:port]`, both sides put in a browser's spelling
// (auth.Origin). Every shape below is a value an operator plausibly writes
// that either no browser can ever send — accepted, it is an allow-list that
// looks configured, a fetch that fails in a console the engine never sees,
// and nothing anywhere saying why — or, with a path, a query, a fragment or
// a user, one that permits more than it reads as: everything but the scheme,
// host and port is dropped before the comparison, so
// `https://ops.example.com/dashboard` would admit every page of that host.
//
// `*` is the sharpest of them, and it is refused rather than honoured: it was
// this field's own previous default, and what it does is let any site a
// logged-in operator visits read every unauthenticated endpoint — which on
// this API means LLM transcripts, diary entries and the whole event stream.
func checkOrigin(path Path, origin string) error {
	var p problems
	switch {
	case origin == "":
		p.add(path, ErrMissing, "an empty origin matches nothing: remove the "+
			"entry, or name a site as scheme://host[:port]")
	case origin == "*":
		p.add(path, ErrShape, "%q isn't allowed: any website could act as "+
			"the signed-in person. List each site instead, e.g. "+
			"https://ops.example.com", origin)
	case !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://"):
		p.add(path, ErrShape, "%q has no scheme, so it matches nothing. "+
			"Write https://%s", origin, origin)
	case strings.HasSuffix(origin, "/"):
		p.add(path, ErrShape, "%q ends in a slash, and an origin has no path: "+
			"it is only scheme://host[:port]. Write %q", origin, strings.TrimRight(origin, "/"))
	default:
		parsed, err := url.Parse(origin)
		switch {
		case err != nil:
			p.add(path, ErrShape, "%q is not a URL a browser can be at (%v). "+
				"Write scheme://host[:port]", origin, err)
		case parsed.Hostname() == "":
			p.add(path, ErrShape, "%q has no host. Write scheme://host[:port]", origin)
		default:
			if parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
				parsed.Fragment != "" || parsed.User != nil {
				p.add(path, ErrShape, "%q carries more than an origin, which is "+
					"only scheme://host[:port]: the rest would be dropped and the "+
					"entry would admit every page of %s. Write %q",
					origin, parsed.Host, parsed.Scheme+"://"+parsed.Host)
			}
			p.wrap(checkPort(path, origin, parsed.Port()))
			p.wrap(checkBrowserHost(path, origin, parsed.Hostname()))
		}
	}
	return p.err()
}

// checkPort refuses a port no client can reach. The URL parser checks only
// that a port is digits, so `:99999` would pass it and fail at every caller.
func checkPort(path Path, raw, port string) error {
	var p problems
	if port == "" {
		return nil
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		p.add(path, ErrShape, "%q has port %s, and a port is 1 to 65535", raw, port)
	}
	return p.err()
}

// checkBrowserHost refuses an address a browser reaches whose host the
// cross-site check could not compare: the check puts both sides in a
// browser's spelling (auth.Origin), and these three are spellings a browser
// changes in ways that need more than this engine knows. Kept, each refuses
// every write from the address as it is written.
//
//   - A host outside ASCII: a browser sends its ASCII form (`bücher.example`
//     is `xn--bcher-kva.example`), and spelling it so needs the IDNA tables,
//     which nothing else in the engine has a use for.
//   - An IPv4 address written other than as four decimal octets: a browser
//     reads any host whose last label is a number as IPv4, and rewrites
//     `127.1`, `2130706433`, `0x7f.0.0.1` and `192.168.010.1` (whose leading
//     zero is octal) into the dotted form it sends.
//   - An IPv6 zone (`[fe80::1%25eth0]`), which no browser can address.
//
// The ASCII and dotted forms reach the same host from every browser and every
// link, so they are the ones to write. Only the browser's addresses are held
// to this — `api.external_url` and `api.auth.allowed_origins` — and never the
// public listener's, which no origin is compared against.
func checkBrowserHost(path Path, raw, host string) error {
	var p problems
	switch {
	case strings.IndexFunc(host, func(r rune) bool { return r > unicode.MaxASCII }) >= 0:
		p.add(path, ErrShape, "%q has a host outside ASCII. Write the host in "+
			"its ASCII (xn--) form, which is the form a browser sends", raw)
	case strings.Contains(host, "%"):
		p.add(path, ErrShape, "%q names an IPv6 zone, which no browser can "+
			"address. Write the address without it", raw)
	case !strings.Contains(host, ":") && endsInNumber(host):
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is4() {
			p.add(path, ErrShape, "%q has a host a browser reads as an IPv4 "+
				"address and rewrites before it sends it. Write the address as "+
				"four decimal octets, such as 127.0.0.1", raw)
		}
	}
	return p.err()
}

// endsInNumber is the URL standard's test for a host a browser parses as IPv4:
// its last label — a trailing dot aside — is all digits, or 0x and hex digits.
func endsInNumber(host string) bool {
	labels := strings.Split(host, ".")
	if len(labels) > 1 && labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
	}
	last := strings.ToLower(labels[len(labels)-1])
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	return strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == ""
}

// warnings are the API postures that are VALID and worth reading before this
// deployment runs on them.
//
// NONE OF THESE CAN BE A REFUSAL, and each says why in its own sentence. What
// they share is that the configuration works exactly as written and the
// consequence is somewhere else: on the network between a browser and this
// node, or in a front end the engine cannot tell apart from its callers.
//
// Asked only of a node that serves the API ([Bootstrap.ServesAPI]): each is
// about what a browser meets, and a node without ingress serves none.
func (a API) warnings(serving bool) []Warning {
	var out []Warning
	if !serving {
		return out
	}
	auth := a.Auth

	// AN INSECURE POSTURE SOMEBODY ACKNOWLEDGED IS STILL INSECURE. The
	// acknowledgement is what makes it legal, and it is a decision made
	// once that everybody after inherits — so it is said out loud every
	// time: by `crewlet validate`, and by every start of a node serving
	// the API, which logs this advisory as `config_advisory`. A line that
	// fired only the first time would be a decision nobody can see.
	if auth.AcceptInsecure {
		out = append(out, advisory(field("api.auth.accept_insecure"),
			"insecure sign-in settings are allowed at "+a.ExternalBase()+
				" because `accept_insecure` is on. Remove it once you use "+
				"https and two-step verification"))
	}

	// AN `http://` EXTERNAL URL ON A ROUTABLE HOST means the session
	// cookie cannot carry `Secure`, so every credential this deployment
	// issues travels in the clear and no `__Host-` prefix protects it.
	//
	// A WARNING RATHER THAN A REFUSAL because it is what a tunnel, a
	// staging box and an internal network genuinely look like, and because
	// the one posture it would be a refusal for — an optional second
	// factor — IS refused, by the rule beside it.
	if strings.HasPrefix(a.ExternalBase(), "http://") && !a.externalLoopback() {
		out = append(out, advisory(field("api.external_url"),
			"this deployment uses plain http, so passwords travel "+
				"unencrypted. Put TLS in front and set the https address here"))
	}

	// AN `https://` EXTERNAL URL WITH NO TRUSTED PROXY is one client. The
	// engine never terminates TLS itself, so an https address means
	// something in front of it does — and unless that something is named
	// here, every request's source is ITS address: the sign-in throttle,
	// the audit trail's client and every per-source rule then see the whole
	// company as one caller — the throttle keys every caller's attempts at
	// one login together, so a stranger guessing at somebody's login puts
	// that person's own sign-in behind the same wait, and every audit row
	// names the proxy.
	//
	// A WARNING RATHER THAN A REFUSAL because one front end is right with
	// the list empty — a balancer that passes the client's own address
	// through as the peer rather than in a header — and nothing in this
	// file can tell the two apart.
	if strings.HasPrefix(a.ExternalBase(), "https://") && len(a.TrustedProxies) == 0 {
		out = append(out, advisory(field("api.trusted_proxies"),
			"this deployment uses https but trusts no proxy, so every caller "+
				"shows the proxy's address. Add your proxy's address block, "+
				"unless it passes client addresses through"))
	}

	return out
}
