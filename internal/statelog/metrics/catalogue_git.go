package metrics

// gitInstruments is the instruments of git and of Gitlet, the code host that runs it.
func gitInstruments() []Instrument {
	return []Instrument{
		// ---- git ------------------------------------------------------
		{
			Name: CodeCacheAdoptions, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Pushes this node's git cache was asked to adopt — the " +
				"pack the push's receive-pack wrote, linked into the cache " +
				"once its row applied rather than downloaded back by the " +
				"node that took the push — by `outcome`. `adopted`: linked " +
				"in by the adoption itself. `handed`: a sync or a verify " +
				"was writing the repository's directory, and the adoption " +
				"left the pack to it, staged; it installs the pack from " +
				"that staging rather than the object store before it lets " +
				"the directory go, unless it had already begun to download " +
				"it. `held`: the cache already held the pack. `uncached`: " +
				"the repository's cache left this node between the push's " +
				"sync and its adoption — the repository purged — so there " +
				"was nothing to adopt into. `failed`: the links failed, and " +
				"the next sync downloads the pack, one GET of its bundle; " +
				"rising, it is this node's disk refusing writes.",
		},
		{
			Name: CodeCacheBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds on disk — every " +
				"repository's packs, indexes and refs, plus the downloads and " +
				"pushes in flight — against its byte budget, which it is " +
				"evicted to, least recently used first, and for " +
				"no other reason. Sitting at the budget is a cache doing its " +
				"job; held above it is a repository larger than the budget " +
				"being served, evicted once nothing reads it; climbing past " +
				"it while syncs keep starting cold is a budget too small for " +
				"the repositories this node serves.",
		},
		{
			Name: CodeCacheChained, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories this node's git cache serves from an " +
				"installed published chain — the multi-pack-index layers, " +
				"each with its bitmap and commit-graph layer, that the " +
				"repository's maintainer wrote once for every Gitlet node and " +
				"this node fetched and placed — rather than in its own mode, " +
				"from the index and graph its background work writes itself. " +
				"Beside `crewlet.code.cache.repos`: the rest are repositories " +
				"no publication has reached yet, or whose chain this node " +
				"refused or has not installed (`crewlet.code.cache.layers`). " +
				"Falling all at once across the fleet, with `layers{outcome=" +
				"missing}` rising, is an object store that lost the layers' " +
				"parts — a restore carries none — which every maintainer " +
				"publishes again; a node serves correctly either way, only " +
				"slower while it waits.",
		},
		{
			Name: CodeCacheChecks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"file", "outcome", "trigger"},
			Shows: "Files of this node's git cache held to their own " +
				"checksum — the trailer git writes at the end of every " +
				"pack and index file, the SHA-1 of the bytes before it — " +
				"because git checks none of them when it reads: a damaged " +
				"index file can answer wrong without a word, and a damaged " +
				"pack is sent to clients byte for byte. Before a copy this " +
				"process did not write answers its first request, every " +
				"index file it holds; and behind the serving, a SCRUB of " +
				"every file a copy holds, packs included — once after a " +
				"copy a previous process left first answers, daily while it " +
				"is used, and at once when a git process over it reports a " +
				"pack it could not read or crashes; and when a request — " +
				"or the node's own sync — fails on a pack or an object, " +
				"every index file again, " +
				"whatever was checked before, then every file if that " +
				"repaired nothing. By `file` " +
				"(`commit-graph`, `multi-pack-index`, `bitmap`, `midx-rev`: " +
				"files the node derives from its packs; `layer-midx`, " +
				"`layer-bitmap`, `graph-layer`: a published layer's files as " +
				"its repository's maintainer wrote them, the `.midx` and the " +
				"`.graph` held to the checksum their names carry, and one " +
				"that fails is fetched again with every layer above it; " +
				"`layers-chain`, `graph-chain`: the two chain files the " +
				"node writes over the layers it installed, held line for " +
				"line to them and written again where they differ; " +
				"`provenance`, a run " +
				"of the node's index of which push introduced each commit, " +
				"which no git reads, held to its own SHA-256 trailer — a run " +
				"a previous process left before anything first reads it, " +
				"behind the serving, and every run by a scrub; " +
				"`codeindex-segment`, `codeindex-tip`: a segment and a tip " +
				"map of a repository's code index, which no git reads " +
				"either, held to their own SHA-256 trailers by a scrub; " +
				"`tags`, a copy's tag index, which no git reads either, " +
				"held to its own SHA-256 trailer by a scrub; " +
				"`idx`, `rev`: a " +
				"pack's index and reverse index, stored with its push; " +
				"`pack`, the pack itself, read only by a scrub), `outcome` " +
				"(`sound`; `damaged` — a derived file dropped to be written " +
				"again, a stored one fetched again with its pack, one " +
				"download; `unreadable`, a read of it that failed — " +
				"repaired as though damaged, since a file the disk cannot " +
				"read is one git cannot serve, and never a refused read " +
				"slot or an ended request, which decide nothing and are not " +
				"counted; `replaced`, " +
				"the file renamed over or removed while it was read, so no " +
				"verdict is its) and `trigger` (`first`, a copy's first full " +
				"sync in this process, which reads every index file a " +
				"previous process left, or the scrub after it, which reads " +
				"every pack and every file that sync did not, or the check of " +
				"a `provenance` run a previous process left or a rebuild " +
				"linked; `unvouched`, a " +
				"later full sync reading only what this process has neither " +
				"written nor checked since; `scrub`, a used copy's daily " +
				"scrub; `report`, a scrub a git process's report or crash " +
				"made due; `forced`, the in-place repair a pack's or an " +
				"object's failure asks first — a request's git, or the " +
				"node's own sync's — every index file held to its checksum " +
				"whatever was checked before; `request`, the scrub that " +
				"repair runs at once when it found nothing, when it is " +
				"skipped (a forced check having run within the hour with " +
				"nothing repaired since, whatever it found and however its " +
				"sync ended, or every pack it named held off after the " +
				"store refused it), or when the sync's own git met the damage " +
				"again past it, and a pack a " +
				"git process named in words, confirmed against its row and " +
				"fetched again). Any `damaged` is a disk losing data, or something " +
				"outside the engine writing into the cache: the copy repairs " +
				"it, and a verify of the repository names what else that " +
				"disk holds damaged. The answers the cache keeps of seats' " +
				"reads are no file here: every read of one holds it to its " +
				"own SHA-256, and one that fails is " +
				"`crewlet.code.cache.memo.lookups`' `corrupt`.",
		},
		{
			Name: CodeCacheCheckDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"trigger"},
			Shows: "How long one check of a copy's index files, or one scrub " +
				"of every file it holds, took, by `trigger` (`first`, " +
				"`unvouched`, `scrub`, `report`, `forced` or `request`, as " +
				"`crewlet.code.cache.checks`), counted only where it read " +
				"something. A check is what a copy's first request after a " +
				"restart waited for beside its sync — about a quarter of the " +
				"copy's pack bytes read; a scrub runs behind the serving and " +
				"nobody waits for it, reading every byte the copy holds. " +
				"Both hash on one core, a read slot a 64 MiB piece, and at " +
				"the volume's own throughput when read cold.",
		},
		{
			Name: CodeCacheReports, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"part", "outcome"},
			Shows: "Damage any git process over this node's git cache " +
				"named — whether its request failed or git read around the " +
				"damage and answered — as the cache took it, by the `part` " +
				"named (`commit-graph`, `multi-pack-index`, `chunk`: a " +
				"derived file of either, dropped by a repair round with no " +
				"request waiting and written again at once — on a copy " +
				"serving from an installed published chain " +
				"(`crewlet.code.cache.chained`), which keeps its files on a " +
				"report, the copy's scrub made due instead, which holds every " +
				"file of the chain to its checksum and fetches again a layer " +
				"that fails; `pack`, " +
				"`object`: stored damage, and `hint`, a pack git named as " +
				"holding a corrupt object — git 2.56's bare message, or the " +
				"pack git 2.55's report of that damage names — each making " +
				"the copy's scrub due at once, " +
				"which holds every file it holds to its checksum and fetches " +
				"again what fails; `crash`, a git process dying of a signal " +
				"the node did not send — what a damaged reverse index or " +
				"bitmap does to a clone, naming nothing — which makes the " +
				"scrub due the same way; any of these four named by the git " +
				"a request ran, which repairs what that git fails with " +
				"before its first byte, is held for that request until it " +
				"asks its repair or lets the copy go, and counted then — " +
				"and one the node's own sync's git names, for that sync's " +
				"round until it is answered) and " +
				"its `outcome`: `queued`, a " +
				"repair asked — a round, or the scrub; `deferred`, asked " +
				"while a check of the repository ran, repaired once it ends; " +
				"`stale`, not dropped — the node wrote the file after the " +
				"process that named it began, so the report was of the file " +
				"it replaced, or the copy holds no file of the part named any " +
				"more — and, for a `pack` or an `object`, a failed " +
				"request whose process began before the node last wrote a " +
				"repair into the copy, answered from the copy as it stands " +
				"once no round is repairing it and run once more rather " +
				"than repaired further, or a sync whose own git met it so, " +
				"synced again; `unrouted`, a pack, an object or a hint named " +
				"by a push's own throwaway repository, which may be the " +
				"client's pack, or a derived file named by a compaction's " +
				"build, whose store of the packs it merges holds none; " +
				"`again`, a derived file named damaged within " +
				"a day of its last drop for damage, whatever the node wrote " +
				"of it between: dropped and left to the ordinary schedule " +
				"rather than written again at once; `cooldown`, a stored " +
				"report or a crash within an hour of a scrub of the copy " +
				"that found nothing, which makes no other; `covered`, one " +
				"within an hour of a scrub whose findings still wait for " +
				"their repair, which is its answer; `request`, one held for " +
				"the request whose git named it — or for the sync whose own " +
				"git did — and answered by the repair that request or that " +
				"sync's round asked — its in-place round, its scrub and, " +
				"past both, its rebuild — with no scrub of the copy made due " +
				"beside it (one the request did not repair — it failed past " +
				"its first byte, failed in a way the node does not repair, " +
				"or never failed — is counted under the others as the " +
				"request lets the copy go). `queued` rising is a disk " +
				"losing data under the cache; `again`, a disk that keeps " +
				"losing it; `cooldown` rising, a git process failing on " +
				"something the copy's checksums do not show; `request` " +
				"rising, requests failing on damage they had repaired " +
				"under them.",
		},
		{
			Name: CodeCacheHints, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Packs of this node's git cache that a git process named " +
				"as holding a corrupt object, held to the row that names " +
				"them, one a naming, by the verdict that answered it " +
				"(`outcome`). git 2.56 says a packed object is corrupt " +
				"through a bare message, which nothing reads as damage — " +
				"beside whatever else of the damage it reports, and alone " +
				"for an object whose header states the wrong size — and git " +
				"2.55 through a report of its own, read as a pack's damage. " +
				"A naming is hashed and counted here only where a " +
				"repair waits on its verdict, on either git: a request's git " +
				"in the copy that failed before its first byte, and the " +
				"node's own sync's git — the peel of the tags it sets — " +
				"each with no damage it repairs other than a pack's or an " +
				"object's. Any other naming — by a run that did not fail on " +
				"it, a request that failed past its first byte, or a push's " +
				"own throwaway repository — is hashed by nobody: it makes " +
				"the copy's scrub due, counted only as " +
				"`crewlet.code.cache.reports{part=hint}`. `confirmed`, the " +
				"pack's bytes are not its row's, and that pack is fetched " +
				"again IN PLACE with one download, whichever git named it " +
				"(counted under `crewlet.code.cache.rebuilds{reason=stored}`), " +
				"the repository read whole into a new directory (`corrupt`) " +
				"only when that repair and a scrub of the copy find nothing " +
				"to fetch again; `sound`, its bytes are the row's — after " +
				"2.56's message nothing is repaired, and after 2.55's report " +
				"the damage it names is repaired as any pack's is, with no " +
				"pack named to the repair; `unknown`, no row names the pack " +
				"at the log's head; `unread`, no verdict — the rows or the " +
				"pack could not be read in time, or its files moved under " +
				"the hash; `gone`, the request left before the verdict, the " +
				"hash going on for the next. `confirmed` rising is disk " +
				"damage a verify of the repository names; `sound` rising is " +
				"git naming damage the pack does not hold — a git bug, or, " +
				"on 2.56, a request that made git print the sentence — which " +
				"costs one hash of the pack's files and, on 2.56, no repair " +
				"at all; `unread` and `gone` rising is a node too busy or a " +
				"disk too slow to decide a hash inside its bound. Beside " +
				"`crewlet.code.cache.hint.hashes`, the namings one hash " +
				"answered.",
		},
		{
			Name: CodeCacheHintHashes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Hashes of a pack of this node's git cache that git named " +
				"as holding a corrupt object (`crewlet.code.cache.hints`), by what " +
				"each decided (`outcome`): `confirmed`, `sound`, `unknown` and " +
				"`unread`, as there. One a pack's files however many namings " +
				"wait on it — a naming of a pack being hashed waits for that " +
				"hash, and a verdict is kept for as long as the pack's files " +
				"are the ones it was reached on — each run on the cache's own " +
				"time rather than a request's, a read slot a 64 MiB piece, and " +
				"bounded by the hash alone. Namings less hashes is what " +
				"waiting saved; `unread` here is a hash the node could not " +
				"fit, which the next naming starts again.",
		},
		{
			Name: CodeCacheLayers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Published layers of a repository's chain — a " +
				"multi-pack-index layer with its bitmap and commit-graph " +
				"layer, written once by the repository's maintainer for every " +
				"Gitlet node — that this node's git cache installed, carried, " +
				"cut or refused, by `outcome`. `installed`: fetched from the " +
				"object store, held to its row and placed — by the full sync " +
				"that first read the chain, before it answered, or behind the " +
				"serving by the installer after a delta that named it. " +
				"`adopted`: placed from the files the maintainer itself wrote " +
				"on this node, no download. `kept`: carried into the " +
				"directory a placement or a rebuild made, the chain moving " +
				"around it. `cut`: taken out — the rows no longer name it, a " +
				"pack it names left, one of its files failed its checksum, or " +
				"a layer below it went. `missing`: a part the object store " +
				"holds nothing for while the log's head still names the " +
				"layer — after a restore, which carries no layer, every one, " +
				"until each maintainer publishes again. `corrupt`: bytes that " +
				"are not its row's, files its row does not describe, or a " +
				"placement whose one bitmapped read through it failed — a " +
				"layer over the wrong base. `newer_git`: written by a git " +
				"newer than this node's, which it would not read. `failed`: " +
				"a try the store failed to answer, this node's disk failed a " +
				"write of, whose missing part the log's head could not be " +
				"read about, or whose placement's read through the bitmap " +
				"could not run — no read slot free within ten seconds, or " +
				"past its deadline — by a full sync, which leaves the layer " +
				"to the installer, or by the installer, which tries again a " +
				"minute later, doubling to thirty: counted once a try. A " +
				"refused layer, and every one above it, is never installed " +
				"while the rows name it: the copy serves from the layers " +
				"below it, or in its own mode, correctly and slower, until " +
				"the maintainer's next publication replaces it.",
		},
		{
			Name: CodeCacheLayerInstall, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long one published layer took this node's git cache " +
				"to install, from its staging to its placement — its parts' " +
				"downloads, each beside the others in one of the node's " +
				"fetch slots, its files' syncs and its check against its " +
				"row, and the placement's bitmapped read through it — by " +
				"`outcome` (`installed`, `adopted`, `missing`, `corrupt`, " +
				"`failed`, as `crewlet.code.cache.layers`). A full sync " +
				"fetches its layers beside its packs and a cold copy's first " +
				"answer waits for them; the installer, behind the serving, " +
				"keeps at most half the node's downloads for itself. A " +
				"p95 climbing with `installed` steady is an object store " +
				"slowing down; `failed` climbing, one failing.",
		},
		{
			Name: CodeCacheMaintenanceRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "step"},
			Shows: "Steps of the background work on a repository this node's " +
				"git cache found it will not run, counted once for each copy " +
				"of a repository the cache holds and each step, by `reason` " +
				"and `step`. `memory`: the step's charge — its own figure an " +
				"object of what it walks, and 240 more an object of the " +
				"repository for room beside it for a clone of the repository " +
				"(of the packs merged, for `compact`) — is more than this node " +
				"can give its background work, which " +
				"is its memory (its cgroup's limit where one is set) less what " +
				"it keeps for its git lanes, the store, the broker and the " +
				"engine. `bitmap` (600 bytes an object of the repository): " +
				"the repository is indexed without one, and a clone walks its " +
				"history; `index` (80): it is served from its packs' own " +
				"indexes, a round's index included; `graph` (67, and 100 MiB " +
				"whatever the repository's size for its changed-path filters): its walks go " +
				"by date, and a push of a commit dated before its parent walks " +
				"every commit; `tip-graph` (50 an object of the tail, the " +
				"packs pushed since the repository's last publication, and " +
				"100 MiB whatever its length): a copy " +
				"serving from a published chain walks those packs' commits by " +
				"date. Slower, never wrong. `compact` (550 bytes an object of " +
				"the packs merged, beside a transfer's room of them — 240 " +
				"bytes an object merged, never of the whole repository, whose " +
				"size alone would refuse every merge of a large one — and six " +
				"times max_blob_bytes for the build's delta search): the " +
				"repository's maintainer cannot merge its small packs, " +
				"so its pack count grows with its pushes until a Gitlet node with " +
				"more memory maintains it or this one is given more — and at " +
				"the pack bound its pushes are refused, which " +
				"code_compaction_behind raises. `publish` (600 bytes an object " +
				"of the repository for the layer's bitmap, beside the clone's " +
				"room): the repository's maintainer cannot publish its next " +
				"layer, so every pack pushed since its last publication is served " +
				"outside every bitmap, and every one pushed after. `expunge` (550 " +
				"bytes an object of the packs an expunge sorts, beside the room " +
				"for a clone of the whole repository, whose history its build " +
				"walks, and its delta search; a due scan the room and the " +
				"search alone): a person's purge of the repository, or the " +
				"weekly expunge it is due, waits until a Gitlet node with the " +
				"memory maintains it (`code_cache_expunge_refused` names it), " +
				"and meanwhile what no ref reaches still counts against " +
				"repo_max_bytes, past which its pushes are refused " +
				"`repo_size`. Rising is " +
				"repositories outgrowing the node; the code_maintenance_refused " +
				"alarm stands while any bitmap or publication is refused, and " +
				"never for a tip, whose copy is served through its chain's bitmap.",
		},
		{
			Name: CodeCacheMaintenanceRefused, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"step"},
			Shows: "Repositories this node's git cache cannot write a bitmap " +
				"(`step` `bitmap`), a commit-graph (`graph`) or, serving from " +
				"a published chain, its own tip commit-graph over the chain " +
				"(`tip-graph`) of now, and repositories this node maintains whose " +
				"next layer it cannot publish (`publish`), because one needs more " +
				"memory than the node can give its background work — as its latest " +
				"pass found them. Each refused a bitmap is indexed without one; each " +
				"refused a commit-graph walks its history by date, so a push " +
				"of a commit dated before its parent walks every commit; each " +
				"refused its tip walks the commits pushed since its last " +
				"publication by date — slower, never refused; each refused its " +
				"publication serves every pack pushed since its last one outside " +
				"every bitmap. A bitmap, a graph or a publication above zero raises " +
				"code_maintenance_refused, since a repository refused its graph is " +
				"refused its bitmap too; a tip raises nothing, its copy served " +
				"through its chain's bitmap. Falling back is a repository purged, " +
				"evicted or rebuilt smaller, a node given more memory, which the " +
				"cache reads again every ten seconds, or a publication — by this " +
				"node or by another with the memory, which takes its maintenance " +
				"over — that its copy installs: its bitmap and graph are then the " +
				"chain's, and its tail shorter, which lifts a tip refused for its " +
				"tail's length but never one refused for its repository's size, " +
				"since a tip keeps room beside its tail for a walking clone of the " +
				"whole repository.",
		},
		{
			Name: CodeCacheMemoBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of answers to seats' reads " +
				"by object id — a page of a listing, a file, a diff, a " +
				"comparison — kept so the next read that asks the same is " +
				"answered from a file rather than from git: part of " +
				"`crewlet.code.cache.bytes`, each answer counted at the whole " +
				"disk blocks it takes. Held, with the pages reads are still " +
				"cutting for it, to a sixteenth of the cache's budget before " +
				"any repository is evicted — and no repository is evicted for " +
				"what it holds past that — by dropping the sixteenth of it used " +
				"least recently at once, so on a node seats read busily it " +
				"moves between fifteen and sixteen sixteenths of that share, " +
				"less what is being cut; falling to nothing at once is a " +
				"history restored or recreated under the node, which drops " +
				"every answer read from it.",
		},
		{
			Name: CodeIndexBuilds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Looks this node's indexer took at the code index of a " +
				"`search: indexed` repository — each one every minute, and one " +
				"whose refs a round moved at once — by `outcome`. `built`: the " +
				"index now answers for the " +
				"default branch's tip, only the blobs the tip brought read. " +
				"`current`: it already did, or another build stored first. " +
				"`none`: the repository keeps no index here — its search is " +
				"grep, its default branch names no commit, or this node is not " +
				"among the first two Gitlet nodes of its order, which keep it " +
				"(an index this node kept is dropped once it has been out of " +
				"them ten minutes), so on a fleet of more than two Gitlet nodes " +
				"most looks are `none`. " +
				"`over_share`: the copy keeps no index and the node's indexes " +
				"hold their share of the budget, so none is built — nor a copy " +
				"filled for one — until the share has room or the repository " +
				"is searched here more recently than an index kept, which it " +
				"then displaces; its searches grep meanwhile. " +
				"`no_room`: the budget evicted the copy this node kept the " +
				"index in, and it is filled again only once the budget has room " +
				"for what it held. " +
				"`placing`: this node became one of the repository's two " +
				"index holders within the last ten minutes — another Gitlet node " +
				"left its order — and fills and builds nothing until its place " +
				"has stood that long, so a Gitlet node's restart moves no index. " +
				"`failed`: the copy could not be synced or read, and the next " +
				"look tries again (`code_cache_index_failed` says why). A " +
				"steady `over_share` or `no_room` is a node keeping more " +
				"indexes than its budget holds: see `crewlet.code.index.bytes`.",
		},
		{
			Name: CodeIndexBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of code indexes — the " +
				"trigram segments and tip maps of the repositories searched by " +
				"index that it holds — part of `crewlet.code.cache.bytes`. Held " +
				"to a third of the cache's budget before any repository is " +
				"evicted, by dropping whole the index of the repository " +
				"searched here least recently; the indexer builds a dropped one " +
				"again only into room in the share, or for a repository " +
				"searched more recently than an index it displaces, so a gauge " +
				"pinned at the third, with `over_share` looks, is a node " +
				"keeping more indexes than its budget holds.",
		},
		{
			Name: CodeSearchQueries, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"via"},
			Shows: "Code searches this node asked, by `via`: `index` and `grep`, " +
				"a search of one repository read through its code index or with " +
				"git grep; `scatter`, a search of several, asked of every Gitlet " +
				"node holding them at once. A steady `grep` share of " +
				"one-repository searches of indexed repositories is indexes that " +
				"do not keep up with the pushes: see `crewlet.code.index.builds`.",
		},
		{
			Name: CodeSearchDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"via"},
			Shows: "How long a code search this node asked took, from its " +
				"asking to its answer, by `via` as `crewlet.code.search.queries`. " +
				"A scatter takes one round, or two where a Gitlet node did not " +
				"answer the first, each at most 9 s: a scatter near 9 s is a Gitlet " +
				"node that did not answer, which its answer's coverage names.",
		},
		{
			Name: CodeSearchCoverage, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Repositories a code search this node asked did not search, " +
				"or searched more slowly, by `reason`, as the last Gitlet node " +
				"asked for it answered: `not_answering`, a Gitlet node that did " +
				"not answer for it; `building`, its index being built, with no " +
				"other Gitlet node in its order to ask; `stale`, a node behind the " +
				"log or the company's configuration; `not_permitted`, a grant the " +
				"answering node's chart refused; `no_ref`, no such ref in it; " +
				"`past_limit`, every line of it past the answer's limit, so it " +
				"was passed over or its lines were cut; " +
				"`grep_not_named`, a `search: grep` repository an unfiltered " +
				"search leaves out; `not_indexed`, an indexed repository read " +
				"with git grep. A steady `not_answering` is a Gitlet node to look " +
				"at; `grep_not_named` alone is a company's choice.",
		},
		{
			Name: CodeImportChunks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "The chunks this node, as a repository's home, proposed " +
				"of an import from another host, by `outcome`: `applied`, the " +
				"chunk's packs and refs landed; `ended`, refused for a limit " +
				"the source is past (`repo_size`, `repo_refs`, `ref_conflict`, " +
				"`pack_too_large`), which ends the import with its error; and " +
				"`failed`, refused for anything else — a busy committer, rows " +
				"that moved — or not seen applied, the import tried again from " +
				"its rows a minute later and longer after each failure in a " +
				"row. `failed` climbing on one node with no `applied` beside it " +
				"is an import that cannot make progress: its " +
				"`code_mirror_attempt_failed` lines say why.",
		},
		{
			Name: CodeMirrorRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"dir", "outcome"},
			Shows: "One attempt to bring a mirror level with the other host, " +
				"by `dir` (`pull`: a pull mirror's sync from its source, run " +
				"by its home every 15 minutes; `push`: a push mirror's push to " +
				"its target, run by its home at most once a minute while the " +
				"repository moves and every 15 minutes while it is idle) and " +
				"`outcome`: `synced`, a sync landed what the source moved; " +
				"`pushed`, a push moved refs at the target; `current`, the " +
				"source moved nothing, or the target already held every " +
				"covered ref as it should — or nothing the push mirror " +
				"covers had moved since its last attempt left the target " +
				"level, within 15 minutes, and the target was not dialed; " +
				"`failed`, the other host " +
				"unreachable, its credential refused, a ref it rejected, a " +
				"push mirror's seat that no longer reads the repository, or a " +
				"step refused or not seen applied — the mirror tried again a " +
				"minute later, and longer after each failure in a row, up to " +
				"its interval. A mirror whose other host went away shows as " +
				"`failed` four times an hour, every one with its " +
				"`code_mirror_attempt_failed` line, and raises " +
				"`code_mirror_failing` once it has gone half an hour.",
		},
		{
			Name: CodeCacheDroppingBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of packs an expunge's " +
				"begin has left dropping — what no ref reached when the " +
				"expunge was planned, kept apart from the live packs so a " +
				"client still reading one of their objects is served it and " +
				"no push may rely on one: part of `crewlet.code.cache.bytes`. " +
				"Zero but while an expunge is open on a repository this node " +
				"holds; it falls back as each expunge commits and the next " +
				"sync of the repository takes the dropping packs out. Held " +
				"up for long is an expunge that began and does not commit — " +
				"its maintainer's rescue failing — which also refuses every " +
				"layer the repository would publish meanwhile.",
		},
		{
			Name: CodeCacheMemoLookups, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Seats' reads by object id that this node's git cache looked " +
				"up among the answers it keeps, by `outcome`: `hit`, answered " +
				"from the answer kept, with no git run; `miss`, none kept, so " +
				"the read ran its git and keeps what it cut; `probe_hit` and " +
				"`probe_miss`, a read's probe for a part of an answer it may " +
				"find under any of several keys (a directory's window of last " +
				"commits, under each commit its walk passes), where a miss " +
				"computes nothing under that key; `corrupt`, a kept " +
				"answer that did not read back whole — removed, and computed " +
				"again, so rising is this node's disk damaging what it " +
				"holds. Misses climbing while `crewlet.code.cache.memo.bytes` " +
				"sits at its sixteenth of the budget, with few repositories " +
				"evicted, are a share too small for the answers seats ask " +
				"again.",
		},
		{
			Name: CodeCacheProvenanceBackfilled, Kind: KindCounter, Unit: UnitCount,
			Shows: "Pushes this node's git cache read into its indexes of which " +
				"push introduced each commit BEHIND the serving: the range a " +
				"full sync passed over — the pushes made while the node was " +
				"down, every node holding a copy reading its own — and a " +
				"repository's history back to the start of the push history " +
				"the data nodes keep, read only by the node that answers a " +
				"seat's reads of it as its home, once a seat has asked. Every " +
				"sync folds the pushes it applied into the index before it " +
				"answers, and none of those is counted here. A page at a " +
				"time, each followed by a pause of twice its own duration " +
				"times the fleet's Gitlet nodes for each data node, so the " +
				"fleet's backfills hold under half of one reader of each data " +
				"node. Rising after a restart is the gap closing; rising for " +
				"hours on one node is one large history read once — " +
				"`crewlet.code.cache.provenance.behind` says what is left.",
		},
		{
			Name: CodeCacheProvenanceBehind, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories whose index of which push introduced each " +
				"commit is behind on this node: a range a full sync passed " +
				"over that the backfill has not yet read, or a history a " +
				"seat's reads asked this node, as the repository's home, to " +
				"read and that it has not yet read to its start. A copy " +
				"nobody asked about is never read back further than its first " +
				"full sync, so it is never counted. Until it is zero, an " +
				"answer naming a commit's push says which range it does not " +
				"hold rather than answering that no push introduced it. " +
				"Staying above zero is a backfill that cannot read the data " +
				"nodes — its warnings, code_cache_backfill_failed, say why — " +
				"or a history larger than its pace reads in the time it has " +
				"been asked for.",
		},
		{
			Name: CodeCacheRebuildRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "rule"},
			Shows: "Rebuilds this node's git cache refused, by the `reason` " +
				"the git process gave (`corrupt`, `missing` or `shape` — a " +
				"shape only once reading the directory whole in place left " +
				"the pool still refusing it, since that repair downloads " +
				"nothing and is never refused) and " +
				"the `rule` that refused it. `unchanged`: the repository's " +
				"cache holds exactly what a rebuild that did not end the " +
				"failure read less than a day ago, and nothing has written " +
				"into it since, so another would make the same directory — " +
				"never for `shape`, whose refusal is itself something " +
				"written into it; " +
				"`cooldown`: the last attempt began too recently — ten " +
				"minutes after the first of a line of attempts that keep " +
				"failing, twice as long after each one after it, at most a " +
				"day, a line starting again once a day passes with no " +
				"rebuild asked for. Each refusal fails the request that asked " +
				"at once rather than downloading the whole repository again. " +
				"Rising at all is " +
				"damage no rebuild repairs — a pack the object store lost or " +
				"holds damaged, rows naming an object no pack holds, a " +
				"directory something keeps writing into — which " +
				"a verify of the repository names; flat while " +
				"`crewlet.code.cache.rebuilds` rises is damage each rebuild " +
				"ended.",
		},
		{
			Name: CodeCacheRebuilds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Repositories this node's git cache read whole from the " +
				"rows again, by `reason`. `corrupt`, `derived`, `missing` and " +
				"`shape` are a repair a git process asked for — a request's " +
				"or a sync's own — read from the format strings of git's " +
				"own reports, which no request can write. `corrupt` and " +
				"`missing` are a rebuild into " +
				"a new directory beside the damaged one: it found a pack or " +
				"an index it could not read, or an object a row names that " +
				"no pack held — the last step of its repair, whichever git " +
				"named it, a request's or a sync's own, after an in-place " +
				"repair and a scrub of every file the copy holds found " +
				"nothing to fetch again, so it links every pack the scrub " +
				"found whole and downloads only the rest, nothing at all " +
				"when the damage is in the rows. `derived` is a " +
				"damaged derived file (the multi-pack-index with its bitmap " +
				"and reverse index, the commit-graph, or a chunk-format file " +
				"of either), named by any git process over the copy and " +
				"repaired IN PLACE — the same directory read whole again, " +
				"the files the damage named DROPPED so the round or the " +
				"maintainer rewrites sound ones over the same packs, none " +
				"downloaded — since those are derived files, not the " +
				"repository; damage the drop does not end escalates to " +
				"`corrupt`; a pack or object failure — a request's or a " +
				"sync's own — whose in-place repair only dropped derived " +
				"files is counted `derived` too. `shape` is the " +
				"pool refusing to run git there because the directory is " +
				"not the shape the engine writes (its config edited, a " +
				"commondir, an objects directory not its own, or an " +
				"alternates file planted in it), repaired IN PLACE — the " +
				"same directory read whole again, every pack it holds " +
				"reused and none downloaded — and rebuilt into a new one " +
				"only when the pool still refuses it after that; " +
				"any of them rising is disk or object-store damage, or " +
				"something outside the engine writing into the cache, which " +
				"a verify of the repository names — every pack checked " +
				"against its row, the ref file against the digest written " +
				"with it, the directory's shape, " +
				"and git's connectivity check. `verify` is that check, asked " +
				"to repair what it found: read whole IN PLACE, as `shape` " +
				"is, where everything it found is the copy's own " +
				"directory out of place — a file or entry the engine does " +
				"not write there, a pack no row names, its shape — every " +
				"pack, index, bitmap and graph kept and none downloaded, " +
				"but a pack no row names, which leaves as the copy moves " +
				"into its next directory, the index and graph built over " +
				"it along (`crewlet.code.cache.swaps`); " +
				"rebuilt into a new directory otherwise, or for what that " +
				"read leaves — a shape written past its sweep included, " +
				"never counted under `shape` — each counted once it has " +
				"read the copy whole, so a read in place that shape " +
				"stopped is not. `stored` is " +
				"no read of the rows' history at all: a pack whose stored " +
				"files — the pack, its index or its reverse index — a check " +
				"before serving or a scrub behind it found failing their " +
				"own checksum, a full sync found at another size than " +
				"their row's, a full sync found the copy had lost (named " +
				"by the rows at a position it had read), " +
				"or a git process named in words " +
				"and its row confirmed, fetched again IN PLACE with one " +
				"download a pack, renamed over the damaged files, counted " +
				"once a sync that did it — the first repair of a pack or " +
				"object failure, a request's git or a sync's own, which " +
				"reaches `corrupt` or " +
				"`missing` only when it and a scrub find nothing; rising is " +
				"a disk losing data under the cache. " +
				"`stream` is " +
				"the code log " +
				"RECREATED under the cache (provisioned anew, its stamp not " +
				"the one the cache read under); `rewound` is the log gone " +
				"BACK under it on the same stream — a broker restored from " +
				"an older copy — seen in a read at the log's head below a " +
				"position the cache had read. Either of those counts every " +
				"repository on the node once, as its next sync converges to " +
				"the rows the log holds rather than taking a delta from a " +
				"position that log never had or has since given to other " +
				"records. A repository marked to be read whole for more " +
				"than one reason at once is read once and counted once, " +
				"under the first: damaged derived files found on a " +
				"copy already marked for its shape or for the log are " +
				"counted under that reason, dropped all the same " +
				"(logged `code_cache_derived_repaired`).",
		},
		{
			Name: CodeCacheRepos, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories with a cache on this node. One leaves when " +
				"its rows are purged or the budget evicts it; every one that " +
				"arrives cost a cold sync of the whole repository. Falling " +
				"while the cache's bytes stay at the budget is eviction " +
				"churn: every repository it drops is a cold sync the next " +
				"time it is fetched.",
		},
		{
			Name: CodeCacheSyncDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long a completed cache sync took, by `kind` (`delta`: " +
				"the rows moved since the cache's position; `full`: the whole " +
				"repository — a cold cache, the first sync of a repository " +
				"after a restart, a log recreated under it), from its round's " +
				"start: the read of its rows, its downloads and its writes, " +
				"and for a `full` sync its wait for one of the eight " +
				"full reads the node runs at once " +
				"(`crewlet.code.cache.full_rounds`). A delta p95 climbing is a " +
				"node falling behind its pushes. A full sync's time is mostly " +
				"its downloads, sixteen at once, on a cache that never held " +
				"the repository — but on a node bringing up many " +
				"repositories at once, above all one restarted over the cache " +
				"it kept, which downloads nothing, it is mostly that wait: " +
				"`full_rounds` `waiting` climbing says so, not the object " +
				"store.",
		},
		{
			Name: CodeCacheSwaps, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"graph"},
			Shows: "Repositories whose cache on this node moved into its next " +
				"directory because packs left it — a compaction retired them, " +
				"or a full read found pack files no row names — or because a " +
				"published chain was placed in a copy a request may be " +
				"reading, built by hard links of every pack still live beside " +
				"the old directory, which the requests reading it keep until " +
				"they end; by what became of the commit-graph. `kept`: every " +
				"pack that left was a compaction's, which drops no commit, so " +
				"the graph came along and the multi-pack index and its bitmap " +
				"alone are written again. `dropped`: the copy held a graph — " +
				"its own, or its chain's graph layers — and the next " +
				"directory holds none: a pack left that no retirement " +
				"accounts for — the code log recreated or restored under the " +
				"node, or a full read that could not say which were retired " +
				"— or the chain was cut whole, and the background indexing " +
				"writes the graph again at once. `none`: the copy held no " +
				"graph to keep — not written yet, or past what the node's " +
				"memory holds for the repository — so nothing was lost. " +
				"`chain`: the next directory serves from an installed " +
				"published chain — one placed, or carried while packs left — " +
				"whose commit-graph layers came with it, so nothing is " +
				"written again. `failed`: the swap could not be " +
				"made — a link refused, the disk full — and the old directory " +
				"goes on serving, every live pack in it, until the " +
				"repository's next sync tries again (`code_cache_swap_failed` " +
				"names the step). `failed` rising is a cache volume out of " +
				"room or links; `dropped` beside a quiet log is full reads " +
				"that cannot see their retirements, every one a graph " +
				"rewritten.",
		},
		{
			Name: CodeCacheFullRounds, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"state"},
			Shows: "Full reads of a repository's rows this node has against " +
				"the estate now, by `state`: `running`, each holding one of " +
				"the node's eight slots from its first page to its last, and " +
				"`waiting`, in line for one, first come first served. A full " +
				"read is every repository's first sync after the node starts " +
				"or joins, a rebuild, every repository's next sync once the " +
				"code log was recreated or went back under the cache, and a " +
				"check's or a hint's read of the same rows; a sync after a " +
				"push reads only what moved and takes no slot. Every page of " +
				"a full read is one read on a data node — one of the 64 " +
				"answers its lane gives the whole fleet's reads at once, and " +
				"one of the reader connections every one of those reads " +
				"takes, eight on the smallest data node (on a node that holds " +
				"the data, its own store's) — so the slots keep a node coming " +
				"up with hundreds of repositories to what the smallest data " +
				"node scans at once, rather than every page while the " +
				"fleet's reads wait behind them. `running` at eight while " +
				"`waiting` climbs is a node bringing up more repositories " +
				"than it reads at once: each waits inside its own request's " +
				"time, and they come up eight reads at a time — about four " +
				"seconds for 500 repositories of a full page of refs each, " +
				"from a data node of eight cores.",
		},
		{
			Name: CodeCacheReadPageDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long one page of a repository's rows took the estate " +
				"to answer this node's cache, by `kind` (`full`: a page of a " +
				"whole repository's rows — a full sync's, a check's or a " +
				"hint's; `delta`: a page of what moved since the cache's " +
				"position), from the ask to the answer: the data node's lane " +
				"and reader connection, its scan of the page and the crossing " +
				"— or, on a node that holds the data, its own store's. A full " +
				"page of refs, ten thousand of them, takes about 60 ms to " +
				"scan, and a repository of a few hundred refs is one page of " +
				"a few milliseconds: a `full` p95 far under 60 ms is full " +
				"reads holding the node's eight slots " +
				"(`crewlet.code.cache.full_rounds`) briefly, and one climbing " +
				"past it is a data node whose readers queue — " +
				"`crewlet.store.pool.wait` there says so. It is production's " +
				"reading of the page the node's eight full reads at once " +
				"were measured against.",
		},
		{
			Name: CodeGitWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"class"},
			Shows: "How long a git process waited for a slot on this node, by " +
				"`class` (`read`, `transfer`, `maintenance`). A p95 " +
				"approaching ten seconds is a node about to answer clones and pushes " +
				"with 503 — every slot of one lane busy for as long as a local git read may take. " +
				"`transfer` waiting is a node serving as many clones, pushes and downloads as " +
				"`gitlet.max_transfers` admits (more Gitlet nodes, or more memory and a " +
				"wider `gitlet.max_transfers`); `read` waiting is a node out of " +
				"`gitlet.max_processes`, one core a slot.",
		},
		{
			Name: CodeGitCPU, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"class"},
			Shows: "CPU a git process tree spent, by lane: `transfer` is clones, " +
				"pushes and downloads, `read` the cache's reads and a push's scans and " +
				"fetch checks, `maintenance` the bitmaps, indexes and checks. " +
				"One observation per process, its whole cost, recorded when it " +
				"ends: its user and system time and that of every process it " +
				"waited for (upload-pack's pack-objects, receive-pack's " +
				"index-pack) — and for one the node ended (a deadline, a stall, " +
				"a client gone) at least what its whole group had spent when it " +
				"was signalled; on macOS, only what the process and the " +
				"children it had reaped spent. So the sum is what a lane's runs " +
				"cost, read over a window longer than the runs it counts — a " +
				"clone may run for hours and a bitmap for a quarter of an hour, " +
				"each landing whole in the window it ends in — and never the " +
				"share of the node git is taking now, which is the container's " +
				"own CPU usage. Read the sum and the count rather than the " +
				"percentiles: a run past 64 s of CPU lands in the last bin.",
		},
		{
			Name: CodeGitProcesses, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"class"},
			Shows: "Git processes running on this node, by `class`. `transfer` " +
				"pinned at `gitlet.max_transfers` while `crewlet.code.git.wait` " +
				"climbs is a node serving as many clones and pushes as its memory " +
				"was sized for; `maintenance` is background index rebuilding, " +
				"at most half of `gitlet.max_processes`.",
		},
		{
			Name: CodeGitLentCores, Kind: KindGauge, Unit: UnitCount,
			Shows: "Cores of this node's read lane lent right now to pushes as " +
				"extra index-pack threads — up to three a push, never the last " +
				"free core. A loan is made when a push's receive-pack starts — " +
				"the one that receives its pack, never the refs advertisement " +
				"its first request is answered with — and repaid once its pack " +
				"is indexed, so it covers the client's whole upload: " +
				"lent cores held high while `crewlet.code.git.processes` for " +
				"`read` sits low is slow uploads holding loans, and the reason " +
				"a later push on this node indexes on one core.",
		},
		{
			Name: CodeGitShareWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"share"},
			Shows: "How long a clone, fetch, push or download waited for its transfer " +
				"slot on this node, by the `share` it waited in (`person`: the " +
				"holder of a human seat — a person through any credential of " +
				"theirs, or a Tier A token or service account the identity " +
				"directory binds to a human seat nobody else holds; " +
				"`machine`: a credential acting as nobody, a Tier A token or a " +
				"service account's machine token bound to no seat; `run`: a " +
				"box's git on `/box/git/` — a coding box's, with its run's " +
				"credential, or a check's box's, with its check's). " +
				"When every slot is held a released one goes to the class " +
				"furthest below its weighted share of what the lane spent " +
				"lately (person 4 : machine 2 : run 1), so `machine` climbing " +
				"while `person` stays flat is the share doing its job — CI " +
				"waiting behind CI, not people behind CI — and `person` and " +
				"`machine` both climbing is a lane too narrow for its traffic: " +
				"more memory and a wider " +
				"`gitlet.max_transfers`, or more Gitlet nodes. Coding boxes together " +
				"hold at most half the lane, so `run` climbing alone is boxes " +
				"waiting behind each other at that bound, never a person " +
				"behind a box.",
		},
		{
			Name: CodeGitShare, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"share"},
			Shows: "What each `share` of this node's transfer lane has spent " +
				"lately, as the scheduler reads it: core-seconds, every run " +
				"charged the greater of the CPU it spent and the time it held " +
				"its slot at the share of a core one slot stands for — the " +
				"node's cores over its transfer slots, never more than one: a " +
				"quarter at four slots a core, a whole core where memory or " +
				"`gitlet.max_transfers` leaves fewer slots than cores — decayed " +
				"over a minute, the figure the next contended slot is granted " +
				"by. Set at each " +
				"admission and release, so a lane idle since holds its last " +
				"reading. One class's reading far above its weight's share of " +
				"the total while the others' waits are short is a class alone " +
				"on the lane, which runs it at full width — `person` and " +
				"`machine` alike, while `run`, a coding box's, holds at most " +
				"half of it.",
		},
		{
			Name: CodeSeatworkHeld, Kind: KindGauge, Unit: UnitCount,
			Shows: "Seat git work running at once on this Gitlet node, of half its " +
				"read lane: the units of the node's seat git-work share held " +
				"now — server-side reads of a repository by object id running " +
				"their git, the engine's own among them, and the builds of " +
				"engine writes, a seat's or a person's commit made through a " +
				"tool. The share is half of " +
				"`gitlet.max_processes` " +
				"(at least one), and the other half of the read lane stays the " +
				"node's own and its persons' — a push's scans, a sync's peel, " +
				"a fetch's check of its wants. Pinned at the share while the " +
				"count of `crewlet.code.seatwork.waits{outcome=unready}` " +
				"climbs is a node whose seats ask more git than half its " +
				"cores, and sends the rest to another Gitlet node.",
		},
		{
			Name: CodeSeatworkWaits, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome", "work"},
			Shows: "How long every unit of this Gitlet node's seat git-work " +
				"share asked for waited, by `outcome` — `granted`, taken " +
				"within two seconds; `unready`, none free in two seconds, " +
				"answered so the asker is served by another Gitlet node, its " +
				"wait the whole two — and by `work` (`read`: a seat's or a " +
				"person's server-side read of a repository by object id; " +
				"`write`: the build of an engine write, a commit made through " +
				"a tool; `engine`: the engine's own read — a revision's facts " +
				"or a checks file a repository's home reads, or a read made " +
				"for a check). One whose " +
				"caller went away while it waited is not counted. The wait " +
				"is a whole term of the attempt a seat's read or write is " +
				"given on one Gitlet node, so the `granted` tail nearing two " +
				"seconds is a node about to send its seats' work on; the " +
				"`unready` count is a node at its share for whole seconds: " +
				"more Gitlet nodes, or more cores and a wider " +
				"`gitlet.max_processes`, which widens the share with the lane.",
		},
		// ---- Gitlet ---------------------------------------------------
		{
			Name: CodePushDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long a git push took on this node, from its request to its " +
				"answer, by `outcome` (`applied`, `pending`, `unknown`, `refused`, " +
				"`failed`). `pending` growing is a code log whose records are " +
				"durable and slow to apply here; `unknown` is a committer that " +
				"stopped answering; the tail of `applied` is what a person waits " +
				"for a push.",
		},
		{
			Name: CodePushBytes, Kind: KindHistogram, Unit: UnitBytes,
			Shows: "The size of each push's bundle — the pack its receive-pack " +
				"kept and the two indexes it wrote — observed once the pack is " +
				"checked, whatever becomes of the push: the one object the object " +
				"store stores for it, against `code.limits.max_push_bytes` (what " +
				"the client may send) and the 1.25 GiB one push may store.",
		},
		{
			Name: CodeFetchDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"command", "sync"},
			Shows: "How long a fetch request took on this node, by `command` " +
				"(`advertise`, `ls-refs`, `fetch`) and by what the cache had to " +
				"do first (`sync`: `none`, `delta`, `full`). A `full` tail is cold " +
				"caches downloading whole repositories; a `delta` tail is the " +
				"barrier and the sync every answer that names a ref waits for.",
		},
		{
			Name: CodeHostSpoolWaits, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Fetch requests large enough to spool — a partial clone's " +
				"lazy fetch past a mebibyte of wants — that could not start " +
				"spooling, or take room for the rest of their body, at once on " +
				"this node and waited their turn, by `outcome`: `granted`, " +
				"served after its wait; `expired`, answered 503 after waiting " +
				"55 s unserved; `abandoned`, its request ended while it " +
				"waited; and `refused`, answered 503 at once because it had to " +
				"wait and found no place to — the node's waiting room full, " +
				"four places a git transfer slot, or its credential's quarter " +
				"of it, a lane's width. git does not retry a lazy fetch, so " +
				"every `expired` or `refused` is a checkout that failed, and " +
				"git prints only the status: the `code_fetch_spool_unserved` " +
				"line names the token and what held it back. `granted` " +
				"climbing is a credential at its quarter of the node's " +
				"spooling — one token behind several CI runners, which a " +
				"token per runner ends — or a node whose spooling is full: " +
				"more git transfer slots (more memory, a wider " +
				"`gitlet.max_transfers`) or more Gitlet nodes.",
		},
		{
			Name: CodeHostSpoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long each fetch counted by " +
				"`crewlet.code.host.spool.waits` waited for room to spool, by " +
				"the same `outcome` (`granted`, `expired`, `abandoned`; a " +
				"`refused` one waited for nothing). The `granted` tail is what " +
				"the wait adds to a checkout; one nearing 55 s is a node about " +
				"to answer checkouts 503.",
		},
		{
			Name: CodeBoxResyncs, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "A coding box's reads that went to the code log's head on " +
				"this node — a fetch whose want the node's copy lacked, a " +
				"request past the bound its admitted row set, a repository " +
				"its run was given that its run's floor would refuse, a run " +
				"whose row names no floor — and a check's box's alike, its " +
				"reads at /box/gitlet/ this node answers among them, each " +
				"a barrier on the code log, " +
				"by `outcome`: `taken`, paid from its run's bucket on its " +
				"seat — the seat bucket its turns' code tools on this node " +
				"pay too — or a check's attempt's on its repository's checks " +
				"(16 at once, one more every 11.5 minutes: the census's " +
				"125 a seat-day); `spent`, answered at the run's floor " +
				"instead, the box told what it asked for is not there and " +
				"when its re-syncs come back. `taken` is the boxes' share of " +
				"the code log's barriers; `spent` climbing is a box asking " +
				"in a loop for what nobody pushed — the " +
				"`code_box_resyncs_spent` line names its seat and run.",
		},
		{
			Name: CodeBoxInflightRefused, Kind: KindCounter, Unit: UnitCount,
			Shows: "A coding box's git requests this node answered 503 " +
				"(`run_busy`) because its run — or a check's box's attempt " +
				"— already had 16 in flight here, twice the repositories " +
				"a run is given. Any is a box " +
				"sending requests faster than they are served, which its " +
				"retry absorbs; climbing is one run's loop, which the " +
				"`code_box_unavailable` line names by its turn and launch.",
		},
		{
			Name: CodeHostWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"kind", "outcome"},
			Shows: "Every engine write this Gitlet node built or answered — a " +
				"commit, a revert, a cherry-pick, a branch created or deleted, " +
				"a review's suggestions applied through a tool, a change merged or rebased, " +
				"a seat's or a person's — by `kind` (`commit_files`, `revert`, " +
				"`cherry_pick`, `create_branch`, `delete_branch`, " +
				"`apply_suggestions`, `merge_change`, `rebase_change`) and " +
				"`outcome`: `applied`, `pending` and `unknown` as a push's, " +
				"from its row — a merge's from its merge record, or its " +
				"enqueue's where its target's queue took it; `refused`, a " +
				"refusal of the change or of its push (a conflict, nothing to " +
				"change, a suggestion outdated or overlapping, a stale head, a " +
				"protection, the grant, a merge its gate or its target's moves " +
				"refused) or a write the call " +
				"named wrong; `unready`, this node could not answer in time " +
				"and another Gitlet node was asked; and `failed`, the node's own " +
				"failure. `unready` climbing is a node behind its syncs or at " +
				"its share of seat git work; `failed` is never routine — the " +
				"`code_write` line names it.",
		},
		{
			Name: CodeHostWriteDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind", "stage"},
			Shows: "How long each engine write took on this node, by `kind` " +
				"and `stage`: `sync`, bringing the copy to the caller's floor " +
				"and judging the write on it, the check of where its branch is " +
				"and of the objects it names among it; `build`, its git — the " +
				"read of what each path it names and each directory above one " +
				"is, the lookups of what it changes, an apply's placement of its " +
				"suggestions, its objects, its commit and its pack, under a unit " +
				"of the seat git-work share; `land`, the " +
				"landing every push takes — the upload, the proposal and the " +
				"wait for its row, a merge's its merge record's and every " +
				"rebuild onto a target that moved; and `total`. A `build` tail " +
				"near ten seconds — thirty for a `merge_change`, whose one " +
				"merge is a mergeability's, and forty for a `rebase_change`, " +
				"each of whose commits replays one — is changes too wide or too deep " +
				"for an engine write, answered too_large; a `land` tail is the " +
				"code log's, as a push's is.",
		},
	}
}
