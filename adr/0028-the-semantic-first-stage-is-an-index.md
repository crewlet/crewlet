# ADR-0028 — The semantic first stage is an index

- **Status:** accepted
- **Authority:** `internal/search`
- **Enforced-by:** `internal/search.TestIVFRecallMeetsTheFloorCurve`, `internal/search.TestEveryHolderBuildsTheSameIndex`, `internal/search.TestAPartialRepublicationStillFilesEveryRow`, `internal/search.TestTheIndexWaitsForEveryReaderAndForTheLog`
- **Measured:** at 40 000 sources of the topical fixture at 3 072 dimensions, the index — trained, measured and installed as the embedding duty installs it — probed half its lists and answered at 73 ms p95 with one reader and 219 ms with eight, against 116 ms and 294 ms for the full scan on the same corpus and queries, at recall 0.992 against the scan's 0.994 (`BenchmarkSemanticIVFUnderLoad`): 1.84 and 5.48 µs a source against 2.90 and 7.34. At 10 000 sources the same training found no probe count below every list that met the floor, and installed nothing. The share a training needs is the CORPUS's answer, not the index's: at 20 000 sources the isotropic fixture needed every list (declined) and the topical one half (`TestIVFRecallMeetsTheFloorCurve`); at 120 000 the topical fixture needed an eighth (recall 0.985) and the isotropic one half (0.966, installed); at 500 000 the isotropic fixture needed every list (0.948, declined) and the topical one half (0.993) — where aggregate recall met the 0.88 floor at 8 of 2 048 lists and a single head miss on 25 held-out queries persisted to 512 (`BenchmarkIVFRecallAtScale`). A NARROWED search reading the unfiltered count of lists lost its answer: at 20 000 topical sources, with the index installed at half its lists, recall was 0.949 with three head misses narrowed to the tenth of the corpus that is pages, and 0.68 to 0.74 narrowed to one container, against 1.000 for the full scan. A training tick reads about 120 µs a source (every code, the sampled documents and one exact pass, `BenchmarkIndexTraining`) and runs a k-means that grows with the square of the list count: 33 s over 2 048 lists at 500 000 sources and 19 s filing every code, against 126 s and 39 s over 4 096, on four shared cores with every one of them working — the duty runs both on half the cores (`internal/search`), where the same pair measured 42 s and 27 s. 40 sampled rows a centroid measured within one standard error of training on every row, and ten k-means rounds within noise of twenty.
- **Cost-when-tried:** the full scan. At 2.90 µs a source with one reader and 7.34 µs with eight, a thousand seats' first-year corpus — a third of it per data node under the fan-out — is 0.97 s idle and about 2.4 s under load against the one-second interactive target, and the only remedy the scan had was another data node holding another whole copy.
- **Tag-status:** unreleased

## The decision

The first stage of semantic search is an INVERTED FILE over the 1-bit sign
codes the narrow table already holds. The corpus's rows are filed in lists by
k-means in Hamming space, a query ranks the lists by its own code and reads
the nearest ones, and the exact f32 rerank above it is unchanged — the index
decides which documents are looked at and never how they are ordered.

It is replicated state like the vectors it indexes. The embedding duty — the
vector log's one writer — trains it from the corpus's own codes with a
seed derived from the log and the index it replaces, measures its recall
against the exact scan on held-out documents (sampled from the corpus, kept
out of the k-means and never counted as their own answer), and PUBLISHES it as
a record on the vector log; every holder's applier installs it, files each
vector written after it as it is written, and re-files the older rows as
reassign records say. The index's generation is the centroids record's own
position, which the broker mints once, so two trainings are two generations
rather than one number with two meanings. Filing is integer Hamming arithmetic
with the lowest list winning a tie, so every holder files every row in the
same list on every architecture.

The rollout's key ranges are cut ONCE, by the training, and carried in the
centroids record; a reassign record names only its index and its batch
number, on a subject that names both. So every publication of a batch says
what every other publication of it said, and whatever the compaction keeps of
any number of partial re-publications tiles the key space for a node that
replays it. Every record about an index nests its scope under the centroids
record's, so a node deferring a centroids record a later build reshaped
retains that index's batches and measurements behind it.

The probe count is MEASURED, never configured: each training installs the
smallest count whose recall meets the evaluation's floor curve with no head
miss IN EVERY SHAPE a search is issued in — unfiltered, narrowed to each
source, narrowed to one container — and installs nothing when that count would
read more than half the lists. A narrowed search reads lists until it has seen
as many of its own rows as an unfiltered search reads, and runs the full scan
past half of them. The duty re-measures an installed index every day, adjusts
the count without re-filing a row, and retrains in the same tick when no count
within half the lists meets the floor any more. It retrains when the corpus
has doubled or halved, and when the fullest list has grown past four times the
mean AND doubled its share since the training filed it. Below
`search.IVFMinCorpus`, on a corpus whose training installed nothing, and in
a space the index was not trained in, the first stage is the full scan it
always was.

Nothing about the index is published until EVERY node the vector log counts
advertises a build that reads its records (the position heartbeat carries each
log's record version; an evicted node is not counted), and the duty decides
nothing from a node that has not applied the whole log. A build older than the
index's records cannot even defer them — its envelope refuses their kind and
its applier stops — so publishing on the rolling upgrade's usual contract
would stop every old node at the first one.

## Why the obvious alternatives are wrong

**A graph index** (HNSW, DiskANN) is the recall/latency frontier on paper and
the wrong shape here: a second structure with its own persistence and its own
concurrent-update story, and a build that is not a function of the records —
every holder would build a different graph from the same log. An inverted
file's lists are a column on a table the snapshot already carries, and its
centroids are one record.

**Training per node** needs no records and gives every node an index; it
gives them DIFFERENT indexes, from different moments of the log, so one
holder finds a document another misses, and a snapshot carries whichever the
donor happened to build. **A counter as the generation** — the last plus one —
is minted twice by an unresolved publish or a lease that moves mid-training,
and on a compacted subject a replaying holder and a live one then hold two
centroid sets under one number.

**A float k-means** ranks the same lists in all but the last bit, and the last
bit is the point: two CPUs that disagree there file one document in two lists,
which is the objection to a float in anything every node must compute alike.

**A fixed probe count** is a recall promise nobody measured. At a hundred and
twenty thousand sources the topical fixture meets the floor at an eighth of
its lists and the isotropic one needs half; at twenty thousand the isotropic
one needs every list and gets no index at all — one constant would be a lie on
one corpus and a waste on the other, and a company's corpus is neither
fixture.

**Measuring only the unfiltered search** certified a first stage no caller
runs: every search the engine issues is narrowed, and the lists an unfiltered
probe reads hold a small share of a narrowed search's answer (0.949 and under
0.75, above). Reading more lists for a narrowed search, measured by the same
training, is what keeps the recall an index installs at the recall its
searches get.

**Cutting the rollout at each publication** from whatever ids the duty's node
held then, on subjects numbered by batch alone, let a re-publication after new
ids arrived — stopped partway, or with a publish whose outcome was unknown —
leave the compacted log holding two cuts that no longer tiled the key space: a
replaying node kept those rows unfiled for ever, and the duty's own node, which
the superseded batches had filed, never saw a reason to publish again.

**Judging imbalance against the mean alone** retrained for ever a corpus the
k-means cannot balance: identical codes are nearest the same centroid whatever
the seed, and 2 000 copies of one code in 22 000 sources left a list of ≈ 2 020
against a mean of 85 after every training — a retrain, a new generation and a
full rollout every two ticks.

**Publishing on the deferral contract** — a later kind at a higher version, as
every other new kind is — stops, rather than defers, every build whose
envelope validates the subject's kind, which every build before the index
does.

**Relaxing "no head miss"** would buy far more than any list count. At five
hundred thousand topical sources the aggregate floor is met at 8 of 2 048
lists and the no-head-miss rule holds the probe count at half. The rule is
`crewlet search eval`'s own definition of passing, and an index that passed
by a weaker one would install a first stage the evaluation then reports as
failing — so the criterion moves in both places or neither, and that is a
decision about what a search promises rather than about the index.

## What this does not decide

Whether the 1-bit first stage suits a company's corpus at all — that is
`crewlet search eval`, which now measures the index and the full scan side by
side, and `ivf_recall_below_floor`, which fires when even every list misses
the floor. The source filter's plan, which is still the primary key's seek.
