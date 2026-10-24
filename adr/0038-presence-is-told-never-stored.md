# ADR-0038: Presence is told, never stored

- **Status:** accepted
- **Authority:** `internal/presence`
- **Enforced-by:** `internal/presence.TestPresenceWritesNoRowAnywhere`, `internal/presence.TestAPresenceFrameTheFleetDidNotSealShowsNobody`, `internal/presence.TestPresenceExpiresAfterThreeMissedHeartbeats`, `internal/presence.TestABroadcastIsNeverRetained`
- **Tag-status:** unreleased

## The decision

Presence answers one question, "who is in this artifact right now, and what
are they looking at", and the answer is worthless forty-five seconds after it
stops being refreshed. So **who has to agree on it is nobody**: no row, no log
record and no coordination key holds it. That is a different answer from every
one the earlier records give (the coordination store of
[ADR-0003](0003-fleet-agreement-state-lives-in-coordination.md), the compacted
changelog of [ADR-0014](0014-a-compacted-changelog-is-the-fourth-answer.md) and
the shared store of
[ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md)),
and it is written down as one so that nobody later "fixes" it into durable
storage.

- **A roster lives in the memory of the node holding the sockets.** A browser
  tells its node its focus and activity in a client frame on the socket,
  throttled at the client and coalesced by the node, each room on two clocks
  (its sockets' fan-out and its frames to the fleet), so a move crosses nodes
  in one short window on each side. The frame changes no
  company state, which is why it does not contradict the read-only socket of
  [ADR-0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md):
  that rule protects writes a lost answer must be able to retry, and a lost
  presence frame is simply superseded by the next one.
- **Nodes tell each other on an ephemeral channel.** Two queue verbs,
  `Broadcast` and `Listen`, are core publish and subscribe on the broker: at
  most once, unordered, no stream, no consumer, no acknowledgement, and
  refused with `queue.ErrUnavailable` when the broker cannot be reached, so a
  frame is never buffered into a reconnect. A node listens on an artifact's
  presence subject only while one of its sockets is in that artifact's room.
- **A roster heals itself.** Each node rebroadcasts its local roster every
  fifteen seconds; an entry expires forty-five seconds after the last frame
  that named it, which is three missed rebroadcasts; and a node gaining its
  first member of a room asks its peers once, within the fleet's read budget,
  and shows each answer as it arrives, rather than waiting fifteen seconds.
- **A frame names a person, so it is sealed.** Every broadcast frame is sealed
  under the fleet keyring, in a key domain of its own, with a sixty-second
  validity inside the seal, through a one-way form of `internal/signedask`. A
  frame the fleet did not seal shows nobody.
- **Large rooms degrade by design.** Past fifty members a room presents the
  fifty most recently active individually plus counts, and returns to full
  rosters below forty.
- **Agents are present.** A tool session reading or writing an artifact
  announces itself as a person's tab does, so a person sees which agent is
  editing which row.

Who may be in a room is decided like a read: joining asks `get_artifact`,
three-valued, and the decision is taken again on every credential change and
every share, unshare, trash, transfer and purge of that artifact. A refusal
withdraws the room on a socket that stays open.

## Why the obvious alternatives are wrong

**Heartbeats in the coordination store**, a key per viewer with a time to live,
make every cursor move a replicated consensus write, and coordination's
retention is a bucket's age rather than a per-write expiry
([ADR-0003](0003-fleet-agreement-state-lives-in-coordination.md)), so the store
would keep, and then sweep, a record of every glance.

**The event stream** would persist a durable message per cursor move, with a
consumer, an acknowledgement and a retention term, for a fact nobody will ever
read back.

**Each node polling every other** turns a room into many-to-many requests on a
timer, which is the broadcast with worse latency and more traffic.

**Unsigned frames**, as the inbox relay sends, would let anything on the broker
put a colleague in a room they are not in. The relay is unsigned for its stated
reason, that a forged answer only causes a re-read; a presence frame causes a
display, so that reasoning does not carry over.

## Amendments to earlier records

[ADR-0001](0001-one-broker-carries-the-stream-and-the-coordination-store.md):
`Broadcast` and `Listen` join `Ask` and `Serve` as the broker's ephemeral verbs,
certified on both backends by `queuetest`, and nothing above `internal/queue`
branches on which backend carries them.

[ADR-0031](0031-every-state-log-record-is-signed.md): signing reaches beyond
state-log records to presence frames, sealed one way under the same keyring,
because a frame's reader acts on who it names.

## What the spikes confirm

Spike S6 measures presence fan-out: frames a second and bytes on the busiest
node for a thirty-member room, and the cost of the fifty-member degradation.
The method and the numbers are in
[`docs/reference/artifact-capacity.md`](../docs/reference/artifact-capacity.md).

## What this does not decide

It does not decide **what changed, by whom and when**: that is the durable
history of the artifacts and datastore domains. It does not decide **who has
opened an artifact**: "seen by" is a durable, debounced record a company may
switch off, and it is not presence. And it does not decide **a view audit
log**, which is not part of this release.
