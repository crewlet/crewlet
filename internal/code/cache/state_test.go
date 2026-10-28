package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// stateDoc is the state file as this suite writes and reads it: the on-disk
// format, spelled out here rather than borrowed from the package, so a change
// to the format is a change a case sees.
type stateDoc struct {
	Format        int                   `json:"format"`
	RepoID        string                `json:"repo_id"`
	StreamCreated time.Time             `json:"stream_created"`
	Position      statelog.Position     `json:"position"`
	Repo          code.RepoRef          `json:"repo"`
	RefsDigest    string                `json:"refs_digest"`
	Refs          int                   `json:"refs"`
	Packs         int                   `json:"packs"`
	DiskBytes     int64                 `json:"disk_bytes"`
	Derived       map[string]derivedDoc `json:"derived,omitempty"`
	Maintained    struct {
		LastRun time.Time     `json:"last_run"`
		Took    time.Duration `json:"took"`
	} `json:"maintained"`
	LeavingSince statelog.Position `json:"leaving_since,omitzero"`
}

// derivedDoc is one declared derived file as the state records it, keyed by
// its declaration's name (cache.DerivedMidx, cache.DerivedGraph,
// cache.DerivedProvenance).
type derivedDoc struct {
	Bytes  int64  `json:"bytes"`
	Covers int    `json:"covers"`
	Step   string `json:"step,omitempty"`
	// Provenance is the index of pushes' record: only its entry carries one.
	Provenance *provenanceDoc `json:"provenance,omitempty"`
	// Cohorts is the memo's record: only its entry carries one.
	Cohorts []cohortDoc `json:"cohorts,omitempty"`
	// Layers is an installed published chain's record — the ids its chain
	// file lists — which only the two chains' entries carry.
	Layers []string `json:"layers,omitempty"`
	// Tip is the node's tip commit-graph layer, which only the commit-graph
	// chain's entry carries: the line its chain file lists after Layers.
	Tip string `json:"tip,omitempty"`
	// TipOver is the digest of the tail the last tip write covered, on the
	// same entry.
	TipOver string `json:"tip_over,omitempty"`
}

// cohortDoc is one cohort of a memo as the state records it.
type cohortDoc struct {
	N     uint64 `json:"n"`
	Bytes int64  `json:"bytes"`
}

// provenanceDoc is the index of pushes' record as the state holds it — every
// position packed.
type provenanceDoc struct {
	Stream     time.Time `json:"stream"`
	From       int64     `json:"from"`
	Through    int64     `json:"through"`
	GapFrom    int64     `json:"gap_from,omitempty"`
	GapThrough int64     `json:"gap_through,omitempty"`
	Complete   bool      `json:"complete,omitempty"`
	Oldest     int64     `json:"oldest,omitempty"`
	OldestAt   time.Time `json:"oldest_at,omitzero"`
	Next       uint64    `json:"next"`
	Runs       []struct {
		N       uint64 `json:"n"`
		Bytes   int64  `json:"bytes"`
		From    int64  `json:"from"`
		Through int64  `json:"through"`
		Pushes  int64  `json:"pushes"`
		Commits int64  `json:"commits"`
		Moves   int64  `json:"moves"`
	} `json:"runs,omitempty"`
}

// midx and graph are the two declarations' entries: the zero value where the
// state records none.
func (doc stateDoc) midx() derivedDoc  { return doc.Derived[string(cache.DerivedMidx)] }
func (doc stateDoc) graph() derivedDoc { return doc.Derived[string(cache.DerivedGraph)] }

// overPacks is the state's derived files built over the packs — every entry
// but the index of pushes', which every full round makes, empty where no
// delta has folded a push, and which a case about the packs' files does not
// ask about.
func (doc stateDoc) overPacks() map[string]derivedDoc {
	out := map[string]derivedDoc{}
	for name, d := range doc.Derived {
		if name != string(cache.DerivedProvenance) {
			out[name] = d
		}
	}
	return out
}

// derivedBytes is what the state's derived files hold together: what a boot
// counts beside DiskBytes.
func (doc stateDoc) derivedBytes() int64 {
	var n int64
	for _, d := range doc.Derived {
		n += d.Bytes
	}
	return n
}

// writeState writes doc as the state of the incarnation at dir.
func writeState(t testing.TB, dir string, doc stateDoc) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "crewlet-state.json"), string(raw))
}

// readStateDoc reads the state of the incarnation at dir.
func readStateDoc(t testing.TB, dir string) stateDoc {
	t.Helper()
	var doc stateDoc
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "crewlet-state.json"))), &doc); err != nil {
		t.Fatalf("the state at %s does not decode: %v", dir, err)
	}
	return doc
}

// aState is a state with every field set: the maintainer's multi-pack-index
// over the three packs with its bitmap, and its commit-graph, and pack files a
// compaction retired below the position still leaving the directory.
func aState(id string) stateDoc {
	doc := stateDoc{
		Format:        2,
		RepoID:        id,
		StreamCreated: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		Position:      statelog.Position{Stream: topics.CodeLogStream, Generation: 2, Seq: 4711},
		Repo: code.RepoRef{
			ID: id, Name: "payments", Status: code.RepoStatusActive, DefaultBranch: "trunk",
			Protections: []code.Protection{{Pattern: "refs/heads/trunk", DirectPush: code.DirectNone}},
			Refs:        12, LivePacks: 3, LiveBytes: 9000,
		},
		RefsDigest: strings.Repeat("ab", 32),
		Refs:       12,
		Packs:      3,
		DiskBytes:  9100,
		Derived: map[string]derivedDoc{
			string(cache.DerivedMidx):  {Bytes: 400, Covers: 3, Step: string(cache.StepBitmap)},
			string(cache.DerivedGraph): {Bytes: 120, Covers: 3, Step: string(cache.StepGraph)},
		},
	}
	doc.Maintained.LastRun, doc.Maintained.Took = time.Date(2026, 10, 1, 9, 5, 0, 0, time.UTC), 1500*time.Millisecond
	doc.LeavingSince = statelog.Position{Stream: topics.CodeLogStream, Generation: 2, Seq: 4700}
	return doc
}

// formatOne is a state of format 1 — the layout before each derived file was
// recorded by its declaration, the maintainer's files one struct of their own
// — as the build that wrote it spelled it.
func formatOne(id string) string {
	return `{"format":1,"repo_id":"` + id + `","stream_created":"2026-10-01T09:00:00Z",` +
		`"position":{"stream":"` + topics.CodeLogStream + `","generation":2,"seq":4711},"repo":{"id":"` + id + `"},` +
		`"refs_digest":"` + strings.Repeat("ab", 32) + `","refs":12,"packs":3,"disk_bytes":9100,` +
		`"midx":{"packs":3,"bitmap":true,"graph":true,"bytes":520,"last_run":"2026-10-01T09:05:00Z",` +
		`"took":1500000000}}`
}

// A STATE OF ANOTHER BUILD'S FORMAT IS THAT BUILD'S: what its directory means
// is for it to say, so the incarnation goes and the repository starts cold —
// format 1, the layout before each derived file was recorded by its
// declaration, as much as a format 3 a later build might write. A state this
// build cannot read at all — cut short, of format zero, another repository's,
// or recording a derived file as no declaration of this build's could leave
// it — says nothing, and the incarnation is KEPT with no state: its first
// round reads the whole answer and reuses what is installed, where throwing
// it away would download it again. A derived file the state recorded and
// this build does not declare would be bytes a boot counts that no
// declaration's files make up.
func TestAStateOfAnotherFormatIsDiscarded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	older := "a1d0e7f2-3b4c-4d5e-8f60-718293a4b5c6"
	write(t, w.path("repos", older+".1.git", "crewlet-state.json"), formatOne(older))
	newer := aState(repoA)
	newer.Format = 3
	writeState(t, w.path("repos", repoA+".1.git"), newer)
	write(t, w.path("repos", repoB+".1.git", "crewlet-state.json"), `{"format":2,"repo_id":"`+repoB+`","posit`)
	c3 := "c3a1f9d2-6b1e-4c2a-8f3d-2e5b7a9c0d14"
	zero := aState(c3)
	zero.Format = 0
	writeState(t, w.path("repos", c3+".1.git"), zero)
	c4 := "d4b2e0a3-7c2f-4d3b-9a4e-3f6c8b0d1e25"
	writeState(t, w.path("repos", c4+".1.git"), aState(repoA))
	// Derived files recorded as nothing this build declares could leave them.
	recorded := map[string]func(doc *stateDoc){
		"e5c3f1b4-8d3a-4e4c-9b5f-4a7d9c1e2f36": func(doc *stateDoc) {
			doc.Derived["provenance"] = derivedDoc{Bytes: 10, Covers: 3}
		},
		"f6d4a2c5-9e4b-4f5d-8c6a-5b8e0d2f3a47": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedMidx)] = derivedDoc{Bytes: 400, Covers: 3, Step: string(cache.StepGraph)}
		},
		"a7e5b3d6-0f5c-4a6e-9d7b-6c9f1e3a4b58": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraph)] = derivedDoc{Bytes: -1, Covers: 3, Step: string(cache.StepGraph)}
		},
		// Packs covered by an index no write made: only a write sets what
		// an index covers, so read as it stands the maintainer would take
		// it for an index it need not write.
		"b8f6c4e7-1a6d-4b7f-8e8c-7d0a2f4b5c69": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedMidx)] = derivedDoc{Bytes: 400, Covers: 3}
		},
		// A memo of bytes and no cohorts: the record a build that kept the memo
		// in one directory wrote, whose bytes no cohort of this build's holds.
		"c9a7d5f8-2b7e-4c8a-9f9d-8e1b3a5c6d70": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedMemo)] = derivedDoc{Bytes: 8192, Step: string(cache.StepMemo)}
		},
		// A memo whose cohorts are out of order: the trim takes a record's first
		// cohort for its oldest.
		"d0b8e6a9-3c8f-4d9b-8a0e-9f2c4b6d7e81": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedMemo)] = derivedDoc{Bytes: 8192, Step: string(cache.StepMemo),
				Cohorts: []cohortDoc{{N: 2, Bytes: 4096}, {N: 1, Bytes: 4096}}}
		},
		// Cohorts on a declaration that keeps no memo.
		"e1c9f7b0-4d9a-4e0c-9b1f-0a3d5c7e8f92": func(doc *stateDoc) {
			midx := doc.midx()
			midx.Cohorts = []cohortDoc{{N: 1, Bytes: 0}}
			doc.Derived[string(cache.DerivedMidx)] = midx
		},
		// Published layers on a declaration that installs none.
		"f2d0a8c1-5e0b-4f1d-8c2a-1b4e6d8f9a03": func(doc *stateDoc) {
			midx := doc.midx()
			midx.Layers = []string{strings.Repeat("1", 40)}
			doc.Derived[string(cache.DerivedMidx)] = midx
		},
		// An installed chain no install recorded — the record is what says it
		// was installed — and an install recorded over no layer.
		"a3e1b9d2-6f1c-4a2e-9d3b-2c5f7e9a0b14": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedLayers)] = derivedDoc{Bytes: 4096, Layers: []string{strings.Repeat("1", 40)}}
		},
		"b4f2c0e3-7a2d-4b3f-8e4c-3d6a8f0b1c25": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer)}
		},
		// A chain longer than a published one holds, one naming no checksum,
		// and one naming a layer twice.
		"c5a3d1f4-8b3e-4c4a-9f5d-4e7b9a1c2d36": func(doc *stateDoc) {
			var ids []string
			for i := range code.MaxPublishedLayers + 1 {
				ids = append(ids, fmt.Sprintf("%040x", i+1))
			}
			doc.Derived[string(cache.DerivedLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer),
				Layers: ids}
		},
		"d6b4e2a5-9c4f-4d5b-8a6e-5f8c0b2d3e47": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer),
				Layers: []string{"abc"}}
		},
		"e7c5f3b6-0d5a-4e6c-9b7f-6a9d1c3e4f58": func(doc *stateDoc) {
			id := strings.Repeat("2", 40)
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer),
				Layers: []string{id, id}}
		},
		// The node's tip on a declaration that keeps none; a tip alone recorded
		// by the install, which wrote no line of that chain file; a tip over
		// layers recorded by the tip's write, which wrote none of them; a tip
		// that is no checksum; and a tip that is a layer of the chain beneath
		// it.
		"f8d6a4c7-1e6b-4f7d-8c8a-7b0e2d4f5a69": func(doc *stateDoc) {
			midx := doc.midx()
			midx.Tip = strings.Repeat("3", 40)
			doc.Derived[string(cache.DerivedMidx)] = midx
		},
		"a9e7b5d8-2f7c-4a8e-9d9b-8c1f3e5a6b70": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer),
				Tip: strings.Repeat("3", 40)}
		},
		"b0f8c6e9-3a8d-4b9f-8e0c-9d2a4f6b7c81": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096,
				Step: string(cache.StepTipGraph), Layers: []string{strings.Repeat("2", 40)}, Tip: strings.Repeat("3", 40)}
		},
		"c1a9d7f0-4b9e-4c0a-9f1d-0e3b5a7c8d92": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096,
				Step: string(cache.StepTipGraph), Tip: "abc"}
		},
		"d2b0e8a1-5c0f-4d1b-8a2e-1f4c6b8d9e03": func(doc *stateDoc) {
			id := strings.Repeat("2", 40)
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096, Step: string(cache.StepLayer),
				Layers: []string{id}, Tip: id}
		},
		// The digest of the tail a tip covered on a declaration that keeps no
		// tip; one that is no digest; and one beside a chain file that lists
		// nothing.
		"e3c1f9b2-6d1a-4e2c-9b3f-2a5d7c9e0f14": func(doc *stateDoc) {
			midx := doc.midx()
			midx.TipOver = strings.Repeat("4", 32)
			doc.Derived[string(cache.DerivedMidx)] = midx
		},
		"f4d2a0c3-7e2b-4f3d-8c4a-3b6e8d0f1a25": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096,
				Step: string(cache.StepTipGraph), Tip: strings.Repeat("3", 40), TipOver: strings.Repeat("4", 40)}
		},
		"a5e3b1d4-8f3c-4a4e-9d5b-4c7f9e1a2b36": func(doc *stateDoc) {
			doc.Derived[string(cache.DerivedGraphLayers)] = derivedDoc{Bytes: 4096,
				TipOver: strings.Repeat("4", 32)}
		},
	}
	unreadables := []string{repoB, c3, c4}
	for id, change := range recorded {
		doc := aState(id)
		change(&doc)
		writeState(t, w.path("repos", id+".1.git"), doc)
		unreadables = append(unreadables, id)
	}

	c := w.open(t)
	repos := cache.Repos(c)
	why := map[string]string{}
	for _, line := range w.logs.lines(t, "code_cache_trashed") {
		why[filepath.Base(fmt.Sprint(line["path"]))] = fmt.Sprint(line["why"])
	}
	for id, format := range map[string]string{older: "format 1", repoA: "format 3"} {
		if _, ok := repos[id]; ok || exists(w.path("repos", id+".1.git")) {
			t.Errorf("an incarnation with a state of %s was kept (repos %v)", format, repos)
		}
		if got := why[id+".1.git"]; !strings.Contains(got, format) {
			t.Errorf("the trashed incarnation's reason %q does not name its %s", got, format)
		}
	}

	unreadable := map[string]bool{}
	for _, line := range w.logs.lines(t, "code_cache_state_unreadable") {
		unreadable[fmt.Sprint(line["repo"])] = true
	}
	for _, id := range unreadables {
		if repos[id] != 1 || cache.HasState(c, id) {
			t.Errorf("%s: kept at %d, with a state %v — want kept at incarnation 1 with none", id, repos[id], cache.HasState(c, id))
		}
		if !unreadable[id] {
			t.Errorf("%s's unreadable state was not logged code_cache_state_unreadable", id)
		}
	}
}

// WHAT A STATE SAYS IS WHAT THE NEXT BOOT KNOWS, field for field: a boot reads
// nothing but the disk, so a field the state drops is a fact every restart
// forgets — each derived file's bytes among them, or the budget would
// undercount every warm cache after a restart, and the last maintenance run,
// or every repository would be due at once after one.
func TestTheStateIsWhatTheNextBootKnows(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dir := w.path("repos", repoA+".4.git")
	written := aState(repoA)
	writeState(t, dir, written)

	c := w.open(t)
	if !cache.HasState(c, repoA) {
		t.Fatal("a valid state was not read")
	}
	opened := w.logs.lines(t, "code_cache_opened")
	if len(opened) != 1 || opened[0]["bytes"] != float64(written.DiskBytes+written.derivedBytes()) {
		t.Errorf("code_cache_opened = %v, want the state's %d + %d bytes", opened, written.DiskBytes,
			written.derivedBytes())
	}
	// What the maintainer reads of it is the state's two entries.
	if packs, bitmap, graph := cache.Indexed(c, repoA); packs != written.midx().Covers || !bitmap || !graph {
		t.Errorf("the boot restored an index over %d packs (bitmap %v) and a graph %v, want the state's %+v",
			packs, bitmap, graph, written.Derived)
	}
	s, ok := cache.Pin(c, repoA)
	if !ok {
		t.Fatal("no incarnation to pin")
	}
	if s.Position != written.Position || !reflect.DeepEqual(s.Repo, written.Repo) || s.Dir != dir || cache.Incarnation(s) != 4 {
		t.Errorf("handed out %+v at incarnation %d, want the state's position %v and row %+v at %s",
			s, cache.Incarnation(s), written.Position, written.Repo, dir)
	}
	s.Release()

	// Written back from memory, it is the same document.
	if err := cache.Persist(t.Context(), c, repoA, false); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := readStateDoc(t, dir); !reflect.DeepEqual(got, written) {
		t.Errorf("the state written back is\n%+v\nwant\n%+v", got, written)
	}
	if got := names(t, w.path("tmp")); len(got) != 0 {
		t.Errorf("tmp/ holds %v after the state's rename", got)
	}
}

// A DERIVED FILE GONE FROM UNDER ITS STATE IS FORGOTTEN by the first full
// round, and only the bytes still on disk are counted. dropFor moves a
// derived file into trash/ and the state that records it is renamed in only
// at the round's end, so a process killed between the two leaves a state
// naming a multi-pack-index with its bitmap and a commit-graph over a
// directory that holds neither: a boot restores that record, and the first
// full round's measure is the one thing that corrects it. Kept, the
// maintainer would read the copy as indexed, bitmapped and graphed, and
// serve it with none of the three until MidxPacks more packs arrived — and
// with no graph, a push dated before its parent walks every commit. A part
// left beside no main file (a bitmap a killed writer left with no index) is
// counted in the declaration's entry, since it is bytes the budget holds, and
// never read as made.
func TestADerivedFileGoneFromUnderItsStateIsForgotten(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		// keep is whether a file of objects/pack named so stays.
		keep func(name string) bool
	}{
		{"every file gone", func(string) bool { return false }},
		{"a bitmap beside no index", func(name string) bool { return strings.HasSuffix(name, ".bitmap") }},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on)
			s := w.sync(t, c, repo)
			dir := s.Dir
			s.Release()
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			made := readStateDoc(t, dir)
			if made.midx().Step != string(cache.StepBitmap) || made.midx().Covers != 2 ||
				made.graph().Step != string(cache.StepGraph) {
				t.Fatalf("the maintainer's run left the state's derived files %+v, want the index over 2 packs "+
					"with its bitmap and the graph", made.Derived)
			}

			// What a process killed after dropFor's moves and before the
			// round's state was renamed in leaves: the files gone, the state
			// as it was.
			c.Stop(context.Background())
			packDir := filepath.Join(dir, "objects", "pack")
			var kept int64
			for _, name := range names(t, packDir) {
				if !strings.HasPrefix(name, "multi-pack-index") {
					continue
				}
				path := filepath.Join(packDir, name)
				if arm.keep(name) {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					kept += info.Size()
					continue
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(dir, "objects", "info", "commit-graph")); err != nil {
				t.Fatal(err)
			}
			if arm.keep("x.bitmap") && kept == 0 {
				t.Fatal("the maintainer's run left no bitmap to keep")
			}

			c = w.open(t, clk.on)
			w.sync(t, c, repo).Release()
			if packs, bitmap, graph := cache.Indexed(c, repo); packs != 0 || bitmap || graph {
				t.Errorf("after the first full round over a directory with no index and no graph, the "+
					"maintainer reads an index over %d packs (bitmap %v) and a graph %v", packs, bitmap, graph)
			}
			doc := readStateDoc(t, dir)
			switch {
			case kept == 0 && len(doc.overPacks()) != 0:
				t.Errorf("the state records derived files %+v that are not on disk", doc.Derived)
			case kept != 0 && (len(doc.overPacks()) != 1 || !reflect.DeepEqual(doc.midx(), derivedDoc{Bytes: kept})):
				t.Errorf("the state records derived files %+v, want only the index's bitmap's %d bytes, "+
					"made by no write", doc.Derived, kept)
			}
			if got, want := cache.Total(c), heldBy(t, dir); got != want {
				t.Errorf("the cache counts %d bytes, want the %d its directory holds", got, want)
			}

			c = w.reopen(t, c, clk.on)
			opened := w.logs.lines(t, "code_cache_opened")
			if got, want := opened[len(opened)-1]["bytes"], float64(doc.DiskBytes+kept); got != want {
				t.Errorf("the next boot counts %v bytes, want the packs' and refs' %d and the %d left of the "+
					"derived files", got, doc.DiskBytes, kept)
			}
			// No more packs than the two: what is due is what is missing,
			// once this process has read the copy whole.
			w.sync(t, c, repo).Release()
			clk.Add(cache.MidxMinInterval)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v over a copy with no index and no graph, want the repository", ran)
			}
			if packs, bitmap, graph := cache.Indexed(c, repo); packs != 2 || !bitmap || !graph {
				t.Errorf("after the maintainer's run it reads an index over %d packs (bitmap %v) and a graph %v, "+
					"want both written again", packs, bitmap, graph)
			}
			bitmapOf(t, packDir)
			if !statOK(t, filepath.Join(dir, "objects", "info", "commit-graph")) {
				t.Error("the maintainer's run wrote no commit-graph")
			}
		})
	}
}

// A ROUND'S STATE IS RENAMED IN ONLY PAST sync.state, and a write that stops
// there leaves the old state whole and nothing in tmp/: a crash between the
// two is the case the crash matrix holds a sync at, and the state is never
// half written.
func TestARoundsStateIsRenamedInPastItsFaultPoint(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	dir := w.path("repos", repoA+".1.git")
	// Indented, so a state written back from memory — compact, the same
	// document — is told apart from the one on disk by its bytes.
	indented, err := json.MarshalIndent(aState(repoA), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	before := string(indented)
	write(t, filepath.Join(dir, "crewlet-state.json"), before)

	faults := &codefault.Script{}
	refused := errors.New("held at sync.state")
	faults.Arm(codefault.SyncState, codefault.Action{Err: refused})
	c := w.open(t, func(o *cache.Options) { o.Faults = faults })

	if err := cache.Persist(t.Context(), c, repoA, true); !errors.Is(err, refused) {
		t.Fatalf("a round's persist past an armed sync.state = %v, want its error", err)
	}
	if got := read(t, filepath.Join(dir, "crewlet-state.json")); got != before {
		t.Errorf("the state changed although its write stopped at sync.state:\n%s", got)
	}
	if got := names(t, w.path("tmp")); len(got) != 0 {
		t.Errorf("tmp/ holds %v after a write that stopped", got)
	}

	// The maintainer's write is no round: it never reaches the point.
	hits := faults.Hits(codefault.SyncState)
	if err := cache.Persist(t.Context(), c, repoA, false); err != nil {
		t.Fatalf("a maintainer's persist: %v", err)
	}
	if faults.Hits(codefault.SyncState) != hits {
		t.Error("a write that is not a round's hit sync.state")
	}
	if got := read(t, filepath.Join(dir, "crewlet-state.json")); got == before {
		t.Error("the maintainer's write left the state as it was")
	}
}
