# ADR-0061: A derived object any git node can rebuild is published, kept from collection and never audited

- **Status:** accepted
- **Authority:** `internal/objstore`
- **Enforced-by:** `internal/objstore/references.TestEveryDeclarationStatesItsStanding`, `internal/objstore/collect.TestADerivedReferenceIsKeptAndNeverAudited`, `internal/backup.TestABackupCarriesNoDerivedObject`, `internal/code/cache.TestALayerIsInstalledOnlyOverItsOwnPacks`, `internal/code/cache.TestTheCanaryRefusesAChainOverTheWrongBase`, `internal/code/compact.TestAPublicationIsBuiltInAScratchNamingExactlyItsBase`, `internal/code/compact.TestAPublicationIsNeverBuiltOverABaseTheStoreLost`, `internal/code/host.TestTheHostPassesTheSuite` (its `LY1_ALayerIsInstalled`, `LY2_ALayerTheStoreLostFallsBack` and `LY3_ACorruptLayerFallsBack`), `internal/code/codetest.TestTheSuiteCatchesALayerOverAPackTheNodeLacks`
- **Measured:** on git 2.56.0, 219 679 objects in 64 packs, four vCPUs: a whole multi-pack-index and bitmap 1.43 s and 167.8 MiB; a bitmapped incremental tip over one sixty-fourth of the objects 1.89 s and 127.5 MiB, over an eighth 1.52 s and 131.6 MiB — a tip costs the history, not its new objects; a tip without a bitmap 0.042 s, and no bitmap read beneath it afterwards; a layer over the full packs of a cut refused for closure; an installed layer read with its bitmap in 0.158 s on a node that never wrote it; a layer written over a chain file naming only its base 1.03 s, against 196–200 s with layers above that base still named, and for `multi-pack-index compact` of two non-root layers with one above them
- **Cost-when-tried:** M2 gave every git node its own whole multi-pack-index and bitmap of every repository it serves: O(history) memory and minutes on every node at every duty cycle, refused outright on any node whose memory could not hold the largest repository's bitmap — so the default repo_max_bytes was served without a bitmap on every node of ordinary size. The first replacement designed, node-local bitmapped tips over a published base of full packs, was measured unbuildable on the pin before a line of it was written: the tips cost the history and the full-pack base fails git's closure check
- **Tag-status:** unreleased

## The decision

**A derived object any git node can build again — a repository's published
multi-pack-index layer, its bitmap and its commit-graph layer — is built ONCE,
stored in the object store, and named by rows of the `objstore.Derived`
standing** ([ADR-0051](0051-a-retired-reference-keeps-its-object-for-a-grace.md)):
kept from collection while a row names it, never asked after by the audit,
never carried by a backup, its rows removed with no grace when a publication
replaces them. The code host's first derived table is `code_layer_parts`.

**One node a repository publishes: its MAINTAINER**, the first git node of the
repository's rendezvous order whose advertised background budget holds the
publication — the bitmap over the whole history — and the home where none
does (`internal/estate`'s `Router.Maintainer`). On a fleet of alike nodes it
is the home. It also compacts the repository, so the two writers of its packs
run on one node, where a merge never takes a pack a publication in flight there
planned over — and merges the packs pushed since beside it, which its layer
neither names nor needs. A publication is a compaction record carrying the
layer, decided by the repository's committer:

- a layer covers EVERY live pack its base does not, as of its plan — git writes
  a bitmap only over packs closed over what their commits reach;
- it is WRITTEN in a scratch whose chain files name exactly its base, never
  merged with `multi-pack-index compact`, and the chain is merged
  geometrically, git's own split factor (2) and new-layer threshold (8);
- it is never built over a base the store no longer holds;
- its parts and its repack's outputs are uploaded at once, so every key is
  named within `objstore.RecordWithin`.

**Every git node installs the published chain, and checks it first.** A layer
is installed only:

- over packs the node holds byte for byte;
- with its files matching its row (`code.CheckLayer`: the multi-pack-index's
  trailer, its pack names, the bitmap's owner, the graph's base);
- in the rows' chain order, the order being the only thing that names a
  layer's base;
- with a read through its bitmap succeeding before it becomes visible (the
  canary).

A layer that fails any of these, a download that fails, or a layer written by a
newer git than the node's is not installed. The node serves from the prefix
below it, or in OWN mode with its own index — never an error. A refusal on the
maintainer's own node is a REPAIR: the maintainer publishes again over the base
it holds, replacing the refused layer and every one above it. A restore, which
carries no layer, is repaired the same way: every node serves in own mode until
each repository's maintainer publishes again.

## Why the obvious alternatives are wrong

- **Every node writes its own layers.** This is M2's cost paid N times. A
  bitmapped layer costs the history on git 2.56.0 however few objects it adds
  (its writer reuses no bitmap of the base), and a layer without a bitmap
  switches every bitmap beneath it off.
- **Audit derived objects as Required.** A lost layer would raise
  `objects_missing`, the alarm for lost DATA, about an object any node rebuilds
  in minutes. It also makes every backup carry gigabytes a restore can do
  without.
- **Keep them Retired.** A Retired row keeps its object only for a grace. A
  published layer is live for as long as the chain names it.
- **Publish over exactly the full packs.** Measured: git refuses a bitmap over
  packs that are not closed, and the full outputs of a cut are not closed
  without the last one. A layer covers every pack its base does not, as of its
  plan.
- **Merge layers with `multi-pack-index compact`.** Measured at 196 s for a
  merge of two non-root layers with anything above them in the chain file,
  against about a second for the same layer written directly over a chain file
  naming only its base. A compaction from the root, which reuses bitmaps,
  cannot take the new packs a publication exists to add.
- **Place the publication on the home whatever its size.** A bitmap over the
  largest repository the defaults admit needs tens of gigabytes; on a fleet of
  ordinary nodes with one large one, the home of such a repository would refuse
  its publication for memory, and nobody would publish it.

## What this does not decide

- Whether a future git's tip layer follows its new objects: `BenchmarkMidx`
  `step=tip` is re-run at every pin bump (CONTRIBUTING.md), and a pin on which
  it does re-opens node-local tips.
- Expunge: the one rewrite that drops objects, and with them every layer naming
  its packs.
- Data-less git nodes, which would install layers over packs they stream rather
  than hold.

## Amendments to earlier records

[ADR-0051](0051-a-retired-reference-keeps-its-object-for-a-grace.md): the
`Derived` standing's first table is `code_layer_parts`, and what "the engine
builds them again" means for it is this record's: a node whose layer is
missing or damaged serves without it, and the repository's maintainer
publishes a replacement.

[ADR-0059](0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md): who
compacts a repository and publishes its layers is its MAINTAINER, the first git
node of the same rendezvous order whose advertised background budget holds the
publication — the home on a fleet of alike nodes. Reads and engine writes stay
with the home.

[ADR-0060](0060-a-compaction-merges-packs-without-judging-reachability.md): a
repository's small packs are merged by its maintainer, which also raises
`code_compaction_behind`; and a pack a published layer names is never a
compaction's input — a node's chain loads only over every pack it names — so a
published repository's small packs are merged by its publications' repacks.
