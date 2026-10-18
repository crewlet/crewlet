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
| Presence | Best effort (a join shown within 1 second, a cursor across nodes within 150 ms) | none |

"Artifact ready" is reported by the dashboard (`POST /artifacts/ready`)
because no server metric sees a slow DNS answer, a blocked frame or a page
that never finishes its handshake. It is the `crewlet.artifacts.ready.duration`
instrument (see [Metrics](metrics.md)).

---

## What bounds the scale

- **The datastore's write rate is a company figure, not a node figure.** Every
  data node applies every record of every log through one writer into one
  replicated file, so adding nodes adds readers and availability, never write
  throughput. A datastore write is one record whose apply touches its
  documents, their field index rows and one history row; the limit that
  matters is rows a second, shared with every other domain. Measured (DS-1),
  a one-cell edit of a twelve-column row writes 21 rows and a pasted block of
  100 rows writes 2,804, because a changed document's whole field index is
  rewritten.
- **Small records are bounded by the fsync, large ones by the applier.** With
  `sync: always`, every record is one fsynced append on every broker member
  and one fsynced commit on every data node, so a node takes a few hundred
  small records a second whatever their size; past that, rows cost CPU.
- **Every datastore byte is held on every data node** and rides every snapshot
  and backup, which is why `artifacts.limits.data_bytes` defaults to 1 GiB and
  the footprint quota counts real bytes. Measured (DS-2), the file holds about
  twice what the applier meters today; see the findings below.
- **On the `nats` object store every broker member holds every object**, and a
  replaced member re-replicates them all, which is why the bucket has a
  reservation and why large estates belong on `s3`. Measured (S7), each object
  held costs a member about 550 bytes of memory whatever its size, and a large
  publish slows the write-ahead log it shares disks with.
- **Presence costs memory and frames, never storage**: a map per room on each
  node holding its sockets, and a few hundred bytes per broadcast. Measured
  (S6), a room of 200 viewers costs the fleet under a tenth of one core.

---

## The spikes

Each spike is a reproducible Go benchmark or test in
`internal/artifactspike`, run against the real publisher, applier, object
store, queue and hubs rather than a model of them. Two parts are not Go: S1,
which is a browser procedure, and the browser half of S4. Run them on a
machine of your own with:

```sh
go test -run '^$' -bench . -benchtime 1x -timeout 90m ./internal/artifactspike/...
```

`-bench DS1`, `-bench S6` and so on run one spike. A full run takes about
fifteen minutes and under 1 GiB of temporary disk (every broker member is held
to 1 GiB and every run's directories are removed when it ends). A benchmark
reports its figures as custom metrics (`rows/s`, `rtt-p95-ms`, ...); quantiles
are exact, read from every sample rather than estimated from a histogram.

A measurement that contradicts a bound in `internal/artifacts/limits.go` is
reported with the measured value and the constant is changed, with the record
citing it, by the change that owns it; the spikes never change a decision.

### The run recorded here

| | |
|---|---|
| Commit | `feat/artifacts-p15` at `2033e8132` (S6 rerun at `21f6024a0`, after the presence fix below; S7 memory at `beb07f219`) |
| Machine | Apple M1 Pro, 8 cores, 16 GiB, macOS 27.0.1, internal SSD |
| Fsync | `sync: always`; on darwin every fsync is a full flush to the medium (`F_FULLFSYNC`), several milliseconds each |
| Topology | `solo`: one embedded broker and one data node. `fleet3`: a three-member embedded cluster, every stream at three replicas, a data node on each member, all in one process on one disk |
| Load | The machine was shared with other work (load average around six) |

**These are development-machine figures, not the reference.** Darwin's full
flush makes every fsync an order of magnitude slower than a Linux server's,
and a three-member fleet on one disk shares one device's flushes three ways,
where a real fleet has a disk per member. Read them as a floor and as the
shape of each cost; the bounds are confirmed by the reference run (Linux, gp3
volumes at 3,000 IOPS, one member per host), whose procedure is at the end of
this page.

### S1: origin isolation

**Measures**, in current Chrome, Firefox and Safari, by hand: with a per-artifact
origin, that two artifacts cannot read each other's storage and that the
dashboard's cookie is not sent on an artifact's requests; with the shared
mode, that a document's origin is opaque, that storage throws, and that the
handshake binds the port to the frame that sent the nonce.

**Target**: every check holds in all three browsers. The procedure is below
(not run for this record).

### S2 and DS-1: datastore write rate and contention

**Measures** sustained datastore writes through the real publisher and
applier (`BenchmarkDS1WriteRate`): 32 closed-loop writers for 8 seconds, each
waiting for its own verdict, on one store and on ten, on one node and on the
three-member fleet, in three shapes of a twelve-column review sheet seeded
with 1,000 rows: one cell (a merge of one field), one row (a merge of eleven
fields) and a paste (100 rows, six fields each, the largest write a store
takes). Rows are the runner's own count (`crewlet.statelog.apply.batch.rows`),
and the slowest node's rate is the fleet's. The writer's per-node rate guard is
lifted for the run (its clock runs ahead), because the guard is set below the
ceiling this spike measures.

| Topology | Shape | Stores | Records/s | Rows/s | Rows/record | CPU ms/record | Round trip p50 / p95 / p99 (ms) |
|---|---|---|---|---|---|---|---|
| solo | cell | 1 | 148 | 3,136 | 21 | 5.9 | 230 / 306 / 321 |
| solo | cell | 10 | 154 | 3,317 | 21 | 6.2 | 200 / 311 / 368 |
| solo | row | 1 | 159 | 5,092 | 32 | 7.1 | 187 / 309 / 332 |
| solo | row | 10 | 138 | 4,421 | 32 | 7.3 | 219 / 363 / 392 |
| solo | paste | 1 | 4.7 | 13,262 | 2,804 | 227 | 5,013 / 5,175 / 5,179 |
| solo | paste | 10 | 4.4 | 12,237 | 2,804 | 228 | 5,017 / 5,800 / 5,801 |
| fleet3 | cell | 1 | 56 | 1,159 | 21 | 17 | 558 / 907 / 1,195 |
| fleet3 | cell | 10 | 56 | 1,164 | 21 | 18 | 484 / 1,099 / 1,200 |
| fleet3 | row | 1 | 57 | 1,837 | 32 | 19 | 518 / 876 / 1,004 |
| fleet3 | row | 10 | 55 | 1,770 | 32 | 21 | 530 / 979 / 1,088 |
| fleet3 | paste | 1 | 4.3 | 12,065 | 2,804 | 688 | 5,033 / 5,416 / 5,442 |
| fleet3 | paste | 10 | 4.3 | 12,130 | 2,804 | 691 | 5,032 / 5,430 / 5,472 |

CPU on the fleet is the whole fleet's (three brokers and three appliers) per
record. The paste rows' round trips sit at five seconds because 32 writers
queue more records than the applier drains in the writer's resolve budget, so
a verdict answers `pending` (54 to 56 of each run's writes did) and is read
later: nothing is lost, and the round trip measures the queue.

A second sample of the one-cell fleet row gave 54 records/s and a p95 of
1,066 ms.

`BenchmarkDS1Review` is the review at its peak (plan section 9.1) on the
three-member fleet, open-loop for 20 seconds: 50 editors each editing a cell
every two seconds on average, a fifth of the edits on one hot row.

| Writes/s | Round trip p50 / p95 / p99 (ms) | Within 250 ms |
|---|---|---|
| 24.1 | 383 / 931 / 1,257 | 17.7 percent |
| 24.1 (second sample) | 357 / 962 / 1,341 | 20.4 percent |

`BenchmarkDS1Contention` is 30 writers on ten rows of one store for 10
seconds, half reading a row and writing it back guarded by the version read,
half writing blind; it fails if a blind write ever conflicts.

| Half | Applied | Conflict | Round trip p50 / p95 / p99 (ms) |
|---|---|---|---|
| Guarded | 88 | 195 | 489 / 1,018 / 1,224 |
| Blind | 297 | 0 | 482 / 977 / 1,161 |
| Guarded (second sample) | 78 | 206 | 527 / 870 / 1,213 |
| Blind (second sample) | 293 | 0 | 504 / 873 / 1,210 |

**Reading.** The thirty-person review (5 to 15 writes a second, bursts of 50)
and the plan's 25 records a second both fit inside the fleet's measured rate
(56 a second) on this machine, so throughput holds with about twice the
headroom; latency does not: the datastore op objective (p95 within 250 ms) is
missed by a factor of four, because each write waits for a quorum of members
to flush its append and for its own node's applier to flush the commit, darwin's
flushes are slow, and here three members and three appliers share one disk. Blind writes never
conflicted; guarded writes on ten rows written by thirty people conflicted on
69 to 73 percent of attempts, which is the correct verdict for a version that moved
and the reason a sheet's cell edit is blind and a long text is guarded.

**Target**: a thirty-person review at its peak applied with headroom on the
smallest supported fleet, and the per-node rate limits (20 writes a second per
principal per store, bursts of 100, 200 a second per store) set below the
measured ceiling; blind writes never conflict, guarded writes conflict only
when the document moved, and the datastore op objective holds under
contention. **On this machine**: headroom holds, blind writes hold, the
objective does not, and the per-store limit (200 cost units a second, a
one-cell edit being one unit) is above the measured ceiling for small records
(140 to 160 a second on one node, 55 on the fleet). See the findings.

### DS-2: datastore footprint

**Measures** (`BenchmarkDS2Footprint`) one store of 10,000 review rows, filled
in 100 pastes and then given a week of edits (2,000 cell edits and 500 row
rewrites): its catalogue row as the applier metered it, the bytes it added to
the live replicated file and its write-ahead log, and to a snapshot (`VACUUM
INTO`, which is what a backup is), each against the same file before the store
existed; then each table's share of the snapshot, measured by emptying the
tables one at a time.

| Measure | Value |
|---|---|
| Documents | 10,000 |
| Canonical bodies | 3.16 MiB |
| Field index entries | 130,000 |
| History (metered) | 6.21 MiB |
| Metered footprint | 15.32 MiB |
| Snapshot bytes | 31.62 MiB (3,315 bytes a document) |
| Live file and log | 32.68 MiB |
| Snapshot / metered | 2.06 |
| Snapshot bytes per MiB of canonical bodies | 10.0 MiB |

| Table | Snapshot share | Against what is metered |
|---|---|---|
| `artifact_data_fields` | 18.89 MiB | 152 bytes an entry, metered at 48 |
| `artifact_data_writes` and `artifact_data_touches` | 7.21 MiB | 1.16 times the metered history |
| `artifact_data_docs` | 4.97 MiB | 1.57 times the bodies |

**Reading.** The field index is the store's largest cost: every entry is a row
keyed by five columns (stored twice, since the estate's tables carry a rowid)
and appears again in the numeric and text indexes. A store therefore holds
about 10 bytes on disk for every byte of document, and the metered footprint
is half the real one.

**Target**: the metered footprint within 10 percent of the measured bytes.
**Missed** by a factor of two; see the findings.

### S3: streaming from a sandbox

**Measures** (`BenchmarkS3StreamPublish`) a 64 MiB publish (four 16 MiB files,
the per-publish ceiling) streamed out of a sandbox through the upload
service's transfer leg (`artifactupload.Service.Transfer`), read 32 KiB at a
time as a network body is, into the `nats` bucket: the wall time from the
intent to the last file stored, the share of the object store's pace
allowance that was (`objstore.PaceFor`, 30 seconds a MiB: 32 minutes for the
publish), and the process heap while it ran and once the broker's caches had
expired.

| Backend | Topology | Seconds | MiB/s | Share of pace | Heap rise / settled |
|---|---|---|---|---|---|
| nats | solo | 2.6 | 24.3 | 0.14 percent | 53 MiB / 5.5 MiB |
| nats | fleet3 | 4.8 | 13.3 | 0.25 percent | 234 MiB / 96 MiB |

The heap is the process's, and the embedded broker runs in it as it does in a
node: the fleet row is three members' worth. The transfer leg itself never
holds a file whole (the bucket is filled 128 KiB a message); the rise is the
broker's message block cache and its replication buffers, which expire.

**Target**: the transfer finishes far inside the twelve hours a key may wait
for its record, and memory on the node stays flat (nothing buffered whole).
**Holds**: under a third of one percent of the allowance, and the settled heap
after a publish is a few MiB a member. The `s3` arm runs against a real S3
endpoint; its procedure is below (not run for this record).

### S4: the linter and the strict script policy

**Measures** (`BenchmarkS4Corpus`, held by `TestTheS4CorpusReadsAsRecorded`)
the publish-time linter and the served script policy over a committed corpus
of model-shaped pages (`internal/artifactspike/testdata/s4`): dashboards,
games, forms, tools and landing pages written the way a model writes them, with
CDN libraries pinned and unpinned, inline handlers, browser storage, remote
APIs, embeds and import maps. Each page goes through the shipped linter with
the CDN copy-in answered locally; each page it publishes is then read against
the policy the artifact origin serves (`internal/api/pagepolicy`) by a static
reading that names the shapes each directive stops. `corpus.json` records
every page's verdict.

**The corpus is 55 pages, not the plan's 200.** It is written by hand to cover
every shape the linter and the policy decide on, at least once and most of
them several times; the share of each shape in what models really publish is
what the browser half below measures on captured pages.

| Outcome | Pages | Share |
|---|---|---|
| Refused by the linter | 12 | 21.8 percent |
| Published, and nothing the policy stops | 26 | 47.3 percent |
| Published, and something the policy stops | 17 | 30.9 percent |
| Published with browser storage (breaks in shared mode only) | 5 | 9.1 percent |
| Published with an import map naming another origin (an escape, below) | 2 | 3.6 percent |

| Refusal | Pages | A typical source |
|---|---|---|
| `remote_script` | 4 | `d3js.org`, `cdn.plot.ly`, an unlisted CDN, a CDN over `http` |
| `unpinned_version` | 3 | `cdn.jsdelivr.net/npm/chart.js`, Tailwind's Play CDN with no version |
| `remote_import` | 2 | `import mermaid from "https://cdn.jsdelivr.net/..."` |
| `form_off_origin` | 1 | a form posting to a form service |
| `javascript_url` | 1 | `<a href="javascript:void(0)">` |
| `refresh_off_origin` | 1 | a meta refresh to another site |

| What the policy stops | Pages | Directive |
|---|---|---|
| A script reaching another origin (fetch, socket, map tiles, audio) | 6 | `connect-src`, `img-src`, `media-src` |
| String evaluation (`eval`, a string timer, lodash's template compiler) | 3 | `script-src` without `'unsafe-eval'` |
| A handler a script writes (`innerHTML`, `setAttribute`) | 2 | `'unsafe-hashes'` admits published handlers only |
| An image or embed from another origin | 2 | `img-src`, `frame-src` |
| An iframe | 1 | `frame-src 'none'` |
| A form that submits by navigating | 1 | `form-action 'none'` |
| `window.open` | 1 | the frame's sandbox |
| A worker started from a URL | 1 | `worker-src blob:` |
| A base element | 1 | `base-uri 'none'` |

The linter takes 120 to 170 microseconds a page.

**Reading.** The CDN rules refuse what models most often write (an unpinned
jsDelivr reference, `d3js.org`, Plotly's own CDN, an ES module import), and
each refusal names the fix. What breaks after publishing is mostly what the
origin is for: a page cannot call another site's API or hotlink its images.
Three shapes break silently and the linter could name them: string evaluation
in the page's own script, handlers written by script, and import maps (which
escape the copy-in rather than break).

**Target**: the share of pages that break under the strict policy reported per
cause, each cause either fixed in the linter or stated in the authoring guide.
**Reported**; the causes the linter does not yet name are in the findings.

### S5: edits

**Measures** (`BenchmarkS5Edits`, held by `TestTheS5EditsApplyAsRecorded`)
republishes through the shipped edit application at the publish's own limits:
39 republishes (`testdata/s5/cases.json`) over the corpus's pages in the
shapes a model's edits take: a unique quote of the page it read, several edits
in one publish, a quote widened until it is unique, a quote too short to be
unique, a quote made from memory (indentation, quotes, entities or wording
drifted), and an edit quoting text an earlier edit of the same publish already
replaced.

| Measure | Value |
|---|---|
| Applied | 25 (64.1 percent) |
| Refused, `old_str` matches nothing | 9 (23.1 percent): every drifted quote and every stale one |
| Refused, `old_str` matches several places | 5 (12.8 percent) |
| Most edits in one publish | 4 |
| Bytes an applied publish's edits carry, against the whole files | 6.6 percent |

**Reading.** The share applied is the committed mix's, not a model's: the set
exists to hold the application's answers to every shape, and it shows each
refusal names the right remedy (quote the page again, or quote more of it).
An applied edit carries one fifteenth of the bytes a whole republish would on
these pages, and less on the 300 KB pages the plan's estimate is about. The
rate a real agent achieves is measured from a live deployment and replayed
through the same harness (procedure below).

**Target**: the refusal causes reported, and the edit limit (64 per publish)
confirmed against the distribution. **Causes reported**; the limit is
confirmed only by captured republishes.

### S6: presence fan-out

**Measures** (`BenchmarkS6PresenceFanOut`) a room of 30, 50 and 200 viewers
spread evenly over the three-member fleet, through the shipped presence hub on
the queue's broadcast and the socket hub's room routing, with every fan-out
pushed to the room as the socket service pushes it. Each viewer moves its
focus once a second (an active reader) or ten times a second (the per-socket
ceiling) for 20 seconds. A probe on the first node moves its focus once a
second and is timed to the moment the last node's sockets are shown it.

| Viewers | Moves/s each | Frames/s per socket | Busiest node to its sockets | Broker per node, sent / heard | Fleet CPU | Join shown everywhere | Cursor across nodes p50 / p95 |
|---|---|---|---|---|---|---|---|
| 30 | 1 | 4.8 | 69 KiB/s | 2.7 / 8.1 KiB/s | 0.030 cores | 0.4 s | 274 / 400 ms |
| 30 | 10 | 4.9 | 306 KiB/s | 10.4 / 31.1 KiB/s | 0.039 cores | 2.0 s | 279 / 402 ms |
| 50 | 1 | 4.8 | 183 KiB/s | 4.0 / 12.1 KiB/s | 0.036 cores | 2.2 s | 86 / 248 ms |
| 50 | 10 | 4.9 | 826 KiB/s | 16.7 / 50.2 KiB/s | 0.043 cores | 2.0 s | 258 / 301 ms (7 of 19 probes never shown) |
| 200 | 1 | 4.9 | 2,587 KiB/s | 13.5 / 40.6 KiB/s | 0.054 cores | 2.2 s | 233 / 288 ms |
| 200 | 10 | 4.9 | 3,339 KiB/s | 64.2 / 192.6 KiB/s | 0.077 cores | 2.0 s | 19 of 19 probes never shown |

Before the presence fix below, four of the six runs took 15 seconds (one
rebroadcast) to show a join on every node.

**Reading.** Coalescing holds every socket to five frames a second whatever
the room does, and the broker carries tens of KiB a second a node, so
presence's CPU and broker cost is negligible. What grows with the room is the
frame: a room past fifty presents its fifty most recent members, and when
everyone moves, the fifty change constantly, so each frame names most of them
again (about 10 KiB a frame, 50 KiB a second a socket at 200 viewers), and one
viewer's move is shown only for the moment it is among the most recent (every
probe move in the busiest room, and 7 of 19 in a room of 51, were displaced
before the other node's next frame). A join is
shown everywhere once the join-time ask has waited out its budget (2 seconds,
`eventfan.FleetReadBudget`), and a cursor crosses nodes in two coalescing
windows, one on each node (200 ms each), so its p50 lands anywhere between a
few tens of ms and 400 ms by where in each window the move falls.

**Target**: the per-artifact coalescing (5 frames a second) and the client
throttle (10 a second) keep the busiest node's socket traffic bounded, and the
change visibility objective holds while presence runs. **Holds** for frames;
the best-effort presence bounds (join within 1 second, cursor within 150 ms)
are missed by construction, which the findings say how to close.

### S7: the object store under a publish

**Measures** three things on the `nats` bucket:

- `BenchmarkS7WALUnderPublish`: on the three-member fleet, the append latency
  of a state log (a probe stream appended to 20 times a second) and the round
  trip of datastore writes (5 a second), before and while two 64 MiB
  publishes stream into the bucket through the transfer leg.
- `BenchmarkS7BucketCeiling`: how much content a bucket reservation holds
  before the broker refuses a put with `objstore.ErrFull`.
- `BenchmarkS7BrokerMemoryPerObject`: the heap each object held costs the
  process, the broker included, over 100,000 one-byte objects (without the
  fsync, which the figure does not depend on).

| Load | Before the publish p50 / p95 / p99 | During the publish p50 / p95 / p99 |
|---|---|---|
| State-log append | 31 / 68 / 207 ms | 73 / 219 / 321 ms |
| Datastore round trip | 111 / 240 / 366 ms | 196 / 1,835 / 3,035 ms |
| State-log append (second sample) | 32 / 62 / 76 ms | 70 / 196 / 270 ms |
| Datastore round trip (second sample) | 107 / 159 / 166 ms | 169 / 807 / 1,170 ms |

Each 64 MiB publish took 16 to 17 seconds under that load (about 4 MiB/s). In an
earlier attempt at four times the probe and datastore load, both publishes
failed: a 16 MiB put was refused `nats: timeout waiting for ack` (see the
findings).

| Files | Reservation | Content held | Overhead per object |
|---|---|---|---|
| 1 MiB | 64 MiB | 98.4 percent (63 objects) | 16.6 KiB |
| 4 KiB | 8 MiB | 89.6 percent (1,835 objects) | 475 bytes |

| Memory per object held | Per million objects |
|---|---|
| about 550 bytes (552 and 550 in two runs) | about 525 MiB, on every member |

**Reading.** On shared disks the bucket and the write-ahead log contend for
the same flushes: the log's own appends take two to three times as long, and
datastore writes, several flushes deep, slow by five to ten times at the tail. A
reservation holds what it says for files of a few KiB and up (90 to 98
percent), and a member pays about half a KiB of memory per object it holds,
however small, so an estate of millions of small files costs gigabytes of
memory on every member before its bytes matter.

**Target**: append latency within the datastore op objective during the
publish, and a recommended ceiling for `nats` object bytes past which `s3` is
the advice. **On this machine** the log's appends stay inside 250 ms at p95
and datastore writes do not. The recommendation: keep the default `nats`
bucket for estates up to tens of GiB and a few hundred thousand objects, and
move to `s3` past either, or wherever the bucket and the logs share a disk
under a datastore-heavy load. Member replacement at 10, 50 and 100 GiB is a
procedure (below).

---

## Findings

Each finding names the package that owns the fix. A bound in
`internal/artifacts/limits.go` or another package's constant is reported here
with the measured value and left to its owner, except where this change fixed
it.

1. **Presence joins waited a rebroadcast (fixed in this change,
   `internal/presence`).** The answer to a join-time ask was applied as a full
   replacement after the ask's budget, deleting every member the peer gained
   in between until its next rebroadcast: 15 seconds to show a room joined at
   once on three nodes. A frame older than one already heard from its node now
   fills in and never undoes (`TestALateFullFrameFillsInAndNeverUndoes`); the
   same rooms show everybody in 2 seconds.
2. **The metered footprint is half the real one (`internal/artifactdata`).** A
   field index entry costs 152 bytes on disk and is metered at 48
   (`fieldEntryBytes`); document rows cost 1.57 times their bodies and history
   1.16 times what is metered. Either meter the measured costs, or state the
   quotas as metered units; `data_bytes` of 1 GiB holds about 2 GiB today.
3. **A changed document rewrites its whole field index (`internal/artifactdata`).**
   A one-cell edit of a twelve-column row writes 21 rows, against the review's
   15 with changed paths re-exploded (review 02, DT-03 and DT-05), and the
   writer's cost weighs it as one unit (`rowsPerUnit` 16), from the op's
   fields rather than the document's. Re-exploding only changed paths, or
   weighing a write by the document's indexed fields, brings the cost model
   and the rows together.
4. **The per-store write limit is above the measured ceiling on this machine
   (`internal/artifacts`, `DataWritesPerStorePerSecond` 200).** One node took
   140 to 160 one-cell records a second and the three-member fleet 55, so the
   guard does not protect the node here. The constant stays until the
   reference run; if that run's ceiling is under twice the guard, the guard
   comes down.
5. **The datastore op objective fails on a development machine
   (`internal/objectives`, `DataRoundTripWithin` 250 ms).** The review's peak
   on the three-member fleet answered 18 to 20 percent of writes within
   250 ms (p95 near 950 ms). Whether it holds is the reference run's to say.
6. **A large put can fail under load (`internal/objstore/natsobj`).** The
   library's object put sends every 128 KiB chunk of an object at once (128
   for a 16 MiB file) and gives each acknowledgement the client's default five
   seconds, with no flow control; on slow flushes with the log busy, the last
   acknowledgement came later and the put failed. Bounding the chunks in
   flight, or giving the put a deadline from its pace allowance, makes a put's
   success independent of the disk's flush time.
7. **An import map escapes the copy-in (`internal/artifactupload/lint`).** A
   page whose import map names a module on another origin is published, and
   the module loads in the viewer's browser from wherever the map points
   (under the nonce of the module that imports it); the same reference written
   as a literal import is refused `remote_import`. The linter should read
   `<script type="importmap">` and hold every remote entry to the same rule.
8. **Three silent breaks the linter could name (`internal/artifactupload/lint`).**
   String evaluation in the page's own script (`eval`, `new Function`, a
   string timer, a template compiler), handlers written by script, and a form
   that submits by navigating are published and stop working; a warning for
   each, or a sentence in the authoring guide, closes them.
9. **Presence's best-effort bounds are missed by construction
   (`internal/presence`, `internal/objectives`).** A join is shown everywhere
   only when the ask's budget ends (2 seconds), because replies are applied
   together; applying each as it arrives meets the 1-second bound. A cursor
   crosses two coalescing windows (one on each node, 200 ms each, so a p50
   near 240 ms); the 150 ms bound needs remote frames fanned out without a
   second window, or the coalescing rate (`PresenceCoalesceRate` 5) raised to
   10.
10. **A busy large room sends large frames (`internal/presence`).** At 200
    viewers moving ten times a second, each socket receives about 50 KiB a
    second; capping how many members one frame may re-present, or presenting
    a large room's top fifty by join order rather than recency, bounds it.

---

## Procedures

### S1: origin isolation, by hand

1. Serve a deployment with `api.artifacts` in `per_artifact` mode (a wildcard
   name and certificate, as [Serving Artifacts](../guides/artifacts-serving.md)
   describes) and publish two artifacts, each a page that writes and reads
   `localStorage` and `document.cookie` and prints what it finds.
2. In Chrome, Firefox and Safari, open each in the dashboard; confirm neither
   sees the other's values and that the developer tools show no dashboard
   cookie on any artifact request.
3. Switch the deployment to `shared` mode, reopen both, and confirm
   `self.origin` prints `null`, `localStorage` throws, and a handshake
   message posted from another frame with the right nonce is ignored.

### S3: the `s3` arm

1. `docker compose --profile s3 up -d --wait`, then create the bucket
   (`aws --endpoint-url http://127.0.0.1:8333 s3 mb s3://crewlet-files`).
2. Run with `CREWLET_SPIKE_S3_ENDPOINT=http://127.0.0.1:8333`,
   `CREWLET_SPIKE_S3_BUCKET=crewlet-files` and, for a store that checks
   them, `CREWLET_SPIKE_S3_ACCESS_KEY_ID` and
   `CREWLET_SPIKE_S3_SECRET_ACCESS_KEY`:
   `go test -run '^$' -bench S3StreamPublish -benchtime 1x ./internal/artifactspike`.
   The arm is registered only when the endpoint and bucket are named, and
   leaves its objects under `crewlet-spike/`.
3. From a sandbox host, the same run against the production bucket's region
   measures the cross-network leg the review asks about.

### S4: the browser half

1. Publish every page of `internal/artifactspike/testdata/s4` that the record
   says is published, on a node serving artifacts, and open each in Chrome
   with the console open.
2. Count each page's `Content-Security-Policy` violations and whether it
   renders and responds; compare with `breaks` in `corpus.json`. A page the
   static reading calls clean that logs a violation is a rule the reading is
   missing; add it, with the page that proves it.
3. Add captured model-made pages to the corpus (each with its record) until it
   holds the plan's 200; the corpus test then holds every one.

### S5: real republishes

1. On a deployment where agents publish, read
   `crewlet.artifacts.publish.refusals{reason="edit_refused"}` against the
   count of publishes carrying edits: that ratio is the live refusal rate.
2. To see why, capture republishes as a cases file of the committed shape
   (the base file, the edits, the outcome the engine answered) and replay it:
   `CREWLET_SPIKE_S5_CASES=/path/to/cases.json go test -run '^$' -bench S5Edits -benchtime 1x ./internal/artifactspike`.
   Bases are read from the corpus directory unless a base is an absolute path.
3. The distribution of edits per publish in the capture confirms or moves
   `MaxEditsPerPublish`.

### S7: member replacement

1. On three hosts with a disk each, run a fleet with `store.objects.backend:
   nats` and fill the bucket to 10, 50 and 100 GiB.
2. Stop one member, remove its store directory, and start it again; time from
   its start until the bucket's stream reports the member current
   (`nats stream info OBJ_crewlet_files`), and watch the other members' log
   append latency while it catches up.
3. Repeat with 1,000,000 small objects to confirm the memory figure above at
   scale.

### The reference run

Run the whole suite on Linux with one broker member per host on gp3 volumes
(3,000 IOPS, 125 MB/s), `stream.sync: always`, and nothing else on the
machines. For the fleet spikes on one host, give each member its own volume.
Record the commit, the instance type and the figures beside the run above;
findings 4, 5 and 6 are decided by it.
