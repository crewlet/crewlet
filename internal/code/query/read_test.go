package query_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A PATH REACHES GIT ONLY AS DATA. Every kind is read over paths built to be
// read as options — a directory `--output=x`, files `-r` and `a:b`, a name
// with a newline — by abbreviated ids, and every argument list a read ran is
// held to its template: the kind's fixed options, whole ids at their places,
// `--disambiguate=` validated hex, a history's window as numbers the server
// formats and a blame's range as two, and — for a history and a blame alone,
// which read no path on a standard input — the path as the ONE argument after
// `--`, where git reads it as a path whatever it spells: a history's as a
// literal pathspec, a blame's as itself. Every other path was fed to
// cat-file's batch, `<id>:<path>`, where it is a name like any other, and a
// filtered stat's and a patch's files to diff-pairs as the raw records
// diff-tree wrote. This is what keeps diff-tree's, diff-pairs', rev-list's,
// log's and blame's `--output` out — the writers gate argues their run-time
// arguments in its allowance — so an argument a read adds is red here, and
// so is a path moved before `--`. And every path `--output=x` spells is read
// as the path it is: its history lists its commit, its blame its line. A
// landed-in read's ancestry checks are held too: whole ids, both sides. And a
// change's revisions: their facts walk and diff the paths by ids alone, and
// lex a Go file's two versions — `--output=y.go`'s — from blobs a batch is
// fed by id, anchors on them are found by the batch and diffed by pairs
// numbered rather than named, and an interdiff kept to them filters git's
// records as a filtered stat does, its touched paths no pathspec.
//
// Mutations: the log's path before `--`, or `--output=x` added to the log's
// or the blame's arguments → red.
func TestAPathReachesGitOnlyAsData(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	paths := []string{"--output=x/f", "-r", "a:b", "nl\nname", "--output=y.go"}
	files := map[string]string{}
	for _, p := range paths {
		files[p] = "one\n"
	}
	base := x.commit(files, "base")
	for _, p := range paths {
		files[p] = "two\n"
	}
	files["-M"] = "added\n"
	head := x.commit(files, "head")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head[:7], Path: "--output=x"}))
	mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head, Recursive: true}))
	for _, p := range paths {
		mustRead(t, s, blobQ(repo, code.BlobQuery{Rev: head[:9], Path: p}))
	}
	mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base[:7], Head: head[:7], Paths: []string{"--output=x", "-r"}}))
	mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, Renames: true}))
	for _, p := range []string{"--output=x/f", "-M", "-r"} {
		mustRead(t, s, diffQ(repo, code.DiffQuery{Base: base, Head: head, File: p}))
	}
	mustRead(t, s, compareQ(repo, base[:8], head))
	tip, at := x.tip("refs/heads/main")
	mustRead(t, s, landedQ(repo, base[:7], "refs/heads/main", tip, at, true))

	// A CHANGE'S REVISIONS: its facts, which walk and diff these paths; its
	// anchors on them, placed by a batch and by one diff-pairs fed numbers;
	// and an interdiff kept to them, whose filter is judged here.
	facts := mustRead(t, s, factsQ(repo, head, base)).RevisionFacts
	if len(facts.Touched) != len(paths)+1 || facts.PatchID == "" {
		t.Fatalf("the facts of the head = %+v, want every path touched and an id", facts)
	}
	var anchors []code.LineAnchor
	for i, p := range paths {
		blob := strings.TrimSpace(x.git("rev-parse", base+":"+p))
		anchors = append(anchors, code.LineAnchor{ID: fmt.Sprintf("a%d", i), Path: p, Side: code.SideNew,
			BlobOID: blob, Start: 1, End: 1})
	}
	mustRead(t, s, reanchorQ(repo, code.ReanchorQuery{Head: head, Anchors: anchors}))
	mustRead(t, s, interdiffQ(repo, code.InterdiffQuery{FromBase: head, FromHead: base, ToBase: base, ToHead: head,
		ToTouched: []string{"--output=x/f", "-r", "a:b"}}))
	mustRead(t, s, interdiffQ(repo, code.InterdiffQuery{FromBase: head, FromHead: base, ToBase: base, ToHead: head,
		ToTouched: []string{"-r"}, File: "-r"}))

	// A HISTORY AND A BLAME, each path the one argument after `--`: every
	// path's history is its two commits, and its blame its one line of the
	// head's, whatever the path spells.
	for _, p := range paths {
		got := commits(pages(t, s, logQ(repo, code.LogQuery{Rev: head[:8], Path: p, Since: historyDay}),
			code.MaxGitAnswerBytes))
		if !slices.Equal(got, []string{head, base}) {
			t.Errorf("the history of %q lists %v, want the head and the base", p, got)
		}
		blamed := mustRead(t, s, blameQ(repo, code.BlameQuery{Rev: head[:9], Path: p}))
		if l := blamed.Blame.Lines; len(l) != 1 || l[0].Commit != head || l[0].Text != "two" {
			t.Errorf("the blame of %q names %+v, want its one line, the head's", p, l)
		}
	}
	mustRead(t, s, logQ(repo, code.LogQuery{Rev: head, Grep: "head", Limit: 1}))
	if q := logQ(repo, code.LogQuery{Rev: head, Limit: 1}); mustRead(t, s, q).Next != "" {
		q.Cursor = mustRead(t, s, q).Next
		mustRead(t, s, q)
	}

	ran := rec.ran()
	if len(ran) < 20 {
		t.Fatalf("the reads ran %d processes; the case reads nothing", len(ran))
	}
	for _, c := range ran {
		if !fitsTemplate(c.Args) {
			t.Errorf("a read ran git %q, which no template of a read admits", c.Args)
		}
		if c.Args[0] != "log" && c.Args[0] != "blame" && slices.Contains(c.Args, "--") {
			t.Errorf("a read ran git %q, naming a path as an argument", c.Args)
		}
	}
	for _, sub := range []string{"log", "blame"} {
		saw := map[string]bool{}
		for _, c := range ran {
			if c.Args[0] == sub && len(c.Args) > 2 && c.Args[len(c.Args)-2] == "--" {
				saw[strings.TrimPrefix(c.Args[len(c.Args)-1], logLiteralTemplate)] = true
			}
		}
		for _, p := range paths {
			if !saw[p] {
				t.Errorf("no %s named %q as its one argument after --", sub, p)
			}
		}
	}
	if !slices.ContainsFunc(ran, func(c gitproc.Cmd) bool {
		return slices.Equal(c.Args, []string{"cat-file", "--batch", "--buffer"})
	}) {
		t.Error("the facts lexed no Go file's versions: --output=y.go's change ran no batch of its blobs")
	}
	fed := strings.Join(rec.fed("cat-file"), "\x00")
	for _, p := range paths {
		if !strings.Contains(fed, head+":"+p+"\x00") {
			t.Errorf("no batch was fed %q as <id>:<path>", p)
		}
	}
	pairs := strings.Join(rec.fed("diff-pairs"), "")
	for _, p := range []string{"--output=x/f", "-r", "-M", "a:b"} {
		if !strings.Contains(pairs, "\x00"+p+"\x00") {
			t.Errorf("diff-pairs was fed no raw record of %q", p)
		}
	}
}

// templates are the argument lists a read runs: "<oid>" a whole id, "<range>"
// two joined by "...", and "<abbrevs>" one or more `--disambiguate=` of
// validated hex, ending the list.
var templates = [][]string{
	{"cat-file", "--batch-check", "-Z"},
	{"cat-file", "blob", "<oid>"},
	{"rev-parse", "<abbrevs>"},
	{"ls-tree", "-z", "-l", "<oid>"},
	{"ls-tree", "-z", "-l", "-r", "<oid>"},
	{"diff-tree", "-z", "-r", "--raw", "--numstat", "--no-ext-diff", "--no-textconv", "<oid>", "<oid>"},
	{"diff-tree", "-z", "-r", "--raw", "--numstat", "--no-ext-diff", "--no-textconv", "-M", "<oid>", "<oid>"},
	{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff", "--no-textconv", "<oid>", "<oid>"},
	{"diff-tree", "-z", "-r", "--raw", "--no-ext-diff", "--no-textconv", "-M", "<oid>", "<oid>"},
	{"diff-pairs", "-z", "--numstat", "--no-ext-diff", "--no-textconv"},
	{"diff-pairs", "-z", "-p", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "-U3"},
	{"diff-pairs", "-z", "-p", "-U0", "--no-color", "--no-ext-diff", "--no-textconv"},
	{"merge-base", "<oid>", "<oid>"},
	{"merge-base", "--is-ancestor", "<oid>", "<oid>"},
	{"rev-list", "--left-right", "--count", "<range>"},
	{"diff-tree", "-r", "--raw", "-z", "--no-renames", "<oid>", "<oid>"},
	{"cat-file", "--batch", "--buffer"},
	{"diff-tree", "-r", "-p", "--full-index", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "-U3",
		"<oid>", "<oid>"},
	{"patch-id", "--verbatim"},
}

// logLiteralTemplate is the magic a history's path follows `--` with.
const logLiteralTemplate = ":(literal)"

// historyTemplates are a history's argument lists — its fixed options, its
// format with or without the message, its window (a since, a skip, both or
// neither), the commit, and its path as a literal pathspec, the one argument
// after `--`, or none — and a blame's: its range, the commit, and its path,
// the one argument after `--`. "<since>" and "<skip>" are the numbers the
// server formats, "<literal>" the literal magic and any path, "<path>" any
// path, "<lines>" two line numbers.
func historyTemplates() [][]string {
	var out [][]string
	for _, format := range []string{"--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%ct%x00%s%x00",
		"--format=%H%x00%P%x00%an%x00%ae%x00%at%x00%ct%x00%s%x00%B%x00"} {
		for _, window := range [][]string{{}, {"<since>"}, {"<skip>"}, {"<since>", "<skip>"}} {
			for _, path := range [][]string{{}, {"--", "<literal>"}} {
				out = append(out, slices.Concat([]string{"log", "-z", "--topo-order", "--no-mailmap", "--no-color",
					"--no-notes", format}, window, []string{"<oid>"}, path))
			}
		}
	}
	return append(out, []string{"blame", "--porcelain", "-L", "<lines>", "<oid>", "--", "<path>"})
}

// fitsTemplate reports whether args is one of templates, or a history's or a
// blame's.
func fitsTemplate(args []string) bool {
	for _, tmpl := range slices.Concat(templates, historyTemplates()) {
		if fits(args, tmpl) {
			return true
		}
	}
	return false
}

// digits reports whether s is a number the server formats: decimal digits
// alone.
func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func fits(args, tmpl []string) bool {
	if len(tmpl) > 0 && tmpl[len(tmpl)-1] == "<abbrevs>" {
		head := tmpl[:len(tmpl)-1]
		if len(args) <= len(head) || !slices.Equal(args[:len(head)], head) {
			return false
		}
		for _, a := range args[len(head):] {
			hex, ok := strings.CutPrefix(a, "--disambiguate=")
			if !ok || !code.ValidRev(hex) || gitref.ValidOID(hex) {
				return false
			}
		}
		return true
	}
	if len(args) != len(tmpl) {
		return false
	}
	for i, want := range tmpl {
		switch want {
		case "<oid>":
			if !gitref.ValidOID(args[i]) {
				return false
			}
		case "<range>":
			a, b, ok := strings.Cut(args[i], "...")
			if !ok || !gitref.ValidOID(a) || !gitref.ValidOID(b) {
				return false
			}
		case "<since>":
			if n, ok := strings.CutPrefix(args[i], "--max-age="); !ok || !digits(n) {
				return false
			}
		case "<skip>":
			if n, ok := strings.CutPrefix(args[i], "--skip="); !ok || !digits(n) {
				return false
			}
		case "<lines>":
			if a, b, ok := strings.Cut(args[i], ","); !ok || !digits(a) || !digits(b) {
				return false
			}
		case "<literal>":
			if !strings.HasPrefix(args[i], logLiteralTemplate) {
				return false
			}
		case "<path>":
			// A blame's path, after `--`: whatever it spells, a path.
		default:
			if args[i] != want {
				return false
			}
		}
	}
	return true
}

// AN ABBREVIATION IS NEVER A REF. A tag and a branch named by commit A's
// seven-digit abbreviation, both pointing at B: a listing at that abbreviation
// is A's — cat-file's own reading of the name would have answered B's, a ref
// before an object — and an abbreviation two objects share is refused asking
// for more of its digits, never answered for either.
//
// Mutation: expand an abbreviation by cat-file — the listing is B's, red.
func TestAnAbbreviationIsNeverARef(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	a := x.commit(map[string]string{"a.txt": "A\n"}, "A")
	b := x.commit(map[string]string{"b.txt": "B\n"}, "B")
	short := a[:7]
	x.git("tag", short, b)
	x.git("branch", short, b)
	x.push(codetest.RepoName, "refs/tags/"+short, "refs/heads/"+short)

	// Two files whose blobs' ids share seven digits.
	one, two := collidingBlobs(t)
	x.commit(map[string]string{"one.txt": one, "two.txt": two}, "C")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	got := mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: short}))
	var names []string
	for _, e := range got.Tree.Entries {
		names = append(names, e.Path)
	}
	if got.Rev != a || !slices.Equal(names, []string{"a.txt"}) {
		t.Errorf("the listing at %s is %s's %v, want commit %s's [a.txt]", short, got.Rev, names, a)
	}

	shared := blobID(one)[:7]
	_, err := s.Read(t.Context(), blobQ(repo, code.BlobQuery{Rev: shared}))
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "more than one object") {
		t.Errorf("a read at %s, which two blobs begin with = %v, want ErrInvalid asking for more digits",
			shared, err)
	}
}

// collidingBlobs is two file contents whose blob ids share their first seven
// hex digits: drawn until two do — about twenty thousand draws at 28 bits,
// the birthday bound.
func collidingBlobs(t *testing.T) (string, string) {
	t.Helper()
	seen := map[string]string{}
	for i := range 10_000_000 {
		content := fmt.Sprintf("draw %d\n", i)
		prefix := blobID(content)[:7]
		if other, ok := seen[prefix]; ok {
			return other, content
		}
		seen[prefix] = content
	}
	t.Fatal("no two of ten million drawn blobs share seven digits")
	return "", ""
}

// blobID is git's id of a blob holding content.
func blobID(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

// holdingCache holds the first sync of one repository until released, as a
// cold copy's fill holds a read.
type holdingCache struct {
	query.Cache
	repo     string
	once     sync.Once
	entered  chan struct{}
	released chan struct{}
}

func (h *holdingCache) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	if repoID == h.repo {
		h.once.Do(func() { close(h.entered) })
		select {
		case <-h.released:
		case <-ctx.Done():
			return cache.Synced{}, ctx.Err()
		}
	}
	return h.Cache.Sync(ctx, repoID, fresh)
}

// A READ WAITING ON ITS SYNC HOLDS NO SHARE. On a node whose share is ONE
// unit, a read of a repository whose copy is filling waits on its sync while
// a read of a warm repository is served — in less time than the share's wait,
// so it never waited for the unit — because a unit is held around a read's
// own git and nothing else: held across a fill, one read would refuse every
// other seat read on such a node. The filling read is served once its copy is.
//
// Mutation: take the unit before the sync — the warm read waits for the unit
// past its second and fails, red.
func TestAReadWaitingOnItsSyncHoldsNoShare(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.f.Declare(t, code.Declared{Name: "beta", Settings: codetest.Settings()})
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	x.push("beta", "main")
	warm, cold := x.repo(codetest.RepoName), x.repo("beta")
	node := x.f.Nodes()[0]
	held := &holdingCache{Cache: node.Cache(), repo: cold, entered: make(chan struct{}), released: make(chan struct{})}
	share := code.NewGitShare(node.ID(), 2, nil)
	if share.Units() != 1 {
		t.Fatalf("the share holds %d units, want 1", share.Units())
	}
	s := x.server(0, query.Options{Cache: held, Share: share})
	query.SetSyncWait(s, time.Minute)
	mustRead(t, s, treeQ(warm, code.TreeQuery{Rev: head}))

	filled := make(chan error, 1)
	go func() {
		_, err := s.Read(t.Context(), treeQ(cold, code.TreeQuery{Rev: head}))
		filled <- err
	}()
	<-held.entered
	// SHORTER THAN THE SHARE'S WAIT, beyond the reply's margin a read
	// leaves its asker: a read that waited for the unit would not be
	// answered in it.
	ctx, cancel := context.WithTimeout(t.Context(), code.ReplyMargin+code.GitShareWait/2)
	defer cancel()
	if _, err := s.Read(ctx, treeQ(warm, code.TreeQuery{Rev: head})); err != nil {
		t.Errorf("a warm read beside one waiting on its fill = %v, want it served at once", err)
	}
	close(held.released)
	if err := <-filled; err != nil {
		t.Errorf("the read that waited on its fill = %v, want it served once filled", err)
	}
}

// countingCache records the level of every sync asked of it.
type countingCache struct {
	query.Cache
	mu     sync.Mutex
	levels []statelog.ReadLevel
}

func (c *countingCache) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	c.mu.Lock()
	c.levels = append(c.levels, fresh.Level)
	c.mu.Unlock()
	return c.Cache.Sync(ctx, repoID, fresh)
}

// take is every level asked since the last take.
func (c *countingCache) take() []statelog.ReadLevel {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.levels
	c.levels = nil
	return out
}

// AN ID THE COPY LACKS IS LOOKED FOR ONCE AT THE LOG'S HEAD. A git node whose
// copy predates a push is asked, at the asker's floor (which it has reached),
// for the push's commit: it syncs ONCE at the log's head — the asker may know
// of a push this node has not synced — and answers. An id nobody pushed, whole
// or abbreviated, is looked for the same once and then answered
// ErrObjectNotFound naming it, never synced for again.
func TestAnOidTheCacheLacksIsLookedForOnceAtTheHead(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}}, codetest.Settings())
	first := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[1].Cache()}
	s := x.server(1, query.Options{Cache: counting})
	floor := mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: first})).Position
	counting.take()

	second := x.commit(map[string]string{"f": "2\n"}, "two")
	q := treeQ(repo, code.TreeQuery{Rev: second})
	q.Fresh = statelog.FloorAt(statelog.ReadSession, floor)
	if a := mustRead(t, s, q); a.Rev != second {
		t.Errorf("the read of %s answered %s", second, a.Rev)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadSession,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the floor's and one at the log's head", got)
	}

	for _, rev := range []string{strings.Repeat("ab", 20), "0000000"} {
		_, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: rev}))
		if !errors.Is(err, code.ErrObjectNotFound) || !strings.Contains(err.Error(), rev) {
			t.Errorf("a read of %s, which nobody pushed = %v, want ErrObjectNotFound naming it", rev, err)
		}
		if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
			statelog.ReadLinearizable}) {
			t.Errorf("syncs for %s: %v, want the copy as it stands and one at the log's head", rev, got)
		}
	}
}

// A SEAT'S SECOND LOOK IS ITS TURN'S TO PAY: a seat's read of an id nobody
// pushed looks once at the log's head, until its turn's sixteen on the node
// are spent; then it is answered from the copy at its floor, with no sync at
// the head, saying it was not judged again and when the turn's re-syncs come
// back. Another turn of the seat finds the SEAT's bucket spent (the scope,
// M3-decisions U36); a person's read is charged to nobody; and once the refill
// has earned one, the turn looks at the head once more.
//
// Mutation: no bucket (resync answering nil) — the seventeenth read syncs at
// the log's head — red.
func TestASeatPastItsResyncsIsAnsweredAtItsFloor(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[0].Cache()}
	var mu sync.Mutex
	at := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return at
	}
	s := x.server(0, query.Options{Cache: counting, Roster: company, Resyncs: code.NewResyncs(clock, nil)})
	missing := strings.Repeat("cd", 20)
	// read is who's read of missing in turn: whether it synced at the log's
	// head, and its refusal.
	read := func(who code.Principal, turn string) (bool, error) {
		t.Helper()
		counting.take()
		q := treeQ(repo, code.TreeQuery{Rev: missing})
		q.Asker, q.Turn = who, turn
		_, err := s.Read(t.Context(), q)
		if !errors.Is(err, code.ErrObjectNotFound) || !strings.Contains(err.Error(), missing) {
			t.Fatalf("%+v's read of %s, which nobody pushed = %v, want ErrObjectNotFound naming it", who, missing,
				err)
		}
		return slices.Contains(counting.take(), statelog.ReadLinearizable), err
	}

	for i := range code.ResyncBurst {
		if head, _ := read(seat("eng-dev"), "turn-a"); !head {
			t.Fatalf("read %d of the turn's %d did not look at the log's head", i+1, code.ResyncBurst)
		}
	}
	if head, err := read(seat("eng-dev"), "turn-a"); head ||
		!strings.Contains(err.Error(), "at the position this turn has read") ||
		!strings.Contains(err.Error(), "(not re-judged at linearizable: this turn's re-syncs are spent until ") {
		t.Errorf("past its re-syncs the turn's read = %v, looking at the head: %v; want it answered at its floor, "+
			"saying so", err, head)
	}
	if head, err := read(seat("eng-dev"), "turn-b"); head ||
		!strings.Contains(err.Error(), "this seat's re-syncs are spent until ") {
		t.Errorf("another turn of the seat = %v, looking at the head: %v; want its seat's bucket spent", err, head)
	}
	if head, _ := read(person, ""); !head {
		t.Error("a person's read past the seat's re-syncs did not look at the log's head")
	}

	mu.Lock()
	at = at.Add(24 * time.Hour / 125)
	mu.Unlock()
	if head, _ := read(seat("eng-dev"), "turn-a"); !head {
		t.Error("once the refill had earned one, the turn's read did not look at the log's head")
	}
	if head, _ := read(seat("eng-dev"), "turn-a"); head {
		t.Error("the refill's one re-sync was taken twice")
	}
}

// A READ MADE FOR A CHECK IS ITS ATTEMPT'S TO PAY: a check's box reading, at
// /box/code/, a commit the git node's copy does not hold looks once at the
// log's head a read until its attempt's sixteen on the node are spent; then
// it is answered at the check's session, with no sync at the head, naming
// the check's re-syncs.
//
// Mutation: a read made for a check charged to nobody (resync answering nil
// for one) — the seventeenth read syncs at the log's head — red.
func TestACheckPastItsResyncsIsAnsweredAtItsSession(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[0].Cache()}
	s := x.server(0, query.Options{Cache: counting})
	missing := strings.Repeat("ef", 20)
	key := code.CheckKey{RepoID: repo, OID: missing, Target: "refs/heads/main", Name: "unit", Attempt: 1}
	read := func() (bool, error) {
		t.Helper()
		counting.take()
		_, err := s.Read(t.Context(), code.GitQuery{Kind: code.GitTree, RepoID: repo, Check: &key, Home: true,
			Tree: &code.TreeQuery{Rev: missing}})
		if !errors.Is(err, code.ErrObjectNotFound) || !strings.Contains(err.Error(), missing) {
			t.Fatalf("a check's read of %s, which nobody pushed = %v, want ErrObjectNotFound naming it", missing, err)
		}
		return slices.Contains(counting.take(), statelog.ReadLinearizable), err
	}
	for i := range code.ResyncBurst {
		if head, _ := read(); !head {
			t.Fatalf("read %d of the check's %d did not look at the log's head", i+1, code.ResyncBurst)
		}
	}
	if head, err := read(); head || !strings.Contains(err.Error(), "at the position this check was queued at") ||
		!strings.Contains(err.Error(), "this check's re-syncs are spent until ") {
		t.Errorf("past its re-syncs the check's read = %v, looking at the head: %v; want it answered at its "+
			"session, saying so", err, head)
	}
}

// gatedObjects is an object store whose reads wait while its gate is shut,
// counting every read.
type gatedObjects struct {
	*memobj.Backend
	mu   sync.Mutex
	gate chan struct{}
	gets atomic.Int64
}

func (g *gatedObjects) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	g.gets.Add(1)
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Backend.Get(ctx, name, off, n)
}

// shut shuts the gate, counting reads from here.
func (g *gatedObjects) shut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = make(chan struct{})
	g.gets.Store(0)
}

// open opens it.
func (g *gatedObjects) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gate)
	g.gate = nil
}

// A COLD REPOSITORY ANSWERS UNREADY AND KEEPS SYNCING. A git node that has
// never held a repository is asked for it while its download waits on the
// object store: past the read's sync wait (50 ms here) it answers
// ErrGitUnready{syncing} — about itself, so the asker may ask another node —
// and the round it began goes on under the cache's own lifetime: once the store
// answers, the next read is served from that round's download, the repository
// downloaded once.
func TestAColdRepositoryAnswersUnreadyAndKeepsSyncing(t *testing.T) {
	t.Parallel()
	objects := &gatedObjects{Backend: memobj.New()}
	x := newFixture(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}, Objects: objects},
		codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	rows, err := x.f.Rows(t.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	packs := len(rows.Packs)
	s := x.server(1, query.Options{})
	query.SetSyncWait(s, 50*time.Millisecond)

	objects.shut()
	_, err = s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head}))
	if !unready(err, code.UnreadySyncing) {
		t.Fatalf("a read of a repository whose download waits = %v, want ErrGitUnready{syncing}", err)
	}
	objects.open()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("a read once the store answers = %v, want it served", err)
	}
	if got := objects.gets.Load(); got != int64(packs) {
		t.Errorf("the repository's %d packs were read %d times, want once each: the round the first read "+
			"began went on", packs, got)
	}
}

// failingCache answers every sync of repo with err, as a cache whose sync
// failed answers it.
type failingCache struct {
	query.Cache
	repo string
	err  error
}

func (c failingCache) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	if repoID == c.repo {
		return cache.Synced{}, c.err
	}
	return c.Cache.Sync(ctx, repoID, fresh)
}

// wedgedPool runs every git process on the pool beneath but the object check,
// which it answers as a process its deadline ended.
type wedgedPool struct{ query.Pool }

func (p wedgedPool) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if len(c.Args) > 1 && c.Args[0] == "cat-file" && c.Args[1] == "--batch-check" {
		return gitproc.Result{Exit: -1}, fmt.Errorf("gitproc: git cat-file ran past its %v deadline and was "+
			"killed: %w", c.Deadline, gitproc.ErrDeadline)
	}
	return p.Pool.Run(ctx, c)
}

// ONLY A FAILURE OF THE NODE'S OWN LETS ANOTHER NODE FILL. A sync that failed
// on what every git node's copy is filled from — the code log, the object
// store, a pack the store lacks or holds damaged, a bundle that is not its
// row's, rows this build cannot install, a repair the cache refuses to repeat
// because a whole read of them did not end the damage — is answered
// `syncing`, which keeps the walk's one fill where it is: another node's fill
// would meet the same failure, a download for nothing, and every git node's at
// once against a store that throttles. A sync that failed for a reason of the
// node's own — its cache stopping, its disk refusing a write — and a short
// check over its copy that ran its whole bound are answered `node_failed`,
// which lets the walk have the next node fill: kept, the walk's filler stayed
// on a node that would serve the read no sooner for being asked.
//
// Mutations: answer every failed sync `syncing` — the node's own rows red;
// let a refused repair pass the fill — its row red.
func TestOnlyAFailureOfTheNodesOwnLetsAnotherNodeFill(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	node := x.f.Nodes()[0]
	pack := code.PackRow{PackID: "019a0c2e-0000-7000-8000-000000000001"}
	for name, tc := range map[string]struct {
		err  error
		want code.UnreadyReason
	}{
		"the code log":                   {fmt.Errorf("%w: no quorum", cache.ErrLogRead), code.UnreadySyncing},
		"the object store":               {fmt.Errorf("%w: SlowDown", cache.ErrStoreRead), code.UnreadySyncing},
		"a pack the store lacks":         {&cache.ErrPackMissing{Repo: repo, Pack: pack, Err: objstore.ErrNotFound}, code.UnreadySyncing},
		"a bundle that is not its row's": {&cache.ErrCorruptPack{Repo: repo, Pack: pack, Err: code.ErrBadBundle}, code.UnreadySyncing},
		"rows this build cannot install": {fmt.Errorf("%w: a ref", cache.ErrBadSnapshot), code.UnreadySyncing},
		"a repair it will not repeat":    {fmt.Errorf("%w: rebuilt an hour ago", cache.ErrRebuildRefused), code.UnreadySyncing},
		"its cache stopping":             {cache.ErrStopped, code.UnreadyNodeFailed},
		"its disk":                       {&os.PathError{Op: "write", Path: "pack", Err: syscall.ENOSPC}, code.UnreadyNodeFailed},
	} {
		s := x.server(0, query.Options{Cache: failingCache{Cache: node.Cache(), repo: repo, err: tc.err}})
		if _, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); !unready(err, tc.want) {
			t.Errorf("a sync that failed on %s = %v, want ErrGitUnready{%s}", name, err, tc.want)
		}
	}
	s := x.server(0, query.Options{Git: wedgedPool{Pool: node.Pool()}})
	if _, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); !unready(err, code.UnreadyNodeFailed) {
		t.Errorf("a read whose object check ran its whole bound = %v, want ErrGitUnready{node_failed}", err)
	}
}

// A NODE THAT HOLDS NO COPY FILLS ONLY AS HOME (ADR-0082). A git node that has
// never held a repository, asked a read that is NOT its walk's effective
// home's, answers ErrGitUnready{not_held} at once and starts nothing — no
// round, no GET of the object store — because a round, once begun, runs on
// under the cache's own lifetime after its read stops waiting: with every node
// free to fill, one read of a repository nobody held downloaded it on every
// git node the walk reached. Asked as the home, the same node fills: the cold
// round starts, the read is answered syncing while the download waits, and
// once it lands the copy serves. And a copy the node HOLDS answers whatever
// the stamp says — behind the asker's floor, brought there by a delta.
//
// Mutation: drop the check — the unstamped read starts the download, a GET
// counted and the copy held, red.
func TestANodeThatHoldsNoCopyFillsOnlyAsHome(t *testing.T) {
	t.Parallel()
	objects := &gatedObjects{Backend: memobj.New()}
	x := newFixture(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}, Objects: objects},
		codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	node := x.f.Nodes()[1]
	s := x.server(1, query.Options{})
	query.SetSyncWait(s, 50*time.Millisecond)
	away := func(q code.GitQuery) code.GitQuery {
		q.Home = false
		return q
	}
	// served asks q again for as long as the node answers it syncing, as an
	// asker told a node is syncing asks again, and returns the first other
	// answer. Each read waits the server's 50 ms on the round it joins, and on
	// a loaded runner a round (a download and git's own work) takes longer,
	// so a step that wants the copy served awaits it rather than racing that
	// wait. The 30 s is a hang detector, which no passing run comes near.
	served := func(q code.GitQuery) (code.GitAnswer, error) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			a, err := s.Read(t.Context(), q)
			if !unready(err, code.UnreadySyncing) || time.Now().After(deadline) {
				return a, err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	objects.shut()
	_, err := s.Read(t.Context(), away(treeQ(repo, code.TreeQuery{Rev: head})))
	if !unready(err, code.UnreadyNotHeld) {
		t.Fatalf("a read not stamped home, of a repository the node holds no copy of = %v, want "+
			"ErrGitUnready{not_held}", err)
	}
	if got := objects.gets.Load(); got != 0 {
		t.Errorf("the node read %d objects for a read it was not asked to fill, want none", got)
	}
	if node.Cache().Holds(repo) {
		t.Error("the node began a copy for a read it was not asked to fill")
	}

	_, err = s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head}))
	if !unready(err, code.UnreadySyncing) {
		t.Fatalf("the same read asked of the node as home, its download held = %v, want "+
			"ErrGitUnready{syncing}", err)
	}
	if !node.Cache().Holds(repo) {
		t.Error("asked as home, the node holds no copy and is filling none")
	}
	objects.open()
	if _, err := served(away(treeQ(repo, code.TreeQuery{Rev: head}))); err != nil {
		t.Fatalf("a read not stamped home of the copy the home's read filled = %v, want it served", err)
	}

	second := x.commit(map[string]string{"f": "2\n"}, "two")
	rows, err := x.f.Rows(t.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	q := away(treeQ(repo, code.TreeQuery{Rev: second}))
	q.Fresh = statelog.FloorAt(statelog.ReadSession, rows.Position)
	a, err := served(q)
	if err != nil {
		t.Fatalf("a held copy behind the asker's floor, asked not as home = %v, want it served", err)
	}
	if a.Rev != second {
		t.Errorf("a held copy behind the asker's floor, asked not as home, answered %s, want %s", a.Rev, second)
	}
}

// A UNIT REPOSITORY REFUSES A SEAT OUTSIDE IT, on the chart of the node that
// answers (ADR-0081): a seat of another unit is refused naming the
// visibility, and nothing of the repository is read for it; a seat of the
// owning unit, its lead and a person are served; a handle the chart holds no
// seat for stands nowhere.
func TestAUnitRepositoryRefusesASeatOutsideIt(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{Git: rec.on(x, 0), Roster: company})

	for _, who := range []code.Principal{seat("eng-dev"), seat("eng-lead"), person} {
		q := treeQ(repo, code.TreeQuery{Rev: head})
		q.Asker = who
		if _, err := s.Read(t.Context(), q); err != nil {
			t.Errorf("%+v's read = %v, want it served", who, err)
		}
	}
	before := len(rec.ran())
	for who, why := range map[string]string{"sales-rep": "visibility: unit", "ghost": "no seat of the chart"} {
		q := treeQ(repo, code.TreeQuery{Rev: head})
		q.Asker = seat(who)
		_, err := s.Read(t.Context(), q)
		if !errors.Is(err, code.ErrNotPermitted) || !strings.Contains(err.Error(), why) {
			t.Errorf("%s's read = %v, want ErrNotPermitted saying %q", who, err, why)
		}
	}
	if ran := rec.ran()[before:]; len(ran) != 0 {
		t.Errorf("refused reads ran git %v, want nothing read for them", ran)
	}
}

// A READ MADE FOR A SEAT IS HELD TO THAT SEAT'S GRANT, on the git node that
// answers it, from the chart and the row it judges the asker by
// (code.JudgeRead, the data node's judgment too): eng-dev reads a
// `visibility: unit` repository of its own unit, and read for sales-rep — the
// colleague whose ask woke its turn, who reads none of it — it is refused
// ErrAboveCeiling naming the visibility, never ErrNotPermitted, which would be
// false of eng-dev; and nothing of the repository is read for it, the page
// eng-dev's own read memoized just before included. Read for eng-lead, who
// reads it, the same read is answered from that memo; and sales-rep reading
// for eng-dev is refused about itself.
//
// Mutation: the grant step judging the asker alone → eng-dev is served the
// page for sales-rep, from the memo.
func TestAReadMadeForASeatIsHeldToThatSeatsGrant(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	s := x.server(0, query.Options{Git: rec.on(x, 0), Roster: company})
	own := treeQ(repo, code.TreeQuery{Rev: head})
	own.Asker = seat("eng-dev")
	mustRead(t, s, own)
	before := len(rec.ran())

	forLead := own
	forLead.CeilingSeat = "eng-lead"
	if a := mustRead(t, s, forLead); !a.Memo {
		t.Errorf("eng-dev's read for eng-lead, who reads the repository, was not answered from the memo its " +
			"own read filled")
	}
	forOutsider := own
	forOutsider.CeilingSeat = "sales-rep"
	_, err := s.Read(t.Context(), forOutsider)
	if !errors.Is(err, code.ErrAboveCeiling) || errors.Is(err, code.ErrNotPermitted) ||
		!strings.Contains(err.Error(), `seat "sales-rep"`) || !strings.Contains(err.Error(), "visibility: unit") {
		t.Errorf("eng-dev's read for sales-rep = %v, want ErrAboveCeiling naming sales-rep and the visibility", err)
	}
	outsiderFor := own
	outsiderFor.Asker, outsiderFor.CeilingSeat = seat("sales-rep"), "eng-dev"
	if _, err := s.Read(t.Context(), outsiderFor); !errors.Is(err, code.ErrNotPermitted) ||
		!strings.Contains(err.Error(), `seat "sales-rep"`) {
		t.Errorf("sales-rep's read for eng-dev = %v, want ErrNotPermitted about sales-rep itself", err)
	}
	if ran := rec.ran()[before:]; len(ran) != 0 {
		t.Errorf("a memo hit and two refused reads ran git %v, want none", ran)
	}
}

// A REFUSAL IS JUDGED AT THE LOG'S HEAD. A git node whose copy of a
// repository predates the activation that made it `visibility: company` would
// refuse a seat outside the owning unit from the row it holds; it syncs once at
// the log's head instead — the code host never refuses on a stale read — and
// serves the seat from the row that says company. A read the head refuses too
// is refused after that one look, never a second.
//
// Mutation: refuse from the copy as it stands — the seat is refused, red.
func TestARefusalIsJudgedAtTheLogsHead(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	x := newFixture(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[1].Cache()}
	s := x.server(1, query.Options{Cache: counting, Roster: company})
	mustRead(t, s, treeQ(repo, code.TreeQuery{Rev: head}))
	outside := treeQ(repo, code.TreeQuery{Rev: head})
	outside.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), outside); !errors.Is(err, code.ErrNotPermitted) {
		t.Fatalf("a seat outside the unit = %v, want ErrNotPermitted", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale, statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the person's and the seat's as the copy stood, then the seat's at the log's "+
			"head once", got)
	}

	settings.Visibility = code.VisibilityCompany
	x.f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings})
	if _, err := s.Read(t.Context(), outside); err != nil {
		t.Errorf("the same seat once the repository is company-wide = %v, want it served from the log's head", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the copy as it stood, then one at the log's head", got)
	}
}

// A READ WITH NO CHART IS UNREADY HERE, NEVER REFUSED. A git node holding no
// chart yet — no epoch installed, at boot or before a first apply — cannot
// judge a seat, which is not "this seat may not read it": the seat's read is
// ErrGitUnready{no_chart}, about this node, so the asker asks another, and no
// sync is started for it. A person's read reads no chart and is served.
//
// Mutation: judge the seat with no chart — GrantOf's none — ErrNotPermitted,
// red.
func TestAReadWithNoChartIsUnreadyHere(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[0].Cache()}
	s := x.server(0, query.Options{Cache: counting, Roster: func() code.Roster { return nil }})

	q := treeQ(repo, code.TreeQuery{Rev: head})
	q.Asker = seat("eng-dev")
	if _, err := s.Read(t.Context(), q); !unready(err, code.UnreadyNoChart) {
		t.Errorf("a seat's read on a node with no chart = %v, want ErrGitUnready{no_chart}", err)
	}
	if got := counting.take(); len(got) != 0 {
		t.Errorf("a read this node cannot judge synced %v, want nothing started", got)
	}
	if _, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); err != nil {
		t.Errorf("a person's read on a node with no chart = %v, want it served", err)
	}
}

// A GIT NODE BEHIND ITS CONFIGURATION SERVES NO SEAT. Confirmed behind the
// company's configuration (shed or stuck), a node judges no seat's grant from
// the chart the fleet replaced — the revision it failed may be the one that
// revoked it — so a seat's read is ErrGitUnready{behind_config} and starts no
// sync, while a person's, which reads no chart, is served.
//
// Mutation: drop the arm — the seat is served, red.
func TestAGitNodeBehindItsConfigurationServesNoSeat(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: x.f.Nodes()[0].Cache()}
	s := x.server(0, query.Options{Cache: counting, Roster: company, Current: func() bool { return false }})

	q := treeQ(repo, code.TreeQuery{Rev: head})
	q.Asker = seat("eng-lead")
	if _, err := s.Read(t.Context(), q); !unready(err, code.UnreadyBehindConfig) {
		t.Errorf("a seat's read on a node behind its configuration = %v, want ErrGitUnready{behind_config}", err)
	}
	if got := counting.take(); len(got) != 0 {
		t.Errorf("a read this node may not judge synced %v, want nothing started", got)
	}
	if _, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); err != nil {
		t.Errorf("a person's read on a node behind its configuration = %v, want it served", err)
	}
}

// A DAMAGED RUN GIVES NO ANSWER. Over a copy whose multi-pack-index is
// truncated, git reads around the damage, answers from the packs' own indexes
// and exits 0 — and says what it read around in its trace. Such a run is no
// answer: the read is ErrGitUnready{syncing}, about this node, while the
// cache repairs. No process a read runs names a Repairer, so what it reported
// went to the cache's observer at once, which counted it and queued the
// repair; once that round has dropped the index, the next read is served.
//
// Mutation: answer from the damaged run — served while damaged, red.
func TestADamagedRunGivesNoAnswer(t *testing.T) {
	t.Parallel()
	rec := &recording{}
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	x.commit(map[string]string{"g": "2\n"}, "two")
	repo := x.repo(codetest.RepoName)
	node := x.f.Nodes()[0]
	synced, err := node.Cache().Sync(t.Context(), repo, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	dir := synced.Dir
	synced.Release()
	gittest.WriteServed(t, x.p.Git(), dir)
	gittest.Damage(t, dir, gittest.MidxTruncated)
	s := x.server(0, query.Options{Git: rec.on(x, 0)})

	_, err = s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head}))
	if !unready(err, code.UnreadySyncing) || !strings.Contains(err.Error(), "damage") {
		t.Fatalf("a read over a damaged index = %v, want ErrGitUnready{syncing} naming the damage", err)
	}
	for _, c := range rec.ran() {
		if c.Repairer != 0 {
			t.Errorf("git %q named a repairer: a read holds no report for a repair of its own", c.Args)
		}
	}
	if got := counter(node.Metrics(), metrics.CodeCacheReports, map[string]string{"part": "multi-pack-index"}); got == 0 {
		t.Error("the cache's observer counted no report of the multi-pack-index: the run's report reached nobody")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err = s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("a read once the cache repaired its copy = %v, want it served", err)
	}
}
