# ADR-0155: A wait across a link ends for silence only when the link carries nothing at the floor

- **Status:** accepted
- **Authority:** `internal/queue/jetstream`
- **Enforced-by:**
  - `internal/queue/jetstream.TestALinksMeterCountsWhatCrossesItEachWay`
  - `internal/queue/jetstream.TestALinksMeterStandsStillOnABlackholedLink`
  - `internal/learning/memsync.TestAHydrationBehindADownloadOnItsLinkIsNotRefused`
  - `internal/learning/memsync.TestAHydrationOnASilentLinkIsRefusedForItsSilence`
  - `internal/learning/memsync.TestAReplayHearingNothingOnAMovingLinkIsPlacedAgainPastOneTransportPull`
  - `internal/jsprovision.TestACeilingRunsOnlyWhileItsLinkIsStill`
  - `internal/engine.TestANodesHydrationReadsItsLinksMeter`
  - `internal/objstore/natsobj.TestATransferBehindAMovingLinkIsNotStalled`
  - `internal/queue/jetstream.TestTheDataPathIsProvenEndToEnd`
- **Measured:** the meter reads its link every 250 ms (a quarter of a read term) and keeps a minute of readings; a link is moving at 70% of the floor of 30 s a MiB, about 24.5 KB/s, in either direction, midway between the 40% of background a documented fleet's quiet link carries and the floor a busy one does; one message at the transport's 8 MiB takes 240 s to cross at the floor, and 248 s to arrive with its handling, which caps every hold but the data path's probe, capped at 246 s on the shipped pings (two minutes, two out) with a probe period and a read term; a replay is placed again past 8.5 MB read across a link (one transport message and a 139,810-byte window) and refused at 253 s with nothing heard; a held wait is judged again every 250 ms (a request) or every second (a replay), so a link that falls still ends it within its span and that judgment of its last byte
- **Cost-when-tried:** a replay timed by words alone was refused behind one download in flight on its own link (15.5 s of it against a 20.5 s stall at the shipped lease), and behind any event past about 0.67 MiB; at the floor lease the stall was 10.2 s, so one download refused it there too
- **Tag-status:** unreleased

## The decision

**Across a link, a wait that hears nothing is silence only while the link
itself carries nothing.** A node whose connection to the broker crosses a link
— a leaf's link to the members, any node's dial to an external cluster —
meters it at its own socket (`LinkMeter`): the bytes read, and the bytes the
far end acknowledged. A wait that would end because nothing was heard for its
span reads the meter first. Where the link did not carry seven tenths of the
floor over that span, in either direction, the wait ends as it did. Where it
did, the wait is held: the reply may be among what the link carries.

**And every hold is capped, by choice, at one message at the transport's
bound** — but the data path's probe (`Queue.DataPath`), held to its link's
own ping tolerance, past which the link is closed as stale whatever it
carries. A wait held past its span is held by at most what one message at the
transport's ceiling takes to cross at the floor (`Timing.Following`'s beyond,
memsync's `quietCap`), and a replay that has watched one such message and one
of its own pulls cross with nothing for it is placed again (memsync's
`quietBytes`). That is not a bound on what may be ahead of a reply: any number
of messages may be (below), beside the node's own bulk, held to its link's one
window (ADR-0161). A reply behind more than the cap allows is cut at the
cap, where it was cut at its span before, and a replay behind more is placed
again needlessly. A held wait is judged again as the meter reads (a request's
every quarter second, a replay's every second, over the span before each
judgment), so a link that falls still ends it within its span and a judgment
of its last byte. A member crosses no link, has no meter, and waits on time
alone.

**Only where nothing on the path terminates TCP.** The meter reads the node's
own socket, so it sees what the next hop took: a proxy that terminates TCP in
front of the far end (a service mesh's sidecar, a load balancer that buffers)
acknowledges what it buffered, and a far end that stopped reading looks, for as
long as that buffer takes to fill, like one that reads. The guides require the
link's port to be left out of mesh capture and kept clear of such proxies
(`docs/guides/satellite-nodes.md`, `docs/guides/deployment.md`); NAT, a
firewall and a VPN that forwards packets do not terminate TCP.

The first readers are a seat's hydration and the ceilings of its hedged
requests, and the object store's transfers across a link: a put's wait for its
acknowledgements, and a download's or a walk's for its next message, whose
consumer is made again once two heartbeats go missing on a still link or the
link has read one message at the transport's bound and a window since it
heard a word (`internal/objstore/natsobj`). A wait that has no meter to read
keeps its time bound; making every request bound across a link read the meter
is a separate change.

## Why the obvious alternative is wrong

**A timeout derived from what may sit ahead.** Messages are not bounded to a
window's crossing (#1035, Route A): an event, a fleet record, an estate
answer each ride whole up to the transport's 8 MiB, and any number of them may
be ahead. A finite bound either refuses a busy link — the seat, again — or,
capped at one ceiling message each way, is eight minutes at the floor.

**The libraries' counters** (`Leafz` `InBytes`/`OutBytes`, a connection's
`InBytes`). nats-server counts a message in once its read loop has parsed all
of it and out when it queues it; nats.go counts a message once it is whole.
For the four minutes a ceiling message crosses the floor every one of them
stands still, so a wait reading them refuses mid-message.

**Movement on any byte.** A quiet link still carries its fleet's presence
reads, pings and heartbeats; read as moving, it would hold a wait whose copy
stopped answering to the caps on every link.

## What this does not decide

What a link's window is, or how many transfers share it (the object store's
and memsync's own docs); what the lease gate makes of the data path's verdict
(ADR for the gate); and the time bounds of requests
that read no meter yet, which keep their own until they are given one.
