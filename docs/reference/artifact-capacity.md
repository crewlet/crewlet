# Artifact Capacity

How far [artifacts](../concepts/artifacts.md) go on one deployment, what the
engine promises about their speed, and how each figure is measured. The bounds
themselves are defined once, in `internal/artifacts` (`limits.go`), and
enforced from there; this page is the evidence behind them and the method
anybody can rerun.

---

## Service objectives

The engine measures each of these against a named constant in
`internal/objectives`, so an alarm, a dashboard and this page read the same
number.

| Measure | A good event | Objective over 30 days |
|---|---|---|
| Publish success | A publish answered `2xx`, or a `4xx` the client caused, within 30 seconds excluding the transfer | 99.9 percent |
| Publish latency | The publish request, excluding the transfer | p95 under 2 seconds |
| View first byte | An entry document's first byte under 300 ms, and not a `5xx` | 99.9 percent |
| Artifact ready | From the ticket's mint to the page's handshake, under 3 seconds, as the browser reports it | 99.5 percent |
| Datastore op | A write's verdict on a fleet within 250 ms | p95, and 99.9 percent within 5 seconds |
| Change visibility | From the applier's commit to the socket frame | p99 under 1 second |
| Presence | Best effort | none |

"Artifact ready" is reported by the dashboard (`POST /artifacts/ready`)
because no server metric sees a slow DNS answer, a blocked frame or a page
that never finishes its handshake. It is the `crewlet.artifacts.ready.duration`
instrument (see [Metrics](metrics.md)).

---

## What bounds the scale

- **The datastore's write rate is a company figure, not a node figure.** Every
  data node applies every record of every log through one writer into one
  replicated file, so adding nodes adds readers and availability, never write
  throughput. A datastore write is one record whose apply touches one document,
  its field index rows and one history row; the limit that matters is rows a
  second, shared with every other domain.
- **Every datastore byte is held on every data node** and rides every snapshot
  and backup, which is why `artifacts.limits.data_bytes` defaults to 1 GiB and
  the footprint quota counts real bytes (bodies, field index entries and
  history).
- **On the `nats` object store every broker member holds every object**, and a
  replaced member re-replicates them all, which is why the bucket has a
  reservation and why large estates belong on `s3`.
- **Presence costs memory and frames, never storage**: a map per room on each
  node holding its sockets, and a few hundred bytes per broadcast.

---

## The spikes

Each spike is a reproducible Go benchmark or test in
`internal/artifactspike`, run against the real publisher, applier, object
store and queue rather than a model of them, except S1, which is a browser
procedure. Run them on a machine of your own with:

```sh
go test -run '^$' -bench . -benchtime 1x -timeout 60m ./internal/artifactspike/...
```

Each result is recorded below its spike with the commit it ran at, the
hardware, the broker topology and the object store backend. A measurement that
contradicts a bound in `internal/artifacts/limits.go` changes that constant,
and the record citing it, in the same change; it never changes a decision.

### S1: origin isolation

**Measures**, in current Chrome, Firefox and Safari, by hand: with a per-artifact
origin, that two artifacts cannot read each other's storage and that the
dashboard's cookie is not sent on an artifact's requests; with the shared
mode, that a document's origin is opaque, that storage throws, and that the
handshake binds the port to the frame that sent the nonce.

**Target**: every check holds in all three browsers.

### S2 and DS-1: datastore write rate and contention

**Measures** sustained datastore writes, in rows a second, through the real
publisher and applier: one store, then ten stores at once, on one node and on a
three-member embedded fleet with `sync: always`, with writes shaped like a
review sheet (one to fifteen fields, a history row each) and like a bulk paste
(100 ops in one write). The same harness then runs thirty concurrent writers
on one store, half of them guarded by version and half blind, and records each
verdict and its latency.

**Target**: a thirty-person review at its peak (5 to 15 writes a second on one
store, bursts of 50) applied with headroom on the smallest supported fleet,
and the per-node rate limits (20 writes a second per principal per store,
bursts of 100, 200 a second per store) set below the measured ceiling. Blind
writes never conflict, guarded writes conflict only when the document moved,
and the datastore op objective holds under contention.

### DS-2: datastore footprint

**Measures** the real on-disk bytes of a 10 000-document store with a week of
history, in the replicated file and in a snapshot, against the footprint the
applier meters for the same store.

**Target**: the metered footprint within 10 percent of the measured bytes, so
`data_store_bytes` and `data_bytes` mean what they say.

### S3: streaming from a sandbox

**Measures** a 64 MiB publish streamed out of a sandbox through the engine's
transfer leg into both object store backends, at the object store's pace
floor.

**Target**: the transfer finishes far inside the twelve hours a key may wait
for its record, and memory on the node stays flat (nothing buffered whole).

### S4: the linter and the strict script policy

**Measures** the publish-time linter and the served script policy over a
committed corpus of 200 model-shaped pages: how many publish, how many are refused
and why, and how many render with a console policy violation.

**Target**: the share of pages that break under the strict policy reported per
cause, each cause either fixed in the linter or stated in the authoring guide.

### S5: edits

**Measures** how often `old_str` and `new_str` edits from real republishes
apply exactly once, and the bytes they save against whole-file publishes.

**Target**: the refusal causes reported, and the edit limit (64 per publish)
confirmed against the distribution.

### S6: presence fan-out

**Measures** frames a second and bytes on the busiest node for a thirty-member
room spread over three nodes, and the cost of a room past fifty members
presenting fifty plus counts.

**Target**: the per-artifact coalescing (5 frames a second) and the client
throttle (10 a second) keep the busiest node's socket traffic bounded, and the
change visibility objective holds while presence runs.

### S7: the object store under a publish

**Measures** the `nats` object bucket's ceiling (the reservation derived from
`stream.store_max_bytes`) and, on a three-member embedded fleet with
`sync: always`, the state-log append latency at the 99th percentile while a
64 MiB publish streams into the `nats` bucket, and member replacement time at
10, 50 and 100 GiB of objects.

**Target**: append latency within the datastore op objective during the
publish, and a recommended ceiling for `nats` object bytes past which `s3` is
the advice.
