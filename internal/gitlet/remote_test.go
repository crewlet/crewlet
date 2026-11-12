package gitlet_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN IMPORT AND A PULL MIRROR THROUGH THE WHOLE PATH: their records proposed to
// a committer, decided in one snapshot, published on a real embedded broker,
// applied by the shipped applier and answered from the rows they wrote.

// chunkOf is chunk seq of repo's import on n, landing one pack of objects
// objects and cmds, its id minted now.
func chunkOf(n *node, repo string, seq int64, objects int64, cmds ...gitlet.Command) gitlet.ImportChunk {
	id := statelog.NewOpID(time.Now(), "")
	c := gitlet.ImportChunk{RepoID: repo, ChunkID: id, Node: n.id, Seq: seq, Commands: cmds}
	if objects > 0 {
		c.Packs = []gitlet.PackRef{compactOutput(id, 0, objects, harnessAt)}
	}
	return c
}

// syncOf is a sync of the pull mirror repo on n at drop epoch epoch, landing
// one pack and cmds, its id minted now.
func syncOf(n *node, repo string, epoch int64, cmds ...gitlet.Command) gitlet.MirrorSync {
	id := statelog.NewOpID(time.Now(), "")
	return gitlet.MirrorSync{RepoID: repo, SyncID: id, Node: n.id, DropEpoch: epoch, Commands: cmds,
		Packs: []gitlet.PackRef{compactOutput(id, 0, 4, harnessAt)}, FetchedAt: harnessAt}
}

// requireSynced fails t unless res is an import or mirror record applied at a
// position.
func requireSynced(t *testing.T, what string, res gitlet.SyncResult, err error) {
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
func requireStale(t *testing.T, n *node, what string, before uint64, res gitlet.SyncResult, err error,
	refusal gitlet.Refusal) {
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
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), gitlet.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || got.Status != gitlet.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", got, err)
	}
	n.drain()
	repo := got.RepoID
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	main, feature := "refs/heads/main", "refs/heads/feature"

	ahead := chunkOf(n, repo, 2, 3, update(feature, gitlet.ZeroOID, oidOf("F")))
	before := n.end()
	res, err := c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &ahead})
	requireStale(t, n, "a chunk ahead of its turn", before, res, err, gitlet.RefusalStale)

	empty := chunkOf(n, repo, 1, 0)
	before = n.end()
	if _, err := c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &empty}); !errors.Is(err,
		gitlet.ErrInvalidProposal) || n.end() != before {
		t.Fatalf("a chunk landing no pack and setting no ref answered %v; want it refused invalid, unrecorded", err)
	}

	first := chunkOf(n, repo, 1, 5, update(main, gitlet.ZeroOID, oidOf("A")))
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &first})
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

	again := chunkOf(n, repo, 1, 5, update(main, gitlet.ZeroOID, oidOf("A")))
	before = n.end()
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &again})
	requireStale(t, n, "the first chunk asked again", before, res, err, gitlet.RefusalStale)

	moved := chunkOf(n, repo, 2, 0, update(main, oidOf("Z"), oidOf("B")))
	before = n.end()
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &moved})
	requireStale(t, n, "a chunk naming a ref as it is not", before, res, err, gitlet.RefusalStale)

	forced := update(main, oidOf("A"), oidOf("B"))
	forced.Forced = true
	second := chunkOf(n, repo, 2, 2, forced, update(feature, gitlet.ZeroOID, oidOf("F")))
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &second})
	requireSynced(t, "the second chunk", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ? AND ref = ? AND old = ?`, repo,
		main, oidOf("A")); got != 1 {
		t.Fatalf("the forced command of the second chunk left no drop of %s", oidOf("A"))
	}
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND import_seq = 2 AND status = 'importing'`,
		repo); got != 1 {
		t.Fatalf("after two chunks the repository is not importing at chunk 2")
	}

	elsewhere := gitlet.ImportDone{RepoID: repo, Node: n.id, Seq: 2, DefaultBranch: "feature"}
	before = n.end()
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Done: &elsewhere})
	requireStale(t, n, "an end moving the default off the declared branch the import brought", before, res, err,
		gitlet.RefusalStale)

	done := gitlet.ImportDone{RepoID: repo, Node: n.id, Seq: 2}
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Done: &done})
	requireSynced(t, "the import's end", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_repos WHERE id = ? AND status = 'active' AND imported_at != 0
		AND import_error = ''`, repo); got != 1 {
		t.Fatalf("the import's end did not make the repository active, imported")
	}
	late := chunkOf(n, repo, 3, 1, update("refs/tags/v1", gitlet.ZeroOID, oidOf("T")))
	for what, p := range map[string]gitlet.ImportProposal{"a second end": {Done: &done},
		"a chunk after the end": {Chunk: &late}} {
		before = n.end()
		res, err = c.ProposeImport(ctx, p)
		requireStale(t, n, what, before, res, err, gitlet.RefusalStale)
	}
}

// A REPOSITORY'S DETAIL SAYS WHERE IT STANDS WITH OTHER HOSTS: an imported
// repository's detail names the import's source, how many of its chunks have
// landed and, once it ended, when and why it ended short; a pull mirror's names
// its source and its last sync's instant — what a person who imported it or
// set the mirror reads its progress by; a push mirror's names it whole, the
// seat it pushes as and its refs included, and none once it is removed. A
// repository with neither details none.
//
// EVERY RECORD IS STAMPED BY A CLOCK THE CASE HOLDS: a minute past harnessAt
// for each place in the log, the broker's instant the drain hands the
// applier — so the sync's instant is past the import's end because its
// record comes after the end's in the log, read with no wall clock between
// them.
//
// Mutations: serve no import progress (Chunks left 0) — the detail after a
// chunk says none landed, red; drop the sync's instant from the read — the
// mirror reads as never synced, red; serve no push mirror — the detail of one
// says none, red; leave a sync's instant where its import's end put it — the
// mirror reads as never synced since, red.
func TestARepositorysDetailSaysWhereItStandsWithOtherHosts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Minute) }
	ctx := t.Context()
	detail := func(name string) gitlet.RemoteState {
		t.Helper()
		a, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRepo, Repo: name})
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

	made, err := n.store.EnsureRepo(ctx, n.activate(t1), gitlet.Declared{Name: "imported",
		Settings: suiteSettings(), Import: importSource})
	if err != nil || made.Status != gitlet.RepoStatusImporting {
		t.Fatalf("ensure imported: %+v, %v; want it created importing", made, err)
	}
	n.drain()
	c, _ := committerOn(t, n, n.pub)
	if got := detail("imported").Import; got == nil || got.Source != *importSource || got.Chunks != 0 ||
		!got.DoneAt.IsZero() || got.Error != "" {
		t.Fatalf("an import just begun details %+v, want its source, no chunk landed and no end", got)
	}
	first := chunkOf(n, made.RepoID, 1, 5, update("refs/heads/main", gitlet.ZeroOID, oidOf("A")))
	res, err := c.ProposeImport(ctx, gitlet.ImportProposal{Chunk: &first})
	requireSynced(t, "the first chunk", res, err)
	if got := detail("imported").Import; got == nil || got.Chunks != 1 || !got.DoneAt.IsZero() {
		t.Fatalf("an import one chunk in details %+v, want one chunk landed and no end", got)
	}
	short := "a blob past code.limits.max_blob_bytes"
	done := gitlet.ImportDone{RepoID: made.RepoID, Node: n.id, Seq: 1, Error: short}
	res, err = c.ProposeImport(ctx, gitlet.ImportProposal{Done: &done})
	requireSynced(t, "the import's end", res, err)
	if got := detail("imported").Import; got == nil || got.Chunks != 1 || got.DoneAt.IsZero() ||
		got.Error != short {
		t.Fatalf("an import ended short details %+v, want its end's instant and why", got)
	}

	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	n.ensure("web", s, t2)
	endImport(t, n, c, web, 0)
	imported := detail("web")
	if got := imported; got.MirrorFrom == nil || *got.MirrorFrom != *mirrorSource || got.Import == nil ||
		got.Import.Source != (gitlet.ImportSource{URL: mirrorSource.URL, Credential: mirrorSource.Credential}) ||
		got.Import.DoneAt.IsZero() || !got.MirrorSyncedAt.Equal(got.Import.DoneAt) {
		t.Fatalf("a pull mirror imported whole details %+v, want its source, the import from it ended and its "+
			"last sync at that end", got)
	}
	sync := syncOf(n, web, 0, update("refs/heads/synced", gitlet.ZeroOID, oidOf("A")))
	res, err = c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: sync})
	requireSynced(t, "the sync", res, err)
	//fixedwait:stated both are their records' instants, which the case's clock stamped by their places in the log
	if got := detail("web"); !got.MirrorSyncedAt.After(imported.MirrorSyncedAt) {
		t.Fatalf("a pull mirror synced details %+v, want its last sync's instant past its import's end", got)
	}

	// A PUSH MIRROR WHOLE — its host, its credential, the seat it pushes as
	// and its refs in their order — which the dashboard draws the mirror and
	// its seat from; and none once its settings no longer name one.
	target := *mirrorTarget
	target.Refs = []string{"refs/heads/main", "refs/tags/*"}
	published := suiteSettings()
	published.MirrorTo = &target
	n.ensure("published", published, t3)
	if got := detail("published").MirrorTo; got == nil || !reflect.DeepEqual(*got, target) {
		t.Fatalf("a push mirror details %+v, want it whole: %+v", got, target)
	}
	n.ensure("published", suiteSettings(), harnessAt)
	if got := detail("published"); got.MirrorTo != nil {
		t.Fatalf("a push mirror removed still details %+v", got.MirrorTo)
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
	pushed, err := c.Propose(ctx, proposal(repo, update(main, gitlet.ZeroOID, oidOf("A"))))
	if err != nil || pushed.Answer.Refused != "" {
		t.Fatalf("push main: %+v, %v", pushed, err)
	}

	plain := syncOf(n, repo, 0, update(main, oidOf("A"), oidOf("B")))
	before := n.end()
	res, err := c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: plain})
	requireStale(t, n, "a sync of a repository following no source", before, res, err, gitlet.RefusalStale)

	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	n.ensure("web", s, t2)
	endImport(t, n, c, repo, 0)

	first := syncOf(n, repo, 0, update(main, oidOf("A"), oidOf("B")))
	res, err = c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: first})
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
	res, err = c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: behind})
	requireStale(t, n, "a sync over a moved ref", before, res, err, gitlet.RefusalStale)

	epoch := syncOf(n, repo, 1, update(main, oidOf("B"), oidOf("C")))
	before = n.end()
	res, err = c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: epoch})
	requireStale(t, n, "a sync cut at another drop epoch", before, res, err, gitlet.RefusalExpunging)
}

// A SYNC OF NOTHING MOVES THE MIRROR OFF THE LISTING'S HEAD: a pull mirror
// whose home found its source level lands a sync carrying no pack and no
// command — answered applied at its record, whatever drop epoch its copy held,
// since it names no object — which writes no push row and moves the mirror's
// last sync, so `mirrors_due`, least recently synced first, lists the mirror
// never synced ahead of it. A sync of nothing for a repository that follows no
// source is stale, and a sync of packs and no ref is no proposal at all.
//
// Mutation: refuse a sync of nothing as a sync that moves no ref, and it is
// refused invalid; drop the decide's way past the epoch rule, and it is refused
// expunging; let its apply land a push, and a push row of nothing appears;
// answer it from a push row, and it is answered unknown; leave its apply's
// instant unwritten, and the mirror stays at the head of the listing.
func TestASyncOfNothingMovesTheMirrorOffTheListingsHead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ctx := t.Context()
	plain := repoOn(t, n, "plain")
	s := suiteSettings()
	s.MirrorFrom = mirrorSource
	// Every repository made at one activation before any is made a mirror at
	// a later one: activations only move forward.
	// Each import a mirror_from starts ends short, so each is an active
	// pull mirror never synced: one that landed whole is synced at its end.
	names := []string{"first", "second"}
	ids := map[string]string{}
	for _, name := range names {
		ids[name] = repoOn(t, n, name)
	}
	c, _ := committerOn(t, n, n.pub)
	for _, name := range names {
		n.ensure(name, s, t2)
		endImportShort(t, n, c, ids[name], 0, "the source is unreachable")
	}
	stale := statelog.Freshness{Level: statelog.ReadStale}
	pulls := func() []gitlet.MirrorCandidate {
		t.Helper()
		a, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue, Fresh: stale})
		if err != nil || a.Next != "" {
			t.Fatalf("mirrors_due answered %+v, %v; want one page", a, err)
		}
		return a.Mirrors
	}
	listed := pulls()
	if len(listed) != 2 || !listed[0].SyncedAt.IsZero() || !listed[1].SyncedAt.IsZero() {
		t.Fatalf("mirrors_due listed %+v; want the two pull mirrors, never synced", listed)
	}
	head, other := listed[0].ID, listed[1].ID

	nothing := func(repo string, epoch int64) gitlet.MirrorSync {
		return gitlet.MirrorSync{RepoID: repo, SyncID: statelog.NewOpID(time.Now(), ""), Node: n.id,
			DropEpoch: epoch, FetchedAt: harnessAt}
	}
	before := n.end()
	res, err := c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: nothing(plain, 0)})
	requireStale(t, n, "a sync of nothing for a repository following no source", before, res, err,
		gitlet.RefusalStale)

	packsAlone := syncOf(n, head, 0)
	before = n.end()
	if _, err := c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: packsAlone}); !errors.Is(err,
		gitlet.ErrInvalidProposal) || n.end() != before {
		t.Fatalf("a sync landing packs and moving no ref answered %v; want it refused invalid, unrecorded", err)
	}

	level := nothing(head, 7)
	res, err = c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: level})
	requireSynced(t, "a sync of nothing at another drop epoch", res, err)
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, level.SyncID); got != 0 {
		t.Fatalf("a sync of nothing wrote %d push rows, want none", got)
	}
	listed = pulls()
	if len(listed) != 2 || listed[0].ID != other || !listed[0].SyncedAt.IsZero() || listed[1].ID != head ||
		listed[1].SyncedAt.IsZero() {
		t.Fatalf("after a sync of nothing mirrors_due listed %+v; want %s never synced, then %s synced", listed,
			other, head)
	}
}

// THE MIRRORS LISTING PAGES BOTH ARMS IN ITS ORDER: read two rows a page to
// its end, `mirrors_due` names every push mirror and every pull mirror exactly
// once — the push mirrors first, by id, handing over to the pull mirrors in
// the middle of a page — and the pull mirrors LEAST RECENTLY SYNCED FIRST, the
// two never synced (a tie at zero, which a page boundary splits) by id ahead
// of the two synced, the one synced first ahead of the one synced after it
// though its id is the greater: the order a home's stop at the first mirror
// synced within MirrorInterval relies on.
//
// Mutation: order the pull mirrors newest first, and the never-synced come
// last; drop the id from the pull keyset's tie, and the second never-synced
// mirror is skipped; restart the push arm's cursor from its first row, and
// page two reads page one again for ever; end a page at its push arm's end,
// and no page hands over.
func TestTheMirrorsListingPagesBothArmsInItsOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ctx := t.Context()
	gitlet.SetPageLimits(n.reader, 2, 1<<20)
	to, from := suiteSettings(), suiteSettings()
	to.MirrorTo, from.MirrorFrom = mirrorTarget, mirrorSource
	// Every repository made at one activation before any is made a mirror at
	// a later one: activations only move forward.
	var pushes, pulls []string
	for i := range 3 {
		pushes = append(pushes, repoOn(t, n, fmt.Sprintf("out-%d", i)))
	}
	for i := range 4 {
		pulls = append(pulls, repoOn(t, n, fmt.Sprintf("in-%d", i)))
	}
	for i := range 3 {
		n.ensure(fmt.Sprintf("out-%d", i), to, t2)
	}
	// Each pull mirror's import ends short, so each is active and never
	// synced until a sync lands: one that landed whole is synced at its end.
	c, _ := committerOn(t, n, n.pub)
	for i := range 4 {
		n.ensure(fmt.Sprintf("in-%d", i), from, t2)
		endImportShort(t, n, c, pulls[i], 0, "the source is unreachable")
	}
	slices.Sort(pushes)
	slices.Sort(pulls)
	// The greater id synced first, so the order by instant is not the
	// order by id.
	for _, repo := range []string{pulls[3], pulls[2]} {
		sync := syncOf(n, repo, 0, update("refs/heads/synced", gitlet.ZeroOID, oidOf("A")))
		res, err := c.ProposeMirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: sync})
		requireSynced(t, "the sync of "+repo, res, err)
	}

	var listed []gitlet.MirrorCandidate
	handedOver := false
	cursor := ""
	for page := 0; ; page++ {
		if page == 2*(len(pushes)+len(pulls)) {
			t.Fatalf("mirrors_due is still paging after %d pages, %d rows: %v", page, len(listed), listed)
		}
		a, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue, Cursor: cursor,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil || len(a.Mirrors) > 2 {
			t.Fatalf("mirrors_due at %q answered %d rows, %v; want at most two", cursor, len(a.Mirrors), err)
		}
		if len(a.Mirrors) == 2 && a.Mirrors[0].To != nil && a.Mirrors[1].From != nil {
			handedOver = true
		}
		listed = append(listed, a.Mirrors...)
		if a.Next == "" {
			break
		}
		cursor = a.Next
	}
	var gotPushes, gotPulls []string
	for i, m := range listed {
		switch {
		case m.To != nil && m.From == nil && len(gotPulls) == 0:
			gotPushes = append(gotPushes, m.ID)
		case m.From != nil && m.To == nil:
			gotPulls = append(gotPulls, m.ID)
		default:
			t.Fatalf("row %d of mirrors_due is %+v: want the push mirrors, then the pull mirrors, each naming "+
				"its own remote alone", i, m)
		}
	}
	if !slices.Equal(gotPushes, pushes) {
		t.Errorf("mirrors_due listed the push mirrors %v, want each once by id: %v", gotPushes, pushes)
	}
	if want := []string{pulls[0], pulls[1], pulls[3], pulls[2]}; !slices.Equal(gotPulls, want) {
		t.Errorf("mirrors_due listed the pull mirrors %v, want each once, least recently synced first: %v",
			gotPulls, want)
	}
	if !handedOver {
		t.Errorf("no page of mirrors_due went on from the push mirrors into the pull mirrors")
	}
}

// A RECORD IS CUT TO THE BYTES IT CARRIES: a chunk offered MaxImportRefs refs
// whose names JSON spells long — `&`, six bytes each, at MaxRefNameBytes — is
// past what one record carries, and Validate refuses it whole; Fitted cuts it
// to the most of them, from the first, that fit beside its pack — laid out and
// not yet stored, measured as stored — which Validate admits once the pack is
// stored, where one more would not be. Refs of plain names are cut at
// MaxImportRefs, the rows' bound, and a sync is cut as a chunk is.
//
// Mutation: cut by the count alone, and the long refs are not cut; drop the
// count, and the plain ones are not; measure a pack not yet stored as it
// is, and no step is cut at all.
func TestARecordIsCutToTheBytesItCarries(t *testing.T) {
	t.Parallel()
	long := make([]gitlet.Command, gitlet.MaxImportRefs)
	plain := make([]gitlet.Command, gitlet.MaxImportRefs+1)
	for i := range long {
		long[i] = update(fmt.Sprintf("refs/tags/%s%05d", strings.Repeat("&", gitlet.MaxRefNameBytes-15), i),
			gitlet.ZeroOID, oidOf(fmt.Sprint("long", i)))
	}
	for i := range plain {
		plain[i] = update(fmt.Sprintf("refs/tags/v%05d", i), gitlet.ZeroOID, oidOf(fmt.Sprint("plain", i)))
	}
	chunk := func(cmds []gitlet.Command, stored bool) gitlet.ImportProposal {
		id := statelog.NewOpID(time.Now(), "")
		pack := compactOutput(id, 0, 5, harnessAt)
		if !stored {
			pack.Object, pack.Hash = objstore.Key{}, ""
		}
		return gitlet.ImportProposal{Chunk: &gitlet.ImportChunk{RepoID: "0193a3b2-5f8e-7c2a-9d4e-1b2c3d4e5f60",
			ChunkID: id, Node: "node-a", Seq: 1, Packs: []gitlet.PackRef{pack}, Commands: cmds}}
	}
	if err := chunk(long, true).Validate(); !errors.Is(err, gitlet.ErrInvalidProposal) {
		t.Fatalf("a chunk of %d refs of the longest escaped names validated %v; want it past one record",
			len(long), err)
	}

	laid := chunk(long, false)
	cut, err := laid.Fitted()
	if err != nil {
		t.Fatalf("cut a chunk of long refs whose pack is not yet stored: %v", err)
	}
	n := len(cut.Chunk.Commands)
	if n == 0 || n >= len(long) || !slices.Equal(cut.Chunk.Commands, long[:n]) {
		t.Fatalf("a chunk of %d long refs was cut to %d; want some, from the first, and not all", len(long), n)
	}
	stored := chunk(long[:n], true)
	stored.Chunk.ChunkID, stored.Chunk.Packs[0].PackID = laid.Chunk.ChunkID, laid.Chunk.Packs[0].PackID
	if err := stored.Validate(); err != nil {
		t.Errorf("the cut chunk, its pack stored, validated %v; want it admitted", err)
	}
	stored.Chunk.Commands = long[:n+1]
	if err := stored.Validate(); !errors.Is(err, gitlet.ErrInvalidProposal) {
		t.Errorf("the cut chunk and one more ref validated %v; want it past one record: the cut is not the most",
			err)
	}

	if cut, err := chunk(plain, false).Fitted(); err != nil || len(cut.Chunk.Commands) != gitlet.MaxImportRefs {
		t.Errorf("a chunk of %d plain refs was cut to %d, %v; want MaxImportRefs (%d)", len(plain),
			len(cut.Chunk.Commands), err, gitlet.MaxImportRefs)
	}
	sync := gitlet.MirrorSyncProposal{Sync: gitlet.MirrorSync{RepoID: "0193a3b2-5f8e-7c2a-9d4e-1b2c3d4e5f60",
		SyncID: statelog.NewOpID(time.Now(), ""), Node: "node-a", Commands: long, FetchedAt: harnessAt}}
	if cut, err := sync.Fitted(); err != nil || len(cut.Sync.Commands) == 0 || len(cut.Sync.Commands) >= len(long) {
		t.Errorf("a sync of %d long refs was cut to %d, %v; want some and not all", len(long),
			len(cut.Sync.Commands), err)
	}
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
	got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), gitlet.Declared{Name: "imported",
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
	c, _ := committerOn(t, n, n.pub)
	endImport(t, n, c, pulled, 0)

	r := n.reader
	stale := statelog.Freshness{Level: statelog.ReadStale}
	imports, err := r.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadImportsDue, Fresh: stale})
	if err != nil || len(imports.Imports) != 1 || imports.Imports[0].ID != got.RepoID ||
		imports.Imports[0].Source != *importSource || imports.Imports[0].Seq != 0 {
		t.Fatalf("imports_due answered %+v, %v; want the imported repository, its source, no chunk", imports, err)
	}
	mirrors, err := r.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue, Fresh: stale})
	if err != nil || len(mirrors.Mirrors) != 1 || mirrors.Mirrors[0].ID != pulled ||
		mirrors.Mirrors[0].From == nil || *mirrors.Mirrors[0].From != *mirrorSource || mirrors.Mirrors[0].To != nil {
		t.Fatalf("mirrors_due answered %+v, %v; want the pull mirror and its source alone", mirrors, err)
	}
}
