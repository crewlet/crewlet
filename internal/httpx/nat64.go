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

// discoverNAT64 asks r for the network-specific NAT64 prefixes of the network
// it resolves on, by RFC 7050's discovery: an AAAA lookup of ipv4only.arpa,
// which a DNS64 resolver answers by synthesizing from the name's A records
// with each prefix it translates through.
//
// NO RECORD IS AN ANSWER: the name has no AAAA record of its own, so NODATA
// (or NXDOMAIN, which Go reports alike, as a not-found DNS error) means no
// DNS64 here, and the well-known prefix alone applies. Any other failure is
// the resolver's own, returned for [Rule.Public] to refuse the address that
// needed the answer with: an address is never judged on a guess about which
// prefixes are translated.
//
// A PREFIX COUNTS ONLY WHERE THE ANSWER CANNOT WIDEN THE RULE ([honoured]).
// Every answer is read first, so RFC 7050's reading of an ambiguous answer
// sees all of them, and a prefix that does not count is then dropped: the
// addresses under it are judged as on a network that names none.
//
// A [Rule] asks it at most once, for the first address whose verdict its
// answer can change, and nothing keeps the answer longer than that one dial
// or resolve: one AAAA lookup beside the host's own, against a dial that is
// about to fetch a file or a repository, is cheaper than a cache whose
// lifetime would have to track the network's.
func discoverNAT64(ctx context.Context, r Resolver) ([]netip.Prefix, error) {
	answers, err := r.LookupNetIP(ctx, "ip6", ipv4onlyArpa)
	if err != nil && !noRecord(err) {
		return nil, err
	}
	named := synthesizedPrefixes(answers)
	return slices.DeleteFunc(named, func(p netip.Prefix) bool { return !honoured(p) }), nil
}

// honoured reports whether a NAT64 prefix the network's resolver named is one
// the rule takes its word for: one lying where that word cannot widen the rule
// into a network that is not on the internet.
//
// THE RESOLVER IS NOT TRUSTED WITH WHERE A PREFIX LIES. An address under a
// prefix that counts is judged by the 32 bits it carries and by nothing else,
// so a resolver that answered ipv4only.arpa falsely (a poisoned cache, a rogue
// resolver a DHCP lease handed out) would otherwise choose which addresses
// those are. Naming a unique-local prefix, it would have the rule call public
// an internal host whose low 32 bits read as a public IPv4 address, and the
// rule exists to keep a dial out of exactly that space. So a prefix counts in
// two places alone, wholly inside one of them:
//   - global unicast outside the blocks the allowlist refuses there. Every
//     address of such a prefix is public by the allowlist already, and judging
//     one by the address it carries can only refuse it, so a false answer
//     there makes the rule stricter, never wider. The whole prefix is held
//     clear of those blocks, not its first address alone: a block added
//     there later may be smaller than a prefix, and lie inside one.
//   - RFC 8215's local-use block, 64:ff9b:1::/48, and no wider: it is
//     reserved for translation and nothing else, so that a network's own
//     prefix need not be unique-local.
//
// A prefix named anywhere else is ignored, and the allowlist refuses every
// address under it. The well-known prefix needs no naming ([wellKnownNAT64]).
// The cost falls on a DNS64 network whose translator uses a unique-local or
// other non-global prefix: through it, the rule reaches no host that has only
// an IPv4 address. Such a network translates through the well-known prefix, a
// prefix in 64:ff9b:1::/48 or one in its own global unicast space, or its
// dialer reaches the host through a proxy its caller trusts.
//
// [namedCanHold] states the same two places for one address, and changes with
// them.
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

// namedCanHold reports whether ip lies where a prefix the network names counts
// ([honoured]): in 64:ff9b:1::/48, or in global unicast outside the blocks the
// allowlist refuses there. Only there can the network's prefixes change ip's
// verdict, so only for such an address does a [Rule] ask for them.
//
// IT IS honoured's TWO PLACES, STATED FOR AN ADDRESS, and the two change
// together: every address of a prefix that counts must be one this answers
// true for. Otherwise an address under a counted prefix would be judged as on
// a network naming none, and one inside 2000::/3 carrying 10.0.0.5 would be
// called public by the allowlist alone. The well-known prefix lies in neither
// place, so an address in it is judged by what it carries, whatever the
// network names. An address with a zone lies in no block (netip.Prefix.Contains
// answers false), and is refused without the resolver being asked.
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
