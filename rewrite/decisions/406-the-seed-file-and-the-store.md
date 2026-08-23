# d-406 — `-company` names a seed; the store is what a node serves

**Status:** DECIDED. Written while wiring the reconciler
(`go/internal/engine/reconcile.go`, `go/cmd/crewlet/reconcile.go`).
Related: `404` (an epoch is published, never mutated), `docs/concepts/control-plane.md`.

## The question

`crewlet run -company company.yaml` boots a node from a file. The control
plane says a node serves the revision the activation pointer names. Those are
two sources of truth, and something has to reconcile them.

Three answers were available, and two of them are wrong in a way that only
shows up in production.

**"The file wins."** Then a `PUT /config` on one node is invisible to every
other, and a restart silently reverts it. This is the fan-out failure the
control plane exists to remove, reintroduced at the CLI.

**"The store wins and the file is ignored once the store has anything."** Then
an operator edits `company.yaml`, restarts, and *nothing happens* — with
nothing anywhere saying why. This repository has removed that failure shape
repeatedly; adding it back at the most-used entry point is worse than either
alternative.

## The rule

**The file is a SEED, reconciled into the store like any other change.** On
boot, the node compares the file against the active revision's *opened*
document and imports it as a new revision when they differ. Then the reconciler
converges on the pointer, exactly as it would for a revision that arrived any
other way.

So:

- A first run seeds and activates. A single node with a file works with no
  extra command, which is what the quickstart needs.
- A boot with an unchanged file imports nothing. A node restarts many times
  over its life; importing on each would mint a revision per restart and move
  the pointer, so **every peer in the fleet would rebuild its epoch every time
  any node restarted**.
- A boot with an edited file imports once, chained to the revision it replaces.
  The operator's edit takes effect and the history says what it replaced.
- A `PUT /config` on any node reaches every node, because the pointer is the
  authority and the file only ever *proposes*.

The one configuration this handles badly is two nodes holding *different*
files: each imports on boot and the other re-imports on its next one. That is a
genuine misconfiguration rather than a supported mode, and it is visible — the
revision list churns and every import logs its parent.

## Comparison is against the OPENED document

The seed compares the file's JSON against `secrets.Open(cipher, active.Payload)`,
never against the stored bytes. With a keyring configured the stored form is
ciphertext with a fresh nonce, so it differs on every seal: a byte comparison
would import a new revision on **every single boot**, move the pointer, and
make the whole fleet rebuild its epoch each time any node restarted.

A node whose keyring cannot open the active revision **refuses to seed** rather
than importing over it. Seeding there would replace a company nobody can
decrypt with one this node happens to have on disk — silently, on a restart.

## Ordering at boot

```
engine.New (file company)  →  seed  →  one reconcile tick  →  engine.Start  →  serve  →  poll loop
```

The tick runs **before** `Start`, so seats are claimed under the epoch the node
will actually serve rather than under the file's and then moved. Its failure is
not fatal: a node that cannot reach the current revision still serves the one
it has, which is the whole point of publishing an epoch rather than mutating
one.

The poll loop starts **after** the node is serving. A reconcile landing
mid-boot would apply an epoch to a node that has claimed nothing yet.

## Two readers for one document

The stored form is JSON produced by marshalling a parsed `config.Company`, and
it is read by `config.DecodeCompany` rather than `config.ParseCompany`. They
differ deliberately in two ways:

- The stored form **carries `providers.llm_order`**, the declaration order of a
  Go map — recoverable only while the YAML document exists, and written into
  the stored form precisely so a node booting from a revision resolves an
  unpinned seat to the same model the authoring node did. The YAML reader
  rejects it as an unknown setting.
- The stored reader is **lenient about unknown fields** where the authored one
  fails closed. A typo in a file a person wrote is a mistake to catch at the
  door; an unrecognised key in a stored revision is a peer running a newer
  build, and rejecting that makes a mixed-version fleet an outage in the older
  direction.

Both are validated. `${VAR}` references stay verbatim through all of it — they
are resolved where a provider, transport or MCP server is constructed, which is
what makes re-activating an unchanged revision pick up a rotated credential
rather than rebuild identical values.

## What `degraded` means here, and why it is not reachable yet

The control plane's three outcomes are `ok` / `error` / `degraded`, and only
`ok` counts as converged. The apply path reports the first two: a revision is
built before anything is published, so a revision that cannot be built is
refused with the previous epoch still current and still correct. There is no
rollback path because there was no mutation.

`degraded` — the apply failed *after* a restart-required subsystem was mutated,
so rollback could not restore it — **is not reachable in this build**, and
saying so is more useful than a hook with nothing behind it. It becomes
reachable when the first subsystem that cannot be un-applied is wired: the
per-role MCP children (Phase 6) and the notification transports (Phase 7). Both
are applied last when they arrive, so the window in which `degraded` is
reachable stays as small as the ordering can make it.

## `TicksBehind` is zero, deliberately

`configplane.DecidePosture` accepts a `TicksBehind` so a reconciler whose apply
is *asynchronous* can distinguish "behind and still working on it" from "behind
and stalled". This reconciler applies synchronously inside its tick, so by the
time a posture is asked for the node has either reached the epoch or recorded a
failure — and the failure is what `SelfStatus` already carries. A counter
incremented beside it would move only when `SelfStatus` was already `error`, so
it could never change a decision. Mutation testing found exactly that: removing
the increment changed no outcome, so the counter went rather than acquiring a
test that pinned nothing.
