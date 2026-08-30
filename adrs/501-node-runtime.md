# adr-501 — NodeRuntime over request/reply

Status: **Accepted** · Applies to: `internal/node`

**Decision: no request/reply.** The owner-only facts worth having fleet-wide
ride on the node presence lease, which is already renewed on a timer and
already read by every node. `NodeRuntime` stays what it is: the seam for what
*this* process can answer about itself.

## The question

`/health` answers about the node that served it. Behind a load balancer a
refresh tells a different story each time, so an operator cannot see the
fleet's in-flight work, drain state or config posture from one place. The
obvious answer is to serve those over NATS request/reply — the lease table
locates the owner, the caller asks it directly — leaving the envelope,
timeouts and authz to be settled.

## Why not

Three costs, and none of them is the implementation.

**Every answer becomes partial.** Some nodes reply, some time out, one is
mid-restart. Every consumer then needs a three-valued render for every field,
and the dashboard's existing rule — an absent precondition is an em dash,
never a zero — would have to be re-derived per node rather than per slice.
A fleet view that showed four nodes and a spinner is worse than one that
shows four nodes and how stale each row is.

**It is a new trust edge.** A request carries an operator's authority across
a node boundary. The receiver either trusts the caller's assertion — which
makes any node able to speak for any operator — or re-verifies, which means
the auth posture is now a distributed property with two places to get it
wrong. Today `/config` is operator-gated in exactly one place.

**It duplicates a mechanism that already works.** Ownership is already
answered fleet-wide from the lease table, deliberately and for this reason:
"the lease table is the one place that knows which node holds what, and every
node reads the same rows." Config posture is already answered fleet-wide from
`config_apply_status`, per node, with the timestamp of each node's last
report. Adding a second, synchronous path to the same question is how the two
come to disagree.

## What we do instead

`coord.Lease.Meta` already rides on the node presence lease and is re-sent on
**every** heartbeat — that is not an addition, it is the existing mechanism
for "what this node IS", carrying roles and labels today. The live facts go
beside them under their own key:

- `in_flight` — turns running on that node
- `draining` — set from the moment a drain begins
- `posture` — serve / wait / shed / isolated / stuck
- `started_at` — that engine's own start, which on a split deployment is a
  different process on a different clock

Freshness is the heartbeat interval, which is exactly the freshness every
other column in that view already has, and the view already prints
`expires_in` so a stale row reads as stale rather than as current.

## What this deliberately does not solve

**Live MCP tools.** A seat's tool surface is derived from the company config
and the MCP servers' own responses, so any node holding the epoch can compute
what it *should* be; the registry's origin grammar already reports what it
*is* locally. Shipping a tool catalogue per node on a heartbeat would put
kilobytes on a lease row to answer a question that has a local answer.

**Seat memory.** It is in the store. Every node reads the same rows.

If a future surface genuinely needs a synchronous answer from one specific
node — an operator action rather than a read, say — that is when to design
the envelope, and it should be designed as an action with a result rather
than as a general RPC over reads that shared state already answers.
