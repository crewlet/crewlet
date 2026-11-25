# ADR-0173: A node takes or keeps a hold only while its data path answers

- **Status:** accepted
- **Authority:** `internal/queue/jetstream`
- **Enforced-by:**
  - `internal/queue/jetstream/jetstreamtest.TestTheDataPathOfALeafOnACutOffMemberIsRefused`
  - `internal/queue/jetstream.TestTheDataPathIsProvenEndToEnd`
  - `internal/queue/jetstream.TestALeaseLinkThatStopsAnsweringIsMoved`
  - `internal/queue/jetstream.TestALeaseLinkWhoseFirstHopIsDownIsNotMoved`
  - `internal/queue/jetstream.TestALeaseLinkIsJudgedOnlyByProbesThatCrossedIt`
  - `internal/queue/jetstream.TestALeaseLinkBackFromALongCutStaysWhereItLands`
  - `internal/queue/jetstream.TestRelinkMovesTheLeaseLinkAlone`
  - `internal/coord/kv.TestACarrierRefusesAHoldBeforeAnythingIsSent`
  - `internal/engine.TestAHoldIsRefusedWhileTheDataPathIsDead`
  - `internal/engine.TestALeafsHoldIsRefusedOnceItsDataLinkFallsSilent`
- **Measured:** a probe every 5 s of about 120 bytes, answered in about 700 (164 B/s, 0.5% of the floor); refused 5.03 s after a probe unanswered while the data link carried nothing, 30 s after a success where the fleet answers "not available", and at 246 s on the shipped pings (two minutes, two out) where the link moves and nothing answers; so a data flow that dies silently stops renewals within about ten seconds and frees the node's seats within about 55 s at the shipped 45 s lease, and a member cut off from its peers within 30 s and a probe period, 80 s with the lease. A lease link on a member that answers nothing is moved 5.03 s after its first unanswered probe, 5 to 10 s into the silence, or at the first "not available" 30 s past its last success; at the 15 s floor lease, renewed every 4 s, a lease outlives its last renewal by 11 to 15 s, so a move and the leaf's one-second redial mostly leave a renewal room to land on the new link.
- **Cost-when-tried:** not tried in the tree. A gate on the first hop alone keeps a cut-off member's leaves holding seats for the whole partition, and a silently dead flow's until the leaf's pings close it, up to six minutes; co-locating the two links needs connection steering the broker does not offer.
- **Tag-status:** unreleased

## The decision

**A node takes or keeps a hold only while its data path answers end to end.**
Its leases ride a link of their own (ADR-0174), and the two links land on
members independently, so a lease link through one member says nothing of a
data link through another, cut off from its peers. The queue
(`internal/queue/jetstream`'s `Queue.DataPath`) probes the fleet's JetStream
over its own connection, the path the work takes, and refuses:

- at once while the first hop is down (the connection, or a leaf's data link);
- once a probe has gone unanswered past a request's term while the data link
  carried nothing (ADR-0155's silence);
- once the fleet has answered "not available" for 30 s with no success;
- once a probe has gone unanswered past the link's own ping tolerance, moving
  or not.

The KV lease store (`internal/coord/kv`'s `Config.Carrier`) asks it before a
claim or a renewal, refusing as unknown before anything is sent; a release and
every read are never refused, since giving a hold up is always right. The
engine (`internal/engine`) wires it into the KV store alone: in-process leases
have no link to fail apart on.

**The lease link is probed and moved the same way.** A member cut off from
its peers keeps answering a leaf's pings, and a leaf remote has no ping of its
own, so the lease connection's link gets the same probe, unmetered. Refused,
it is dropped so its remote dials the next member (an external dial
reconnects to the next server), and `Queue.LeaseLink`, which `/ready` reads,
reports the verdict. Only a probe that crossed a link judges it: none is sent
while the first hop is down, a leaf's own no-responders is not the fleet's
answer, and a link back up, moved or redialled, is proven afresh, its "not
available" budget starting then.

## Why the obvious alternative is wrong

**The first hop alone.** A data link to a member cut off from its peers reads
linked while every stream operation through it fails, and the lease link
through another member keeps renewing: alive to its peers, deaf to its work,
for the whole partition.

**Co-locating the two links.** It needs a fixed URL order, sending every leaf
to one member first; the links still part on a lone reset of the data link,
and converging them means kicking the lease link until both land together,
the leases failing during each kick. It also leaves the silently dead flow.

## What this does not decide

A link that moves while its probe is never answered is refused only at the
link's own ping tolerance, since a probe queued behind a crossing is not a
dead path; a buffering proxy on the leaf port, which takes bytes the far side
never receives, reads as moving. That tolerance is the leaf's own ping cut,
read from the options in force, so #1288's tolerance moves the 246 s with it.
A lease link refused while every member answers nothing is moved to each in
turn, each judged afresh, and the node's leases lapse on their TTL as they
would on one link.
