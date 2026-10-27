package code_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A REPOSITORY'S PUBLISHED LAYERS THROUGH THE WHOLE PATH: published as a
// compaction's record on a real embedded broker, applied by the shipped
// applier, served by a snapshot and listed by the publisher's due listing.

// liveChecksums is the git checksum of every live pack repo holds on n, sorted.
func (n *node) liveChecksums(repo string) []string {
	n.t.Helper()
	var out []string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(),
			`SELECT git_checksum FROM code_packs WHERE repo_id = ? ORDER BY git_checksum`, repo)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var sum string
			if err := rows.Scan(&sum); err != nil {
				return err
			}
			out = append(out, sum)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read %s's packs: %v", repo, err)
	}
	return out
}

// layerNamed is a published layer named for name over packs (sorted), on
// base, its bytes one part whose key was minted at minted.
func layerNamed(name, base string, minted time.Time, packs ...string) *code.LayerRef {
	sorted := slices.Clone(packs)
	slices.Sort(sorted)
	return &code.LayerRef{
		LayerID: oidOf("layer " + name), BaseID: base, Packs: sorted,
		Parts:     []code.LayerPart{{Object: objstore.KeyAt(minted), Hash: objstore.HashOf([]byte(name)), Size: 120}},
		MidxBytes: 100, BitmapBytes: 20, Objects: int64(len(packs)), Git: "2.56.0", TookMs: 9,
	}
}

// publishLayer appends a layer-only publication of l on repo through n's
// publisher — a record built here, as [node.publish] builds them — and
// answers where it landed.
func (n *node) publishLayer(repo string, l *code.LayerRef) statelog.Position {
	n.t.Helper()
	if _, err := n.publish(code.RepoSubject(repo), code.OpCompact, code.Scope{code.RepoPath(repo)},
		code.Compaction{RepoID: repo, CompactionID: statelog.NewOpID(time.Now(), ""), Node: n.id,
			Seen: n.waiter.Committed(), Layer: l}); err != nil {
		n.t.Fatalf("publish %s on %s: %v", l.LayerID, repo, err)
	}
	return n.waiter.Committed()
}

// A PUBLICATION IS ANSWERED FROM ITS LAYER'S ROW: proposed to a committer and
// decided, its record lands, and it is answered applied at its position from
// the rows — its layer's among them; asked again, the ledger collapses it onto
// that record. A publish answering applied for a record that wrote no layer is
// answered unknown, naming the layer.
//
// Mutation: answer a publication from its outputs alone, and the decide-only
// publish answers applied for a layer no row holds.
func TestAPublicationIsAnsweredFromItsLayersRow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, _ := packedRepo(t, n, "web", 3)
	c, _ := committerOn(t, n, n.pub)
	p := code.CompactionProposal{Compaction: code.Compaction{RepoID: repo,
		CompactionID: statelog.NewOpID(time.Now(), ""), Node: n.id, Seen: n.waiter.Committed(),
		Layer: layerNamed("root", "", harnessAt, n.liveChecksums(repo)...)}}
	first, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, first, err)
	if got := n.count(`SELECT COUNT(*) FROM code_layers WHERE repo_id = ? AND layer_id = ? AND added_position = ?`,
		repo, p.Compaction.Layer.LayerID, first.Position.Packed()); got != 1 {
		t.Fatalf("the publication's layer is %d rows at its position, want 1", got)
	}
	before := n.end()
	again, err := c.ProposeCompaction(t.Context(), p)
	requireApplied(t, again, err)
	if again.Position != first.Position || n.end() != before {
		t.Fatalf("asked again it answered at %v with %d records appended, want %v and none", again.Position,
			n.end()-before, first.Position)
	}

	// A LAYER ON THE ROOT over the pack pushed since, which a decide-only
	// publish answers applied while it writes nothing.
	logs := &lineLog{}
	d, _ := committerOn(t, n, decideOnly{n}, func(o *code.CommitterOptions) {
		o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	pushPacks(t, n, repo, 1)
	var tail []string
	for _, sum := range n.liveChecksums(repo) {
		if !slices.Contains(p.Compaction.Layer.Packs, sum) {
			tail = append(tail, sum)
		}
	}
	q := code.CompactionProposal{Compaction: code.Compaction{RepoID: repo,
		CompactionID: statelog.NewOpID(time.Now(), ""), Node: n.id, Seen: n.waiter.Committed(),
		Layer: layerNamed("upper", p.Compaction.Layer.LayerID, harnessAt, tail...)}}
	res, err := d.ProposeCompaction(t.Context(), q)
	if err != nil || res.Outcome != statelog.OutcomeUnknown || !strings.Contains(res.Detail,
		"layer "+q.Compaction.Layer.LayerID) {
		t.Fatalf("a publication whose record wrote no layer answered %+v, %v; want unknown naming its layer", res, err)
	}
	if lines := logs.lines("code_compaction_unanswerable"); len(lines) != 1 {
		t.Fatalf("logged %v, want code_compaction_unanswerable once", lines)
	}
}

// A SNAPSHOT SERVES THE CHAIN ONLY WHEN IT MOVED: a full answer serves the
// whole chain — an empty one included — and a delta serves it exactly when a
// record changed it above the delta's floor, never when the floor is at or
// past that record; and no cursor resumes the phase, which page 1 serves
// whole.
//
// Mutation: serve the chain on a delta from its own record's position — the
// comparison at or past rather than past — and the delta from there carries
// it.
func TestADeltaIsServedTheChainOnlyWhenItMoved(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1"))))
	n.push(id, entry("p2", set("refs/heads/topic", code.ZeroOID, oidOf("c2"))))
	empty := snapshot(t, n.reader, id, statelog.Position{})
	if !empty[0].Full || !empty[0].LayersServed || len(empty[0].Layers) != 0 {
		t.Fatalf("a full answer over no chain served layers %v (served %v), want the empty chain served",
			empty[0].Layers, empty[0].LayersServed)
	}
	before := n.waiter.Committed()
	root := layerNamed("root", "", harnessAt, n.liveChecksums(id)...)
	published := n.publishLayer(id, root)
	n.push(id, entry("p3", set("refs/heads/later", code.ZeroOID, oidOf("c3"))))

	for name, tc := range map[string]struct {
		since  statelog.Position
		full   bool
		served bool
	}{
		"a full answer":                       {statelog.Position{}, true, true},
		"a delta from below the publication":  {before, false, true},
		"a delta from the publication itself": {published, false, false},
	} {
		pages := snapshot(t, n.reader, id, tc.since)
		if pages[0].Full != tc.full || pages[0].LayersServed != tc.served {
			t.Errorf("%s: full %v, layers served %v; want %v and %v", name, pages[0].Full,
				pages[0].LayersServed, tc.full, tc.served)
			continue
		}
		var want []code.LayerRow
		if tc.served {
			want = []code.LayerRow{{Seq: 1, Layer: *root, Added: published}}
		}
		if !reflect.DeepEqual(pages[0].Layers, want) {
			t.Errorf("%s: the chain served is %+v, want %+v", name, pages[0].Layers, want)
		}
	}

	resumed := code.StateQuery{RepoID: id, Fresh: stale, Cursor: &code.StateCursor{Full: true, Phase: "layers",
		From: published, Seen: published, Stream: time.Now()}}
	if _, err := n.reader.Snapshot(t.Context(), resumed); !errors.Is(err, code.ErrBadCursor) ||
		!strings.Contains(err.Error(), "no page resumes it") {
		t.Fatalf("a cursor resuming the layers phase: %v, want ErrBadCursor", err)
	}
}

// THE LAYERS PHASE FITS PAGE ONE: the widest chain the bounds admit —
// MaxPublishedLayers layers of MaxLayerParts parts, covering MaxPacksPerRepo
// packs between them — is served whole on a snapshot's first page with every
// pack beside it; and it is charged to that page, so a page with room for the
// chain and a few packs carries no more than those, the rest of the packs on
// the pages after it and no later page carrying a layer.
//
// Mutation: leave the phase uncharged, and the tight page carries hundreds of
// packs past its budget.
func TestTheLayersPhaseFitsPageOne(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "monorepo")
	sums := make([]string, code.MaxPacksPerRepo)
	for i := range sums {
		sums[i] = fmt.Sprintf("%040x", i+1)
	}
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		vars := n.db.Caps().MaxVariables
		if _, err := store.InsertRows(t.Context(), tx, vars,
			`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes, reverse_bytes,
				git_checksum, objects, kind, state, added_position) VALUES `,
			`(?, ?, 'pack', ?, ?, 1000000, 1000, 100, ?, 1000, 'push', 'live', 1)`, ``, len(sums),
			func(i int) []any {
				return []any{id, fmt.Sprintf("push-%06d", i), objstore.KeyAt(harnessAt).String(),
					string(objstore.HashOf([]byte(sums[i]))), sums[i]}
			}); err != nil {
			return err
		}
		per := code.MaxPacksPerRepo / code.MaxPublishedLayers
		base := ""
		for l := range code.MaxPublishedLayers {
			layer := oidOf(fmt.Sprint("layer", l))
			packs, err := json.Marshal(sums[l*per : (l+1)*per])
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO code_layers (repo_id, seq, layer_id, base_id,
				graph_id, packs, midx_bytes, bitmap_bytes, graph_bytes, objects, commits, git, took_ms,
				added_position) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '2.56.0', ?, ?)`, id, l+1, layer, base,
				oidOf(fmt.Sprint("graph", l)), string(packs), int64(code.MaxLayerParts-1)*code.LayerPartBytes,
				1<<29, 1<<29, 300_000_000, 90_000_000, 3_600_000, 1<<40+l); err != nil {
				return err
			}
			if _, err := store.InsertRows(t.Context(), tx, vars,
				`INSERT INTO code_layer_parts (repo_id, layer_id, part, object, hash, size) VALUES `,
				`(?, ?, ?, ?, ?, ?)`, ``, code.MaxLayerParts, func(p int) []any {
					return []any{id, layer, p, objstore.KeyAt(harnessAt).String(),
						string(objstore.HashOf([]byte(fmt.Sprint(layer, p)))), code.LayerPartBytes}
				}); err != nil {
				return err
			}
			base = layer
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE code_repos SET live_packs = ?, layers_position = ?
			WHERE id = ?`, len(sums), 1<<40+code.MaxPublishedLayers, id)
		return err
	}); err != nil {
		t.Fatalf("seed the widest chain: %v", err)
	}

	page, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: stale})
	if err != nil {
		t.Fatal(err)
	}
	if !page.LayersServed || len(page.Layers) != code.MaxPublishedLayers || page.Next != nil ||
		len(page.Packs) != len(sums) {
		t.Fatalf("the widest chain's first page served %d layers (served %v) and %d packs, next %v; want "+
			"every layer and every pack on one page", len(page.Layers), page.LayersServed, len(page.Packs),
			page.Next)
	}
	chain := 0
	for _, row := range page.Layers {
		if len(row.Layer.Parts) != code.MaxLayerParts || len(row.Layer.Packs) != code.MaxPacksPerRepo/code.MaxPublishedLayers {
			t.Fatalf("layer %d was served with %d parts and %d packs", row.Seq, len(row.Layer.Parts),
				len(row.Layer.Packs))
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		chain += len(encoded)
	}
	if chain >= code.MaxPageBytes/16 {
		t.Fatalf("the widest chain encodes to %d bytes, past a sixteenth of a page (%d): the phase is "+
			"served whole on page 1 because it is well under one", chain, code.MaxPageBytes)
	}
	t.Logf("the widest chain the bounds admit encodes to %d bytes, against a page's %d", chain, code.MaxPageBytes)

	// A PAGE WITH ROOM FOR THE CHAIN AND FOUR PACKS.
	packBytes, err := json.Marshal(page.Packs[0])
	if err != nil {
		t.Fatal(err)
	}
	code.SetPageLimits(n.reader, code.MaxSnapshotItems, chain+4*len(packBytes))
	pages := snapshot(t, n.reader, id, statelog.Position{})
	if len(pages[0].Layers) != code.MaxPublishedLayers || len(pages[0].Packs) > 5 {
		t.Fatalf("a page sized for the chain and four packs served %d layers and %d packs", len(pages[0].Layers),
			len(pages[0].Packs))
	}
	served := 0
	for i, p := range pages {
		served += len(p.Packs)
		if i > 0 && (p.LayersServed || len(p.Layers) > 0) {
			t.Fatalf("page %d served layers again", i+1)
		}
	}
	if served < len(sums) {
		t.Fatalf("the pages served %d packs of %d", served, len(sums))
	}
}

// THE PUBLICATION DUE LISTING IS LEAST RECENTLY PUBLISHED FIRST: every active
// and archived repository whose live packs are past its published ones, the
// one never published first and then by when each was last published, then by
// id — paged by its cursor, a page's rows in that order across the pages —
// each with what a publication is due by; and never a deleted one, nor one
// whose live packs are no more than its published ones (e). The counts are the
// last recount's, so a repository whose every pack a layer covers is listed
// after a byte-identical push, until the next recount
// (TestAPackPushedLikeAPublishedOneIsListedUntilTheNextRecount).
//
// Mutation: list the most recently published first, and the repository
// waiting longest is planned last.
func TestThePublicationDueListingIsLeastRecentlyPublishedFirst(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	type row struct {
		live, published, objects, publishedObjects, at, took int64
	}
	rows := map[string]row{
		"a": {9, 4, 900, 400, 3_000, 70}, "b": {5, 0, 50, 0, 0, 0}, "c": {7, 6, 70, 60, 1_000, 30},
		"d": {8, 2, 80, 20, 2_000, 20}, "e": {4, 4, 40, 40, 500, 10}, "f": {6, 1, 60, 10, 1_000, 40},
		"g": {3, 1, 30, 10, 4_000, 50},
	}
	ids := map[string]string{}
	for name, r := range rows {
		ids[name] = n.ensure(name, suiteSettings(), t1).RepoID
		n.exec(`UPDATE code_repos SET live_packs = ?, published_packs = ?, live_objects = ?, published_objects = ?,
			published_at = ?, publish_took_ms = ? WHERE id = ?`, r.live, r.published, r.objects, r.publishedObjects,
			r.at, r.took, ids[name])
	}
	// AN ARCHIVED ONE IS LISTED, a deleted one never, nor one whose live
	// packs are no more than its published ones (e).
	n.exec(`UPDATE code_repos SET status = 'archived' WHERE id = ?`, ids["d"])
	if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), ids["g"]); err != nil {
		t.Fatal(err)
	}
	n.drain()
	code.SetPageLimits(n.reader, 2, code.MaxPageBytes)
	var (
		got   []code.PublicationCandidate
		pages int
	)
	q := code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: stale}
	for {
		page, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		got = append(got, page.Publish...)
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
		if pages > 10 {
			t.Fatal("the listing did not end")
		}
	}
	want := []string{ids["b"]}
	// THE TIE AT 1 000, by id.
	if ids["c"] < ids["f"] {
		want = append(want, ids["c"], ids["f"])
	} else {
		want = append(want, ids["f"], ids["c"])
	}
	want = append(want, ids["d"], ids["a"])
	var listed []string
	for _, c := range got {
		listed = append(listed, c.ID)
		r := rows[c.Name]
		var published time.Time
		if r.at != 0 {
			published = store.DecodeTime(r.at)
		}
		var version int64
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT version FROM code_repos WHERE id = ?`,
				c.ID).Scan(&version)
		}); err != nil {
			t.Fatal(err)
		}
		if c.LivePacks != r.live || c.PublishedPacks != r.published || c.LiveObjects != r.objects ||
			c.PublishedObjects != r.publishedObjects || !c.PublishedAt.Equal(published) ||
			c.PublishTook != time.Duration(r.took)*time.Millisecond || !c.LastPushAt.IsZero() ||
			c.Version.Packed() != version {
			t.Errorf("%s is listed as %+v, want its row %+v at version %d", c.Name, c, r, version)
		}
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("the publication due listing is %v, want %v: least recently published first, then by id, no "+
			"deleted repository and none published whole", listed, want)
	}
	if pages < 3 {
		t.Fatalf("the listing took %d pages of two, want at least three", pages)
	}
}

// A PACK PUSHED BYTE-IDENTICAL TO A PUBLISHED ONE IS LISTED UNTIL THE NEXT
// RECOUNT: published_packs is the live pack rows the last publication's
// recount found covered, and a push moves only live_packs, so a pack whose git
// checksum a layer already names — two rows, one checksum — lists its
// repository with a tail of one and nothing a plan could publish; the next
// publication, over a pack that is new, recounts both rows of that checksum as
// covered and the repository leaves the listing.
//
// Mutation: recount a publication by checksum rather than by row — each
// covered checksum counted once — and the repository is still listed after the
// publication that covers every row it holds.
func TestAPackPushedLikeAPublishedOneIsListedUntilTheNextRecount(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, _ := packedRepo(t, n, "web", 3)
	listed := func() (code.PublicationCandidate, bool) {
		t.Helper()
		page, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: stale})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Publish {
			if c.ID == repo {
				return c, true
			}
		}
		return code.PublicationCandidate{}, false
	}
	root := layerNamed("root", "", harnessAt, n.liveChecksums(repo)...)
	n.publishLayer(repo, root)
	if c, ok := listed(); ok {
		t.Fatalf("a repository whose every pack the root covers is listed: %+v", c)
	}

	// A PUSH OF A PACK BYTE-IDENTICAL TO ONE THE ROOT COVERS: a row of its
	// own, under a checksum the root names.
	again := entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/again", code.ZeroOID, oidOf("again")))
	again.Pack.GitChecksum = root.Packs[0]
	n.push(repo, again)
	if c, ok := listed(); !ok || c.LivePacks != 4 || c.PublishedPacks != 3 {
		t.Fatalf("after a push of a pack the root names, the listing holds %+v (listed %v); want the repository "+
			"with 4 live packs and 3 published — a push moves the live count alone", c, ok)
	}

	// THE NEXT PUBLICATION, over a pack that is new.
	pushPacks(t, n, repo, 1)
	var fresh []string
	for _, sum := range n.liveChecksums(repo) {
		if !slices.Contains(root.Packs, sum) {
			fresh = append(fresh, sum)
		}
	}
	n.publishLayer(repo, layerNamed("upper", root.LayerID, harnessAt, fresh...))
	if c, ok := listed(); ok {
		t.Fatalf("after the publication over the new pack the repository is still listed: %+v; want both rows of "+
			"the root's checksum recounted as covered", c)
	}
	if got := n.count(`SELECT published_packs FROM code_repos WHERE id = ?`, repo); got != 5 {
		t.Fatalf("the recount found %d published packs, want all 5 rows", got)
	}
}
