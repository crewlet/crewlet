// Package mirror is a repository's HOME git node filling it from another host:
// an IMPORT ([code.CreateRepo.Import], a pull mirror's first fill) and a PULL
// MIRROR's syncs ([code.RepoSettings.MirrorFrom]) — M6-D16 and M6-D19. The
// home is the first git node of the repository's rendezvous order over the
// live git nodes (ADR-0059), and [Mirrors] is a worker of its loop
// (internal/code/home's Loop, through that package's Worker interface, which
// this package satisfies without importing it): its listing is the
// `imports_due` and `mirrors_due` reads (internal/code's ReadImportsDue and
// ReadMirrorsDue), its work one repository's import or sync, nudged by the
// code log's watch and listed again by the loop's backstop. The rows are the
// work list, and what lands is a record of the repository's own subject
// through its one committer queue (S27): an import's chunks and its end
// (code.ImportChunk, code.ImportDone, Committer.ProposeImport) and a sync
// (code.MirrorSync, Committer.ProposeMirrorSync).
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
// cost internal/code/cache's "A MERGE'S REPOSITORIES" measured) — at
// code.CompactPackBytes, the cut every stored pack of a rewrite is held to.
// Each output is checked for a blob past code.limits.max_blob_bytes, the one
// limit a decide cannot judge since it reads no blob, and stored by
// code.StoreBundle under an id derived from the record that lands it.
//
// THE RECORDS, PACKS FIRST: an import lands its outputs before any ref that
// needs them — chunks of packs alone while more than code.MaxImportPacks are
// left, then the refs, code.MaxImportRefs a chunk, the first of them riding
// with the last packs — each chunk at the next Seq, so a chunk asked twice or
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
// # Its bounds
//
// AN ATTEMPT OUTLIVES A TURN. A home's turn is one backstop long (a minute),
// and an import of a large repository is a fetch of up to its repo_max_bytes:
// so a turn only STARTS an attempt, on a goroutine of this package's own, and
// the turns after it find the attempt running and leave it ([Mirrors.Work]).
// Each of its steps is bounded on its own — the fetch by
// gitproc.RemoteDeadline over what it may bring, the cut and the scan by
// gitproc.PackWorkDeadline, each upload by code.UploadDeadline — and every
// attempt ends with [Mirrors.Stop]. At most [MaxAttempts] run at once on a
// node. A pull mirror is synced once every code.MirrorInterval: listed by the
// `mirrors_due` read until its last sync is within it, and, where its source
// moved nothing and so no record moved its row, held back by this node's
// memory of its last attempt.
//
// ITS IMPORTS: internal/code/query, internal/code/compact, internal/code/host,
// internal/code/home, internal/code/checkrun and this package never import
// each other: each reaches the others' work through the estate's router.
package mirror
