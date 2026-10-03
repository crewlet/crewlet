package config

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

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

	// Port is the bind port. 0 serves no HTTP at all — no dashboard, no
	// REST API, and no webhook endpoint, so every integration goes deaf.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port; 0 disables the HTTP surface entirely."`

	// ExternalURL is where a BROWSER and a third-party app reach this
	// deployment. REQUIRED once the API is served.
	//
	// # One fact, one field
	//
	// It replaced `api.external_url` outright rather than
	// sitting beside it, because three separate things need the same
	// answer and two fields is two answers:
	//
	//   - the session cookie's `Secure` flag and its `__Host-` prefix,
	//     which follow this scheme. The engine sits behind a
	//     TLS-terminating proxy and therefore cannot read `r.TLS` — the
	//     request it sees is plain http on a loopback socket — so there
	//     is nothing else to derive them from;
	//   - the origin a non-GET request and a socket handshake are checked
	//     against;
	//   - the base every vendor webhook and every pasted manifest is
	//     built on, which is the job the retired field had.
	//
	// # Why it moved to Tier A
	//
	// The old field was Tier B, so it was a `${VAR}` a node resolved at
	// the surface that used it — and a node that could not resolve it
	// built a manifest from the literal seven characters. The cookie and
	// the origin check cannot work that way: they are decided before a
	// company document is loaded, and they are part of what authenticates
	// the request that would go on to load one. A value the authentication
	// path depends on belongs in the tier that holds the keyring.
	//
	// Written as scheme://host[:port], with no path: it is a BASE, and a
	// path here would be concatenated into every URL built from it.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" desc:"Where a browser and a vendor reach this deployment, e.g. https://crewlet.example.com. Required once port is set."`

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

	Auth APIAuth `yaml:"auth,omitempty" json:"auth"`
}

// Serving reports whether this configuration binds an HTTP surface at all.
//
// The one predicate every rule below turns on, in one place: a deployment that
// serves nothing needs no external URL, no grant ceiling and no credential,
// and a deployment that serves needs all three. The keyring is not among them:
// every node needs it, serving or not ([Bootstrap.Validate]).
func (a API) Serving() bool { return a.Port != 0 }

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
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *API) validate(path Path) error {
	var p problems
	// Unbounded, a port of 70000 passes validation and fails at bind,
	// long after `crewlet validate` said the config was good.
	if a.Port < 0 || a.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange,
			"must be 0 (no HTTP surface) or a port 1..65535, got %d", a.Port)
	}
	p.wrap(a.validateExternalURL(path))
	for i, block := range a.TrustedProxies {
		p.wrap(checkTrustedProxy(idx(at(path, "trusted_proxies"), i), block))
	}
	p.wrap(a.Auth.validate(at(path, "auth"), *a))
	return p.err()
}

// validateExternalURL is the required-once-served rule and the shape rules
// under it.
func (a *API) validateExternalURL(path Path) error {
	var p problems
	raw := strings.TrimSpace(a.ExternalURL)
	if raw == "" {
		if a.Serving() {
			p.add(at(path, "external_url"), ErrMissing,
				"required once `api.port` is set: it decides the session "+
					"cookie's Secure flag and __Host- prefix, the origin every "+
					"write is checked against and the base every webhook URL is "+
					"built on. The engine sits behind "+
					"a TLS-terminating proxy and cannot read any of that off the "+
					"request. Write it as scheme://host[:port], e.g. "+
					"https://crewlet.example.com or http://localhost:%d",
				DefaultAPIPort)
		}
		return p.err()
	}
	if !hasHTTPScheme(raw) {
		p.add(at(path, "external_url"), ErrShape,
			"%q must start with http:// or https://: a browser Origin, a "+
				"cookie's Secure flag and every webhook URL are all decided by the "+
				"scheme, so a value without one decides them wrongly rather "+
				"than failing", raw)
		return p.err()
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		p.add(at(path, "external_url"), ErrShape, "%q is not a URL: %v", raw, err)
		return p.err()
	}
	if parsed.Hostname() == "" {
		p.add(at(path, "external_url"), ErrShape,
			"%q names no host: it is the address a browser and a vendor reach "+
				"this deployment on", raw)
	}
	// A PATH, A QUERY OR A FRAGMENT IS REFUSED rather than trimmed. Every
	// consumer concatenates onto this value, so `https://x/crewlet/`
	// yields `https://x/crewlet/webhooks/github` — which may be exactly
	// what a path-routing proxy needs — while a query or a fragment yields
	// an address no proxy can route and no vendor will accept. Trimming
	// the first would break the deployment it is correct for, so only the
	// two that can never be right are refused, and the trailing slash is
	// the one thing normalised.
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		p.add(at(path, "external_url"), ErrShape,
			"%q carries a query or a fragment: every URL this engine builds is "+
				"this value with a path appended, so neither can survive the "+
				"concatenation. Write the scheme, host, port and path prefix "+
				"alone", raw)
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
				"%q is an address rather than a block, and it is refused rather "+
					"than read as one host: the other reading is a subnet "+
					"somebody abbreviated, and trusting a whole subnet by "+
					"accident is not a mistake this can make on your behalf. "+
					"Write %s/%d for the one host", entry, entry, bits)
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
				"%q trusts every peer's forwarded header, which means every "+
					"caller chooses their own client address — their own rate "+
					"limit bucket and their own audit row. Name the proxies' "+
					"own block, e.g. 10.0.0.0/8", entry)
		}
		if prefix.Addr() != prefix.Masked().Addr() {
			p.add(path, ErrShape,
				"%q has host bits set below its prefix length, so it is not "+
					"the block it reads as. Write %s", entry, prefix.Masked())
		}
	}
	return p.err()
}

// AuthBackend is how the PEOPLE of a company prove who they are.
//
// It governs people only. The Tier A tokens in [APIAuth.Tokens] are the
// deployment's own machine credentials and are accepted on every backend,
// including [AuthBackendNone] — which is what makes `none` a coherent posture
// rather than an unreachable engine.
type AuthBackend string

const (
	// AuthBackendLocal is passwords, a second factor and recovery codes
	// held by this engine.
	AuthBackendLocal AuthBackend = "local"

	// AuthBackendNone is a deployment with no people at all: the Tier A
	// tokens are the only credentials, which is the posture a laptop and
	// a CI fixture run in.
	AuthBackendNone AuthBackend = "none"
)

// AuthBackends is the closed set.
var AuthBackends = []AuthBackend{AuthBackendLocal, AuthBackendNone}

// Valid reports whether a backend off the wire is one this build knows.
func (b AuthBackend) Valid() bool { return slices.Contains(AuthBackends, b) }

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
// public reader is now a named token holding read grants and nothing else,
// which is listable, revocable without a restart, and present in the audit log.
//
// `disabled` authenticated the EMPTY credential into full operator authority,
// with no check on the bind address anywhere. `crewlet run -dev-principal`
// replaces it: a flag rather than a field, because a field gets copied into an
// image, and refused unless `api.host` BINDS loopback and the binary is a
// development build. The bind rather than `api.external_url`, deliberately and
// unlike `api.auth.local`'s insecure rule: that one is about whether a cookie
// crosses plaintext through a proxy, where the external address is the truth,
// and this is about who can open a socket to this process, where the bind
// is.
//
// `oidc` signed people in through an identity provider, and sign-in through a
// third party is not something this engine does any more: a person proves who
// they are with a password and a second factor held here, or the deployment
// has no people at all.
//
// `bootstrap` opened a second way in for the first person — a one-time code a
// node wrote beside its store — beside the one every serving node already
// requires. The Tier A token below IS the credential a company has before it
// has anybody, so the first person is invited under it exactly as everybody
// after them is, and the founder route, its race between two codes and its
// setting went.
//
// All four are refused by name if they appear — see retiredBootstrapFields.
type APIAuth struct {
	// Backend is how PEOPLE sign in. Empty derives from whether the
	// `local` block is present: `local` if it is, and `none` if not —
	// which is the honest reading of a file that says nothing about
	// people, and never a password backend somebody did not ask for.
	Backend AuthBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=local|none" desc:"How people sign in. Empty derives from whether local is present."`

	// MaxGrants is THE CEILING: the most authority this deployment will
	// let a company's own directory confer. REQUIRED once the API is
	// served.
	//
	// # Why a ceiling exists at all, when every grant is already declared
	//
	// Because the declarations are made in a tier this one does not
	// trust. A person's grants live in the replicated store and are
	// written by whoever holds people.manage. Without a bound, a
	// directory write confers ANY capability it names — including the
	// secret store — and the operator who owns the machines never said
	// it could. Tier A states the bound and never
	// reads Tier B, which is the same direction every other Tier A
	// posture runs in.
	//
	// It is intersected at DECISION time, per node, per request: nothing
	// is written when it changes, so lowering it takes effect on the next
	// request that node serves and needs no config apply, no migration
	// and no restart of the fleet. A fleet whose nodes disagree is
	// therefore a LEGAL state during a rollout — and one nobody could see,
	// which is why each node publishes a hash of its own resolved ceiling
	// on its presence lease.
	//
	// REQUIRED RATHER THAN DEFAULTED TO ALL ELEVEN, because a default that
	// grants everything is a ceiling that does nothing, and one that
	// grants a subset silently locks out whatever it left out. Eleven
	// lines in a config diff is what a reviewer needs to see.
	MaxGrants []iam.Grant `yaml:"max_grants,omitempty" json:"max_grants,omitempty" desc:"The most authority the directory may confer. Required once the API is served."`

	// Session bounds how long a proof of identity lasts.
	Session APISession `yaml:"session,omitempty" json:"session,omitzero"`

	// Audit is how long the identity estate keeps what happened.
	Audit APIAudit `yaml:"audit,omitempty" json:"audit,omitzero"`

	// Local configures the password backend. A POINTER because its
	// presence is what `backend` derives from, and a value type cannot
	// tell an absent block from one whose every field is empty.
	Local *APILocal `yaml:"local,omitempty" json:"local,omitempty"`

	// Tokens are this deployment's own machine credentials: break-glass,
	// the operator CLI, a CI pipeline. At least one is REQUIRED once the
	// API is served — see [APIAuth.validate].
	Tokens []APIToken `yaml:"tokens,omitempty" json:"tokens,omitempty" desc:"The deployment's machine credentials. At least one is required once the API is served."`

	// AllowedOrigins are the browser origins CORS permits. Empty means
	// SAME-ORIGIN ONLY: the dashboard is served by this process, so it
	// needs no entry. The previous default was "*", which let any site a
	// logged-in operator visited read every unauthenticated endpoint.
	AllowedOrigins []string `yaml:"allowed_origins,omitempty" json:"allowed_origins,omitempty" desc:"Additional browser origins this deployment is reached on. Empty = same-origin only."`
}

// Resolved is which backend this block configures, with the derivation
// applied: what is declared, or what the presence of a block implies.
func (a APIAuth) Resolved() AuthBackend {
	if a.Backend != "" {
		return a.Backend
	}
	if a.Local != nil {
		return AuthBackendLocal
	}
	return AuthBackendNone
}

// Ceiling is the resolved ceiling as a set, for the per-request intersection.
func (a APIAuth) Ceiling() []iam.Grant { return a.MaxGrants }

// CeilingHash is a stable digest of the resolved ceiling, for the value a node
// publishes about itself.
//
// SORTED AND DEDUPLICATED FIRST, so two nodes running the same ceiling written
// in a different order — which is every fleet whose config is assembled by a
// template — publish the same hash. An unsorted digest would report a mixed
// ceiling on a fleet that has none, and an operator who saw that once would
// stop believing the signal.
//
// It is a HASH rather than the list because it rides a presence lease's Meta,
// which every node in the fleet reads on every heartbeat: ten strings per node
// is a payload that grows with the fleet to say one bit.
func (a APIAuth) CeilingHash() string {
	// EMPTY IS NOT A CEILING OF NOTHING. A node that serves no HTTP
	// surface declares no ceiling, and hashing the empty set would give
	// it a digest that differs from every serving node's — drawing a
	// disagreement across a fleet whose worker nodes simply have no
	// opinion. The absent value is what says "not saying"; see
	// [coord.NodeStatus.GrantCeilingHash].
	if len(a.MaxGrants) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(a.MaxGrants))
	for _, g := range a.MaxGrants {
		sorted = append(sorted, string(g))
	}
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:8])
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
	// every session in the company, writing the configuration, the chart
	// or the credential store — as GitHub's sudo mode applies one window
	// to all of its sensitive actions. An hour keeps the stricter end of
	// that practice. A second, shorter window for some of the gestures was
	// tried and went: it sent an administrator who had proved half an hour
	// earlier to prove again between one screen and the next, while what
	// actually keeps those gestures from a stolen credential is not a
	// proof's age — they ask a grant no machine token may carry, or are
	// refused to any request a machine token presented.
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
		"below an hour a session ends inside its own step-up window, and past a "+
			"month a bearer already issued outlives any change to this setting, "+
			"because the deadline is signed into it")
	check("step_up", s.StepUpRaw, SessionStepUpFloor, SessionStepUpCeiling,
		"below five minutes an administrator re-proves their identity between "+
			"one screen and the next, and past a day a step-up is not one")
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
		"below ninety days a grant change is gone before the quarter it was "+
			"made in is reviewed, and past about three years this is a "+
			"retention policy rather than an audit horizon")
	checkDurationRange(&p, at(path, "sessions"), a.SessionsRaw,
		AuditSessionsFloor, AuditSessionsCeiling,
		"below a week an intrusion is unreconstructable, and past four hundred "+
			"days the sign-in rows outlive the change rows that explain them")
	// THE ORDER BETWEEN THEM IS THE RULE. A session row from three
	// hundred days ago answers "who signed in"; what it does not answer
	// is what that person was allowed to do, which is a change row. Keep
	// the sessions longer than the changes and an investigation finds a
	// sign-in it cannot interpret.
	if a.Sessions() > a.Changes() {
		p.add(at(path, "sessions"), ErrConflict,
			"%s is longer than `changes` (%s), so a sign-in would outlive the "+
				"record of what that person was allowed to do when they made "+
				"it. Raise `changes` or lower this", a.Sessions(), a.Changes())
	}
	return p.err()
}

// APILocal is the password backend: what this engine itself asks of a person.
type APILocal struct {
	// TOTP is whether a second factor is REQUIRED or merely offered. It
	// has no default and its zero value is refused — see [iam.SecondFactor]
	// for why this of all settings may not be inherited silently.
	TOTP iam.SecondFactor `yaml:"totp,omitempty" json:"totp,omitempty" js:"enum=required|optional" desc:"Whether a second factor is required. No default: state one."`

	// AcceptInsecure is the deliberate acknowledgement that this
	// deployment is reachable off loopback with a posture that should not
	// be. It is what lets `totp: optional` and an `http://` external URL
	// stand on a routable address, and it is logged at WARN on every
	// start for the whole life of the deployment.
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
}

// MaxPasswordLength is the ceiling under [APILocal.MinPasswordLength].
//
// A MINIMUM THAT NOBODY CAN SATISFY IS A DEPLOYMENT NOBODY CAN ENROL IN, and
// 256 is already far past any memorable passphrase — so a larger number here
// is a unit mistake rather than a policy.
const MaxPasswordLength = 256

// Passwords is the effective minimum password length.
func (l *APILocal) Passwords() int {
	if l == nil || l.MinPasswordLength == 0 {
		return iam.MinPasswordChars
	}
	return l.MinPasswordLength
}

func (l *APILocal) validate(path Path, api API) error {
	var p problems
	if !l.TOTP.Valid() {
		if l.TOTP == "" {
			p.add(at(path, "totp"), ErrMissing,
				"state `required` or `optional`: there is no default, because "+
					"the value a deployment would inherit is the one that "+
					"silently has no second factor")
		} else {
			p.add(at(path, "totp"), ErrUnknownValue,
				"%q (want %s or %s)", l.TOTP, iam.SecondFactorRequired,
				iam.SecondFactorOptional)
		}
	}
	// AN OPTIONAL SECOND FACTOR OFF LOOPBACK IS A FAULT unless somebody
	// said so out loud. The judgement is on where a BROWSER reaches this
	// deployment, not on the bind address: a hardened node binds loopback
	// behind its proxy, so a bind check would permit the insecure posture
	// in exactly the deployment that must refuse it.
	if l.TOTP == iam.SecondFactorOptional && api.Serving() &&
		!api.externalLoopback() && !l.AcceptInsecure {
		p.add(at(path, "totp"), ErrConflict,
			"`optional` on a deployment a browser reaches at %s means a "+
				"password alone signs somebody in over the network. Write "+
				"`required`, or acknowledge it with `accept_insecure: true` — "+
				"which is logged at WARN on every start", api.ExternalBase())
	}
	if n := l.MinPasswordLength; n != 0 {
		switch {
		case n < iam.MinPasswordChars:
			p.add(at(path, "min_password_length"), ErrOutOfRange,
				"%d is below the engine's own floor of %d, and it is refused "+
					"rather than raised: a file asking for %d is a file whose "+
					"writer believes %d is enough. Remove the line to take the "+
					"floor", n, iam.MinPasswordChars, n, n)
		case n > MaxPasswordLength:
			p.add(at(path, "min_password_length"), ErrOutOfRange,
				"%d is past %d, which is already far longer than any passphrase "+
					"somebody will type: a minimum nobody can satisfy is a "+
					"deployment nobody can enrol in", n, MaxPasswordLength)
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
	Token string `yaml:"token" json:"token" js:"required" desc:"Token value or ${VAR} reference."`

	// Grants is what this credential may do. REQUIRED and non-empty: a
	// credential's blast radius is stated at the pin, never inferred.
	//
	// THIS IS THE CHANGE THAT MAKES A TOKEN SAFE TO HAVE. Before it, one
	// token was the whole operator surface — the config document, the
	// secret store, the fleet, every transcript — so the CI pipeline that
	// needed to file a work item held the credential that could read
	// every key the company owns.
	Grants []iam.Grant `yaml:"grants,omitempty" json:"grants,omitempty" desc:"What this credential may do. Required and non-empty."`
}

func (a *APIAuth) validate(path Path, api API) error {
	var p problems

	if a.Backend != "" && !a.Backend.Valid() {
		p.add(at(path, "backend"), ErrUnknownValue,
			"%q (want %s or %s)", a.Backend, AuthBackendLocal,
			AuthBackendNone)
	}

	p.wrap(a.validateCeiling(path, api))
	p.wrap(a.Session.validate(at(path, "session")))
	p.wrap(a.Audit.validate(at(path, "audit")))
	p.wrap(a.validateBackendBlocks(path, api))
	p.wrap(a.validateTokens(path, api))

	for i, origin := range a.AllowedOrigins {
		p.wrap(checkOrigin(idx(at(path, "allowed_origins"), i), origin))
	}
	return p.err()
}

// validateCeiling is the required-once-served rule on max_grants.
func (a *APIAuth) validateCeiling(path Path, api API) error {
	var p problems
	if len(a.MaxGrants) == 0 {
		if api.Serving() {
			p.add(at(path, "max_grants"), ErrMissing,
				"required once `api.port` is set: it is the bound on what this "+
					"company's own directory may confer on anybody, and the tier "+
					"that holds the keyring is the tier that states it. There is no "+
					"default, because one granting everything is a ceiling that "+
					"does nothing and one granting a subset silently locks out "+
					"whatever it left out. The full set is %v", iam.AllGrants)
		}
		return p.err()
	}
	seen := make(map[iam.Grant]struct{}, len(a.MaxGrants))
	for i, g := range a.MaxGrants {
		gp := idx(at(path, "max_grants"), i)
		if !g.Valid() {
			p.add(gp, ErrUnknownValue,
				"%q is not a grant this build knows. The set is %v", g, iam.AllGrants)
		}
		if _, dup := seen[g]; dup {
			p.add(gp, ErrConflict, "duplicate grant %q", g)
		}
		seen[g] = struct{}{}
	}
	return p.err()
}

// validateBackendBlocks refuses a backend with no block to configure it and a
// block no backend reads.
func (a *APIAuth) validateBackendBlocks(path Path, api API) error {
	var p problems
	resolved := a.Resolved()

	// A BLOCK NO BACKEND READS IS REFUSED rather than ignored, because
	// ignoring it is how a deployment runs with `backend: none` while its
	// file carries a fully configured password policy and everybody
	// believes sign-in is set up.
	if a.Local != nil && resolved != AuthBackendLocal {
		p.add(at(path, "local"), ErrConflict,
			"`backend: %s` reads nothing under `local`, so this block "+
				"configures nothing and looks configured. Remove it, or write "+
				"`backend: %s`", resolved, AuthBackendLocal)
	}
	if resolved == AuthBackendLocal {
		if a.Local == nil {
			// NOT DEFAULTED, because the one setting this block
			// exists for has no safe default: `totp` decides
			// whether a password alone is enough, and a block that
			// defaulted would be answering that question on the
			// operator's behalf.
			p.add(at(path, "local"), ErrMissing,
				"`backend: %s` needs a `local:` block, because the one thing "+
					"it has to state — whether a second factor is required — "+
					"has no safe default", AuthBackendLocal)
		} else {
			p.wrap(a.Local.validate(at(path, "local"), api))
		}
	}
	return p.err()
}

// validateTokens is the token list: the label, the value, the entropy floor,
// the grants and the required-once-served rule.
func (a *APIAuth) validateTokens(path Path, api API) error {
	var p problems
	seen := make(map[string]struct{}, len(a.Tokens))
	for i, t := range a.Tokens {
		tp := idx(at(path, "tokens"), i)
		if t.ID == "" {
			p.add(at(tp, "id"), ErrMissing,
				"every token needs a label: it names the login `token:<id>` every "+
					"write made with the token is recorded under")
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
				"token id %q would act under the login %q, which is not a "+
					"machine handle: use lowercase letters, digits and hyphens, "+
					"optionally joined by colons (founder, ci-pipeline, "+
					"ci:release), in at most %d characters so the login stays "+
					"within %d. It is recorded as the author of every write "+
					"made with the token, and a login outside the grammar can "+
					"never be bound to a seat", t.ID, iam.TokenLogin(t.ID),
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
				"token must not be empty. If this is a ${VAR}, the variable is "+
					"unset — Tier A resolves from the environment alone, and an "+
					"unset reference expands to nothing rather than failing")
		default:
			if err := secrets.CheckOperatorToken(t.Token); err != nil {
				// THE VALUE NEVER REACHES THE MESSAGE. This
				// refusal is printed by `crewlet validate`,
				// answered over /config and written to a log.
				p.add(at(tp, "token"), ErrUnknownValue, "%s", err.Error())
			}
		}

		if len(t.Grants) == 0 {
			p.add(at(tp, "grants"), ErrMissing,
				"required and non-empty: a credential's blast radius is stated "+
					"where it is pinned, never inferred. Name what this token "+
					"is for — the full set is %v", iam.AllGrants)
		}
		p.wrap(checkGrants(at(tp, "grants"), t.Grants, a.MaxGrants,
			"a token may not carry authority the deployment's own ceiling "+
				"withholds from everybody else"))
	}

	// A DEPLOYMENT WITH NO TIER A TOKEN IS A FAULT, on both backends, and
	// it is a fault rather than a warning because each leaves a different
	// way to be locked out of your own company and both are permanent:
	//
	//   - on `local`, an administrator who is throttled, who lost their
	//     second factor, or whose password is refused has nothing else to
	//     present;
	//   - on `none`, it is the only credential that exists at all.
	//
	// And on every one of them, a fresh deployment's identity estate is
	// EMPTY: somebody has to invite the first person, and the token is
	// the only credential that exists to do it with.
	if api.Serving() && len(a.Tokens) == 0 {
		p.add(at(path, "tokens"), ErrMissing,
			"at least one token is required once `api.port` is set, on every "+
				"backend. The identity estate of a fresh deployment is empty, "+
				"so this is what invites the first person (`crewlet iam "+
				"invite`); and on a running one it is the way back in when an "+
				"administrator has locked themselves out. Generate one with "+
				"`crewlet secrets keygen` and point `token:` at a ${VAR}")
	}
	return p.err()
}

// checkGrants refuses a grant this build does not know and one the ceiling
// withholds.
//
// AN ABSENT CEILING CLAMPS NOTHING HERE. Its own required-once-served rule has
// already been reported by then, and re-reporting every grant as "outside the
// ceiling" would bury that one real fault under ten derived ones.
func checkGrants(path Path, grants, ceiling []iam.Grant, why string) error {
	var p problems
	for i, g := range grants {
		gp := idx(path, i)
		if !g.Valid() {
			p.add(gp, ErrUnknownValue,
				"%q is not a grant this build knows. The set is %v", g, iam.AllGrants)
			continue
		}
		if len(ceiling) > 0 && !slices.Contains(ceiling, g) {
			p.add(gp, ErrConflict,
				"%q is outside `api.auth.max_grants`, and %s. Add it to the "+
					"ceiling if this deployment allows it, or drop it here",
				g, why)
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
			"%q is not a duration, write it as 24h, 7d is 168h, 30m", raw)
		return
	}
	if d < lo || d > hi {
		p.add(path, ErrOutOfRange, "%s is outside %s..%s: %s", raw, lo, hi, why)
	}
}

// checkOrigin refuses an origin a browser will never match.
//
// # Why each of these is refused rather than accepted and ignored
//
// A CORS allow-list is compared against the browser's `Origin` header
// EXACTLY, and the header is always `scheme://host[:port]` with no path and
// no trailing slash. Every shape below is a value an operator plausibly
// writes and no browser can ever equal — so accepting it produces an
// allow-list that looks configured, a fetch that fails in a console the
// engine never sees, and nothing anywhere saying why.
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
		p.add(path, ErrShape, "%q is not an origin and is refused rather "+
			"than honoured: it would let any site a logged-in operator "+
			"visits read this API — which carries LLM transcripts, diary "+
			"entries and the whole event stream. Name each site, as "+
			"https://ops.example.com", origin)
	case !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://"):
		p.add(path, ErrShape, "%q has no scheme: a browser's Origin header "+
			"is always scheme://host[:port], so this matches nothing. Write "+
			"https://%s", origin, origin)
	case strings.HasSuffix(origin, "/"):
		p.add(path, ErrShape, "%q ends in a slash: a browser's Origin "+
			"header carries no path, so this matches nothing. Write %q",
			origin, strings.TrimRight(origin, "/"))
	default:
		if rest := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://"); strings.Contains(rest, "/") {
			p.add(path, ErrShape, "%q carries a path: a browser's Origin "+
				"header is the scheme, host and port alone, so this matches "+
				"nothing", origin)
		}
	}
	return p.err()
}

// warnings are the API postures that are VALID and worth reading before this
// deployment runs on them.
//
// NONE OF THESE CAN BE A REFUSAL, and each says why in its own sentence. What
// they share is that the configuration works exactly as written and the
// consequence is somewhere else: on the network between a browser and this
// node, or in a front end the engine cannot tell apart from its callers.
func (a API) warnings() []Warning {
	var out []Warning
	if !a.Serving() {
		return out
	}
	auth := a.Auth

	// AN INSECURE POSTURE SOMEBODY ACKNOWLEDGED IS STILL INSECURE. The
	// acknowledgement is what makes it legal, and it is a decision made
	// once that everybody after inherits — so `crewlet validate` says it
	// out loud every time, exactly as the engine logs it on every start.
	if local := auth.Local; local != nil && local.AcceptInsecure {
		out = append(out, advisory(field("api.auth.local.accept_insecure"),
			"a posture that would otherwise be refused off loopback is in "+
				"force: this deployment is reached at "+a.ExternalBase()+
				" and `accept_insecure` is what lets it stand. Remove it once "+
				"it is reached over TLS with a second factor required"))
	}

	// AN `http://` EXTERNAL URL ON A ROUTABLE HOST means the session
	// cookie cannot carry `Secure`, so every credential this deployment
	// issues travels in the clear and no `__Host-` prefix protects it.
	//
	// A WARNING RATHER THAN A REFUSAL because it is what a tunnel, a
	// staging box and an internal network genuinely look like, and because
	// the one posture it would be a refusal for — a password backend with
	// an optional second factor — IS refused, by the rule beside it.
	if strings.HasPrefix(a.ExternalBase(), "http://") && !a.externalLoopback() {
		out = append(out, advisory(field("api.external_url"),
			"a browser reaches this deployment over plain http, so the session "+
				"cookie cannot be marked Secure and every credential it "+
				"carries travels in the clear. Terminate TLS in front of this "+
				"node and name the https address here"))
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
			"this deployment is reached over https, which something in front "+
				"of it terminates, and no proxy is trusted — so every caller's "+
				"source is that front end's address. The sign-in throttle then "+
				"keys every caller's attempts at one login together, so a "+
				"stranger guessing at somebody's login slows that person's own "+
				"sign-in, and every audit row names the front end. Name the "+
				"proxy's own address block here; leave it empty only where the "+
				"front end passes each client's own address through as the peer"))
	}

	return out
}
