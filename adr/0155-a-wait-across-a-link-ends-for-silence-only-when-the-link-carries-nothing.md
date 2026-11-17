# ADR-0155: A wait across a link ends for silence only when the link carries nothing at the floor

- **Status:** accepted
- **Authority:** `internal/queue/jetstream`
- **Enforced-by:** `internal/queue/jetstream.TestALinksMeterCountsWhatCrossesItEachWay`, `internal/queue/jetstream.TestALinksMeterStandsStillOnABlackholedLink`, `internal/learning/memsync.TestAHydrationBehindADownloadOnItsLinkIsNotRefused`, `internal/learning/memsync.TestAHydrationOnASilentLinkIsRefusedForItsSilence`, `internal/learning/memsync.TestAReplayHearingNothingOnAMovingLinkIsPlacedAgainPastOneTransportPull`, `internal/jsprovision.TestACeilingRunsOnlyWhileItsLinkIsStill`, `internal/engine.TestANodesHydrationReadsItsLinksMeter`
- **Measured:** the meter reads its link every 250 ms (a quarter of a read term) and keeps a minute of readings; a link is moving at 70% of the floor of 30 s a MiB, about 24.5 KB/s, in either direction, midway between the 40% of background a documented fleet's quiet link carries and the floor a busy one does; one message at the transport's 8 MiB takes 240 s to cross at the floor, and 248 s to arrive with its handling, which caps every hold; a replay is placed again past 8.9 MB read at the shipped lease (one transport message and a window) and refused at 253 s with nothing heard; a held wait is judged again every 250 ms (a request) or every second (a replay), so a link that falls still ends it within its span and that judgment of its last byte
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

**And every hold is capped by one message at the transport's bound.** No
reply waits behind more than the node's own windowed bulk and one other
message, so a wait held past its span is held by at most what one message at
the transport's ceiling takes to cross at the floor (`Timing.Following`'s
beyond, memsync's `quietCap`), and a replay that has watched that much cross
with nothing for it is placed again (memsync's `quietBytes`). A member crosses
no link, has no meter, and waits on time alone.

The first readers are a seat's hydration and the ceilings of its hedged
requests. A wait that has no meter to read keeps its time bound; making every
request bound across a link read the meter is a separate change.

## Why the obvious alternative is wrong

**A timeout derived from what may sit ahead.** Messages are not bounded to a
heartbeat's crossing (#1035, Route A): an event, a fleet record, an estate
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
and memsync's own docs); what the lease gate refuses on a link that moves but
never answers its probe (ADR for the gate); and the time bounds of requests
that read no meter yet, which keep their own until they are given one.
