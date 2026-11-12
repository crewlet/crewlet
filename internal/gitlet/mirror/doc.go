// Package mirror is a repository's HOME Gitlet node filling it from another host
// and publishing it to one: an IMPORT ([gitlet.CreateRepo.Import], a pull
// mirror's first fill), a PULL MIRROR's syncs ([gitlet.RepoSettings.MirrorFrom])
// and a PUSH MIRROR's pushes ([gitlet.RepoSettings.MirrorTo]) — M6-D16 and
// M6-D19. The home is the first Gitlet node of the repository's rendezvous order
// over the live Gitlet nodes (ADR-0089), and [Mirrors] is a worker of its loop
// (internal/gitlet/home's Loop, through that package's Worker interface, which
// this package satisfies without importing it): its listing is the
// `imports_due` and `mirrors_due` reads (internal/gitlet's ReadImportsDue and
// ReadMirrorsDue), its work one repository's import or sync and its push,
// nudged by the code log's watch and listed again by the loop's backstop. The
// rows are the work list, and what an import or a sync lands is a record of
// the repository's own subject through its one committer queue (S27): an
// import's chunks and its end (gitlet.ImportChunk, gitlet.ImportDone,
// Committer.ProposeImport) and a sync (gitlet.MirrorSync,
// Committer.ProposeMirrorSync). A push lands nothing here: it moves refs at
// the other host, and its account is an event (code_mirror_pushed). An
// import's end is the other account this package gives: once the home hears
// it applied, one code_imported names the source, the chunks, the import's
// error and what the repository holds then, read from the copy synced at or
// past the end's record — the repository's row keeps only its latest import,
// and its chunks' push rows name no source. A sync is no event: its push row
// is.
//
// # One attempt, from the rows
//
// An attempt syncs this node's copy of the repository, makes a SCRATCH over
// its incarnation (cache.Cache.NewScratch: an empty repository whose one
// alternate is the copy's objects), and fetches the source's branches and
// tags into it (`+refs/heads/*`, `+refs/tags/*`) through the one outbound
// dial (gitproc.Cmd.Remote, M6-D18), with transfer.fsckObjects — the check a
// push's receive-pack makes of what it is sent — and fetch.unpackLimit 1, so
// everything fetched stays a pack the cut below can name. Never in the copy:
// what an attempt fetched enters the cache only by an adoption once its rows
// have applied (cache.Cache.Adopt), as a push's pack does, so a copy stays a
// function of rows. The fetch NEGOTIATES FROM THE IMPORTED REFS: git lists an
// alternate's refs as what the fetching side has, so an attempt after a
// restart — or a sync after the last — fetches only what the copy's refs do
// not reach.
//
// What arrived is CUT into the packs the repository stores by ONE
// `pack-objects --stdin-packs` in the scratch: every pack the fetch wrote
// listed, and every live pack of the copy excluded with a `^` line — so an
// object the copy holds already is not stored twice (a fetch completing a
// thin pack appends such objects), and the walk git makes for its name hashes
// stops at the copy's edge rather than walking the repository's history (the
// cost internal/gitlet/cache's "A MERGE'S REPOSITORIES" measured) — at
// gitlet.CompactPackBytes, the cut every stored pack of a rewrite is held to.
// Each output is checked for a blob past code.limits.max_blob_bytes, the one
// limit a decide cannot judge since it reads no blob, and stored by
// gitlet.StoreBundle under an id derived from the record that lands it.
//
// THE RECORDS, PACKS FIRST: an import lands its outputs before any ref that
// needs them — chunks of packs alone while more than gitlet.MaxImportPacks are
// left, then the refs, the first of them riding with the last packs — each
// chunk taking the most refs its record carries, at most gitlet.MaxImportRefs
// and within the bytes one record may hold (gitlet.ImportProposal.Fitted, since
// refs whose names JSON spells long reach those bytes first), cut and checked
// as its committer will check it before a byte of its packs is uploaded, so a
// step no committer would take costs no PUT — each chunk at the next Seq, so
// a chunk asked twice or
// out of turn is refused stale by its committer; then its end, naming the
// source's HEAD branch where the branch the repository declares did not
// arrive (`ls-remote --symref`). A sync lands the same steps as syncs, each
// at the drop epoch the copy held (M6-D4). Every command's old value is the
// ref as the copy held it after the steps before, and a ref the source no
// longer has is deleted: a mirror is its source, force-pushes and deletions
// included, and a command that leaves an object is the committer's to record
// as a drop (code_ref_drops). Its Forced is read here, from the history the
// attempt fetched: `merge-base --is-ancestor` of the old value and the new.
//
// RESUMED FROM ROWS, NEVER FROM MEMORY: an attempt that dies anywhere — a
// node killed between two chunks, a committer busy, a stale refusal — leaves
// only what landed, and the next attempt reads the rows again: the chunks
// that applied are the copy's refs and packs, the next Seq is the row's
// import_seq plus one, and the fetch asks only for what those refs do not
// reach. A pack stored by an attempt whose record never landed is an object
// no row names, which the object store's collector removes after its grace.
// A chunk of packs alone that applied before its refs is the one case a
// restart fetches again in full — no ref of the copy reaches those packs yet —
// and the cut then stores nothing of what they hold.
//
// AN IMPORT THAT CANNOT FINISH ENDS: a blob past the limit, a chunk its
// committer refuses for a limit the source is past (repo_size, repo_refs,
// ref_conflict, pack_too_large), or a source that left an object loose — git's
// dumb HTTP protocol, whose objects no `--stdin-packs` cut names and which
// its walker writes unchecked by transfer.fsckObjects — ends the import with
// ImportDone.Error, the
// repository active with what landed and its import_error saying why — a
// permanent refusal, which another attempt would only meet again. Anything
// else — the source unreachable, its credential refused, a busy committer —
// leaves the repository importing, and the next attempt tries again
// ([RetryAfter]).
//
// # The push mirror follows green
//
// A PUSH publishes what the company's checks passed, never what somebody
// pushed last: a branch a protection rule protects is pushed at its GREEN
// row's commit (code_green, M5's landing rows: every move of a protected
// branch leaves a landing, and the green row is the newest one whose required
// checks passed), and held where the other host has it while it has none;
// every other covered ref — a branch no rule protects, which moves with no
// landing, and every tag — at the copy's tip. Covered are the push mirror's
// own patterns, or [DefaultRefs] (the default branch, read when it pushes,
// and refs/tags/*); the seats' namespace never is (the settings refuse it).
//
// One attempt (Mirrors.push) reads the push mirror and the repository's
// green rows in ONE snapshot (the engine's own `mirror_push` read, so the
// target it dials and the commits it publishes are one version's), syncs this
// node's copy at or past it, judges the seat it pushes as against the chart
// this node holds NOW (gitlet.MirrorSeatRefusal: a later revision may have taken
// the seat's read away), lists what the other host holds of the covered refs
// in ONE `ls-remote --refs` of the arms its patterns reach (`--branches`,
// `--tags`: protocol v2's ref prefixes, so a forge's pull-request refs never
// come), streamed as the copy's own listing is — neither is ever captured,
// since both grow with a repository — and pushes the difference — each ref set where
// the other host holds another object or none, each covered ref the copy no
// longer has deleted, a waiting branch never — with `push --force --porcelain`
// from a SCRATCH over the copy's incarnation, never from the copy, which only
// a sync writes; at most pushBatch refspecs a push. It carries the seat's
// OWN identity at the other host — the token the mirror's credential names,
// sent by the one outbound dial (M6-D18) — never a bot's, and the trailers
// a commit's box wrote (its seat, run and work item, by the executor's
// attribution hook, internal/sandbox/executor) travel with it as they are:
// the mirror inherits what the box's hook wrote and decides nothing about
// them (U25 a). Their keys are not spelled here, since only the attribution
// may spell one.
//
// A FAILED PUSH IS THE HOME'S STATE, in memory alone: a push to another host
// is no record, and the node that pushes is the one that knows whether it
// can. [Mirrors.AlarmReading] counts every mirror — a pull mirror's syncs and
// a push mirror's pushes — not brought level within its own refresh promise,
// gitlet.MirrorInterval plus [AttemptDeadline] (ADR-0015), which raises
// code_mirror_failing on that home. A push mirror is pushed at most once a
// [PushPace] (a minute) when its repository's records nudge it, and once a
// gitlet.MirrorInterval by the backstop while it is idle; an attempt dials the
// other host only where what the mirror covers moved since the last left it
// level, or that was an interval ago (Mirrors.stillLevel), so a seat's pushes
// to its own branches and a check's results dial nobody.
//
// # Its bounds
//
// AN ATTEMPT OUTLIVES A TURN. A home's turn is one backstop long (a minute),
// and an import of a large repository is a fetch of up to its repo_max_bytes:
// so a turn only STARTS an attempt, on a goroutine of this package's own, and
// the turns after it find the attempt running and leave it ([Mirrors.Work]).
// Each of its steps is bounded on its own — the fetch by
// gitproc.RemoteDeadline over what it may bring, the cut and the scan by
// gitproc.PackWorkDeadline, each upload by gitlet.UploadDeadline — and every
// attempt ends with [Mirrors.Stop]. THE CUT WAITS FOR ITS SLOT: the
// maintenance lane refuses a process at once when it is full, and the cut
// comes after a fetch of up to the repository's whole size, so the attempt
// polls the lane for a slot it then holds (gitproc.Pool.Reserve) and keeps
// its scratch meanwhile, rather than ending and fetching everything again.
//
// At most [MaxAttempts] run at once on a node, and one due while they run is
// QUEUED on the worker and started as soon as one ends — the one this node
// attempted longest ago first — so a node brings its mirrors level at
// MaxAttempts over an attempt's length, never at MaxAttempts a pass. A pull
// mirror is synced once every gitlet.MirrorInterval: listed by the
// `mirrors_due` read until its last sync is within it — a sync that found its
// source moved nothing landing a record of nothing (gitlet.MirrorSync.Current),
// which moves that instant all the same, and a pull mirror's import that
// landed whole moving it to its end — so the listing stops at the first
// mirror synced within the interval on every node, this one after a restart
// included; this node's memory of its last attempts holds a mirror whose
// last attempt failed back for RetryAfter. What a whole listing does not
// name, the worker forgets ([Mirrors.Due]).
//
// ITS IMPORTS: internal/gitlet/query, internal/gitlet/compact, internal/gitlet/host,
// internal/gitlet/home, internal/gitlet/checkrun and this package never import
// each other: each reaches the others' work through the estate's router.
package mirror
