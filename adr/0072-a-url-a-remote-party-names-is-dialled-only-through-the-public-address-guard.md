# ADR-0072: A URL a remote party names is dialled only through the public-address guard

- **Status:** accepted
- **Authority:** `internal/httpx`
- **Enforced-by:** `internal/httpx.TestAGuardedClientReachesNoAddressThatIsNotPublic`, `internal/httpx.TestTheGuardJudgesTheAddressItConnectsToNotTheName`, `internal/httpx.TestTheOperatorsProxyIsDialledAsItselfAndAnAddressItIsAskedForIsJudged`, `internal/httpx.TestAURLNamingTheProxysAddressDirectlyIsJudged`, `internal/httpx.TestAGuardedClientRefusesEverythingWhereTheGuardCannotBeInstalled`, `internal/httpx.TestOnlyAnAddressOnThePublicInternetIsPublic`, `internal/httpx.TestNoOtherPackageJudgesWhetherAnAddressIsPublic`, `internal/httpx.TestEveryReservedRangeIsNotPublic`, `internal/httpx.TestANAT64AddressIsJudgedByTheIPv4AddressItCarries`, `internal/httpx.TestAPrefixTheResolverNamesCountsOnlyWhereItCannotWidenTheRule`, `internal/httpx.TestANameIsPublicOnlyWhenEveryAddressItResolvesToIs`, `internal/httpx.TestTheGuardJudgesANAT64AddressByTheIPv4AddressItCarries`, `internal/httpx.TestTheGuardAsksForTheNetworksPrefixesOnlyWhereTheyCanChangeAVerdict`, `internal/httpx.TestTheGuardRacesTheAddressFamiliesOfAName`, `internal/github.TestADeliverableAddressIsOneGitHubReaches`
- **Cost-when-tried:** the rule of which addresses the public internet routes was written twice, once in the artifact CDN fetch's dialer and once in GitHub's reachability check, and the two had already parted: the second admitted carrier-grade NAT, the documentation ranges and multicast, so it judged a webhook address on a tailnet's `100.x` range reachable by github.com, and an app would have been created pointing at an address no delivery can reach. Neither copy judged an IPv4 address behind the NAT64 prefix as the address it is, so `64:ff9b::a9fe:a9fe`, which a translator forwards to the metadata address, read as public to both, and a network-specific NAT64 prefix inside global unicast, which a DNS64 network's own resolver names, read as public whatever IPv4 address it carried.
- **Tag-status:** unreleased

## The decision

**A request to a URL a remote party named is sent through `httpx.Guarded`,
and through nothing else.** A remote party is anyone but the operator: a page
an agent wrote, the metadata a server publishes about itself, the address a
redirect or a discovery document points at. The guarded client's dialer judges
the address the socket is about to connect to, in its Control hook, so a name
is never checked and then resolved again: a resolver that answers once public
and once internal has no second answer to slip through.

**Which addresses are public is one rule in one place, `internal/httpx`**:
not loopback, private, link-local, multicast, unspecified, carrier-grade NAT,
documentation, or any block the IANA special-purpose registries mark as not
globally reachable. IPv4 is held to that registry whole, which is the complete
answer for a space with no unallocated remainder; IPv6 to an allowlist, the
global unicast block less the blocks in it that are not on the internet. An
IPv4 address in IPv6 form is judged as itself, and an address under a NAT64
prefix by the IPv4 address it carries, which is where a translator delivers
it: the well-known prefix on every network, and the network-specific prefixes
the node's own resolver names when asked for `ipv4only.arpa` (RFC 7050),
counted only where that word cannot widen the rule (inside the allowlist, or
in RFC 8215's `64:ff9b:1::/48`).

The rule is reached through three doors, and through nothing else. The
guarded client's dialer makes an `httpx.Rule` once a dial, on the resolver it
resolves the name with, and asks it of each socket's address. A dialer that
hands the address to another program to connect to (git, for an import or a
mirror) takes the host's addresses from `httpx.ResolvePublic`, which answers
them only when every one is public, and dials one of those, never the name
again. A caller with an address and no dial (a configured server's address, a
webhook's deliverability) asks `httpx.Public`, the verdict on a network that
names no prefix of its own. No other package calls the address predicates the
rule is built from.

**An address the operator configured is not a remote party's.** An MCP
server's url, a webhook sink, a self-hosted vendor and a proxy are dialled as
they are, because a private or loopback address there is the deployment the
operator chose.

## Why the obvious alternatives are wrong

**Checking the URL before the request** (parse the host, resolve it, refuse a
private answer, then send) checks one lookup and dials another. The second
lookup is the dialer's own, and a rebinding resolver answers it differently.
Only a check on the socket's own address leaves nothing between the decision
and the connection.

**Guarding every outbound client** would refuse the operator's own private
addresses, which the docs show (`http://localhost:3000` for a local MCP
server, a self-hosted GitLab on the company network), and the shared
transport would stop being one pool for everybody.

**A per-package check** is the ADR-0008 failure this record was written after:
two copies, no comparison, and the second already admitting what the first
refused.

**A list of reserved IPv6 blocks** cannot be completed: IANA keeps carving
blocks out of a space that is mostly unallocated, and such a list calls each
one public until somebody writes its row, as the copies this replaced called
6to4, Teredo and the IPv4-compatible form public, each a way to reach a
private IPv4 address through an IPv6 one.

**Trusting every NAT64 prefix the resolver names** lets a resolver that
answers falsely (a poisoned cache, one a DHCP lease handed out) choose which
addresses are judged by their low 32 bits alone, an internal unique-local host
among them. Counted only inside the allowlist or the local-use block, a false
answer can only refuse more.

**Resolving the name and handing the dialer one judged address at a time**
forgoes net.Dialer's own resolution, its race of the address families (RFC
8305) and its split of the bound across a family's addresses: on a node whose
IPv6 path drops connections, the IPv6 addresses ordered first would spend the
request's time before IPv4 was tried. Judged at the socket, the dial stays the
dialer's own.

## What this does not decide

It does not decide **which** URLs are a remote party's at a given call site:
that is a judgement about where the URL came from, which no walk over source
can make, so it is held by review, and each new caller that follows a remote
party's URL (an OAuth discovery, a probe of an address before it is added)
says so where it builds its client.

It does not make a forward proxy safe. A proxy the operator set is dialled as
itself, for the request the transport sent through it and never by its
address alone, so a URL naming the proxy on a host sent direct is judged like
any other. An address written into the URL is judged before the proxy hears of
it, and a name is resolved by the proxy alone, so a rebinding answer can still
reach whatever the proxy reaches. An operator whose proxy reaches internal
addresses holds the proxy to the same rule.

It does not judge a host name a browser treats as this machine (`localhost`,
`*.localhost`) except through what it resolves to: whether a configured
address names this host is a question several packages ask for reasons of
their own, and it is not this rule.
