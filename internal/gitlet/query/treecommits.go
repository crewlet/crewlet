package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// A DIRECTORY'S LAST COMMITS: each entry's commit as `git log -1 -- <entry>`
// names it, by one first-parent walk, walks alone from merges and narrowed
// walks (the package doc's "# A directory's last commits").

const (
	// treeCommitsSpan is the entries one read resolves from its cursor, before
	// rounding out to whole windows, and the most one page holds: a screen.
	treeCommitsSpan = 100
	// treeWindowEntries is a window's size on average (windowStart), and
	// treeWindowMax the most it holds, in sizes: a run of names that begin
	// none is cut there. Every window's key names the size.
	treeWindowEntries = 64
	treeWindowMax     = 4
	// After treeWalkNarrowRecords records with at most Entries × Walks left,
	// the group walk ends and they go on in walks of at most
	// treeWalkNarrowEntries entries and treeWalkNarrowBytes of pathspecs.
	treeWalkNarrowRecords = 1024
	treeWalkNarrowEntries = 32
	treeWalkNarrowWalks   = 2
	treeWalkNarrowBytes   = 64 << 10
	// treeWalkMemoRecords is the records of the group walk that probe the
	// memo for windows: one probe each until one hits.
	treeWalkMemoRecords = 64
	// treeCommitsLogReserve: the walks leave a tenth of the read's budget for
	// the log of the page's commits.
	treeCommitsLogReserve = 10
)

// treeWindowFormat names a window entry: windowStart's rule (FNV-1a 64 of the
// name, 0 modulo the size, or treeWindowMax sizes into a window) and
// encodeWindow's bytes.
const treeWindowFormat = "tree_window/1"

// treeWalkBounds are the bounds above, held per Server so a case can scale them.
type treeWalkBounds struct {
	span, window, narrowRecords, narrowEntries, narrowWalks, narrowBytes, memoRecords int
}

var defaultTreeWalk = treeWalkBounds{span: treeCommitsSpan, window: treeWindowEntries,
	narrowRecords: treeWalkNarrowRecords, narrowEntries: treeWalkNarrowEntries, narrowWalks: treeWalkNarrowWalks,
	narrowBytes: treeWalkNarrowBytes, memoRecords: treeWalkMemoRecords}

// treeCommitsRead answers a directory's last commits: its listing, the
// routing of the entries from the cursor, and the page cut from them.
func (r *read) treeCommitsRead(ctx context.Context, objs found, at cursor, m *memoRun) (gitlet.GitAnswer, error) {
	q := r.q.TreeCommits
	rev := objs.revs[q.Rev]
	if rev.peeledKind != "commit" {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: tree_commits.rev: %s names a %s; a directory's last commits "+
			"are read from a commit", gitlet.ErrInvalid, q.Rev, rev.peeledKind)
	}
	if objs.target.kind != "tree" {
		return gitlet.GitAnswer{}, r.noDirectory(objs, rev)
	}
	dir := objs.path
	l, err := r.dirWindows(ctx, objs.target.id, dir, at.a)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	if at.a > 0 && at.a >= l.total {
		return gitlet.GitAnswer{}, errBadCursor(r.q.Cursor, r.q.Kind, fmt.Sprintf("an entry before the %d the "+
			"listing holds", l.total))
	}
	rt, err := r.lastCommits(ctx, rev.peeled, dir, l, at)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	listed, err := r.readCommits(ctx, "log of a directory's last commits", r.pageCommits(l, rt.res, at))
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	a, left, err := r.cutTreeCommits(objs, rev.peeled, dir, l, rt.res, at, listed)
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	// The page is cut: the walks left only complete windows for the memo, and
	// none of their ends fails it.
	if m != nil {
		rt.windowsOnly = true
		if err = rt.run(ctx, len(l.want)); err != nil {
			return gitlet.GitAnswer{}, err
		}
	}
	res := rt.res
	if m != nil && res.first != "" {
		for _, w := range l.windows {
			if !w.keepable || res.whole[w.start] {
				continue
			}
			names, commits, whole := make([]string, 0, len(w.members)), make([]string, 0, len(w.members)), true
			for _, i := range w.members {
				c, ok := res.commits[l.want[i].name]
				if !ok {
					whole = false
					break
				}
				names, commits = append(names, l.want[i].name), append(commits, c)
			}
			if whole {
				m.batch.Put(windowKey(r.s.treeWalk.window, res.first, dir, w.start), encodeWindow(names, commits))
			}
		}
	}
	if !a.TreeCommits.Cut() {
		m.put(at, a, left)
	}
	m.place(gitlet.GitTreeCommits, same(memoTail{}))
	return a, nil
}

// pageCommits is the distinct commits of the entries the page can hold, in
// the order they first name them: at most a span of them.
func (r *read) pageCommits(l dirListing, res routed, at cursor) []string {
	var ids []string
	for i := at.a; i < min(at.a+r.s.treeWalk.span, l.total); i++ {
		e := l.want[i-l.from]
		c, ok := res.commits[e.name]
		if e.long || !ok {
			break
		}
		if c != "" && !slices.Contains(ids, c) {
			ids = append(ids, c)
		}
	}
	return ids
}

// cutTreeCommits cuts the page from at: its entries in the listing's order
// while they fit, ending before a long entry and at the first the routing
// left unanswered, and answers it with the room it left.
func (r *read) cutTreeCommits(objs found, rev, dir string, l dirListing, res routed, at cursor,
	listed map[string]gitlet.LogCommit) (gitlet.GitAnswer, int, error) {

	wire, wireB64 := gitlet.WirePath(dir)
	page := &gitlet.TreeCommitsAnswer{Tree: objs.target.id, Path: wire, PathB64: wireB64,
		Entries: []gitlet.TreeCommit{}, Commits: []gitlet.LogCommit{}}
	envelope := gitlet.GitAnswer{Kind: gitlet.GitTreeCommits, Rev: rev, Next: widestCursor('e', 1),
		TreeCommits: &gitlet.TreeCommitsAnswer{Tree: objs.target.id, Path: wire, PathB64: wireB64,
			Entries: []gitlet.TreeCommit{}, Commits: []gitlet.LogCommit{}, Total: widest, OutOfTime: true}}
	left := r.room(envelope)
	next := ""
	end := min(at.a+r.s.treeWalk.span, l.total)
	if end < l.total {
		next = formatCursor(cursor{letter: 'e', a: end})
	}
	onPage := map[string]bool{}
	for i := at.a; i < end; i++ {
		e := l.want[i-l.from]
		if e.long {
			if i == at.a {
				return gitlet.GitAnswer{}, 0, fmt.Errorf("%w: entry %d of the listing of %q is longer than one "+
					"answer holds: read the directory above it", gitlet.ErrSliceTooLarge, i, dir)
			}
			next = formatCursor(cursor{letter: 'e', a: i})
			break
		}
		id, answered := res.commits[e.name]
		tc := gitlet.TreeCommit{Path: e.path, PathB64: e.pathB64, Commit: id}
		if !answered {
			page.OutOfTime = true
			if i > at.a {
				next = formatCursor(cursor{letter: 'e', a: i})
				break
			}
			tc.OutOfTime = true
		}
		cost := jsonLen(tc)
		if len(page.Entries) > 0 {
			cost++ // the comma before it
		}
		pushed, name := 0, func() {}
		var commit *gitlet.LogCommit
		if id != "" && !onPage[id] {
			c := listed[id]
			commit = &c
			cost += jsonLen(c)
			if len(page.Commits) > 0 {
				cost++
			}
			pushed, name = r.pushCost(id)
		}
		if cost+pushed > left {
			if len(page.Entries) == 0 {
				return gitlet.GitAnswer{}, 0, fmt.Errorf("%w: entry %d of the listing of %q encodes to %d bytes "+
					"with its commit, past the %d one answer holds: read the directory above it",
					gitlet.ErrSliceTooLarge, i, dir, cost+pushed, left)
			}
			next = formatCursor(cursor{letter: 'e', a: i})
			break
		}
		name()
		page.Entries = append(page.Entries, tc)
		if commit != nil {
			onPage[id] = true
			page.Commits = append(page.Commits, *commit)
		}
		left -= cost + pushed
		if tc.OutOfTime {
			next = ""
			if i+1 < l.total {
				next = formatCursor(cursor{letter: 'e', a: i + 1})
			}
			break
		}
	}
	page.Total = l.total
	return gitlet.GitAnswer{Kind: gitlet.GitTreeCommits, Rev: rev, Next: next, TreeCommits: page}, left, nil
}

// dirListing is the directory's listing as a read routes it: its total, the
// entries of the windows that cover the read's span, and where each begins.
type dirListing struct {
	total   int
	from    int        // the listing index of want[0]
	want    []dirEntry // in the listing's order
	windows []dirWindow
}

// dirEntry is one entry: its name, its path as an answer carries it, its
// type, and the name of the entry its window begins at ("" for the first).
type dirEntry struct {
	name, path, pathB64, kind, window string
	long                              bool // past the record's bound: on no page, in no kept window
}

// dirWindow is one window of a listing's kept entries: the name it begins at,
// its entries (indices of want) and whether it may be kept.
type dirWindow struct {
	start    string
	members  []int
	keepable bool
}

// cutWindows is want cut into its windows, in the listing's order: each
// whole, since the kept range begins and ends at a window's bounds.
func cutWindows(want []dirEntry) []dirWindow {
	var out []dirWindow
	for i, e := range want {
		if i == 0 || e.window != want[i-1].window {
			out = append(out, dirWindow{start: e.window, keepable: true})
		}
		w := &out[len(out)-1]
		w.members = append(w.members, i)
		w.keepable = w.keepable && !e.long
	}
	for i := range out {
		w := &out[i]
		if !w.keepable {
			continue
		}
		names, commits := make([]string, len(w.members)), make([]string, len(w.members))
		for j, m := range w.members {
			names[j], commits[j] = want[m].name, strings.Repeat("0", gitref.OIDHexLen)
		}
		// Known from the names alone: a window no answer could carry is never
		// probed nor put, so MemoBatch.Put never refuses one.
		w.keepable = len(encodeWindow(names, commits)) <= gitlet.MaxGitAnswerBytes
	}
	return out
}

// windowStart reports whether name begins a window of size by its hash alone:
// FNV-1a 64 of its bytes, 0 modulo the size.
func windowStart(name string, size int) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) // a hash.Hash never fails a write
	return h.Sum64()%uint64(size) == 0
}

// windowRule decides, entry by entry in the listing's order, where windows
// begin: the first entry, an entry whose name begins one (windowStart), and
// an entry treeWindowMax sizes into the window in hand.
type windowRule struct {
	size, held int
	start      string
}

// next is the name of the window entry i (named name, long where past the
// record's bound) is in, and whether it begins it.
func (w *windowRule) next(i int, name string, long bool) (string, bool) {
	natural := !long && windowStart(name, w.size)
	switch {
	case i == 0 && !natural:
		w.start, w.held = "", 0
	case i == 0 || natural || w.held >= treeWindowMax*w.size:
		w.start, w.held = name, 0
	default:
		w.held++
		return w.start, false
	}
	w.held++
	return w.start, true
}

// dirWindows lists tree, the directory dir, with ONE `ls-tree -z <tree>`,
// keeping the windows that cover the span from start.
func (r *read) dirWindows(ctx context.Context, tree, dir string, start int) (dirListing, error) {
	var l dirListing
	span := r.s.treeWalk.span
	rule := windowRule{size: r.s.treeWalk.window}
	ended := false // a window began past the span: the rest is only counted
	recs := newRecords(0, gitproc.MaxLine+gitlet.MaxGitPathBytes, func(rec []byte, long bool) error {
		i := l.total
		l.total++
		if ended {
			return nil
		}
		e, err := parseDirEntry(rec, long, dir)
		if err != nil {
			return err
		}
		window, begins := rule.next(i, e.name, e.long)
		e.window = window
		switch {
		case begins && i >= start+span:
			ended = true
			return nil
		case begins && i <= start:
			l.want, l.from = l.want[:0], i
		}
		l.want = append(l.want, e)
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return dirListing{}, err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"ls-tree", "-z", tree},
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if res.Damage == nil && errors.Is(err, errUnparsed) {
		r.charge(res)
		return dirListing{}, r.unparsed(ctx, "listing of a directory", err)
	}
	narrow := fmt.Errorf("%w: the listing of %q did not finish in the read's %s of git: list a directory below "+
		"it", gitlet.ErrSliceTooLarge, dir, r.s.gitBudget)
	if err = r.failed(ctx, "listing of a directory", res, err, narrow); err != nil {
		return dirListing{}, err
	}
	if err = recs.close(); err != nil {
		return dirListing{}, r.unparsed(ctx, "listing of a directory", fmt.Errorf("%w: %w", errUnparsed, err))
	}
	l.windows = cutWindows(l.want)
	return l, nil
}

// parseDirEntry reads one `ls-tree -z` record, "<mode> <type> <id>\t<name>",
// as an entry of dir; one past the record's bound, or whose path from the
// root no answer could carry, is long and read no further.
func parseDirEntry(rec []byte, long bool, dir string) (dirEntry, error) {
	if long {
		return dirEntry{long: true}, nil
	}
	tab := bytes.IndexByte(rec, '\t')
	if tab < 0 {
		return dirEntry{}, fmt.Errorf("%w: ls-tree answered %q, which is no entry", errUnparsed, truncated(rec))
	}
	fields := strings.Split(string(rec[:tab]), " ")
	name := string(rec[tab+1:])
	if len(fields) != 3 || !octalMode(fields[0]) || !gitref.ValidOID(fields[2]) || name == "" ||
		strings.IndexByte(name, '/') >= 0 {
		return dirEntry{}, fmt.Errorf("%w: ls-tree answered %q, which is no entry", errUnparsed, truncated(rec))
	}
	p := name
	if dir != "" {
		p = dir + "/" + name
	}
	if len(p) > gitlet.MaxGitAnswerBytes {
		return dirEntry{name: name, kind: fields[1], long: true}, nil
	}
	e := dirEntry{name: name, kind: fields[1]}
	e.path, e.pathB64 = gitlet.WirePath(p)
	return e, nil
}

// octalMode reports a mode of six octal digits, as ls-tree writes every one.
func octalMode(s string) bool {
	return len(s) == 6 && strings.Trim(s, "01234567") == ""
}

// routed is what the routing answered: each entry's commit by name ("" for
// none), the walk's first record (its windows' key), the windows probes found
// whole, and whether a walk's end cut it before every entry was answered.
type routed struct {
	commits map[string]string
	first   string
	whole   map[string]bool
	cut     bool
}

// router is one read's routing in progress.
type router struct {
	r       *read
	dir     string
	l       dirListing
	windows []dirWindow
	// rank is each kept entry's priority: the page's own entries first, in
	// the listing's order, then the rest of want in the listing's order.
	rank   []int
	byName map[string]int
	res    routed
	// tasks are the walks the group walk left to run, in rank of the first
	// entry each serves.
	tasks []routeTask
	page  int // the entries of the page, ranked first
	// windowsOnly: the page is answered, so the walks left only complete
	// windows and end quietly (endsQuietly).
	windowsOnly bool
}

// routeTask is a walk after the group walk: a narrowed walk of entries from
// a commit, or one entry walked alone from the merge that left the line.
type routeTask struct {
	from    string
	entries []int
	alone   bool
}

// lastCommits routes l.want at commit, the directory dir, until the page's own
// entries (from at) are answered; run(len(l.want)) routes the rest.
func (r *read) lastCommits(ctx context.Context, commit, dir string, l dirListing, at cursor) (*router, error) {
	rt := &router{r: r, dir: dir, l: l, windows: l.windows, byName: map[string]int{},
		rank: make([]int, len(l.want)), res: routed{commits: map[string]string{}, whole: map[string]bool{}}}
	var page, rest []int
	var line []int
	for i, e := range l.want {
		if !e.long {
			rt.byName[e.name] = i
			line = append(line, i)
		}
		if at.a <= l.from+i && l.from+i < at.a+r.s.treeWalk.span {
			page = append(page, i)
		} else {
			rest = append(rest, i)
		}
	}
	for n, i := range slices.Concat(page, rest) {
		rt.rank[i] = n
	}
	rt.page = len(page)
	if len(line) == 0 {
		return rt, nil
	}
	w := rt.newWalk(line)
	w.probes, w.narrows = true, true
	if err := rt.walk(ctx, w, commit, rt.groupSpec()); err != nil {
		return nil, err
	}
	if w.narrowFrom != "" {
		rt.narrow(w.narrowFrom, w.lineEntries())
	}
	if err := rt.run(ctx, rt.page); err != nil {
		return nil, err
	}
	return rt, nil
}

// run runs the queued walks in rank order while the next serves one of the
// first ranks entries.
func (rt *router) run(ctx context.Context, ranks int) error {
	for len(rt.tasks) > 0 && !rt.res.cut {
		next := 0
		for i, t := range rt.tasks {
			if rt.rank[t.entries[0]] < rt.rank[rt.tasks[next].entries[0]] {
				next = i
			}
		}
		t := rt.tasks[next]
		if rt.rank[t.entries[0]] >= ranks {
			return nil
		}
		rt.tasks = slices.Delete(rt.tasks, next, next+1)
		var err error
		if t.alone {
			err = rt.alone(ctx, t.from, t.entries[0])
		} else {
			err = rt.walk(ctx, rt.newWalk(t.entries), t.from, rt.entrySpecs(t.entries))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// endsQuietly reports whether a walk's end only ends the routing: once the
// page is answered, the budget's end, the answer falling due or a busy lane.
func (rt *router) endsQuietly(ctx context.Context, res gitproc.Result, err error) bool {
	return rt.windowsOnly && res.Damage == nil && err != nil && (errors.Is(err, gitproc.ErrDeadline) ||
		errors.Is(err, gitproc.ErrBusy) || gitlet.AnswerDue(ctx))
}

// groupSpec is the group walk's pathspec: the directory's files, none at the
// root.
func (rt *router) groupSpec() []string {
	if rt.dir == "" {
		return nil
	}
	return []string{"--", logLiteral + rt.dir + "/"}
}

// entrySpec is an entry's literal pathspec: its file, or everything under the
// directory, of its name.
func (rt *router) entrySpec(i int) string {
	if rt.dir == "" {
		return logLiteral + rt.l.want[i].name
	}
	return logLiteral + rt.dir + "/" + rt.l.want[i].name
}

func (rt *router) entrySpecs(entries []int) []string {
	out := []string{"--"}
	for _, i := range entries {
		out = append(out, rt.entrySpec(i))
	}
	return out
}

// narrow queues the narrowed walks of entries from a commit: in rank order,
// at most narrowEntries entries and narrowBytes of pathspecs a walk, an empty
// walk always taking its first.
func (rt *router) narrow(from string, entries []int) {
	b := rt.r.s.treeWalk
	slices.SortFunc(entries, func(x, y int) int { return rt.rank[x] - rt.rank[y] })
	var cur []int
	size := 0
	for _, i := range entries {
		n := len(rt.entrySpec(i))
		if len(cur) > 0 && (len(cur) == b.narrowEntries || size+n > b.narrowBytes) {
			rt.tasks = append(rt.tasks, routeTask{from: from, entries: cur})
			cur, size = nil, 0
		}
		cur, size = append(cur, i), size+n
	}
	if len(cur) > 0 {
		rt.tasks = append(rt.tasks, routeTask{from: from, entries: cur})
	}
}

// answer records entry i's commit, "" for none.
func (rt *router) answer(i int, commit string) { rt.res.commits[rt.l.want[i].name] = commit }

// routeBudget is the deadline of the routing's next process: what is left of
// the read's budget less the share kept for the page's log, or false where
// none is left, which ends the routing as the budget's end does.
func (rt *router) routeBudget() (time.Duration, bool) {
	left := rt.r.gitLeft - rt.r.s.gitBudget/treeCommitsLogReserve
	return left, left > 0
}

// lineWalk is one walk's line: the entries on it, and what it found.
type lineWalk struct {
	rt      *router
	on      []bool
	left    int
	records int
	// probes: the group walk probes the memo for windows; narrows: it ends
	// and narrows once few entries are left (narrowFrom).
	probes, narrows bool
	narrowFrom      string
}

func (rt *router) newWalk(entries []int) *lineWalk {
	w := &lineWalk{rt: rt, on: make([]bool, len(rt.l.want))}
	for _, i := range entries {
		w.on[i] = true
	}
	w.left = len(entries)
	return w
}

// lineEntries is the entries still on w's line.
func (w *lineWalk) lineEntries() []int {
	var out []int
	for i, on := range w.on {
		if on {
			out = append(out, i)
		}
	}
	return out
}

// off takes entry i off the line.
func (w *lineWalk) off(i int) {
	w.on[i] = false
	w.left--
}

// take routes one record of the walk: its windows' probes, then its changes.
func (w *lineWalk) take(rec *walkRecord) error {
	rt, b := w.rt, w.rt.r.s.treeWalk
	k := w.records
	w.records++
	if k == 0 && w.probes {
		rt.res.first = rec.id
	}
	if w.probes && k < b.memoRecords {
		w.probe(rec.id, k == 0)
	}
	for _, i := range rec.changed {
		if !w.on[i] {
			continue
		}
		w.off(i)
		if rec.parents <= 1 {
			rt.answer(i, rec.id)
		} else {
			rt.tasks = append(rt.tasks, routeTask{from: rec.id, entries: []int{i}, alone: true})
		}
	}
	switch {
	case w.left == 0:
		return errStop
	case w.narrows && w.records >= b.narrowRecords && w.left <= b.narrowEntries*b.narrowWalks && rec.parents > 0:
		w.narrowFrom = rec.first
		return errStop
	}
	return nil
}

// probe looks for the windows of the entries on the line kept under commit
// at: the sentinel, the first keepable window holding one, and only where it
// hits every other such window.
func (w *lineWalk) probe(at string, first bool) {
	rt := w.rt
	hit := false
	for _, win := range rt.windows {
		if !win.keepable || !slices.ContainsFunc(win.members, func(i int) bool { return w.on[i] }) {
			continue
		}
		body, ok := rt.r.s.o.Cache.MemoProbe(rt.r.synced, windowKey(rt.r.s.treeWalk.window, at, rt.dir, win.start))
		var held map[string]string
		if ok {
			var err error
			if held, err = decodeWindow(body); err != nil {
				ok = false
			}
		}
		if !ok {
			if !hit {
				return
			}
			continue
		}
		hit = true
		// Every entry on the line the window holds, its own or another
		// window's here: the names it holds are the commit's, whatever
		// window this listing puts them in.
		for name, c := range held {
			if i, kept := rt.byName[name]; kept && w.on[i] {
				rt.answer(i, c)
				w.off(i)
			}
		}
		if first && !slices.ContainsFunc(win.members, func(i int) bool {
			_, found := held[rt.l.want[i].name]
			return !found
		}) {
			rt.res.whole[win.start] = true
		}
	}
}

// walk runs one walk of the line w from a commit over spec, routing each
// record; entries left when its stream ends hold no file (streamEnded), and a
// run the budget ends cuts the routing.
func (rt *router) walk(ctx context.Context, w *lineWalk, from string, spec []string) error {
	r := rt.r
	deadline, ok := rt.routeBudget()
	if !ok {
		rt.res.cut = true
		return nil
	}
	stopped := false
	stream := newTreeWalk(rt, func(rec *walkRecord) error {
		err := w.take(rec)
		stopped = errors.Is(err, errStop)
		return err
	})
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: append([]string{"log", "--first-parent", "-z", "--no-color", "--no-notes", "--root", "--name-status",
			"--no-renames", "--diff-merges=first-parent", "--format=%H%x00%P", from}, spec...),
		Config: r.s.o.Cache.ServeConfig(), Stdout: stream, Class: gitproc.ClassRead, Deadline: deadline,
	})
	switch {
	case res.Damage != nil:
	case errors.Is(err, errUnparsed):
		r.charge(res)
		return r.unparsed(ctx, "directory walk", err)
	case stopped:
		err = nil // the walk ended itself, every entry it serves routed
	case err == nil:
		// The last record is handed on here; the stream ended, so whatever it
		// leaves on the line is the stream's end's, never a narrowing's.
		if err = stream.close(); err != nil && !errors.Is(err, errStop) {
			r.charge(res)
			return r.unparsed(ctx, "directory walk", err)
		}
		w.narrowFrom = ""
		return rt.streamEnded(ctx, w, res)
	case rt.endsQuietly(ctx, res, err),
		errors.Is(err, gitproc.ErrDeadline) && r.metered && !gitlet.AnswerDue(ctx):
		r.charge(res)
		rt.res.cut = true
		//nolint:nilerr // the budget's end cuts the routing, keeping what it answered: the page says so.
		return nil
	}
	return r.failed(ctx, "directory walk", res, err, nil)
}

// streamEnded is a walk that read its line's first commit: every entry left
// on it holds no file and never did, which git names no commit for.
func (rt *router) streamEnded(ctx context.Context, w *lineWalk, res gitproc.Result) error {
	if err := rt.r.failed(ctx, "directory walk", res, nil, nil); err != nil {
		return err
	}
	for _, i := range w.lineEntries() {
		if e := rt.l.want[i]; e.kind != "tree" {
			return rt.r.unparsed(ctx, "directory walk", fmt.Errorf("%w: the walk reached its first commit "+
				"without naming %q, a %s, whose files a record must name", errUnparsed, e.name, e.kind))
		}
		rt.answer(i, "")
	}
	return nil
}

// alone walks entry i alone from a merge that left the line, by git's own
// rule: `log -1 <from> -- <entry>`.
func (rt *router) alone(ctx context.Context, from string, i int) error {
	r := rt.r
	deadline, ok := rt.routeBudget()
	if !ok {
		rt.res.cut = true
		return nil
	}
	var out bytes.Buffer
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"log", "-1", "-z", "--no-color", "--no-notes", "--format=%H", from, "--",
			rt.entrySpec(i)},
		Config: r.s.o.Cache.ServeConfig(), Stdout: &out, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if rt.endsQuietly(ctx, res, err) || res.Damage == nil && errors.Is(err, gitproc.ErrDeadline) && r.metered &&
		!gitlet.AnswerDue(ctx) {
		r.charge(res)
		rt.res.cut = true
		//nolint:nilerr // the budget's end cuts the routing, keeping what it answered: the page says so.
		return nil
	}
	if err = r.failed(ctx, "entry's walk", res, err, nil); err != nil {
		return err
	}
	id, rest, found := strings.Cut(out.String(), "\x00")
	switch {
	case out.Len() == 0:
		rt.answer(i, "")
	case !found || rest != "" || !gitref.ValidOID(id):
		return r.unparsed(ctx, "entry's walk", fmt.Errorf("%w: git log -1 answered %q, which is no commit id",
			errUnparsed, truncated(out.Bytes())))
	default:
		rt.answer(i, id)
	}
	return nil
}

// walkRecord is one record of a directory walk: the commit, its first parent
// and how many it has, and the kept entries a changed file of it is under.
type walkRecord struct {
	id, first string
	parents   int
	changed   []int
}

// treeWalk is a directory walk's Stdout: `record = id NUL parents NUL [ LF
// change { change } ]`, `change = status NUL path NUL`, each record handed to
// take once the next begins or the stream ends.
type treeWalk struct {
	rt   *router
	take func(*walkRecord) error
	// state is which token the next byte is of, tok the token in hand up to
	// its bound and long whether it ran past it; parents reads a record's %P.
	state   walkState
	tok     []byte
	long    bool
	parents parentList
	rec     walkRecord
	open    bool  // a record is in hand
	seen    []int // per kept entry, the record that last named it, plus one
	n       int   // records begun
}

type walkState int

const (
	walkID      walkState = iota // a record's id
	walkParents                  // its parents
	walkAfter                    // after its parents: a line feed and a status, or the next id
	walkPath                     // a change's path
	walkStatus                   // after a path: a status, or the next id
)

// walkTokenBytes is the most of a token a walk holds: newRecords' record bound.
const walkTokenBytes = gitproc.MaxLine + gitlet.MaxGitPathBytes

func newTreeWalk(rt *router, take func(*walkRecord) error) *treeWalk {
	return &treeWalk{rt: rt, take: take, seen: make([]int, len(rt.l.want))}
}

func (s *treeWalk) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			s.add(p)
			break
		}
		s.add(p[:i])
		if err := s.end(); err != nil {
			return n, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// add takes bytes of the token in hand.
func (s *treeWalk) add(b []byte) {
	if s.state == walkParents {
		s.parents.add(b)
		return
	}
	if room := walkTokenBytes - len(s.tok); len(b) > room {
		b, s.long = b[:max(room, 0)], true
	}
	s.tok = append(s.tok, b...)
}

// end ends the token in hand at its NUL.
func (s *treeWalk) end() error {
	tok, long := s.tok, s.long
	s.tok, s.long = s.tok[:0], false
	switch s.state {
	case walkID:
		return s.begin(tok, long)
	case walkParents:
		if !s.parents.end() {
			return fmt.Errorf("%w: a record of a directory walk: its parents are no list of commit ids", errUnparsed)
		}
		s.rec.parents = s.parents.count
		if len(s.parents.ids) > 0 {
			s.rec.first = s.parents.ids[0]
		}
		s.state = walkAfter
		return nil
	case walkAfter, walkStatus:
		if !long && gitref.ValidOID(string(tok)) {
			// The next record's id: no status is 40 hex digits.
			if err := s.flush(); err != nil {
				return err
			}
			return s.begin(tok, long)
		}
		// A record's first status follows the line feed git ends its
		// header with; every later one follows the path before it.
		status, lf := bytes.CutPrefix(tok, []byte("\n"))
		if lf != (s.state == walkAfter) || long || len(status) != 1 || !strings.ContainsRune("ADMT", rune(status[0])) {
			return fmt.Errorf("%w: a record of a directory walk: %q is no status", errUnparsed, truncated(tok))
		}
		s.state = walkPath
		return nil
	}
	s.state = walkStatus
	return s.change(tok, long)
}

// begin begins a record at its id.
func (s *treeWalk) begin(tok []byte, long bool) error {
	if long || !gitref.ValidOID(string(tok)) {
		return fmt.Errorf("%w: a record of a directory walk: %q is no commit id", errUnparsed, truncated(tok))
	}
	s.n++
	s.rec = walkRecord{id: string(tok), changed: s.rec.changed[:0]}
	s.parents.reset()
	s.open, s.state = true, walkParents
	return nil
}

// change names the kept entry a changed path is under, where it names one.
func (s *treeWalk) change(path []byte, long bool) error {
	rt := s.rt
	if rt.dir != "" {
		if !long && string(path) == rt.dir {
			return nil // a submodule's gitlink at dir, which `dir/` matches and no entry's walk does
		}
		prefix := rt.dir + "/"
		if !bytes.HasPrefix(path, []byte(prefix)) {
			return fmt.Errorf("%w: a directory walk of %q named %q, outside it", errUnparsed, rt.dir,
				truncated(path))
		}
		path = path[len(prefix):]
	}
	name, _, whole := bytes.Cut(path, []byte("/"))
	if !whole && long {
		return nil // cut before its entry's name ends: a long entry's, never routed
	}
	if len(name) == 0 {
		return fmt.Errorf("%w: a directory walk named %q, no entry's file", errUnparsed, truncated(path))
	}
	i, ok := rt.byName[string(name)]
	if !ok || s.seen[i] == s.n {
		return nil
	}
	s.seen[i] = s.n
	s.rec.changed = append(s.rec.changed, i)
	return nil
}

// flush hands the record in hand on.
func (s *treeWalk) flush() error {
	if !s.open {
		return nil
	}
	s.open = false
	return s.take(&s.rec)
}

// close hands the last record on, or reports a stream that ended inside one.
func (s *treeWalk) close() error {
	switch {
	case len(s.tok) > 0 || s.long || s.state == walkParents || s.state == walkPath:
		return fmt.Errorf("%w: a directory walk's output ended inside a record", errUnparsed)
	case s.state == walkID:
		return nil // no record at all
	}
	return s.flush()
}

// windowKey keys a window of answers: kept under at, the last commit that
// changed the directory dir (the group walk's first record), beginning at the
// entry start ("" for the first).
func windowKey(size int, at, dir, start string) cache.MemoKey {
	return cache.NewMemoKey(treeWindowFormat, strconv.Itoa(size), at, dir, start)
}

// encodeWindow is a window's answers as a memo entry holds them: how many,
// then each entry's name and commit ("" for none); decodeWindow reads them.
func encodeWindow(names, commits []string) []byte {
	var w pageWriter
	w.int(len(names))
	for i, name := range names {
		w.str(name)
		w.str(commits[i])
	}
	return w
}

func decodeWindow(body []byte) (map[string]string, error) {
	r := &pageReader{s: string(body)}
	n := r.count()
	out := make(map[string]string, n)
	for range n {
		name, c := r.str(), r.str()
		if _, twice := out[name]; twice {
			return nil, errMemoPage
		}
		out[name] = c
	}
	if r.err != nil || r.s != "" {
		return nil, errMemoPage
	}
	return out, nil
}
