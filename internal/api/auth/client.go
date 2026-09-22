package auth

import (
	"net"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// WHO IS ACTUALLY CALLING, behind however many proxies this deployment sits
// behind.
//
// # Why this is a CIDR list rather than a bool
//
// The question a forwarded header poses is not "does this deployment sit
// behind a proxy" — it is "is THIS peer the proxy". A bool answers the first
// while every per-source rule in this engine needs the second, and it is wrong
// in both settings:
//
//   - TRUE trusts a header anybody can send. An attacker picks their own
//     `X-Forwarded-For`, which is their own rate-limit bucket and their own
//     row in the audit trail — so the throttle never fires and the log names
//     whoever they decided to blame.
//   - FALSE behind a real proxy buckets the entire internet under one address.
//     The throttle then locks every honest person in the company out the
//     moment one attacker starts guessing, which is an outage the defence
//     caused.
//
// # What it is used for, and why that makes the default fail-safe
//
// The failed-sign-in throttle, and the source on every refusal this engine
// logs. Empty is the default and means the peer address IS the client address,
// which is correct for a node reached directly and merely pessimistic behind a
// proxy: everybody shares one bucket, which is the second failure above — so
// a deployment behind a proxy has to name it, and this file is what makes that
// setting do anything at all.

// Client is the address a per-source rule keys on.
//
// # The walk, and why it goes right to left
//
// `X-Forwarded-For` is appended to by each hop, so the list reads
// client, proxy1, proxy2 — with the NEAREST proxy last. The peer this node
// accepted from is therefore the rightmost entry's author, and the only
// entries this deployment has any reason to believe are the ones written by
// peers it trusts. So the walk starts at the right and stops at the FIRST
// address that is not a trusted proxy: that is the closest hop this deployment
// cannot vouch for, and everything to its left is whatever that hop chose to
// claim.
//
// Taking the LEFTMOST entry — which reads naturally as "the original client" —
// is the classic form of this bug: it is the one value entirely under the
// caller's control.
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
