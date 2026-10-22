package artifactserve

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ORIGIN, as one value both halves read: the frame the API mints names a
// host, a mode and a sandbox, and the handler that serves the frame checks the
// host and repeats the sandbox in its policy. One value, so the frame a viewer
// is handed and the document it loads can never disagree about where it runs
// or what it may do.

// Placeholder is what an external URL template names an artifact's host label
// with: config's own spelling.
const Placeholder = config.ArtifactPlaceholder

// ErrOrigin reports an origin configuration this handler cannot serve under,
// naming the value. Tier A refuses every such shape first; this is the second
// check, for a value that reached here some other way.
var ErrOrigin = errors.New("artifactserve: the artifact origin is misconfigured")

// OriginsConfig is the origin as Tier A states it.
type OriginsConfig struct {
	// External is `api.artifacts.external_url`: a template naming
	// [Placeholder] at the start of its leftmost label, or a plain origin.
	External string

	// Mode is the deployment's origin mode as config decides it
	// (config.Bootstrap.ArtifactOriginMode): per artifact only where the
	// template names a host on a site that is not the dashboard's.
	Mode artifacts.OriginMode

	// Dashboard are the origins that may frame an artifact: the API's
	// external origin and every origin the API admits
	// (`api.auth.allowed_origins`).
	Dashboard []string
}

// OriginsConfigOf is the origin as a node's Tier A states it: the external URL,
// the mode config decides for it (the site rule, which is config's alone), and
// the dashboard origins that may frame a document, the API's external origin
// first. The ONE reading, so the frames the API mints and the documents this
// handler serves are built from one answer.
func OriginsConfigOf(b *config.Bootstrap) OriginsConfig {
	dashboard := []string{b.API.ExternalBase()}
	dashboard = append(dashboard, b.API.Auth.AllowedOrigins...)
	return OriginsConfig{
		External:  b.API.Artifacts.ExternalBase(),
		Mode:      b.ArtifactOriginMode(),
		Dashboard: dashboard,
	}
}

// Origins is the artifact origin's shape.
type Origins struct {
	mode      artifacts.OriginMode
	scheme    string
	port      string
	templated bool

	// host is the shared origin's host, or the template's host after the
	// placeholder (`.artifacts.example.net`, `--acme.example.net`).
	host string

	ancestors []string
}

// NewOrigins reads the origin from Tier A's values.
func NewOrigins(c OriginsConfig) (Origins, error) {
	if !c.Mode.Valid() {
		return Origins{}, fmt.Errorf("%w: origin mode %q is not one of %s", ErrOrigin,
			c.Mode, words.Or(artifacts.OriginModes))
	}
	external := strings.TrimRight(strings.TrimSpace(c.External), "/")
	templated := strings.Contains(external, Placeholder)
	parsed, err := url.Parse(strings.Replace(external, Placeholder,
		strings.Repeat("a", artifacts.LabelLen), 1))
	switch {
	case external == "":
		return Origins{}, fmt.Errorf("%w: api.artifacts.external_url is unset, so no "+
			"frame can name where it loads from", ErrOrigin)
	case err != nil:
		return Origins{}, fmt.Errorf("%w: api.artifacts.external_url %q: %w", ErrOrigin,
			external, err)
	case parsed.Scheme != "https" && parsed.Scheme != "http":
		return Origins{}, fmt.Errorf("%w: api.artifacts.external_url %q is not an http "+
			"or https URL", ErrOrigin, external)
	case parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil:
		return Origins{}, fmt.Errorf("%w: api.artifacts.external_url %q is not an origin",
			ErrOrigin, external)
	case strings.Count(external, Placeholder) > 1:
		return Origins{}, fmt.Errorf("%w: api.artifacts.external_url %q names %s twice",
			ErrOrigin, external, Placeholder)
	case c.Mode == artifacts.OriginPerArtifact && !templated:
		return Origins{}, fmt.Errorf("%w: a per-artifact origin needs a template naming "+
			"%s in api.artifacts.external_url", ErrOrigin, Placeholder)
	}
	o := Origins{mode: c.Mode, scheme: parsed.Scheme, port: parsed.Port(), templated: templated}
	host := strings.ToLower(parsed.Hostname())
	if templated {
		rest, ok := strings.CutPrefix(strings.ToLower(hostOf(external)), Placeholder)
		if !ok || rest == "" || !strings.Contains(rest, ".") {
			return Origins{}, fmt.Errorf("%w: api.artifacts.external_url %q must name %s at "+
				"the start of its leftmost label, under a domain", ErrOrigin, external, Placeholder)
		}
		host = rest
	}
	o.host = host
	for _, a := range c.Dashboard {
		normalised, err := originOf(a)
		if err != nil {
			return Origins{}, fmt.Errorf("%w: dashboard origin %q: %w", ErrOrigin, a, err)
		}
		if !slices.Contains(o.ancestors, normalised) {
			o.ancestors = append(o.ancestors, normalised)
		}
	}
	if len(o.ancestors) == 0 {
		return Origins{}, fmt.Errorf("%w: no dashboard origin may frame an artifact: set "+
			"api.external_url", ErrOrigin)
	}
	return o, nil
}

// hostOf is the authority's host part of a URL whose host may still carry the
// placeholder (which url.Parse would refuse as a host), lower case.
func hostOf(raw string) string {
	_, rest, _ := strings.Cut(raw, "://")
	authority, _, _ := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return authority
}

// originOf is a URL's origin, scheme://host[:port], lower case.
func originOf(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", errors.New("not an http or https origin")
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// Mode is the origin mode every document is served under.
func (o Origins) Mode() artifacts.OriginMode { return o.mode }

// Secure reports an https artifact origin, which sends HSTS.
func (o Origins) Secure() bool { return o.scheme == "https" }

// Host is the host an artifact is served on, lower case and without a port.
func (o Origins) Host(id artifacts.ID) string {
	if o.templated {
		return id.Label().String() + o.host
	}
	return o.host
}

// Origin is the origin an artifact is served on, as a browser names it.
func (o Origins) Origin(id artifacts.ID) string {
	host := o.Host(id)
	if o.port != "" {
		host = net.JoinHostPort(host, o.port)
	}
	return o.scheme + "://" + host
}

// Serves reports whether a request's Host header names the host artifact id is
// served on.
//
// THE WHOLE HOST, not just the leftmost label: a label alone would let a host
// on another domain, pointed at this listener, serve an artifact under an
// origin that is not the deployment's. In per-artifact mode it is also what
// keeps a ticket for one artifact from opening on another artifact's host,
// whose browser storage would then be in reach. The port is not compared: a
// proxy in front of the listener is free to forward the host with or without
// the default port.
func (o Origins) Serves(hostHeader string, id artifacts.ID) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	return strings.EqualFold(strings.TrimSuffix(host, "."), o.Host(id))
}

// Sandbox is the frame's sandbox tokens, in the order the frame sets them: the
// page runs its scripts and its forms fire their submit events, and in
// per-artifact mode it keeps its real origin (and so its browser storage).
//
// NEVER A POPUP, AN ESCAPE, POINTER LOCK, A MODAL OR TOP NAVIGATION. External
// links go through the bridge, where the dashboard names the destination
// before it opens it; a page that could open a window could leak what it read
// with a click nobody vetted. Downloads arrive with the downloads capability,
// which this release does not serve, so no frame is handed allow-downloads.
func (o Origins) Sandbox() []string {
	tokens := []string{"allow-scripts", "allow-forms"}
	if o.mode == artifacts.OriginPerArtifact {
		tokens = append(tokens, "allow-same-origin")
	}
	return tokens
}

// Policy is the header facts an artifact's responses are built from.
func (o Origins) Policy(id artifacts.ID) pagepolicy.ArtifactOrigin {
	return pagepolicy.ArtifactOrigin{
		Origin:    o.Origin(id),
		Ancestors: slices.Clone(o.ancestors),
		Sandbox:   o.Sandbox(),
		Shared:    o.mode == artifacts.OriginShared,
		Secure:    o.Secure(),
	}
}

// refusalPolicy is the header facts a refusal is built from, which names no
// artifact: only the mode and the scheme reach its headers.
func (o Origins) refusalPolicy() pagepolicy.ArtifactOrigin {
	return pagepolicy.ArtifactOrigin{Shared: o.mode == artifacts.OriginShared, Secure: o.Secure()}
}

// FrameSource is the source expression the dashboard's frame-src names for an
// artifact origin given as `api.artifacts.external_url`: the origin itself, or
// for a template a wildcard over the hosts it names (the leftmost label
// replaced, which is the only position a CSP wildcard may hold). Empty for an
// empty URL, which is a node that serves no artifacts.
func FrameSource(external string) (string, error) {
	external = strings.TrimRight(strings.TrimSpace(external), "/")
	if external == "" {
		return "", nil
	}
	if !strings.Contains(external, Placeholder) {
		return originOf(external)
	}
	scheme, _, _ := strings.Cut(external, "://")
	parsed, err := url.Parse(strings.Replace(external, Placeholder,
		strings.Repeat("a", artifacts.LabelLen), 1))
	if err != nil {
		return "", fmt.Errorf("%w: api.artifacts.external_url %q: %w", ErrOrigin, external, err)
	}
	_, domain, ok := strings.Cut(strings.ToLower(parsed.Hostname()), ".")
	if !ok || domain == "" || (scheme != "https" && scheme != "http") {
		return "", fmt.Errorf("%w: api.artifacts.external_url %q names no domain under its "+
			"artifact label", ErrOrigin, external)
	}
	host := "*." + domain
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return strings.ToLower(scheme) + "://" + host, nil
}
