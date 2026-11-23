package harnessprofile

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// RouteEnv is every variable of an entry's env this profile declares says
// where a call goes, none of it a credential, sorted: its identity_env, and
// the switches among its sign_in_env (a gateway's token there is a
// credential, and left out). What an entry's identity as a session's entry is
// built from (internal/providers/llm/harness's Provider.EntryID).
func (p *Profile) RouteEnv() []string {
	names := slices.Clone(p.IdentityEnv)
	for _, name := range p.SignInEnv {
		if !p.signsIn(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// ErrNotRoute is a route variable's value that is no route ([Profile.RouteValue]).
var ErrNotRoute = errors.New("the value is no route: give an address that parses as a URL " +
	"(scheme://host[:port][/path]), a plain name (letters, digits, '.', '_', '-', '~', '+'), or a " +
	"file's path beginning with '/' or '~'")

// ErrRouteHoldsCredential is an address one of whose path segments begins as a
// credential does ([credentialPrefixes]): a key belongs in the entry's
// credential, and an entry's identity never takes one.
var ErrRouteHoldsCredential = errors.New("a segment of the address's path begins as a credential " +
	"does: put the key in the entry's credential, never in its address")

// plainName is a route variable's value that is no address: a region, a
// project, a host, a switch, a provider type or a file's path.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._/~+-]+$`)

// credentialPrefixes is how the registered runners' credentials begin where no
// profile declares it (token_prefix, api_key_prefix): Anthropic's, OpenAI's and
// most OpenAI-compatible vendors' keys, xAI's, GitHub's tokens and Google's API
// keys. One list here rather than per profile, since a gateway's path can hold
// any vendor's key. A prefix, never an entropy or length test: a gateway's
// account id is 32 hex characters, and a route.
var credentialPrefixes = []string{"sk-", "xai-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "AIza"}

// defaultPorts is the port each scheme reaches unnamed.
var defaultPorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443"}

// RouteValue is value as a route, an entry's identity is built from. An address
// is cleaned of all that cannot change where a request goes: its userinfo,
// query and fragment are dropped, its scheme and host lower-cased, a default
// port dropped, dot segments resolved and one trailing slash dropped, so
// /v1 and /v1/ are one route and /v1/a and /v1/b two (a gateway routed by path).
// A plain name is lower-cased; a file's path is kept as it is. An address with
// a path segment shaped like a credential is [ErrRouteHoldsCredential], and
// anything else [ErrNotRoute]: neither error holds the value.
func (p *Profile) RouteValue(value string) (string, error) {
	if !strings.Contains(value, "://") {
		switch {
		case !plainName.MatchString(value):
			return "", ErrNotRoute
		case strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~"):
			return value, nil
		case strings.Contains(value, "/"):
			return "", ErrNotRoute
		}
		return strings.ToLower(value), nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", ErrNotRoute
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if p.credentialShaped(segment) {
			return "", ErrRouteHoldsCredential
		}
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != defaultPorts[scheme] {
		host += ":" + port
	}
	// ResolveReference resolves an absolute reference's dot segments
	// (RFC 3986, 5.2.4) and nothing else of its path.
	path := (&url.URL{}).ResolveReference(&url.URL{Scheme: scheme, Host: host, Path: u.Path,
		RawPath: u.RawPath}).EscapedPath()
	return scheme + "://" + host + strings.TrimSuffix(path, "/"), nil
}

// credentialShaped reports whether segment begins as a credential does: one of
// [credentialPrefixes], or this profile's own token or key prefix.
func (p *Profile) credentialShaped(segment string) bool {
	for _, prefix := range slices.Concat(credentialPrefixes, []string{p.TokenPrefix, p.APIKeyPrefix}) {
		if prefix != "" && strings.HasPrefix(segment, prefix) {
			return true
		}
	}
	return false
}
