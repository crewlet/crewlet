package executor

import (
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// directHosts is the hosts of the routes of Crewlet's own a run's harness
// dials, each as the box dials it: its own servers' ([crewletServers]), the
// bridge among them, its notes route and its skill route, and the same under
// the wiring of a session the script starts of its own. A seat's other servers
// are not among them: a person's proxy decides those, as in their session.
func directHosts(req sandbox.RunRequest) []string {
	var hosts []string
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	wirings := []sandbox.Wiring{{MCPServers: req.MCPServers, Steer: req.Steer, SkillLoads: req.SkillLoads}}
	if req.FreshWiring != nil {
		wirings = append(wirings, *req.FreshWiring)
	}
	for _, w := range wirings {
		for _, name := range crewletServers(w.MCPServers) {
			if s := w.MCPServers[name]; s.Transport == sandbox.TransportHTTP {
				add(s.URL)
			}
		}
		if w.Steer.Set() {
			add(w.Steer.URL)
		}
		if w.SkillLoads.Set() {
			add(w.SkillLoads.URL)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// noProxyExports is the wrapper's statement keeping Crewlet's own routes off
// any proxy: hosts appended to NO_PROXY and no_proxy, after the login profile
// as [attributionExports] is. A proxy the run's environment, the image, the
// profile or the harness's own settings name (Copilot's `proxyUrl`, Pi's
// `httpProxy`) otherwise carried the bridge's calls, and one that cannot reach
// the engine host cut the run off its tools (measured on Copilot 1.0.114 and
// Pi 1.6.0; NO_PROXY with the bridge's host restored both). Both spellings hold
// both lists, since a client reading one alone (curl reads no_proxy first)
// would otherwise drop the other's entries. "" where there is no host.
func noProxyExports(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	list := shellword.Quote(strings.Join(hosts, ","))
	return `crewlet_np=${no_proxy:-}; [ -z "${NO_PROXY:-}" ] || [ "$NO_PROXY" = "$crewlet_np" ] || ` +
		`crewlet_np="${crewlet_np:+$crewlet_np,}$NO_PROXY"; export NO_PROXY="${crewlet_np:+$crewlet_np,}"` + list +
		` no_proxy="${crewlet_np:+$crewlet_np,}"` + list + `; unset crewlet_np; `
}
