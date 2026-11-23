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
		if !IsCredentialName(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// ErrNotRoute is a route variable's value that is no route ([RouteValue]).
var ErrNotRoute = errors.New("the value is no route: give an address that parses as a URL " +
	"(scheme://host[:port]), or a plain name (letters, digits, '.', '_', '-', '/', '~', '+')")

// plainRoute is a route variable's value that is no address: a region, a
// project, a switch, a provider type or a file's path.
var plainRoute = regexp.MustCompile(`^[A-Za-z0-9._/~+-]+$`)

// RouteValue is value as a route: an address reduced to scheme://host[:port],
// whatever path, query, fragment or userinfo it carried, and a plain name as
// it is. THE HOST IS THE ROUTE: a session is bound so that its sign-in reaches
// no other endpoint's host, and two paths on one host are one service, so a
// credential carried in a path, a query or userinfo never reaches the
// identity. Anything else is [ErrNotRoute], never taken as it is.
func RouteValue(value string) (string, error) {
	if !strings.Contains(value, "://") {
		if !plainRoute.MatchString(value) {
			return "", ErrNotRoute
		}
		return value, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", ErrNotRoute
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
