# ADR-0072: A URL a remote party names is dialled only through the public-address guard

- **Status:** accepted
- **Authority:** `internal/httpx`
- **Enforced-by:** `internal/httpx.TestAGuardedClientReachesNoAddressThatIsNotPublic`, `internal/httpx.TestTheGuardJudgesTheAddressItConnectsToNotTheName`, `internal/httpx.TestTheOperatorsProxyIsDialledAsItselfAndAnAddressItIsAskedForIsJudged`, `internal/httpx.TestAURLNamingTheProxysAddressDirectlyIsJudged`, `internal/httpx.TestAGuardedClientRefusesEverythingWhereTheGuardCannotBeInstalled`, `internal/httpx.TestOnlyAnAddressOnThePublicInternetIsPublic`, `internal/httpx.TestNoOtherPackageJudgesWhetherAnAddressIsPublic`, `internal/github.TestADeliverableAddressIsOneGitHubReaches`
- **Cost-when-tried:** the rule of which addresses the public internet routes was written twice, once in the artifact CDN fetch's dialer and once in GitHub's reachability check, and the two had already parted: the second admitted carrier-grade NAT, the documentation ranges and multicast, so it judged a webhook address on a tailnet's `100.x` range reachable by github.com, and an app would have been created pointing at an address no delivery can reach. Neither copy judged an IPv4 address behind the NAT64 prefix as the address it is, so `64:ff9b::a9fe:a9fe`, which a translator forwards to the metadata address, read as public to both.
- **Tag-status:** unreleased

## The decision

**A request to a URL a remote party named is sent through `httpx.Guarded`,
and through nothing else.** A remote party is anyone but the operator: a page
an agent wrote, the metadata a server publishes about itself, the address a
redirect or a discovery document points at. The guarded client's dialer judges
the address the socket is about to connect to, in its Control hook, so a name
is never checked and then resolved again: a resolver that answers once public
and once internal has no second answer to slip through.

**Which addresses are public is `httpx.Public`, one rule in one place**: not
loopback, private, link-local, multicast, unspecified, carrier-grade NAT,
documentation, or any range the IANA special-purpose registries mark as not
globally reachable; IPv6 only from the global unicast block, and an IPv4
address in IPv6 form or behind the NAT64 prefix judged as itself. A package
that needs the answer asks it, and no other package calls the address
predicates it is built from.

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
