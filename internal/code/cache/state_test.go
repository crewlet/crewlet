package cache_test

import (
	"encoding/json"
	"errors"
	"fmt"
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
	Format        int               `json:"format"`
	RepoID        string            `json:"repo_id"`
	StreamCreated time.Time         `json:"stream_created"`
	Position      statelog.Position `json:"position"`
	Repo          code.RepoRef      `json:"repo"`
	RefsDigest    string            `json:"refs_digest"`
	Refs          int               `json:"refs"`
	Packs         int               `json:"packs"`
	DiskBytes     int64             `json:"disk_bytes"`
	Midx          struct {
		Packs   int           `json:"packs"`
		Bitmap  bool          `json:"bitmap"`
		Graph   bool          `json:"graph"`
		Bytes   int64         `json:"bytes"`
		LastRun time.Time     `json:"last_run"`
		Took    time.Duration `json:"took"`
	} `json:"midx"`
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

// aState is a state with every field set.
func aState(id string) stateDoc {
	doc := stateDoc{
		Format:        1,
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
	}
	doc.Midx.Packs, doc.Midx.Bitmap, doc.Midx.Bytes = 3, true, 400
	doc.Midx.LastRun, doc.Midx.Took = time.Date(2026, 10, 1, 9, 5, 0, 0, time.UTC), 1500*time.Millisecond
	return doc
}

// A STATE OF ANOTHER BUILD'S FORMAT IS THAT BUILD'S: what its directory means
// is for it to say, so the incarnation goes and the repository starts cold. A
// state this build cannot read at all — cut short, of format zero, another
// repository's — says nothing, and the incarnation is KEPT with no state: its
// first round reads the whole answer and reuses what is installed, where
// throwing it away would download it again.
func TestAStateOfAnotherFormatIsDiscarded(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	newer := aState(repoA)
	newer.Format = 2
	writeState(t, w.path("repos", repoA+".1.git"), newer)
	write(t, w.path("repos", repoB+".1.git", "crewlet-state.json"), `{"format":1,"repo_id":"`+repoB+`","posit`)
	c3 := "c3a1f9d2-6b1e-4c2a-8f3d-2e5b7a9c0d14"
	zero := aState(c3)
	zero.Format = 0
	writeState(t, w.path("repos", c3+".1.git"), zero)
	c4 := "d4b2e0a3-7c2f-4d3b-9a4e-3f6c8b0d1e25"
	writeState(t, w.path("repos", c4+".1.git"), aState(repoA))

	c := w.open(t)
	repos := cache.Repos(c)
	if _, ok := repos[repoA]; ok || exists(w.path("repos", repoA+".1.git")) {
		t.Errorf("an incarnation with a state of format 2 was kept (repos %v)", repos)
	}
	var why string
	for _, line := range w.logs.lines(t, "code_cache_trashed") {
		if filepath.Base(fmt.Sprint(line["path"])) == repoA+".1.git" {
			why = fmt.Sprint(line["why"])
		}
	}
	if !strings.Contains(why, "format 2") {
		t.Errorf("the trashed incarnation's reason %q does not name its format", why)
	}

	unreadable := map[string]bool{}
	for _, line := range w.logs.lines(t, "code_cache_state_unreadable") {
		unreadable[fmt.Sprint(line["repo"])] = true
	}
	for _, id := range []string{repoB, c3, c4} {
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
// forgets — the maintainer's bytes among them, or the budget would undercount
// every warm cache after a restart.
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
	if len(opened) != 1 || opened[0]["bytes"] != float64(written.DiskBytes+written.Midx.Bytes) {
		t.Errorf("code_cache_opened = %v, want the state's %d + %d bytes", opened, written.DiskBytes, written.Midx.Bytes)
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
