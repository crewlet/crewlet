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
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- api ------------------------------------------------------------- //

// DefaultAPIHost binds every interface, which is what a container needs.
const DefaultAPIHost = "0.0.0.0"

// DefaultAPIPort is the port the examples, quickstart, image and docs all
// name. 8000 rather than 8080: it is what the things a reader RUNS carry, and
// not the port most developer machines already hold.
//
// It is NOT a default applied to [API.Port]: zero there means "serve no HTTP
// at all", a real posture a default would take away. This constant is what
// the flag, the image and every page quote.
const DefaultAPIPort = 8000

// API is the Tier A HTTP surface: where it binds, where a browser reaches it,
// whose forwarded headers it believes, and how anyone proves who they are.
type API struct {
	// Host is the bind address.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the HTTP surface."`

	// Port is the bind port of this node's ONE listener; node.roles decides
	// what it carries. With the ingress role it is the whole HTTP surface,
	// less the routes [API.Public] moves to its own listener. Without it, only
	// the probes (/health, /ready) face the network, so an orchestrator can
	// probe every node. 0 binds nothing, on any node.
	//
	// On a node that runs seats, its direct boxes dial the tool bridge and
	// telemetry receiver here, on loopback alone; a box off that loopback (a
	// container box) dials CREWLET_MCP_BRIDGE_URL instead
	// (internal/api/mcpbridge). A port does not
	// make a node SERVE the API; the port and the ingress role together do
	// ([Bootstrap.ServesAPI]).
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port of this node's HTTP listener: the whole API with the ingress role (less the routes api.public moves to its own listener); without it, /health and /ready, and on a node with the seats role the tool bridge and telemetry receiver its own direct boxes dial, to this host's loopback alone. 0 binds nothing."`

	// ExternalURL is where a BROWSER reaches this deployment and, while no
	// public listener is set, where a vendor and a sandbox box do too.
	// REQUIRED once this node serves the API ([Bootstrap.ServesAPI]). Written
	// scheme://host[:port] with no query or fragment: URLs append a path.
	//
	// It sets the session cookie's `Secure` flag and `__Host-` prefix (the
	// proxy terminates TLS, so r.TLS cannot say), the origin checked on a
	// non-GET and a socket handshake, and person-facing links. Vendor webhook
	// and manifest URLs use it unless [API.Public] is set, then only
	// [APIPublic.ExternalURL] ([API.WebhookBase]), with no fallback: a vendor
	// handed this address would get a 404. It is Tier A because the cookie and
	// origin check run before any company document loads.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" js:"maxlen=2048" desc:"Where a browser reaches this deployment, e.g. https://crewlet.example.com, and where a vendor does too unless api.public names its own. Required once this node serves the API: a port and the ingress role. At most 2048 characters."`

	// TrustedProxies are the peers whose `X-Forwarded-For` and
	// `X-Forwarded-Proto` this deployment believes, as CIDR blocks.
	//
	// A CIDR LIST, NEVER A BOOL: the question is "is THIS peer the proxy". True
	// trusts a header anybody can send, letting an attacker pick their own
	// rate-limit bucket and audit row; false behind a real proxy buckets the
	// internet under one address and throttles every honest person.
	//
	// Empty (the default) means the peer address is the client address, correct
	// for a node reached directly.
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
// The one predicate the authentication rules turn on: a node that serves the
// API needs an external URL, an admin posture and a credential; one that does
// not needs none. A PORT ALONE IS NOT IT: a node without ingress binds
// api.port for its probes (and a seats node's tool bridge) and serves no route
// anybody signs in to. The keyring is not among them: every node needs it
// ([Bootstrap.validateTopology]). It is the Bootstrap's because half of the
// answer is the node block's roles, which the API block cannot see.
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
// sandbox endpoints are not: they come from CREWLET_MCP_BRIDGE_URL and
// CREWLET_SANDBOX_OTEL_RECEIVER_URL.
//
// It is the public listener's address when api.public is set
// ([APIPublic.ExternalURL]), else [API.ExternalBase], with no fallback: those
// routes are served ONLY on the public socket, so the person's address answers
// 404 for them, and an empty answer ("no public address") is the truthful one
// where it is unset. The one accessor, so no two callers answer differently.
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

// APIPublic is a second listener serving only the routes outside parties call
// without an operator credential: vendor webhooks and app landings under
// /webhooks/, and the sandbox routes /otlp/, /mcp/, /box/git/ and /box/gitlet/.
// Once set they are served ONLY here; api.port answers 404 for them.
//
// A listener, not a proxy path filter, whose one forgotten route would put the
// admin API on the internet unseen. A box off its node's loopback (a container
// on the runtime's network, a satellite's box) reaches only what is published,
// and the sandbox routes take a run's token, no person's credential, so the
// split keeps the admin API out of a box's reach. Probes and /auth stay on
// api.port (/auth is judged against [API.ExternalURL]'s origin); a node without
// ingress binds none, relaying its tool bridge over the broker (mcpbridge).
type APIPublic struct {
	// Host is the bind address. Empty binds every interface, which is the
	// point of a listener meant to be reached from outside.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the public listener. Empty binds every interface."`

	// Port is the bind port. 0 (the default) opens no public listener, and
	// every route stays on api.port.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port for the dedicated public listener serving only /webhooks/ and the sandbox routes /otlp/, /mcp/, /box/git/ and /box/gitlet/; 0 keeps every route on api.port."`

	// ExternalURL is where a VENDOR and a sandbox box reach this listener:
	// the base of every webhook, app manifest and app redirect once the
	// listener is set ([API.WebhookBase]), and the address a box off its
	// node's loopback dials (its CREWLET_MCP_BRIDGE_URL and
	// CREWLET_SANDBOX_OTEL_RECEIVER_URL, its per-run endpoints' base).
	// REQUIRED once the listener is set on an ingress node, because that
	// node is the one building those addresses and handing them to vendors.
	//
	// TIER A, beside [API.ExternalURL] and for its reason: an address a
	// manifest and a webhook registration are built from is a fact about
	// where THIS deployment's sockets are published, which is the
	// operator's, and it is an address by the time anything reads it.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" js:"maxlen=2048" desc:"Where vendors, and sandbox boxes off their node's loopback, reach this listener, e.g. https://hooks.example.com. Required once port is set on an ingress node. At most 2048 characters."`
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
	if err := checkAddressLength(at(path, "external_url"), raw); err != nil {
		probs.wrap(err)
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

// LoopbackURL reports whether an address names a loopback host, one only the
// machine it runs on reaches, by the rule [API.externalLoopback] judges the
// person's address with: the setup listing says by it whether the address
// third-party apps are given is one none of them can deliver to.
//
// THE RULE ITSELF, NOT A SECOND COPY: the address is judged as an
// `api.external_url` is, so the two answers cannot drift apart.
func LoopbackURL(raw string) bool { return API{ExternalURL: raw}.externalLoopback() }

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
	if err := checkAddressLength(at(path, "external_url"), raw); err != nil {
		return err
	}
	p.wrap(checkExternalURL(at(path, "external_url"), raw,
		"the scheme sets cookie security and every link, so it can't be guessed",
		"people use to reach this deployment in a browser"))
	if parsed, err := url.Parse(raw); err == nil && hasHTTPScheme(raw) {
		p.wrap(checkBrowserHost(at(path, "external_url"), raw, parsed.Hostname()))
	}
	return p.err()
}

// MaxExternalURLLength bounds every external address, `api.external_url`,
// `api.public.external_url` and `api.artifacts.external_url`, in characters
// as written: 2,048, past the 2,000 browsers reliably carry. An integration's
// status records the vendors' address exactly, to compare it with the one in
// force, so this bound is what gives that record a widest form
// (internal/integration's status at coord.MaxIntegrationStatusBytes).
const MaxExternalURLLength = 2048

// checkAddressLength refuses an external address past [MaxExternalURLLength]
// characters as written, before any other rule judges it.
func checkAddressLength(path Path, raw string) error {
	var p problems
	if n := utf8.RuneCountInString(raw); n > MaxExternalURLLength {
		p.add(path, ErrOutOfRange, "the address is %d characters; it can be at most %d, the "+
			"longest every browser reliably carries", n, MaxExternalURLLength)
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

// APIAuth is how anyone (a person, a machine, this deployment's own operator)
// proves who they are to the HTTP surface.
//
// There is no `allow_anonymous_read`, `disabled`, `oidc`, `backend`/`local` or
// `bootstrap` key; each is refused as unknown, so a file carrying one fails
// rather than running a posture it no longer describes. Reads need a
// credential (they carry transcripts, diary entries and the event stream); a
// public reader is a named token holding only the apps it reads. The dev
// principal is the `crewlet run -dev-principal` flag, not a field (a field is
// copied into an image), refused unless `api.host` BINDS loopback: the bind,
// not the external URL, because it is about who can open a socket here.
// The first person is invited under the Tier A token below.
type APIAuth struct {
	// Admin is whether THIS NODE grants admin to those who hold it (`serve`),
	// or clamps admin away and keeps every app (`refuse`). REQUIRED once the
	// API is served; the zero is refused, not read as either, because each
	// default is a decision nobody made: `serve` publishes host access on a
	// node meant for colleagues, `refuse` locks administrators out.
	//
	// A node decides because admin is host access, declared in a tier this
	// one does not trust (permissions are written to the replicated store by
	// whoever holds admin). Admin only: the clamp keeps every app
	// ([iam.AdminPosture.Effective] expands first). Applied per node at
	// decision time, nothing written, so nodes may disagree; each publishes
	// its posture on its presence lease.
	Admin iam.AdminPosture `yaml:"admin,omitempty" json:"admin,omitempty" js:"enum=serve|refuse" desc:"Whether this node grants admin to those who hold it (serve), or clamps admin away and keeps every app (refuse). Required once the API is served."`

	// Limits bound the company's sign-in policy on this node (ADR-0152).
	// See [SignInLimits].
	Limits SignInLimits `yaml:"limits,omitempty" json:"limits,omitzero"`

	// Audit is how long the identity estate keeps what happened.
	Audit APIAudit `yaml:"audit,omitempty" json:"audit,omitzero"`

	// AcceptInsecure is the deliberate acknowledgement that this deployment is
	// reachable off loopback with a posture that should not be. It lets
	// `limits.second_factor: optional` stand on a network-reachable address,
	// which [APIAuth.validateSecondFactor] refuses without it, and is logged at
	// WARN on every start. It does not lift plain http off loopback, which is a
	// warning either way ([API.warnings]).
	//
	// A field rather than a flag, unlike `-dev-principal`: the flag
	// authenticates every request as somebody and must never be copied into an
	// image; this weakens a requirement while every credential is still
	// presented, which a staging config may carry.
	AcceptInsecure bool `yaml:"accept_insecure,omitempty" json:"accept_insecure,omitempty" desc:"Acknowledge an insecure posture on a routable address. Logged at WARN on every start."`

	// Tokens are this deployment's own machine credentials: break-glass,
	// the operator CLI, a CI pipeline. At least one is REQUIRED once the
	// API is served — see [APIAuth.validate].
	Tokens []APIToken `yaml:"tokens,omitempty" json:"tokens,omitempty" desc:"The deployment's machine credentials. At least one is required once the API is served."`

	// AllowedOrigins are the browser origins CORS permits. Empty means
	// SAME-ORIGIN ONLY: the dashboard is served by this process, so it
	// needs no entry. The previous default was "*", which let any site a
	// logged-in operator visited read every unauthenticated endpoint.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty" desc:"Additional browser origins this deployment is reached on. Empty = same-origin only."`

	// CompanyWriters, when set, are the ids of the Tier A tokens that alone may
	// CHANGE the company document. Empty leaves every write to the authority
	// table, the posture of a company edited by people.
	//
	// It is for a document something ELSE is the source of (a GitOps pipeline
	// or Kubernetes operator) that overwrites the active revision at every
	// reconcile: a person's edit would be lost silently, so it is refused here,
	// naming who manages the document. Reads and non-document writes are free.
	//
	// A token id list: a Tier A token acts as login `token:<id>`, which every
	// revision records as author and credential. Entries follow
	// [iam.ValidTokenID]; [APIAuth.MayWriteCompany] reads it (ADR-0030).
	CompanyWriters []string `yaml:"company_writers,omitempty" json:"company_writers,omitempty" desc:"Ids of the Tier A tokens that alone may change the company document. Empty = whoever the authority table admits."`
}

// CompanyManaged reports whether the company document is managed: some Tier A
// token ids are named as its only writers.
func (a *APIAuth) CompanyManaged() bool { return len(a.CompanyWriters) > 0 }

// MayWriteCompany reports whether a write made as by may change the company
// document. It is the one reading of [APIAuth.CompanyWriters], for the config
// write path and the viewer answer that tells the dashboard whether to offer
// an edit.
//
// The credential, never the author: by's [iam.Actor.OperatorID] is the
// credential the write came through, a Tier A bearer's being `token:<id>`. A
// person's session, a `pat:<id>` token and a session exchanged from a Tier A
// token each name another credential, so all are refused while the document is
// managed, a lead in their own subtree included: the managing system's next
// reconcile would overwrite exactly that edit. The engine itself is admitted
// ([iam.ActorSystem]); a chart apply is not an edit somebody made here.
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

// MaxPasswordLength is the ceiling under a password floor ([SignIn.MinPasswordLength]).
//
// A MINIMUM THAT NOBODY CAN SATISFY IS A DEPLOYMENT NOBODY CAN ENROL IN, and
// 256 is already far past any memorable passphrase — so a larger number here
// is a unit mistake rather than a policy.
const MaxPasswordLength = 256

// validateSecondFactor is the acknowledgement an optional second factor needs
// off loopback, judged where a browser reaches this deployment rather than on
// the bind address, which a hardened node keeps on loopback behind its proxy.
func (a *APIAuth) validateSecondFactor(path Path, api API, serving bool) error {
	var p problems
	if a.Limits.OptionalSecondFactor() && serving &&
		!api.externalLoopback() && !a.AcceptInsecure {
		p.add(at(path, "limits.second_factor"), ErrConflict,
			"`optional` lets the company allow password-only sign-in at %s. Remove the "+
				"line to require two-step verification, or set `accept_insecure: true`",
			api.ExternalBase())
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
	// `artifacts`, `chatlet`, `gitlet`, `atlas`. REQUIRED and non-empty: a
	// credential's blast radius is stated at the pin, never inferred. The
	// founder's break-glass token is `[admin]`; a CI pipeline that files work is
	// `[worklet]`, one that fetches the company's code `[gitlet]` (ADR-0110; a
	// token bound to no seat never pushes, ADR-0085), one that reads the map
	// `[atlas]` (ADR-0127). A token bound to a person's seat is held to this
	// list as well as to the person.
	//
	// STRICT, unlike a replicated row: an unknown name is refused, naming the
	// set, because a file an operator wrote has a typo, not a newer peer's record.
	Grants []iam.Grant `yaml:"grants,omitempty" json:"grants,omitempty" desc:"What this credential may do: admin, worklet, doclet, artifacts, chatlet, gitlet, atlas. Required and non-empty."`
}

// validate checks the auth block; serving is [Bootstrap.ServesAPI].
func (a *APIAuth) validate(path Path, api API, serving bool) error {
	var p problems

	p.wrap(a.validateAdmin(path, serving))
	p.wrap(a.Limits.validate(at(path, "limits")))
	p.wrap(a.Audit.validate(at(path, "audit")))
	p.wrap(a.validateSecondFactor(path, api, serving))
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
			"%q isn't a valid value. Use %s", a.Admin, words.Or(iam.AdminPostures))
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
						"for Ask Crewlet's grants. Generate one "+
						"with `crewlet secrets keygen`", iam.AssistBearerPrefix)
			}
		}

		if len(t.Grants) == 0 {
			p.add(at(tp, "grants"), ErrMissing,
				"required. List the permissions this token gets, any of %s",
				words.Or(iam.AllGrants))
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
			p.add(gp, ErrUnknownValue, "%s", iam.NotAGrant(g))
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
// An allowance is compared against the browser's `Origin` header, always
// `scheme://host[:port]` in a browser's spelling (auth.Origin). Each refused
// shape is either one no browser can send, an allow-list that looks configured
// while every fetch fails unseen, or one that permits more than it reads as:
// a path, query, fragment or user is dropped before comparison, so
// `https://ops.example.com/dashboard` would admit every page of that host.
//
// `*` is refused, not honoured: any site a logged-in operator visits could
// read every unauthenticated endpoint (LLM transcripts, diary entries, the
// whole event stream).
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
// cross-site check cannot compare: a browser respells these in ways this
// engine cannot reproduce (auth.Origin), so each would refuse every write.
//
//   - A host outside ASCII (`bücher.example` is `xn--bcher-kva.example`): the
//     IDNA tables are needed and nothing else here uses them.
//   - An IPv4 address not written as four decimal octets (`127.1`,
//     `2130706433`, `0x7f.0.0.1`, `192.168.010.1`, octal): a browser rewrites it.
//   - An IPv6 zone (`[fe80::1%25eth0]`), which no browser can address.
//
// Only the browser's addresses are held to this (`api.external_url`,
// `api.auth.allowed_origins`), never the public listener's.
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
	// engine never terminates TLS, so something in front does, and unless it
	// is named here every request's source is ITS address: the sign-in
	// throttle, the audit trail and every per-source rule see the company as
	// one caller, so a stranger guessing at somebody's login puts that
	// person's own sign-in behind the same wait.
	//
	// A WARNING, NOT A REFUSAL: one front end is right with the list empty (a
	// balancer passing the client's address through as the peer) and nothing
	// here can tell the two apart.
	if strings.HasPrefix(a.ExternalBase(), "https://") && len(a.TrustedProxies) == 0 {
		out = append(out, advisory(field("api.trusted_proxies"),
			"this deployment uses https but trusts no proxy, so every caller "+
				"shows the proxy's address. Add your proxy's address block, "+
				"unless it passes client addresses through"))
	}

	return out
}
