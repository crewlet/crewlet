# ADR-0088: A node's alarm transition is the record its wake derives from

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestATrackerHandsOnEveryTransitionOnce`, `internal/engine.TestAnAlarmTransitionIsPublishedUntilTheBrokerTakesIt`, `internal/alarmwake.TestAnAlarmTransitionWakesEachGrantedSeatOnce`
- **Cost-when-tried:** not tried here: before this record no alarm woke anybody, and a seat learned the deployment's health only by asking. The cost the decision avoids is the one each alternative below names — a flap lost between two reads of a current value, a wake that never happens because the log it would be written on is the one the alarm says is full, and a wake lost to a broker blip with a logged line and no retry (ADR-0012's first failure mode).
- **Tag-status:** unreleased

## The decision

An alarm TRANSITION — one instance of the alarm table firing, clearing, or
found firing by a tracker's first evaluation (`firing_at_boot`) — is handed on
by the tracker that observed it (`Tracker.OnTransition`) as one
`AlarmTransition`, whose id the tracker derives from the node, the alarm, the
log a per-log alarm stands on, the change and the instant the instance was
raised. The node publishes it as an inbound delivery (`topics.NotificationsInbound`,
source `alarms`) from an outbox in its memory, retried until the broker
acknowledges it, bounded, its oldest dropped and counted first. THE PUBLISH IS
THE WRITE: the delivery on the inbound stream is the durable record a wake is
derived from, and the notify spine's fleet-wide consumer group derives the
wakes on whichever node takes it — one for every agent seat whose role carries
`health_tools`, each with an id derived from the transition and the seat, so a
republish collapses into the wakes it already made. Nothing commits beside the
publish, so no publish trails a commit.

A transition its node did not live to publish is not recovered, and need not
be: what is firing NOW is every node's own heartbeat's to say (`fleet_health`),
which a wake only prompts a seat to read, and a restarted node's first
evaluation publishes what is still firing as `firing_at_boot`.

## Why the obvious alternative is wrong

A coordination row per firing alarm, watched: the coordination store keeps a
current value, so a fire and its clear inside one watch tick are one value and
the flap is lost, and a watch is not a consumer group — the wake would be a
duty behind a lease, which a lease flap stalls (ADR-0012 refused the duty for
that). A record on a state log: an alarm is one node's fact about itself, not
company state every data node holds identically, and the alarms most worth a
wake are about the logs themselves — a log at its ceiling refuses exactly the
record that would say so. And a publish from the tracker's own goroutine with
nothing behind it: a broker that blinks loses the wake with a logged line,
which is the failure ADR-0012 was written against.

## Amendments to earlier records

[ADR-0012](0012-a-wake-is-derived-not-published.md): a wake whose record is
itself a publish — a node's alarm transition, which no commit precedes — is
derived from the delivery that publish writes. Its durable record is the
inbound stream's message and its consumer group the notify spine's; with one
publisher there is no change to claim, so the derived wake id is the layer that
collapses a republish.

## What this does not decide

Which alarms exist and where each fires (the table's, ADR-0015), who holds the
health grant (the authority table's seat grant), and what a woken seat does: a
wake is a pointer at the fleet's current state, never a remedy carried out on
its own. A second tracker — a node's own table beside the logs' — hands its
transitions on the same way, through the same hook.
