package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// ---- api.artifacts --------------------------------------------------------- //

// ArtifactPlaceholder is what an api.artifacts.external_url template names an
// artifact's host label with. Present, the deployment gives every artifact a
// host of its own; absent, every artifact is served from the one origin.
const ArtifactPlaceholder = "{artifact}"

// APIArtifacts is a THIRD listener, beside api.port and api.public, that serves
// artifact version bytes and nothing else: no guard, session, cookie or API
// route: an artifact is arbitrary script that must never run where the
// dashboard's session runs. Never a Host predicate on api.port: a client-set
// header would guard the one surface where a mistake is an account takeover.
//
// An external URL whose leftmost label starts with [ArtifactPlaceholder]
// (`https://{artifact}.artifacts.example.net`) gives each artifact its own
// origin (wildcard DNS and certificate). A plain origin serves every document
// with an OPAQUE origin via the policy's sandbox directive (one name or port;
// no browser storage). A real origin only off the dashboard's site
// ([Bootstrap.ArtifactOriginMode]).
type APIArtifacts struct {
	// Host is the bind address. Empty binds every interface.
	Host string `yaml:"host,omitempty" json:"host,omitempty" desc:"Bind address for the artifact listener. Empty binds every interface."`

	// Port is the bind port. 0 (the default) serves no artifacts on this
	// node: publishing still works, and the dashboard says which key to set
	// to view them. Bound on ingress nodes only.
	Port int `yaml:"port,omitempty" json:"port,omitempty" js:"min=0;max=65535" desc:"Bind port for the artifact listener, which serves artifact bytes and nothing else; 0 serves none on this node. Ingress nodes only."`

	// ExternalURL is where a browser reaches the artifact origin: a template
	// naming {artifact} at the start of its leftmost label for an origin per
	// artifact, or a plain origin for one shared, opaque origin. REQUIRED
	// once the listener is set on an ingress node.
	ExternalURL string `yaml:"external_url,omitempty" json:"external_url,omitempty" js:"maxlen=2048" desc:"Where browsers reach artifacts: https://{artifact}.artifacts.example.net for an origin per artifact (wildcard DNS and TLS), or a plain origin such as https://artifacts.example.com for one opaque origin. Required once port is set on an ingress node. At most 2048 characters."`
}

// IsZero lets an unset block drop out of a JSON round trip.
func (a APIArtifacts) IsZero() bool { return a.Host == "" && a.Port == 0 && a.ExternalURL == "" }

// Enabled reports whether this node binds the artifact listener at all; a node
// without the ingress role binds none whatever this says.
func (a APIArtifacts) Enabled() bool { return a.Port != 0 }

// Addr is the listener's bind address, host and port.
func (a APIArtifacts) Addr() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// ExternalBase is [APIArtifacts.ExternalURL] trimmed of space and its trailing
// slash: the template or origin every frame URL is built on.
func (a APIArtifacts) ExternalBase() string {
	return strings.TrimRight(strings.TrimSpace(a.ExternalURL), "/")
}

// PerArtifact reports whether the external URL names [ArtifactPlaceholder], so
// every artifact is reached on a host of its own.
func (a APIArtifacts) PerArtifact() bool {
	return strings.Contains(a.ExternalBase(), ArtifactPlaceholder)
}

// sampleLabel stands in for a real label wherever a template must be read as
// a URL: the same length and alphabet as every [artifacts.Label].
var sampleLabel = strings.Repeat("a", artifacts.LabelLen)

// rendered is the external URL with the placeholder filled, so it parses as
// an address.
func (a APIArtifacts) rendered() string {
	return strings.Replace(a.ExternalBase(), ArtifactPlaceholder, sampleLabel, 1)
}

// apex is the host every artifact host shares: the rendered host itself for a
// shared origin, and everything after the leftmost label for a template.
func (a APIArtifacts) apex() string {
	parsed, err := url.Parse(a.rendered())
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if !a.PerArtifact() {
		return host
	}
	if _, rest, ok := strings.Cut(host, "."); ok {
		return rest
	}
	return ""
}

func (a *APIArtifacts) validate(path Path, api *API, ingress bool) error {
	var p problems
	if a.Port < 0 || a.Port > 65535 {
		p.add(at(path, "port"), ErrOutOfRange,
			"must be 0 (no artifact listener) or a port 1..65535, got %d", a.Port)
	}
	if a.Port == 0 && (a.Host != "" || strings.TrimSpace(a.ExternalURL) != "") {
		p.add(at(path, "port"), ErrMissing,
			"is required when host or external_url is set. Without a port, "+
				"the artifact listener doesn't open")
	}
	raw := a.ExternalBase()
	if raw == "" {
		if a.Enabled() && ingress {
			p.add(at(path, "external_url"), ErrMissing,
				"required when the artifact listener is set. Set it to the "+
					"address browsers reach artifacts at, e.g. "+
					"https://{artifact}.artifacts.example.net or "+
					"https://artifacts.example.com")
		}
		return p.err()
	}
	urlPath := at(path, "external_url")
	if err := checkAddressLength(urlPath, strings.TrimSpace(a.ExternalURL)); err != nil {
		p.wrap(err)
		return p.err()
	}
	if strings.Count(raw, ArtifactPlaceholder) > 1 {
		p.add(urlPath, ErrShape, "%q names %s more than once: an artifact's "+
			"host carries its label once, at the start of the leftmost label",
			raw, ArtifactPlaceholder)
		return p.err()
	}
	if a.PerArtifact() {
		p.wrap(checkArtifactTemplate(urlPath, raw))
	}
	rendered := a.rendered()
	p.wrap(checkExternalURL(urlPath, rendered,
		"frames are loaded from it, and a browser refuses an address without a scheme",
		"browsers use to reach artifacts"))
	if p.err() != nil {
		return p.err()
	}
	parsed, err := url.Parse(rendered)
	if err != nil {
		return p.err()
	}
	if strings.Trim(parsed.Path, "/") != "" {
		p.add(urlPath, ErrShape, "%q has a path: the artifact origin serves "+
			"its routes from the root of the host, so the address is only "+
			"scheme://host[:port]", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && !loopbackHost(host) {
		p.add(urlPath, ErrShape, "%q uses plain http on a routable host: an "+
			"artifact's frame and every byte it loads would travel in the "+
			"clear. Use https, or a loopback address for local development", raw)
	}
	p.wrap(a.checkCollisions(urlPath, raw, parsed, api))
	return p.err()
}

// checkArtifactTemplate is the placeholder's one legal position: the start of
// the leftmost label, followed only by label characters, the whole label
// fitting the 63 bytes a DNS label may hold.
func checkArtifactTemplate(path Path, raw string) error {
	var p problems
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || (scheme != "https" && scheme != "http") {
		p.add(path, ErrShape, "%q must start with https:// or http://", raw)
		return p.err()
	}
	if !strings.HasPrefix(rest, ArtifactPlaceholder) {
		p.add(path, ErrShape, "%q must begin its host with %s: the artifact's "+
			"label is the start of the leftmost label, e.g. "+
			"https://{artifact}.artifacts.example.net", raw, ArtifactPlaceholder)
		return p.err()
	}
	label := strings.TrimPrefix(rest, ArtifactPlaceholder)
	if i := strings.IndexAny(label, ".:/"); i >= 0 {
		if label[i] != '.' {
			p.add(path, ErrShape, "%q puts %s in the whole host: an artifact host "+
				"needs a parent domain, e.g. https://{artifact}.artifacts.example.net",
				raw, ArtifactPlaceholder)
			return p.err()
		}
		label = label[:i]
	} else {
		p.add(path, ErrShape, "%q has no parent domain after the label", raw)
		return p.err()
	}
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			p.add(path, ErrShape, "%q follows %s with %q, and a host label "+
				"holds only lowercase letters, digits and hyphens", raw,
				ArtifactPlaceholder, label)
			return p.err()
		}
	}
	if n := artifacts.LabelLen + len(label); n > 63 {
		p.add(path, ErrShape, "%q makes every artifact's label %d bytes long, "+
			"over the 63 a DNS label may hold: shorten %q", raw, n, label)
	}
	return p.err()
}

// checkCollisions refuses an artifact origin the API would treat as its own:
// one the dashboard or the public listener is served on, one the browser
// origin allow-list admits (it would be inside the cross-site write rule),
// or a parent of the API's host.
func (a *APIArtifacts) checkCollisions(path Path, raw string, parsed *url.URL, api *API) error {
	var p problems
	origin := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	apex := a.apex()
	for _, other := range []struct{ name, value string }{
		{"api.external_url", api.ExternalBase()},
		{"api.public.external_url", api.Public.ExternalBase()},
	} {
		theirs, err := url.Parse(other.value)
		if other.value == "" || err != nil || theirs.Hostname() == "" {
			continue
		}
		host := strings.ToLower(theirs.Hostname())
		switch {
		case !a.PerArtifact() && strings.EqualFold(theirs.Scheme+"://"+theirs.Host, origin):
			p.add(path, ErrConflict, "%q is the origin %s is served on: an "+
				"artifact must never run where the API's session runs. Give "+
				"artifacts a host or port of their own", raw, other.name)
		case a.PerArtifact() && artifactHostShape(host, raw, apex):
			p.add(path, ErrConflict, "%q would give an artifact the host %s is "+
				"served on. Give artifacts a domain of their own", raw, other.name)
		case host != apex && strings.HasSuffix(host, "."+apex) && !a.PerArtifact():
			p.add(path, ErrConflict, "%q is a parent domain of %s, so a cookie "+
				"scoped to it would reach the API. Give artifacts a host that "+
				"is not a parent of the API's", raw, other.name)
		}
	}
	if !a.PerArtifact() {
		for i, allowed := range api.Auth.AllowedOrigins {
			if strings.EqualFold(strings.TrimRight(strings.TrimSpace(allowed), "/"), origin) {
				p.add(path, ErrConflict, "%q is api.auth.allowed_origins[%d], so "+
					"the API would admit writes and sockets from artifact pages. "+
					"Remove it from the allow-list or give artifacts another "+
					"origin", raw, i)
			}
		}
	}
	return p.err()
}

// artifactHostShape reports whether host is one a template would give some
// artifact: directly under the apex, with a leftmost label of a label's length
// followed by the template's own suffix.
func artifactHostShape(host, template, apex string) bool {
	first, rest, ok := strings.Cut(host, ".")
	if !ok || rest != apex {
		return false
	}
	_, after, _ := strings.Cut(template, ArtifactPlaceholder)
	suffix, _, _ := strings.Cut(after, ".")
	return len(first) == artifacts.LabelLen+len(suffix) && strings.HasSuffix(first, suffix)
}

// loopbackHost reports whether a browser reaches host on this machine: a
// loopback address, localhost, or a name under localhost.
func loopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// siteOf is a host's registrable domain, or the host itself where none can be
// derived (an address, localhost): two such hosts are one site only when they
// are equal, which errs toward the opaque origin.
func siteOf(host string) string {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if loopbackHost(host) {
		return "localhost"
	}
	if site, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return site
	}
	return host
}

// ArtifactOriginMode is how this deployment isolates one artifact from
// another and from the dashboard, and the ONE answer to it: the frame minting
// on the API side and the serving origin both read it here.
//
// A real origin per artifact needs a template AND a site of its own: an
// artifact host on the dashboard's registrable domain is same-site to it, so
// a fetch the artifact starts would carry the dashboard's SameSite=Lax cookie.
// There every document is served opaque whatever the template says, and
// `crewlet validate` warns.
func (b *Bootstrap) ArtifactOriginMode() artifacts.OriginMode {
	a := b.API.Artifacts
	if !a.PerArtifact() || a.sameSite(b.API) {
		return artifacts.OriginShared
	}
	return artifacts.OriginPerArtifact
}

// sameSite reports whether the artifact origin is on the dashboard's site.
func (a APIArtifacts) sameSite(api API) bool {
	theirs, err := url.Parse(api.ExternalBase())
	if err != nil || theirs.Hostname() == "" {
		return true
	}
	return siteOf(a.apex()) == siteOf(theirs.Hostname())
}

// warnings are what is valid and worth knowing about the artifact origin.
func (a APIArtifacts) warnings(api API, serving bool) []Warning {
	if !serving || a.ExternalBase() == "" || !a.sameSite(api) {
		return nil
	}
	return []Warning{advisory(field("api.artifacts.external_url"),
		"artifacts are on the same site as the dashboard, so every artifact is "+
			"served with an opaque origin and browser storage is unavailable to "+
			"pages. A separate registrable domain gives each artifact an origin "+
			"of its own")}
}

// ---- artifacts (Tier A) ------------------------------------------------------ //

// Artifacts is the Tier A artifacts block: the plan quotas this node enforces,
// and the browser it renders pages in.
//
// TIER A, because a quota is a promise made to whoever runs the deployment: a
// tenant administrator must not be able to raise their own, and a managed
// company document is not necessarily managed by the platform. A hosted
// deployment renders these from the plan.
type Artifacts struct {
	Limits ArtifactLimits `yaml:"limits,omitempty" json:"limits,omitzero"`
	Render ArtifactRender `yaml:"render,omitempty" json:"render,omitzero"`
}

// IsZero lets an unset block drop out of a JSON round trip.
func (a Artifacts) IsZero() bool { return a.Limits == ArtifactLimits{} && a.Render == ArtifactRender{} }

// ArtifactRender is the browser this node renders artifact pages in for a
// seat's render_artifact. TIER A, because it is an address on this node's
// network, which the operator runs and a company document cannot name.
type ArtifactRender struct {
	// BrowserURL is a headless Chromium's DevTools endpoint. Unset, this
	// node renders nothing and the tool says which key to set.
	BrowserURL string `yaml:"browser_url,omitempty" json:"browser_url,omitempty" desc:"A headless Chromium's DevTools endpoint this node renders artifact pages in, for a seat checking its own page before review: http://host:port (its /json/version is read) or the ws:// address it prints. It needs no network of its own: the engine answers every request a render makes. Unset renders nothing."`
}

// IsZero lets an unset block drop out of a JSON round trip.
func (r ArtifactRender) IsZero() bool { return r.BrowserURL == "" }

// ArtifactLimits are the plan quotas. Zero is unset and takes the documented
// default; a value outside its bounds is refused naming them.
type ArtifactLimits struct {
	CompanyBytes   int64 `yaml:"company_bytes,omitempty" json:"company_bytes,omitempty" js:"min=1073741824;max=17592186044416" desc:"Bytes every artifact's retained versions and assets may name, each distinct object counted once; default 10 GiB, 1 GiB..16 TiB."`
	Artifacts      int64 `yaml:"artifacts,omitempty" json:"artifacts,omitempty" js:"min=100;max=10000000" desc:"Artifacts the company may hold, trashed ones included until purged; default 50000, 100..10000000."`
	DataBytes      int64 `yaml:"data_bytes,omitempty" json:"data_bytes,omitempty" js:"min=67108864;max=1099511627776" desc:"Datastore footprint every store together may hold on each data node; default 1 GiB, 64 MiB..1 TiB."`
	DataStoreBytes int64 `yaml:"data_store_bytes,omitempty" json:"data_store_bytes,omitempty" js:"min=1048576;max=1073741824" desc:"Datastore footprint one artifact's store may hold; default 32 MiB, 1 MiB..1 GiB."`
	PendingBytes   int64 `yaml:"pending_bytes,omitempty" json:"pending_bytes,omitempty" js:"min=67108864;max=17179869184" desc:"Bytes one principal may hold reserved for uploads not yet published; default 512 MiB, 64 MiB..16 GiB. A company may hold four times it."`
}

// Stamp resolves the configured quotas against their defaults: the value a
// deciding node stamps into a record.
func (a Artifacts) Stamp() artifacts.Limits {
	l := a.Limits
	return artifacts.Limits{
		CompanyBytes:   orInt64(l.CompanyBytes, artifacts.DefaultCompanyBytes),
		Artifacts:      orInt64(l.Artifacts, artifacts.DefaultArtifacts),
		DataBytes:      orInt64(l.DataBytes, artifacts.DefaultDataBytes),
		DataStoreBytes: orInt64(l.DataStoreBytes, artifacts.DefaultDataStoreBytes),
		PendingBytes:   orInt64(l.PendingBytes, artifacts.DefaultPendingBytes),
	}
}

// orInt64 is v, or fallback where v is unset.
func orInt64(v, fallback int64) int64 {
	if v == 0 {
		return fallback
	}
	return v
}

func (a *Artifacts) validate(path Path) error {
	var p problems
	if err := checkRenderBrowser(a.Render.BrowserURL); err != "" {
		p.add(at(at(path, "render"), "browser_url"), ErrShape, "%s", err)
	}
	limits := at(path, "limits")
	l := a.Limits
	bytesInRange(&p, limits, "company_bytes", l.CompanyBytes,
		artifacts.MinCompanyBytes, artifacts.MaxCompanyBytes)
	if v := l.Artifacts; v != 0 && (v < artifacts.MinArtifacts || v > artifacts.MaxArtifacts) {
		p.add(at(limits, "artifacts"), ErrOutOfRange, "%d is outside %d..%d artifacts",
			v, artifacts.MinArtifacts, artifacts.MaxArtifacts)
	}
	bytesInRange(&p, limits, "data_bytes", l.DataBytes,
		artifacts.MinDataBytes, artifacts.MaxDataBytes)
	bytesInRange(&p, limits, "data_store_bytes", l.DataStoreBytes,
		artifacts.MinDataStoreBytes, artifacts.MaxDataStoreBytes)
	bytesInRange(&p, limits, "pending_bytes", l.PendingBytes,
		artifacts.MinPendingBytes, artifacts.MaxPendingBytes)
	if p.err() != nil {
		return p.err()
	}
	// ONE STORE CANNOT HOLD MORE THAN EVERY STORE TOGETHER: a store at its
	// own ceiling would be refused by the company's first, and the refusal
	// would name the company's quota for a sheet nobody else is writing.
	if resolved := a.Stamp(); resolved.DataStoreBytes > resolved.DataBytes {
		p.add(at(limits, "data_store_bytes"), ErrConflict,
			"%d bytes per store is more than data_bytes %d for every store "+
				"together. Lower data_store_bytes or raise data_bytes",
			resolved.DataStoreBytes, resolved.DataBytes)
	}
	return p.err()
}

// ---- artifacts (Tier B) ------------------------------------------------------ //

// CompanyArtifacts is the company's own artifacts policy, which an
// administrator edits live.
type CompanyArtifacts struct {
	// AgentsManageOthers lets an agent publish over, update and reshare an
	// artifact it does not own. OFF by default: an agent edits data and
	// publishes its own artifacts, and managing a colleague's work is a
	// person's gesture, which limits how far a compromised agent reaches.
	AgentsManageOthers bool `yaml:"agents_manage_others,omitempty" json:"agents_manage_others,omitempty" desc:"Let an agent publish over, update and reshare artifacts it does not own. Off by default."`

	// SeenBy is who sees which colleagues have opened an artifact.
	SeenBy SeenBy `yaml:"seen_by,omitempty" json:"seen_by,omitempty" js:"enum=editors|nobody" desc:"Who sees who has opened an artifact: editors (the default) or nobody."`
}

// IsZero lets an unset block drop out of a JSON round trip.
func (a CompanyArtifacts) IsZero() bool { return a == CompanyArtifacts{} }

// SeenBy is who sees an artifact's openers. Viewers always appear in a live
// roster whatever this says; this governs the durable "seen by" list only.
type SeenBy string

const (
	// SeenByEditors shows the openers to the artifact's editors and owner.
	SeenByEditors SeenBy = "editors"

	// SeenByNobody shows them to nobody.
	SeenByNobody SeenBy = "nobody"
)

// SeenByValues are every value this build reads.
var SeenByValues = []SeenBy{SeenByEditors, SeenByNobody}

// Valid reports whether s is a value this build reads. Empty is not a member:
// it is unset, and [CompanyArtifacts.SeenByOrDefault] reads it as editors.
func (s SeenBy) Valid() bool { return slices.Contains(SeenByValues, s) }

// SeenByOrDefault is the policy in force: editors where nothing was written.
func (a CompanyArtifacts) SeenByOrDefault() SeenBy {
	if a.SeenBy == "" {
		return SeenByEditors
	}
	return a.SeenBy
}

func (a *CompanyArtifacts) validate(path Path) error {
	var p problems
	if a.SeenBy != "" && !a.SeenBy.Valid() {
		p.add(at(path, "seen_by"), ErrUnknownValue,
			"%q is not a seen_by policy: write editors or nobody", a.SeenBy)
	}
	return p.err()
}

// checkRenderBrowser is why raw is no DevTools endpoint a render can dial (an
// http(s) origin or a ws(s) socket, with no user information, query or
// fragment), or empty.
func checkRenderBrowser(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Sprintf("%q is not a URL: %v", raw, err)
	case u.Hostname() == "":
		return fmt.Sprintf("%q names no host", raw)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return fmt.Sprintf("%q carries user information, a query or a fragment; "+
			"name the endpoint alone", raw)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Path != "" && u.Path != "/" {
			return fmt.Sprintf("%q has a path; an http endpoint is the browser's "+
				"origin, such as http://chromium:9222", raw)
		}
	case "ws", "wss":
	default:
		return fmt.Sprintf("%q must start with http://, https://, ws:// or wss://", raw)
	}
	return ""
}
