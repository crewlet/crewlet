package httpx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
)

// wellKnownNAT64 is the NAT64 well-known prefix (RFC 6052). An address in it
// is judged by the IPv4 address it carries on every network, whether or not
// the network's resolver names it: a translator delivers it there wherever
// one routes it, and nowhere else does it lead.
var wellKnownNAT64 = netip.MustParsePrefix("64:ff9b::/96")

// localUseNAT64 is the block RFC 8215 sets aside for a network's own NAT64
// prefixes, so that a network need not take one from its unique-local space.
// A prefix the network names inside it counts ([honoured]); where nothing
// names one, the allowlist refuses the block whole.
var localUseNAT64 = netip.MustParsePrefix("64:ff9b:1::/48")

// nat64Lengths are the prefix lengths RFC 6052 section 2.2 defines an IPv4
// address's position for. No other length is a NAT64 prefix, so discovery
// finds no other.
var nat64Lengths = [...]int{32, 40, 48, 56, 64, 96}

// ipv4onlyArpa is the name RFC 7050 section 2.2 reserves for discovery,
// rooted so no search domain is tried in its place: under a Kubernetes pod's
// ndots:5 every search domain would be asked first, and a wildcard record in
// one would answer for it. It has A records and no AAAA, so every AAAA answer
// for it is one a DNS64 resolver synthesized.
const ipv4onlyArpa = "ipv4only.arpa."

// wellKnownIPv4Only are ipv4only.arpa's two A records (RFC 7050 section 2.2),
// in the order RFC 7050 section 3 searches a synthesized answer for them.
var wellKnownIPv4Only = [...]netip.Addr{
	netip.AddrFrom4([4]byte{192, 0, 0, 170}),
	netip.AddrFrom4([4]byte{192, 0, 0, 171}),
}

// discoverNAT64 asks r for the network's NAT64 prefixes by RFC 7050: an AAAA
// lookup of ipv4only.arpa, which a DNS64 resolver synthesizes per prefix.
//
// NO RECORD IS AN ANSWER: NODATA or NXDOMAIN means no DNS64 here, so the
// well-known prefix alone applies. Any other failure is returned for
// [Rule.Public] to refuse the address that needed it, never a guess.
//
// A prefix counts only where it cannot widen the rule ([honoured]); all
// answers are read first, then non-counting prefixes dropped. A [Rule] asks at
// most once per dial and caches nothing: a lookup is cheaper than a cache
// tracking the network's lifetime.
func discoverNAT64(ctx context.Context, r Resolver) ([]netip.Prefix, error) {
	answers, err := r.LookupNetIP(ctx, "ip6", ipv4onlyArpa)
	if err != nil && !noRecord(err) {
		return nil, err
	}
	named := synthesizedPrefixes(answers)
	return slices.DeleteFunc(named, func(p netip.Prefix) bool { return !honoured(p) }), nil
}

// honoured reports whether the rule takes the resolver's word for a NAT64
// prefix. THE RESOLVER IS NOT TRUSTED WITH WHERE A PREFIX LIES: an address
// under a counted prefix is judged by its low 32 bits alone, so a false
// ipv4only.arpa answer naming a unique-local prefix would call an internal
// host public. A prefix counts only when wholly inside global unicast clear
// of the blocks the allowlist refuses there (a stricter rule at worst; a
// later, smaller block may lie inside a prefix) or RFC 8215's 64:ff9b:1::/48.
// Others are ignored; the well-known prefix needs no naming
// ([wellKnownNAT64]). Cost: a DNS64 network on a unique-local prefix reaches
// no IPv4-only host. [namedCanHold] states the same places; change both.
func honoured(p netip.Prefix) bool {
	if within(p, localUseNAT64) {
		return true
	}
	if !within(p, global6) {
		return false
	}
	for _, b := range notGlobal6 {
		if b.Overlaps(p) {
			return false
		}
	}
	return true
}

// namedCanHold reports whether ip lies where a network-named prefix counts
// ([honoured]): 64:ff9b:1::/48, or global unicast outside the blocks the
// allowlist refuses there. Only there can the network's prefixes change ip's
// verdict, so only for such an address does a [Rule] ask for them.
//
// IT IS honoured's TWO PLACES FOR AN ADDRESS, and they change together: else
// an address under a counted prefix is judged as on a network naming none, and
// one in 2000::/3 carrying 10.0.0.5 passes. The well-known prefix is in
// neither place, so it is judged by what it carries. An address with a zone
// lies in no block and is refused without asking the resolver.
func namedCanHold(ip netip.Addr) bool {
	return localUseNAT64.Contains(ip) || allowlisted(ip)
}

// within reports whether every address of p is in block.
func within(p, block netip.Prefix) bool {
	return p.Bits() >= block.Bits() && block.Contains(p.Addr())
}

// noRecord reports whether err is a lookup that found no record of the type
// asked for, or no such name.
func noRecord(err error) bool {
	dnsErr, ok := errors.AsType[*net.DNSError](err)
	return ok && dnsErr.IsNotFound
}

// synthesizedPrefixes reads the prefixes a DNS64 resolver synthesized answers
// with, by RFC 7050 section 3: 192.0.0.170 or 192.0.0.171 at one of RFC 6052's
// positions, the address up to it being the prefix.
//
// THE OTHER ADDRESS DECIDES WHEN ONE IS AMBIGUOUS: a prefix's own bits can
// hold the searched value elsewhere, so it must be found exactly once, and an
// answer holding it twice repeats the WHOLE search with the other address (the
// other synthesis carries the first value once, and alone would name a prefix
// nothing translates through). The second is also searched when no answer
// holds the first.
func synthesizedPrefixes(answers []netip.Addr) []netip.Prefix {
	for _, wka := range wellKnownIPv4Only {
		if found, ambiguous := searchFor(wka, answers); !ambiguous && len(found) > 0 {
			return found
		}
	}
	return nil
}

// searchFor reads one prefix from each answer holding wka at exactly one of
// RFC 6052's positions, and reports ambiguous when an answer holds it at more
// than one.
func searchFor(wka netip.Addr, answers []netip.Addr) (found []netip.Prefix, ambiguous bool) {
	for _, a := range answers {
		if !a.Is6() || a.Is4In6() || a.Zone() != "" {
			continue
		}
		at := 0
		var prefix netip.Prefix
		for _, bits := range nat64Lengths {
			if carried(a, bits) == wka {
				at++
				prefix = netip.PrefixFrom(a, bits).Masked()
			}
		}
		switch {
		case at > 1:
			return nil, true
		case at == 1 && !slices.Contains(found, prefix):
			found = append(found, prefix)
		}
	}
	return found, false
}

// carried is the IPv4 address an address in a NAT64 prefix of bits carries,
// at the position RFC 6052 section 2.2 fixes for that length: the 32 bits
// after the prefix, skipping bits 64 to 71 (the "u" octet, kept zero for the
// IPv6 interface identifier's sake), which a prefix shorter than /64 has the
// IPv4 address straddle. Bits past the IPv4 address (the suffix) are not
// read: a translator ignores them.
func carried(a netip.Addr, bits int) netip.Addr {
	b := a.As16()
	var v4 [4]byte
	for i, n := bits/8, 0; n < len(v4); i++ {
		if i == 8 {
			continue
		}
		v4[n] = b[i]
		n++
	}
	return netip.AddrFrom4(v4)
}

// translated reports whether ip is in a NAT64 prefix (the well-known one, or
// one of nat64), and if it is, whether the IPv4 address it carries is public.
// An address two prefixes hold is public only when what it carries is public
// read at both positions: the rule cannot know which one the network's
// translator reads.
func translated(ip netip.Addr, nat64 []netip.Prefix) (in, carriesPublic bool) {
	carriesPublic = true
	judge := func(p netip.Prefix) {
		if p.Contains(ip) {
			in = true
			carriesPublic = carriesPublic && !inAny(notGlobal4, carried(ip, p.Bits()))
		}
	}
	judge(wellKnownNAT64)
	for _, p := range nat64 {
		judge(p)
	}
	return in, in && carriesPublic
}
