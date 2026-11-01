# ADR-0087: The fleet's broker admits only a connection that presents a key derived from secrets.keys

- **Status:** accepted
- **Authority:** `internal/queue/jetstream`
- **Enforced-by:** `internal/queue/jetstream.TestTheAuthenticatorAdmitsOnlyItsFourKinds`, `internal/queue/jetstream.TestAMembersClientListenerAdmitsOnlyTheOperatorKey`, `internal/queue/jetstream.TestALeafLinksOnlyWithAKeyItsMemberHolds`, `internal/queue/jetstream.TestAFleetListenerWithNoKeysIsRefused`, `internal/queue/jetstream/jetstreamtest.TestARouteWithoutTheFleetsKeyIsRefused`, `internal/queue/jetstream/jetstreamtest.TestGossipedRoutesAuthenticate`, `internal/queue/jetstream/jetstreamtest.TestAFleetStaysWholeThroughAKeyRotation`
- **Measured:** on nats-server v2.15.0, p50 of the stated runs: an nkey connect costs +210–230 µs over TCP (355–365 → 574–581 µs) and +160–170 µs in process (121–127 → 280–298 µs), two in-process connects a boot; a three-member mesh forms in 80.4–80.9 ms against 79.8–80.1 ms anonymous, and a leaf links in 12.0 ms against 11.6 ms; a route check costs 28–48 ns and an nkey check 55–70 µs. An engine connection in process against one over a member's loopback port: a request and its reply 12 µs against 19 µs, an R3 synchronous publish 257 µs against 309 µs. Through the keyring's rotation, member by member, the mesh stayed whole and an R3 publish through every member succeeded after every restart.
- **Cost-when-tried:** before it, a clustered member's client listener bound every interface with no authentication: an anonymous client created and published to an R3 stream, a server presenting nothing joined the routes, and a leaf presenting nothing linked and reached the fleet's JetStream — and the server's own user list could not refuse anonymity beside the system account, because with the system key in it an anonymous client still connected through the no-auth user nats-server adds to the global account.
- **Tag-status:** unreleased

## The decision

Every embedded broker the engine starts — a solo member, a clustered member, a
member with a leaf listener, a leaf — installs ONE authenticator, through
nats-server's embedding hooks (`CustomClientAuthentication`, and on a clustered
member `CustomRouterAuthentication`), and it admits exactly four kinds of
connection:

- a **route** presenting the username and password derived from a key of this
  node's keyring under `crewlet.broker.route.v1`;
- a **leaf link** presenting the nkey derived under `crewlet.broker.leaf.v1`
  and signing the member's nonce;
- an **operator** on a clustered member's loopback client listener presenting
  the nkey derived under `crewlet.broker.client.v1` and signing its nonce —
  the nats tool, for the broker gestures no engine verb performs, bound to the
  global account and never the system one;
- an **in-process client** of this process signing with a key minted at each
  start that nothing outside the process holds: the engine's own, bound to the
  global account, and the membership code's, bound to the system account.

Every other connection is refused, counted and logged. A clustered member's
client listener — which nats-server needs open for its routes to start — binds
`127.0.0.1`, is advertised to nobody, and admits the operator's key alone;
every connection of the engine is in process.

The keys are the keyring's (ADR-0011), derived per key by the rule every
consumer of `secrets.keys` follows (`internal/runtoken`'s `DeriveKey` and
`KeyTag`): every node accepts every key of its ring and presents the active
one. So the keyring's rotation runbook moves the broker with no step of its
own — and the two-restart order it already keeps (add a key everywhere, then
flip the active key) is the one that keeps a leaf restarted onto a flipped
ring from presenting a key no member holds yet.

## Why the obvious alternative is wrong

nats-server's own authorization blocks are the obvious alternative, and none
carries this. A cluster's block holds one user; a leaf listener's holds one
nkey or a list of passwords; and the client user list cannot refuse anonymity
on a member that declares a system account — measured, an anonymous client
still connected through the no-auth user the server injects. None of them
carries a rotation without either a secret on the wire or an anonymous path.
The embedding hooks carry both: an nkey is a challenge, so a leaf link and an
operator present a public key and a signature, never a secret, and the
authenticator accepts every key of the ring at once.

Signing alone (ADR-0031) is the other alternative, and it leaves too much open.
It covers the state logs and the three instruction channels, and nothing else:
the event envelope a seat's inbox carries, the coordination store's
compare-and-set, the read scatters, stream administration and every read were
anybody's who reached a port.

## What this does not decide

It does not encrypt. A route's password crosses the fleet network in clear once
per connection — a NATS route carries a password or a certificate, never a
signature — and every byte of the fleet's traffic crosses it in clear, as it
always has. So the fleet network stays private, a coding box stays off a
member's host network, and encrypting the links is a decision of its own,
answered for now by the documentation.

It does not authenticate the far side. A member dials its peers' route URLs and
a leaf its members' leaf URLs, and neither checks what answers: a route's
password goes to whatever holds a peer's address. What such an impostor could
forge is bounded by the signing of ADR-0031, which stands unchanged beside
this.

It does not decide an external cluster's authorization. `stream.type: nats`
dials a broker its own operator runs, with whatever credentials that operator
issues; what the engine requires of such a broker is a decision of its own.

## Amendments to earlier records

[ADR-0031](0031-every-state-log-record-is-signed.md): its premise — the broker
has no authentication of its own — no longer holds for the embedded broker,
and its decision stands unchanged. The broker's admission is per connection and
only as private as the network a route's password crosses in clear, and an
external cluster admits whom its operator chooses, so a record, an instruction
or a presence frame is still believed on its own signature or seal, never on
the connection it arrived by.
