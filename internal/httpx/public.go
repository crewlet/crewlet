package httpx

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
)

// notGlobal4 are the IPv4 blocks no address on the public internet is in.
//
// They are IANA's IPv4 Special-Purpose Address Registry, held whole
// (TestEveryReservedRangeIsNotPublic copies it row by row), plus 192.88.99.0/24
// (retired 6to4 relay anycast, which delivers to the IPv4 address a 6to4
// packet carries) and multicast, which IANA keeps elsewhere.
//
// 192.0.0.0/24 is refused whole though 192.0.0.9 and .10 are globally
// reachable: anycast services (PCP, TURN), never a host a dial means to reach.
//
// IPv4 is a list of what is reserved, IPv6 a list of what is allocated
// ([global6]), because IPv4 has no unallocated remainder.
var notGlobal4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 791); 0.0.0.0 is in it
	netip.MustParsePrefix("10.0.0.0/8"),      // private use (RFC 1918)
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space, carrier-grade NAT (RFC 6598)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback (RFC 1122)
	netip.MustParsePrefix("169.254.0.0/16"),  // link local (RFC 3927): the cloud metadata address
	netip.MustParsePrefix("172.16.0.0/12"),   // private use (RFC 1918)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890), whole
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (RFC 5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast, deprecated (RFC 7526)
	netip.MustParsePrefix("192.168.0.0/16"),  // private use (RFC 1918)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (RFC 5737)
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast (RFC 5771)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (RFC 1112); 255.255.255.255 is in it
}

// global6 is global unicast, the one block IANA allocates IPv6 unicast
// addresses from (its IPv6 Address Space registry: "IANA unicast address
// assignments are currently limited to ... 2000::/3"). Every IPv6 address on
// the public internet is in it.
var global6 = netip.MustParsePrefix("2000::/3")

// notGlobal6 are the blocks inside [global6] that are not on the public
// internet: those IANA's IPv6 Special-Purpose Address Registry marks not
// globally reachable, and the two that carry an IPv4 address a relay delivers
// to.
//
// 2001::/23 is refused whole. It holds Teredo, benchmarking and ORCHID, and
// the entries the registry marks globally reachable inside it are anycast
// services (PCP, TURN, DNS-SD, AMT, AS112) and identifiers (ORCHIDv2, drone
// tags), never a host a dial here means to reach.
var notGlobal6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),     // IETF protocol assignments (RFC 2928), whole
	netip.MustParsePrefix("2001:db8::/32"), // documentation (RFC 3849)
	netip.MustParsePrefix("2002::/16"),     // 6to4 (RFC 3056), which carries an IPv4 address
	netip.MustParsePrefix("3fff::/20"),     // documentation (RFC 9637)
}

// Resolver is what the rule asks of a resolver: the addresses a name has, of
// the family network names ("ip" for every address, "ip6" for AAAA records
// alone). *net.Resolver has it; a test answers it from a table and reaches no
// network.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Rule is the public-address rule as it holds on one network: which addresses
// are public there, given the NAT64 prefixes its resolver names. [NewRule] is
// the only way to a Rule that calls any address public, so no dial judges an
// address the prefixes can change without them ([Public] is for a caller with
// no dial): one in a network-specific prefix inside 2000::/3 carrying a
// private IPv4 address would be called public. The zero and nil Rule refuse
// every address.
//
// A Rule is one dial's or resolve's: it asks the resolver for prefixes at most
// once, when an address first needs them, and nothing keeps the answer
// (discoverNAT64). Safe for concurrent use, as a racing dialer uses it.
type Rule struct {
	// resolver is the network's own, the one the host judged is resolved
	// with. It is nil in the zero Rule.
	resolver Resolver

	// mu holds the one ask: a judgment that needs its answer while it is out
	// waits for it.
	mu sync.Mutex
	// asked is set once the resolver has been asked. nat64 and err are what
	// it answered: the network-specific NAT64 prefixes it named where they
	// count ([honoured]), or the failure that kept it from naming any. The
	// well-known prefix counts on every network, and is not among them.
	asked bool
	nat64 []netip.Prefix
	err   error
}

// NewRule returns the public-address rule on the network r resolves on. r
// must be the resolver the host to be judged is resolved with, since only a
// network's own resolver names the prefixes it translates through (RFC 7050;
// discoverNAT64 says how). It asks r nothing until an address needs it.
func NewRule(r Resolver) *Rule {
	return &Rule{resolver: r}
}

// Public reports whether ip is public on the rule's network, or the failure
// that kept it from being judged.
//
// PREFIXES ARE ASKED FOR ONLY WHERE THEY CAN CHANGE THE VERDICT
// ([namedCanHold]), at the first such IPv6 address; every other address has
// the same verdict on any network. So a resolver failure refuses only an address
// that needed the answer, wrapped, never on a guess; an IPv4 host is neither
// refused nor kept waiting. The ask, failure included, stands for the rule's
// life. It is a wall only where a dialer asks it: in net.Dialer's
// ControlContext, of the address each connection is made to, so a resolver
// answering twice cannot slip between judgment and dial.
func (r *Rule) Public(ctx context.Context, ip netip.Addr) (bool, error) {
	if r == nil || r.resolver == nil {
		return false, nil
	}
	if !namedCanHold(ip) {
		return public(ip, nil), nil
	}
	nat64, err := r.named(ctx)
	if err != nil {
		return false, fmt.Errorf("httpx: %s cannot be judged until the resolver answers an AAAA lookup of %s, "+
			"even with no record: it names the NAT64 prefixes this network translates through (RFC 7050): %w",
			ip, ipv4onlyArpa, err)
	}
	return public(ip, nat64), nil
}

// named is what the network's resolver answers when asked for its NAT64
// prefixes, asked the first time it is called and kept for the rule's life.
// Not a sync.OnceValues: the ask takes the context of the judgment that needs
// it first, which a OnceValues' function cannot be handed.
func (r *Rule) named(ctx context.Context) ([]netip.Prefix, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.asked {
		r.nat64, r.err = discoverNAT64(ctx, r.resolver)
		r.asked = true
	}
	return r.nat64, r.err
}

// ResolvePublic returns the addresses host names, each judged public by the
// rule on r's network ([NewRule]), as the address to dial (IPv4-in-IPv6
// unmapped); one that is not refuses the host with [ErrNotPublic].
//
// IT IS THE DOOR FOR A DIALER THAT HANDS THE ADDRESS TO ANOTHER PROGRAM: it
// must dial an address returned here, never the name again (a resolver
// answering twice). A dialer making its own connections uses [Rule.Public]
// at its socket. NAT64 prefixes are asked of r at most once a call, only when
// needed; a failure refuses the host, unless another address is not public:
// that [ErrNotPublic] is the more definite answer, in any order.
func ResolvePublic(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	literal, err := netip.ParseAddr(host)
	if err == nil {
		addrs = []netip.Addr{literal}
	} else {
		if addrs, err = r.LookupNetIP(ctx, "ip", host); err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("httpx: %s resolves to no address", host)
		}
	}
	rule := NewRule(r)
	judged := make([]netip.Addr, 0, len(addrs))
	var unjudged error
	for _, ip := range addrs {
		ip = ip.Unmap()
		ok, err := rule.Public(ctx, ip)
		switch {
		case err != nil:
			if unjudged == nil {
				unjudged = err
			}
		case !ok:
			if literal.IsValid() {
				return nil, fmt.Errorf("%w: %s", ErrNotPublic, host)
			}
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrNotPublic, host, ip)
		default:
			judged = append(judged, ip)
		}
	}
	if unjudged != nil {
		return nil, unjudged
	}
	return judged, nil
}

// public reports whether ip is public on a network translating through the
// NAT64 prefixes nat64 (the well-known one counts everywhere). IPv4 is public
// unless in [notGlobal4]; IPv4-in-IPv6 is judged as IPv4. A ZONED IPv6 ADDRESS
// IS REFUSED FIRST: no internet host is named with one, and the blocks refuse
// it only because Prefix.Contains answers false, while netip's predicates and
// carried ignore the zone, so a reshaped rule would call 2606:4700::1%eth0
// public. A NAT64 address is judged BEFORE the allowlist, by the IPv4 address
// it carries alone, since a translator delivers 64:ff9b::a00:5 to 10.0.0.5 and
// a network-specific prefix may sit in 2000::/3; the prefix is not refused
// whole, as DNS64 hosts resolve into it. IPv6 IS AN ALLOWLIST (2000::/3 minus
// [notGlobal6]): a denylist calls each block IANA newly reserves public. One
// it reserves inside 2000::/3 still needs its row in notGlobal6.
func public(ip netip.Addr, nat64 []netip.Prefix) bool {
	ip = ip.Unmap()
	switch {
	case ip.Is4():
		return !inAny(notGlobal4, ip)
	case ip.Is6():
		if ip.Zone() != "" {
			// Before any block is asked: none contains a zoned address.
			return false
		}
		if in, carriesPublic := translated(ip, nat64); in {
			return carriesPublic
		}
		return allowlisted(ip)
	}
	return false
}

// ServerAddress reports whether ip may be where a server the operator registered
// is reached: a private address included, since an operator's own servers are
// often on one, but never this host (loopback or unspecified), a link-local
// address (where a cloud's metadata service answers) or a multicast group. An
// IPv4 address written in IPv6 form is judged as itself, and a zoned address
// names one of this host's links, so it is refused.
func ServerAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.Zone() == "" && !ip.IsLoopback() && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

// allowlisted reports whether ip is in global unicast and outside every block
// there that is not on the internet ([notGlobal6]): the allowlist an IPv6
// address no NAT64 prefix holds is judged by.
func allowlisted(ip netip.Addr) bool {
	return global6.Contains(ip) && !inAny(notGlobal6, ip)
}

// inAny reports whether ip is in one of blocks.
func inAny(blocks []netip.Prefix, ip netip.Addr) bool {
	for _, b := range blocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}
