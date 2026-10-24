package lint

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// RefKind is what a reference loads, which decides the hosts it may come from
// and how a copied file is served.
type RefKind string

const (
	// RefScript is a classic script element's source.
	RefScript RefKind = "script"

	// RefModule is a module script element's source: copied in only when it
	// imports nothing, since its imports would resolve against the artifact
	// origin.
	RefModule RefKind = "module"

	// RefStylesheet is a linked stylesheet or a stylesheet's @import.
	RefStylesheet RefKind = "stylesheet"

	// RefFont is a font a stylesheet names.
	RefFont RefKind = "font"

	// RefAsset is any other file a stylesheet names: an image, a cursor.
	RefAsset RefKind = "asset"
)

// VersionRule is how a host's paths state an exact version.
type VersionRule string

const (
	// VersionNPM is a package CDN path, `<name>@<version>/<file>` after the
	// host's prefix, `<name>` possibly scoped: the version must be an exact
	// semantic version, never a range, a tag or absent, since npm keeps an
	// exact version's files for ever and a range moves.
	VersionNPM VersionRule = "npm"

	// VersionCDNJS is cdnjs's `/ajax/libs/<name>/<version>/<file>`: every
	// version segment there names one release, which is exact.
	VersionCDNJS VersionRule = "cdnjs"

	// VersionInPath is a host that spells the version as a path segment or
	// in the file's name (`jquery-3.7.1.min.js`, `/ui/1.13.2/...`, `/3.4.17`).
	VersionInPath VersionRule = "in_path"

	// VersionNone is a host whose files carry no version by nature (web
	// fonts and their stylesheets), copied as they stand at publish.
	VersionNone VersionRule = "none"
)

// Host is one CDN host a page may load from.
type Host struct {
	// Name is the host, lower case, no port.
	Name string

	// Prefix is the path every reference to it must start with, empty for
	// any path.
	Prefix string

	// Kinds are what may be loaded from it.
	Kinds []RefKind

	// Version is how its paths state an exact version.
	Version VersionRule
}

// Allowlist is the CDN hosts a page may reference, each copied into the
// artifact's own version at publish (internal/artifactupload/vendor).
type Allowlist struct {
	Hosts []Host
}

// DefaultAllowlist is the hosts the artifact authoring contract names: four
// script CDNs, the jQuery CDN and Tailwind's, and Google Fonts for stylesheets
// and the fonts they name.
//
// WHY THESE: they are the set a page an author or an agent writes for any
// artifact host actually reaches for, so a page that loads elsewhere loads
// here. Every file is copied in at publish, so allowing a host costs nothing
// at view: no viewer's browser ever reaches it.
func DefaultAllowlist() Allowlist {
	cdn := []RefKind{RefScript, RefModule, RefStylesheet, RefFont, RefAsset}
	return Allowlist{Hosts: []Host{
		{Name: "cdnjs.cloudflare.com", Prefix: "/ajax/libs/", Kinds: cdn, Version: VersionCDNJS},
		{Name: "cdn.jsdelivr.net", Prefix: "/npm/", Kinds: cdn, Version: VersionNPM},
		{Name: "unpkg.com", Prefix: "/", Kinds: cdn, Version: VersionNPM},
		{Name: "code.jquery.com", Kinds: []RefKind{RefScript, RefStylesheet, RefAsset}, Version: VersionInPath},
		{Name: "cdn.tailwindcss.com", Kinds: []RefKind{RefScript}, Version: VersionInPath},
		{Name: "fonts.googleapis.com", Kinds: []RefKind{RefStylesheet}, Version: VersionNone},
		{Name: "fonts.gstatic.com", Kinds: []RefKind{RefFont}, Version: VersionNone},
	}}
}

// Names are the allowlisted hosts, the set a fetcher holds every request to.
func (a Allowlist) Names() []string {
	out := make([]string, len(a.Hosts))
	for i, h := range a.Hosts {
		out[i] = h.Name
	}
	return out
}

// ErrNotAllowed reports a reference the allowlist refuses: wrapped by every
// [Allowlist.Check] refusal, whose text names what to change.
var ErrNotAllowed = errors.New("lint: the reference is not one a page may load")

// RefusedRef is a reference [Allowlist.Check] refused, with the finding code
// a linter reports it under.
type RefusedRef struct {
	Code   Code
	Reason string
}

func (e *RefusedRef) Error() string { return e.Reason }

// Unwrap makes every refusal an [ErrNotAllowed].
func (e *RefusedRef) Unwrap() error { return ErrNotAllowed }

// Check refuses a reference of kind k to u that the allowlist does not admit:
// not https, a host it does not list, a kind that host does not serve, a path
// outside its prefix, or a package path without an exact version.
func (a Allowlist) Check(u *url.URL, k RefKind) error {
	code := CodeRemoteScript
	switch k {
	case RefStylesheet:
		code = CodeRemoteStylesheet
	case RefFont, RefAsset:
		code = CodeRemoteBlocked
	}
	switch {
	case u.Scheme != "https":
		return &RefusedRef{Code: code, Reason: fmt.Sprintf("%s is not https: a page loads "+
			"only https files from an allowlisted CDN, copied in at publish", u.Redacted())}
	case u.User != nil || u.Port() != "":
		return &RefusedRef{Code: code, Reason: fmt.Sprintf("%s names a user or a port, "+
			"which no allowlisted CDN reference does", u.Redacted())}
	}
	host := strings.ToLower(u.Hostname())
	i := slices.IndexFunc(a.Hosts, func(h Host) bool { return h.Name == host })
	if i < 0 {
		return &RefusedRef{Code: code, Reason: fmt.Sprintf("%s is not an allowlisted CDN "+
			"(%s); upload the file into the artifact instead", host,
			strings.Join(a.Names(), ", "))}
	}
	h := a.Hosts[i]
	if !slices.Contains(h.Kinds, k) {
		return &RefusedRef{Code: code, Reason: fmt.Sprintf("%s serves no %s files to a "+
			"page; upload the file into the artifact instead", host, k)}
	}
	if h.Prefix != "" && !strings.HasPrefix(u.EscapedPath(), h.Prefix) {
		return &RefusedRef{Code: code, Reason: fmt.Sprintf("%s is allowlisted only under "+
			"%s, and %s is not", host, h.Prefix, u.EscapedPath())}
	}
	if err := h.exact(u.EscapedPath()); err != nil {
		return &RefusedRef{Code: CodeUnpinned, Reason: fmt.Sprintf("%s: %v", u.Redacted(), err)}
	}
	return nil
}

// exactSemver is one exact semantic version: never a range, a tag or a
// partial version.
var exactSemver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// cdnjsVersion is a cdnjs version segment, which always names one release.
var cdnjsVersion = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

// versionToken is a version spelled in a path segment or a file name.
var versionToken = regexp.MustCompile(`(^|[^0-9A-Za-z])v?\d+\.\d+(\.\d+)?([^0-9]|$)`)

// exact refuses a path this host's rule does not pin to one release.
func (h Host) exact(p string) error {
	rest := strings.TrimPrefix(p, h.Prefix)
	if h.Prefix == "/" || h.Prefix == "" {
		rest = strings.TrimPrefix(p, "/")
	}
	segments := strings.Split(rest, "/")
	switch h.Version {
	case VersionNone:
		return nil
	case VersionNPM:
		pkg := segments[0]
		name := pkg
		if strings.HasPrefix(pkg, "@") {
			if len(segments) < 2 {
				return errors.New("a scoped package names no package")
			}
			pkg = segments[1]
			name = segments[0] + "/" + pkg
		}
		at := strings.LastIndex(pkg, "@")
		if at <= 0 {
			return fmt.Errorf("the package %s names no version; pin an exact one "+
				"(%s@1.2.3)", name, name)
		}
		if v := pkg[at+1:]; !exactSemver.MatchString(v) {
			return fmt.Errorf("the package version %q is a range, a tag or partial; pin "+
				"an exact one (1.2.3)", v)
		}
		return nil
	case VersionCDNJS:
		if len(segments) < 3 || !cdnjsVersion.MatchString(segments[1]) {
			return errors.New("a cdnjs path names /ajax/libs/<name>/<version>/<file>")
		}
		return nil
	case VersionInPath:
		for _, s := range segments {
			if versionToken.MatchString(s) {
				return nil
			}
		}
		return errors.New("the path names no exact version; pin one (for example " +
			"https://cdn.tailwindcss.com/3.4.17 or https://code.jquery.com/jquery-3.7.1.min.js)")
	}
	return fmt.Errorf("the host's version rule %q is not one this build knows", h.Version)
}

// IsRemote reports whether a URL as written in a page reaches another origin:
// it names a scheme, or it is protocol-relative. Read as a browser reads it,
// with surrounding control characters and spaces trimmed, tabs and newlines
// removed and a backslash taken for a slash, so `/\evil.example/x.js` is the
// protocol-relative reference a browser resolves it to.
func IsRemote(ref string) bool {
	n := normalizeRef(ref)
	if strings.HasPrefix(n, "//") {
		return true
	}
	return scheme(n) != ""
}

// normalizeRef is a reference as a URL parser reads it before resolving it.
func normalizeRef(ref string) string {
	ref = strings.TrimFunc(ref, func(r rune) bool { return r <= ' ' })
	ref = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return -1
		case '\\':
			return '/'
		}
		return r
	}, ref)
	return ref
}

// scheme is the reference's scheme, lower case, empty for a relative one.
func scheme(n string) string {
	for i, r := range n {
		switch {
		case r == ':':
			if i == 0 {
				return ""
			}
			return strings.ToLower(n[:i])
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return ""
		}
	}
	return ""
}

// absolute parses a remote reference into the URL it names: a
// protocol-relative one is taken as https, which is all an artifact origin
// served over https would resolve it to and the only scheme the allowlist
// admits.
func absolute(ref string) (*url.URL, error) {
	n := normalizeRef(ref)
	if strings.HasPrefix(n, "//") {
		n = "https:" + n
	}
	u, err := url.Parse(n)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("%q names no host", ref)
	}
	return u, nil
}
