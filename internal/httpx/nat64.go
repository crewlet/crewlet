package httpx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// wellKnownNAT64 is the NAT64 well-known prefix (RFC 6052). An address in it
// is judged by the IPv4 address it carries on every network, whether or not
// the network's resolver names it: a translator delivers it there wherever
// one routes it, and nowhere else does it lead.
var wellKnownNAT64 = netip.MustParsePrefix("64:ff9b::/96")

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

// discoverNAT64 asks r for the network-specific NAT64 prefixes of the network
// it resolves on, by RFC 7050's discovery: an AAAA lookup of ipv4only.arpa,
// which a DNS64 resolver answers by synthesizing from the name's A records
// with each prefix it translates through.
//
// NO RECORD IS AN ANSWER: the name has no AAAA record of its own, so NODATA
// (or NXDOMAIN, which Go reports alike, as a not-found DNS error) means no
// DNS64 here, and the well-known prefix alone applies. Any other failure
// fails the discovery, wrapped: an address is never judged on a guess about
// which prefixes are translated, and a resolver that cannot answer this
// cannot be relied on for the host's own answer either.
//
// It is asked for every dial rather than kept: one AAAA lookup beside the
// host's own, against a dial that is about to fetch a file or a repository,
// is cheaper than a cache whose lifetime would have to track the network's.
func discoverNAT64(ctx context.Context, r Resolver) ([]netip.Prefix, error) {
	answers, err := r.LookupNetIP(ctx, "ip6", ipv4onlyArpa)
	if err != nil && !noRecord(err) {
		return nil, fmt.Errorf("httpx: the resolver must answer an AAAA lookup of %s, even with no record, "+
			"before an address is judged: it names the NAT64 prefixes this network translates through "+
			"(RFC 7050): %w", ipv4onlyArpa, err)
	}
	return synthesizedPrefixes(answers), nil
}

// noRecord reports whether err is a lookup that found no record of the type
// asked for, or no such name.
func noRecord(err error) bool {
	dnsErr, ok := errors.AsType[*net.DNSError](err)
	return ok && dnsErr.IsNotFound
}

// synthesizedPrefixes reads the prefixes a DNS64 resolver synthesized
// answers with, by RFC 7050 section 3: each answer holds 192.0.0.170 or
// 192.0.0.171 at one of RFC 6052's positions, and the address up to that
// position is the prefix.
//
// THE OTHER ADDRESS DECIDES WHEN ONE IS AMBIGUOUS. A prefix's own bits can
// hold the value searched for at another position, so RFC 7050 has the value
// found exactly once in an answer, and has the search repeated with the other
// well-known address when an answer holds it twice. The switch is for the
// whole search, not the one answer: the answer synthesized from the other
// address carries the first one in the same prefix bits, ONCE, and read on
// its own it would name a prefix nothing translates through. The second
// address is searched for, too, when no answer holds the first: a resolver
// may answer with the one record it synthesized for 192.0.0.171 alone.
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
