package hostreach

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"

	"github.com/crewlet/crewlet/internal/browser"
)

// routes is every route the box's half dials the engine's loopback by, in the
// order [Check] names the first that arrived: host_network's causes that name
// a route, which reach.js's ROUTES and its proxy name too.
var routes = []browser.EnsureCause{browser.CauseLoopback, browser.CauseGateway, browser.CauseHostAlias,
	browser.CauseRootlessGateway, browser.CauseProxy}

// The engine's own routing tables, which name its default gateways.
const (
	routeFile  = "/proc/net/route"
	route6File = "/proc/net/ipv6_route"
)

// engineGateways is every default gateway of this engine's own routing
// tables, read through read: an IPv6 link-local one too, which the box's dial,
// handed no interface, fails at once. A table that cannot be read is an error,
// the IPv6 one absent aside, as on a kernel booted without IPv6.
func engineGateways(read func(string) ([]byte, error)) ([]string, error) {
	v4, err := read(routeFile)
	if err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", routeFile, err)
	}
	v6, err := read(route6File)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		v6 = nil
	case err != nil:
		return nil, fmt.Errorf("%s is unreadable: %w", route6File, err)
	}
	var out []string
	for i, line := range strings.Split(string(v4), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 || f[1] != "00000000" || f[2] == "00000000" {
			continue
		}
		if b, err := hex.DecodeString(f[2]); err == nil && len(b) == net.IPv4len {
			// The kernel writes the address in its own byte order, little
			// endian on every platform the engine is released for.
			out = append(out, net.IPv4(b[3], b[2], b[1], b[0]).String())
		}
	}
	zero := strings.Repeat("0", 32)
	for line := range strings.Lines(string(v6)) {
		f := strings.Fields(line)
		if len(f) != 10 || f[0] != zero || f[1] != "00" || f[4] == zero {
			continue
		}
		if b, err := hex.DecodeString(f[4]); err == nil && len(b) == net.IPv6len {
			out = append(out, net.IP(b).String())
		}
	}
	return out, nil
}

// engineAddresses is every address of this engine's own interfaces, as addrs
// lists them: where a proxy the box names is one of this host's, which may
// share its loopback.
func engineAddresses(addrs func() ([]net.Addr, error)) ([]string, error) {
	all, err := addrs()
	if err != nil {
		return nil, fmt.Errorf("listing this host's addresses: %w", err)
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		switch a := a.(type) {
		case *net.IPNet:
			out = append(out, a.IP.String())
		case *net.IPAddr:
			out = append(out, a.IP.String())
		}
	}
	return out, nil
}
