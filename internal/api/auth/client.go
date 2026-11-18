package auth

import (
	"net"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// WHO IS ACTUALLY CALLING, behind however many proxies this deployment sits
// behind.

// The trust setting is a CIDR list, not a bool, because the question is "is
// THIS peer the proxy", and a bool is wrong both ways:
//
//   - TRUE trusts a header anybody can send: an attacker picks their own
//     `X-Forwarded-For`, so the throttle never fires and the audit row names
//     whoever they chose to blame.
//   - FALSE behind a real proxy buckets the internet under one address: a
//     stranger's guesses put the victim's own sign-in behind the same wait,
//     and every audit row names the proxy. `crewlet validate` warns about an
//     `https` external URL with no proxy trusted.

// Used by the failed-sign-in throttle and every logged refusal's source. Empty,
// the default, means the peer IS the client: right when reached directly,
// merely pessimistic behind a proxy, which therefore has to name it.

// Clients resolves a caller's own address, holding this deployment's trusted
// blocks.
//
// A VALUE TYPE rather than a free function taking the blocks at each call, for
// [iamdomain.Blinder]'s reason: the blocks are parsed once from Tier A, and a
// signature that took them every time would have each caller deciding where to
// get them from — which is how one of them comes to key on the proxy.
//
// IT IS NOT THE GUARD. The guard holds one and so does the sign-in surface,
// and that is not two answers to one question: both are built from the same
// Tier A by the same pure function, so they cannot disagree. What would be two
// answers is two PARSERS, and there is one.
type Clients struct{ trusted []*net.IPNet }

// NewClients parses Tier A's blocks once.
func NewClients(b *config.Bootstrap) *Clients {
	return &Clients{trusted: TrustedProxies(b)}
}

// Of is the address a per-source rule keys on.
func (c *Clients) Of(r *http.Request) string {
	if c == nil {
		// A RESOLVER NOBODY BUILT trusts no proxy, which is the
		// pessimistic direction: everybody shares their peer's bucket
		// rather than choosing their own.
		return hostOf(r.RemoteAddr)
	}
	return Client(r, c.trusted)
}

// Client is the address a per-source rule keys on.
//
// # The walk, and why it goes right to left
//
// `X-Forwarded-For` is appended to by each hop (client, proxy1, proxy2, the
// NEAREST proxy last), so only entries written by trusted peers are believable.
// The walk stops at the FIRST address that is not a trusted proxy: the closest
// hop this deployment cannot vouch for, and everything left of it is that hop's
// claim. The LEFTMOST entry is the classic bug: entirely caller-controlled.
func Client(r *http.Request, trusted []*net.IPNet) string {
	peer := hostOf(r.RemoteAddr)
	if len(trusted) == 0 || !trustedIP(peer, trusted) {
		// THE PEER IS THE CLIENT. Either this deployment trusts no
		// proxy, or the peer is not one it trusts — and in the second
		// case the header it sent is a claim with nothing behind it.
		return peer
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer
	}
	hops := forwardedHops(forwarded)
	for i := len(hops) - 1; i >= 0; i-- {
		if !trustedIP(hops[i], trusted) {
			return hops[i]
		}
	}
	// EVERY HOP IS A TRUSTED PROXY, which is a chain of this deployment's
	// own infrastructure with no client address in it. The leftmost is
	// then the closest thing to a client there is, and it was written by a
	// peer this deployment trusts.
	if len(hops) > 0 {
		return hops[0]
	}
	return peer
}

// forwardedHops flattens the header into addresses, in order.
//
// EVERY VALUE AND NOT ONLY THE FIRST, because a header may legitimately arrive
// as several lines — each proxy is entitled to append a new one rather than
// extend the last — and reading one of them would drop hops the walk above
// depends on being able to see.
func forwardedHops(values []string) []string {
	var out []string
	for _, value := range values {
		for _, hop := range strings.Split(value, ",") {
			if host := hostOf(strings.TrimSpace(hop)); host != "" {
				out = append(out, host)
			}
		}
	}
	return out
}

// hostOf strips a port and brackets, so an IPv6 literal and an IPv4 one both
// come back as an address.
func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

// trustedIP reports whether an address is inside one of the trusted blocks.
func trustedIP(addr string, trusted []*net.IPNet) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, block := range trusted {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustedProxies parses Tier A's blocks once, at wiring time.
//
// A BLOCK THAT WILL NOT PARSE IS DROPPED rather than failing here, because
// `crewlet validate` already refuses one by name and this runs long after: a
// panic or a refusal at bind time would take a node down over a value the
// operator was told about at the moment they wrote it. Dropping is also the
// SAFE direction — one fewer trusted proxy means one more client keyed to its
// peer, which is pessimistic rather than permissive.
func TrustedProxies(b *config.Bootstrap) []*net.IPNet {
	if b == nil {
		return nil
	}
	out := make([]*net.IPNet, 0, len(b.API.TrustedProxies))
	for _, block := range b.API.TrustedProxies {
		_, parsed, err := net.ParseCIDR(strings.TrimSpace(block))
		if err != nil || parsed == nil {
			log.Warn("api_trusted_proxy_unparsed", "block", block, "error", err)
			continue
		}
		out = append(out, parsed)
	}
	return out
}
