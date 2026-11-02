package code_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN IMPORT AND A PULL MIRROR THROUGH THE WHOLE PATH: their records proposed to
// a committer, decided in one snapshot, published on a real embedded broker,
// applied by the shipped applier and answered from the rows they wrote.

// chunkOf is chunk seq of repo's import on n, landing one pack of objects
// objects and cmds, its id minted now.
func chunkOf(n *node, repo string, seq int64, objects int64, cmds ...code.Command) code.ImportChunk {
	id := statelog.NewOpID(time.Now(), "")
	c := code.ImportChunk{RepoID: repo, ChunkID: id, Node: n.id, Seq: seq, Commands: cmds}
	if objects > 0 {
		c.Packs = []code.PackRef{compactOutput(id, 0, objects, harnessAt)}
	}
	return c
}

// syncOf is a sync of the pull mirror repo on n at drop epoch epoch, landing
// one pack and cmds, its id minted now.
func syncOf(n *node, repo string, epoch int64, cmds ...code.Command) code.MirrorSync {
	id := statelog.NewOpID(time.Now(), "")
	return code.MirrorSync{RepoID: repo, SyncID: id, Node: n.id, DropEpoch: epoch, Commands: cmds,
		Packs: []code.PackRef{compactOutput(id, 0, 4, harnessAt)}, FetchedAt: harnessAt}
}

// requireSynced fails t unless res is an import or mirror record applied at a
// position.
func requireSynced(t *testing.T, what string, res code.SyncResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" {
		t.Fatalf("%s answered %+v, want applied at a position", what, res)
	}
}

// requireStale fails t unless res is refused refusal with nothing appended
// since before.
func requireStale(t *testing.T, n *node, what string, before uint64, res code.SyncResult, err error,
	refusal code.Refusal) {
	t.Helper()
	if err != nil || res.Refusal != refusal || res.Outcome != "" {
		t.Fatalf("%s answered %+v, %v; want refused %s, unrecorded", what, res, err, refusal)
	}
	if end := n.end(); end != before {
		t.Fatalf("%s was refused and appended %d records", what, end-before)
	}
}

// A CHUNK LANDS ONLY IN ITS TURN: an import's chunks land one after another,
// each one more than the repository's import_seq — a chunk ahead of its turn
// and a chunk landed already, asked again under a new attempt's id, are each
// refused stale with nothing appended — and a chunk whose command names a ref
// as it is not is refused stale whole. A chunk that lands writes its refs, its
// pack as a live pack of kind import, its push row `via import` on the
// engine's own account, and a drop for the object its forced command left;
// the import's end makes the repository active — refused stale where it would
// move the default branch off the declared one the import brought — and an
// end, or a chunk, after it is stale. A chunk that lands no pack and sets no
// ref is no record at all.
//
// Mutation: drop the decide's turn rule, and chunk 2 lands before chunk 1;
// write a chunk's pack as a push's kind, and its row says push; drop the
// end's rule for the declared branch, and the end moves the default to
// feature.
func TestAChunkLandsOnlyInItsTurn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), code.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || got.Status != code.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", got, err)
	}
	n.drain()
	repo := got.RepoID
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	main, feature := "refs/heads/main", "refs/heads/feature"

	ahead := chunkOf(n, repo, 2, 3, update(feature, code.ZeroOID, oidOf("F")))
	before := n.end()
	res, err := c.ProposeImport(ctx, code.ImportProposal{Chunk: &ahead})
	requireStale(t, n, "a chunk ahead of its turn", before, res, err, code.RefusalStale)

	empty := chunkOf(n, repo, 1, 0)
	before = n.end()
	if _, err := c.ProposeImport(ctx, code.ImportProposal{Chunk: &empty}); !errors.Is(err,
		code.ErrInvalidProposal) || n.end() != before {
		t.Fatalf("a chunk landing no pack and setting no ref answered %v; want it refused invalid, unrecorded", err)
	}

	first := chunkOf(n, repo, 1, 5, update(main, code.ZeroOID, oidOf("A")))
	res, err = c.ProposeImport(ctx, code.ImportProposal{Chunk: &first})
	requireSynced(t, "the first chunk", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'import'
		AND actor_kind = 'system' AND position = ?`, first.ChunkID, res.Position.Packed()); got != 1 {
		t.Fatalf("the first chunk wrote no push row via import on the engine's account at its record")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND pack_id = ? AND kind = 'import'
		AND state = 'live'`, repo, first.Packs[0].PackID); got != 1 {
		t.Fatalf("the first chunk's pack is not a live pack of kind import")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND ref = ? AND target = ?`, repo, main,
		oidOf("A")); got != 1 {
		t.Fatalf("the first chunk did not set %s", main)
	}

	again := chunkOf(n, repo, 1, 5, update(main, code.ZeroOID, oidOf("A")))
	before = n.end()
	res, err = c.ProposeImport(ctx, code.ImportProposal{Chunk: &again})
	requireStale(t, n, "the first chunk asked again", before, res, err, code.RefusalStale)

	moved := chunkOf(n, repo, 2, 0, update(main, oidOf("Z"), oidOf("B")))
	before = n.end()
	res, err = c.ProposeImport(ctx, code.ImportProposal{Chunk: &moved})
	requireStale(t, n, "a chunk naming a ref as it is not", before, res, err, code.RefusalStale)

	forced := update(main, oidOf("A"), oidOf("B"))
	forced.Forced = true
	second := chunkOf(n, repo, 2, 2, forced, update(feature, code.ZeroOID, oidOf("F")))
	res, err = c.ProposeImport(ctx, code.ImportProposal{Chunk: &second})
	requireSynced(t, "the second chunk", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ? AND ref = ? AND old = ?`, repo,
		main, oidOf("A")); got != 1 {
		t.Fatalf("the forced command of the second chunk left no drop of %s", oidOf("A"))
	}
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND import_seq = 2 AND status = 'importing'`,
		repo); got != 1 {
		t.Fatalf("after two chunks the repository is not importing at chunk 2")
	}

	elsewhere := code.ImportDone{RepoID: repo, Node: n.id, Seq: 2, DefaultBranch: "feature"}
	before = n.end()
	res, err = c.ProposeImport(ctx, code.ImportProposal{Done: &elsewhere})
	requireStale(t, n, "an end moving the default off the declared branch the import brought", before, res, err,
		code.RefusalStale)

	done := code.ImportDone{RepoID: repo, Node: n.id, Seq: 2}
	res, err = c.ProposeImport(ctx, code.ImportProposal{Done: &done})
	requireSynced(t, "the import's end", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND status = 'active' AND imported_at != 0
		AND import_error = ''`, repo); got != 1 {
		t.Fatalf("the import's end did not make the repository active, imported")
	}
	late := chunkOf(n, repo, 3, 1, update("refs/tags/v1", code.ZeroOID, oidOf("T")))
	for what, p := range map[string]code.ImportProposal{"a second end": {Done: &done},
		"a chunk after the end": {Chunk: &late}} {
		before = n.end()
		res, err = c.ProposeImport(ctx, p)
		requireStale(t, n, what, before, res, err, code.RefusalStale)
	}
}

// A REPOSITORY'S DETAIL SAYS WHERE IT STANDS WITH OTHER HOSTS: an imported
// repository's detail names the import's source, how many of its chunks have
// landed and, once it ended, when and why it ended short; a pull mirror's names
// its source and its last sync's instant — what a person who imported it or
// set the mirror reads its progress by. A repository with neither details
// none.
//
// Mutations: serve no import progress (Chunks left 0) — the detail after a
// chunk says none landed, red; drop the sync's instant from the read — the
// mirror reads as never synced, red.
func TestARepositorysDetailSaysWhereItStandsWithOtherHosts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ctx := t.Context()
	detail := func(name string) code.RemoteState {
		t.Helper()
		a, err := n.reader.Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: name})
		if err != nil || a.Repo == nil {
			t.Fatalf("read %s's detail: %+v, %v", name, a.Repo, err)
		}
		return a.Repo.Remotes
	}
	web := repoOn(t, n, "web")
	if got := detail("web"); got.MirrorFrom != nil || got.MirrorTo != nil || got.Import != nil ||
		!got.MirrorSyncedAt.IsZero() {
		t.Fatalf("a repository with no other host details %+v, want none", got)
	}

	made, err := n.store.EnsureRepo(ctx, n.activate(t1), code.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || made.Status != code.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", made, err)
	}
	n.drain()
	c, _ := committerOn(t, n, n.pub)
	if got := detail("imported").Import; got == nil || got.Source != *importSource || got.Chunks != 0 ||
		!got.DoneAt.IsZero() || got.Error != "" {
		t.Fatalf("an import just begun details %+v, want its source, no chunk landed and no end", got)
	}
	first := chunkOf(n, made.RepoID, 1, 5, update("refs/heads/main", code.ZeroOID, oidOf("A")))
	res, err := c.ProposeImport(ctx, code.ImportProposal{Chunk: &first})
	requireSynced(t, "the first chunk", res, err)
	if got := detail("imported").Import; got == nil || got.Chunks != 1 || !got.DoneAt.IsZero() {
		t.Fatalf("an import one chunk in details %+v, want one chunk landed and no end", got)
	}
	short := "a blob past code.limits.max_blob_bytes"
	done := code.ImportDone{RepoID: made.RepoID, Node: n.id, Seq: 1, Error: short}
	res, err = c.ProposeImport(ctx, code.ImportProposal{Done: &done})
	requireSynced(t, "the import's end", res, err)
	if got := detail("imported").Import; got == nil || got.Chunks != 1 || got.DoneAt.IsZero() ||
		got.Error != short {
		t.Fatalf("an import ended short details %+v, want its end's instant and why", got)
	}

	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	n.ensure("web", s, t2)
	if got := detail("web"); got.MirrorFrom == nil || *got.MirrorFrom != *mirrorSource ||
		!got.MirrorSyncedAt.IsZero() || got.Import != nil {
		t.Fatalf("a pull mirror never synced details %+v, want its source and no sync", got)
	}
	sync := syncOf(n, web, 0, update("refs/heads/synced", code.ZeroOID, oidOf("A")))
	res, err = c.ProposeMirrorSync(ctx, code.MirrorSyncProposal{Sync: sync})
	requireSynced(t, "the sync", res, err)
	if got := detail("web"); got.MirrorSyncedAt.IsZero() {
		t.Fatalf("a pull mirror synced details %+v, want its last sync's instant", got)
	}
}

// A MIRROR SYNC IS STALE AGAINST A MOVED REF: a pull mirror's sync lands its
// refs, its pack of kind mirror, its push row `via mirror` and its sync's
// instant; a second sync built over the ref as the first found it is refused
// stale — the next round reads it again — and a sync whose packs were cut at
// another drop epoch is refused expunging, each with nothing appended. A sync
// of a repository that follows no source is stale too.
//
// Mutation: drop the decide's comparison of each command's old value, and the
// second sync lands over the first; drop its epoch rule, and the sync cut at
// another epoch lands.
func TestAMirrorSyncIsStaleAgainstAMovedRef(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	main := "refs/heads/main"
	pushed, err := c.Propose(ctx, proposal(repo, update(main, code.ZeroOID, oidOf("A"))))
	if err != nil || pushed.Answer.Refused != "" {
		t.Fatalf("push main: %+v, %v", pushed, err)
	}

	plain := syncOf(n, repo, 0, update(main, oidOf("A"), oidOf("B")))
	before := n.end()
	res, err := c.ProposeMirrorSync(ctx, code.MirrorSyncProposal{Sync: plain})
	requireStale(t, n, "a sync of a repository following no source", before, res, err, code.RefusalStale)

	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	n.ensure("web", s, t2)

	first := syncOf(n, repo, 0, update(main, oidOf("A"), oidOf("B")))
	res, err = c.ProposeMirrorSync(ctx, code.MirrorSyncProposal{Sync: first})
	requireSynced(t, "the first sync", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ? AND via = 'mirror'
		AND actor_kind = 'system'`, first.SyncID); got != 1 {
		t.Fatalf("the sync wrote no push row via mirror on the engine's account")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND pack_id = ? AND kind = 'mirror'`,
		repo, first.Packs[0].PackID); got != 1 {
		t.Fatalf("the sync's pack is not a pack of kind mirror")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND mirror_synced_at != 0`, repo); got != 1 {
		t.Fatalf("the sync did not record its instant")
	}

	behind := syncOf(n, repo, 0, update(main, oidOf("A"), oidOf("C")))
	before = n.end()
	res, err = c.ProposeMirrorSync(ctx, code.MirrorSyncProposal{Sync: behind})
	requireStale(t, n, "a sync over a moved ref", before, res, err, code.RefusalStale)

	epoch := syncOf(n, repo, 1, update(main, oidOf("B"), oidOf("C")))
	before = n.end()
	res, err = c.ProposeMirrorSync(ctx, code.MirrorSyncProposal{Sync: epoch})
	requireStale(t, n, "a sync cut at another drop epoch", before, res, err, code.RefusalExpunging)
}

// THE IMPORT AND MIRROR LISTINGS NAME WHAT THEIR HOMES OWE: `imports_due`
// lists the repository being imported with its source and its chunks, and
// `mirrors_due` the push mirrors and then the pull mirrors, each naming its
// own remote and no other repository.
//
// Mutation: drop the pull arm from mirrorsDue, and the pull mirror is never
// listed.
func TestTheImportAndMirrorListingsNameWhatTheirHomesOwe(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), code.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil {
		t.Fatalf("ensure imported: %v", err)
	}
	n.drain()
	repoOn(t, n, "plain")
	pulled := repoOn(t, n, "web")
	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	n.ensure("web", s, t2)

	r := n.reader
	stale := statelog.Freshness{Level: statelog.ReadStale}
	imports, err := r.Read(t.Context(), code.ReadQuery{Kind: code.ReadImportsDue, Fresh: stale})
	if err != nil || len(imports.Imports) != 1 || imports.Imports[0].ID != got.RepoID ||
		imports.Imports[0].Source != *importSource || imports.Imports[0].Seq != 0 {
		t.Fatalf("imports_due answered %+v, %v; want the imported repository, its source, no chunk", imports, err)
	}
	mirrors, err := r.Read(t.Context(), code.ReadQuery{Kind: code.ReadMirrorsDue, Fresh: stale})
	if err != nil || len(mirrors.Mirrors) != 1 || mirrors.Mirrors[0].ID != pulled ||
		mirrors.Mirrors[0].From == nil || *mirrors.Mirrors[0].From != *mirrorSource || mirrors.Mirrors[0].To != nil {
		t.Fatalf("mirrors_due answered %+v, %v; want the pull mirror and its source alone", mirrors, err)
	}
}
