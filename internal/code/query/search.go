package query

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A SEARCH ([code.GitSearch], M6-D12, M6-D13) reads a commit two ways, and
// both answer RE2's matches, the same lines in the same order:
//
//   - THE INDEX PATH, where this node's copy keeps a code index answering for
//     the commit searched (cache.Cache.IndexTip): the plan's candidates in
//     every segment, kept where the tip map names them at a path the query's
//     filter admits, read through one cat-file in path order and confirmed
//     line by line (codeindex.Plan.Confirm).
//   - THE GREP PATH, for everything else: git grep -E over one alternation
//     of the escaped strings of the plan's most selective clause
//     (codeindex.Plan.Grep), every line it
//     answers confirmed the same way, and every file past
//     code.MaxIndexedFileBytes dropped by its size from ls-tree, as the index
//     skips it.
//
// The FIRST of a `search: indexed` repository's two index holders, asked as
// its walk's home, whose index does not yet answer for the default branch's
// tip answers code.UnreadyIndexBuilding, so the walk asks the next node in
// the order — the second holder, which answers through its index where it
// has it and greps where not — rather than reading on the slow path a node a
// moment from its index. Only the first, and only as the home: every node
// after it in the walk answers, so a search is never left unanswered while
// both indexes catch up with a push, and a fleet of one git node, whose one
// holder has none after it, greps at once (cache.IndexedTip.PassesOn).
//
// ONE BUDGET, code.SearchBudget, bounds the whole search after the read's
// share is taken: a search that runs it out answers what it found, in order,
// Truncated, and memoizes nothing — its page is no function of its
// arguments. One that stops at its limit of hits, or at the answer's bound,
// is, and is memoized under the commit it read.

// searchTipKey and searchRefKey are a search's places in the map a memo key
// reads whole ids from (read.memoKey): the commit searched and the whole ref
// that named it.
const (
	searchTipKey = "search:tip"
	searchRefKey = "search:ref"
)

// errSearchFull is a search's stream stopped by the search itself — its limit
// of hits, or its answer's bound — never a failure of git's.
var errSearchFull = errors.New("query: the search has all the hits it answers")

// searchRead answers a GitSearch over the synced copy.
func (r *read) searchRead(ctx context.Context) (code.GitAnswer, error) {
	sq := r.q.Search
	plan, err := sq.Query().Plan()
	if err != nil {
		return code.GitAnswer{}, fmt.Errorf("%w: search: %w", code.ErrInvalid, err)
	}
	filter, err := codeindex.NewPathFilter(sq.Paths)
	if err != nil {
		return code.GitAnswer{}, fmt.Errorf("%w: search: %w", code.ErrInvalid, err)
	}
	ref, tip, err := r.searchTarget(ctx)
	if err != nil {
		return code.GitAnswer{}, err
	}
	repo := r.synced.Repo
	held, _ := r.s.o.Cache.IndexTip(r.synced)
	viaIndex := held.Commit != "" && held.Commit == tip
	if !viaIndex && held.PassesOn && r.q.Home && repo.Search == code.SearchIndexed &&
		ref == "refs/heads/"+repo.DefaultBranch {
		return code.GitAnswer{}, r.unready(code.UnreadyIndexBuilding, fmt.Sprintf("%s keeps repository %s's "+
			"code index and is building it for %s's tip %s; the next git node of its order answers meanwhile",
			r.s.o.NodeID, repo.Name, repo.DefaultBranch, tip))
	}
	whole := map[string]string{searchTipKey: tip, searchRefKey: ref}
	r.index([]string{tip})
	if a, ok := r.recall(whole, cursor{}); ok {
		return a, nil
	}
	if a, ok, err := r.await(ctx, whole, cursor{}); err != nil || ok {
		return a, err
	}
	release, err := r.s.o.Share.Take(ctx, code.SeatWorkRead)
	if err != nil {
		if code.AnswerDue(ctx) {
			return code.GitAnswer{}, r.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole "+
				"share until the read's answer fell due, its asker's attempt spent; another git node answers "+
				"meanwhile", r.s.o.NodeID))
		}
		return code.GitAnswer{}, err
	}
	defer release()
	r.metered, r.gitLeft = true, r.s.gitBudget
	// THE SEARCH'S OWN BUDGET, inside the asker's: what ends it here is an
	// answer, Truncated; what ends ctx is the asker's.
	sctx, cancel := context.WithTimeout(ctx, r.s.searchBudget)
	defer cancel()
	s := &searching{r: r, plan: plan, filter: filter, limit: sq.Hits(),
		room: r.s.pageBytes - r.stampCost() - jsonLen(code.GitAnswer{Kind: code.GitSearch,
			Search: &code.SearchAnswer{Tip: tip, Ref: ref, Via: code.SearchViaGrep, SkippedFiles: 1 << 30,
				Truncated: true}}),
		answer: code.SearchAnswer{Hits: []code.SearchHit{}, Tip: tip, Ref: ref}}
	if viaIndex {
		if view, ok := r.s.o.Cache.CodeIndex(r.synced); ok {
			err = s.viaIndex(sctx, view)
		} else {
			viaIndex = false
		}
	}
	if !viaIndex {
		err = s.viaGrep(sctx, tip)
	}
	budget := false
	switch {
	case err == nil:
	case ctx.Err() == nil && (sctx.Err() != nil || errors.Is(err, code.ErrSliceTooLarge)):
		// The search's own budget, or its git's, ran out while its asker
		// still listens: what it found is the answer.
		budget, s.answer.Truncated = true, true
	default:
		return code.GitAnswer{}, err
	}
	release()
	a := code.GitAnswer{Kind: code.GitSearch, Search: &s.answer}
	if !budget {
		if b := r.s.o.Cache.Memo(r.synced); b != nil {
			m := &memoRun{r: r, batch: b, whole: whole}
			m.put(cursor{}, a, r.s.pageBytes-r.stampCost()-jsonLen(a))
			m.place(code.GitSearch, same(memoTail{}))
		}
	}
	return a, nil
}

// searchTarget is a search's steps 5 and 7 — its grant judged and the commit
// it reads resolved (searchTip) — with objects' ONE second look at the log's
// head: NO REFUSAL IS CONCLUDED FROM A COPY THAT MAY BE BEHIND THE LOG, so a
// grant the copy's row refuses, or a ref or commit the copy lacks, is judged
// again once on a copy synced there, that look charged as objects charges it
// and a refusal past the bucket saying it was not judged again
// (code.NotRejudged).
func (r *read) searchTarget(ctx context.Context) (ref, tip string, err error) {
	for {
		refused := r.grant()
		absent := false
		if refused == nil {
			if ref, tip, absent, err = r.searchTip(ctx); err != nil || !absent {
				return ref, tip, err
			}
		}
		note := ""
		if !r.linear {
			spent := r.resync()
			if spent == nil {
				if err := r.sync(ctx, statelog.Freshness{Level: statelog.ReadLinearizable}); err != nil {
					return "", "", err
				}
				continue
			}
			_, _, principal, scope := r.floorWords()
			note = " " + code.NotRejudged(spent, principal, scope, time.Now())
		}
		if refused != nil {
			return "", "", fmt.Errorf("%w%s", refused, note)
		}
		at := r.q.Search.Ref
		if at == "" {
			at = "its default branch " + r.synced.Repo.DefaultBranch
		}
		return "", "", fmt.Errorf("%w: repository %s holds no commit at %s%s", code.ErrObjectNotFound,
			r.synced.Repo.Name, at, note)
	}
}

// searchTip is the whole ref the search names — the default branch's where
// it names none, empty for a commit's id — and the commit it reads: the
// copy's, at its position; absent where the copy holds no commit there,
// which searchTarget looks for once more at the log's head.
func (r *read) searchTip(ctx context.Context) (ref, tip string, absent bool, err error) {
	name := r.q.Search.Ref
	switch {
	case name == "":
		ref = "refs/heads/" + r.synced.Repo.DefaultBranch
	case strings.HasPrefix(name, "refs/"):
		ref = name
	case gitref.ValidOID(name):
	default:
		ref = "refs/heads/" + name
	}
	at := name
	if ref != "" {
		at = ref
	}
	var out bytes.Buffer
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping, Config: r.s.o.Cache.ServeConfig(),
		Args:   []string{"rev-parse", "--verify", "--quiet", "--end-of-options", at + "^{commit}"},
		Stdout: &out, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err == nil {
		tip = strings.TrimSpace(out.String())
		if gitref.ValidOID(tip) {
			return ref, tip, false, nil
		}
		return "", "", false, r.unparsed(ctx, "rev-parse", fmt.Errorf("rev-parse answered %q", out.String()))
	}
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) || exit.Result.Exit != 1 || res.Damage != nil {
		return "", "", false, r.failed(ctx, "rev-parse", res, err, nil)
	}
	return "", "", true, nil
}

// searching is one search's run: what it confirms with, and what it has
// found.
type searching struct {
	r      *read
	plan   *codeindex.Plan
	filter codeindex.PathFilter
	limit  int
	// room is what the hits may encode to: the answer's bound less its
	// envelope's stamps and its own fields at their widest.
	room   int
	answer code.SearchAnswer
	// longIn is the last file counted in SkippedFiles for a line too long to
	// run the expression on: git grep answers a file's lines one at a time,
	// and a file is counted once however many of its lines are.
	longIn string
}

// add confirms one file's lines — a whole blob, or (grep) one line of it
// numbered line — and keeps every hit, answering errSearchFull once the
// search has all it answers.
func (s *searching) add(ctx context.Context, path, blob string, text []byte, line int) error {
	long, err := s.plan.Confirm(ctx, text, func(h codeindex.Hit) bool {
		n := h.Line
		if line > 0 {
			n = line
		}
		p, b64 := code.WirePath(path)
		hit := code.SearchHit{Path: p, PathB64: b64, Line: n, Blob: blob,
			Text: textcut.Within(strings.ToValidUTF8(string(h.Text), "�"), code.MaxSearchLineBytes)}
		cost := jsonLen(hit) + 1
		if len(s.answer.Hits) == s.limit || cost > s.room {
			s.answer.Truncated = true
			return false
		}
		s.room -= cost
		s.answer.Hits = append(s.answer.Hits, hit)
		return true
	})
	if long > 0 && path != s.longIn {
		s.longIn = path
		s.answer.SkippedFiles++
	}
	switch {
	case err != nil:
		return err
	case s.answer.Truncated:
		return errSearchFull
	}
	return nil
}

// indexedFile is a candidate the index path reads.
type indexedFile struct {
	path string
	blob codeindex.OID
}

// viaIndex is the index path over view.
func (s *searching) viaIndex(ctx context.Context, view *cache.CodeIndexView) error {
	s.answer.Via = code.SearchViaIndex
	want := map[codeindex.OID]bool{}
	for _, seg := range view.Segments {
		oids, err := s.plan.Candidates(seg)
		if err != nil {
			return fmt.Errorf("query: the code index of repository %s: %w", s.r.q.RepoID, err)
		}
		for _, o := range oids {
			want[o] = true
		}
	}
	var files []indexedFile
	for i := range view.Map.Len() {
		path := string(view.Map.Path(i))
		if !s.filter.Match(path) {
			continue
		}
		if view.Map.Skipped(i) {
			s.answer.SkippedFiles++
			continue
		}
		if b := view.Map.Blob(i); want[b] {
			files = append(files, indexedFile{path: path, blob: b})
		}
	}
	if len(files) == 0 {
		return nil
	}
	slices.SortFunc(files, func(a, b indexedFile) int { return cmp.Compare(a.path, b.path) })
	var in strings.Builder
	for _, f := range files {
		in.WriteString(f.blob.String())
		in.WriteByte('\n')
	}
	// One answer a line asked, in the order asked: a blob two paths hold is
	// asked, read and confirmed once for each, as git grep reads it.
	next := 0
	out := &catStream{each: func(oid string, body []byte) error {
		if next == len(files) || files[next].blob.String() != oid {
			return fmt.Errorf("%w: cat-file answered %s out of the order asked", errUnparsed, oid)
		}
		f := files[next]
		next++
		return s.add(ctx, f.path, oid, body, 0)
	}}
	r := s.r
	return s.run(ctx, "cat-file of the search's candidates", func(deadline time.Duration) (gitproc.Result, error) {
		return r.s.o.Git.Run(ctx, gitproc.Cmd{Dir: r.synced.Dir, Dropping: r.synced.Dropping,
			Config: r.s.o.Cache.ServeConfig(), Args: []string{"cat-file", "--batch"},
			Stdin: strings.NewReader(in.String()), Stdout: out, Class: gitproc.ClassRead, Deadline: deadline})
	}, out.close)
}

// viaGrep is the grep path over tip.
func (s *searching) viaGrep(ctx context.Context, tip string) error {
	s.answer.Via = code.SearchViaGrep
	// THE FILES PAST THE BOUND, and every file's blob: what git grep -I
	// cannot be told to pass over, and what a hit names.
	r := s.r
	var listing bytes.Buffer
	if err := s.run(ctx, "ls-tree of the search's commit", func(deadline time.Duration) (gitproc.Result, error) {
		return r.s.o.Git.Run(ctx, gitproc.Cmd{Dir: r.synced.Dir, Dropping: r.synced.Dropping,
			Config: r.s.o.Cache.ServeConfig(), Args: []string{"ls-tree", "-r", "-l", "-z", "--end-of-options", tip},
			Stdout: &listing, Class: gitproc.ClassRead, Deadline: deadline})
	}, nil); err != nil {
		return err
	}
	blobs, large := map[string]string{}, map[string]bool{}
	for rec := range bytes.SplitSeq(listing.Bytes(), []byte{0}) {
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		f := strings.Fields(string(meta))
		if !ok || len(f) != 4 || f[1] != "blob" {
			continue
		}
		blobs[string(path)] = f[2]
		if size, err := strconv.ParseInt(f[3], 10, 64); err == nil && size > code.MaxIndexedFileBytes &&
			s.filter.Match(string(path)) {
			large[string(path)] = true
			s.answer.SkippedFiles++
		}
	}
	// ONE PATTERN, the plan's alternation of escaped strings, as an argument
	// of -e, where git reads it as a pattern whatever it starts with
	// (codeindex.Plan.Grep says why one and not a string each); with no
	// clause git reads exactly, every line, each confirmed here.
	tail := []string{"-e", "^"}
	if pattern, fold, ok := s.plan.Grep(); ok {
		tail = []string{"-E", "-e", pattern}
		if fold {
			tail = []string{"-E", "-i", "-e", pattern}
		}
	}
	tail = append(tail, "--end-of-options", tip)
	out := &grepStream{prefix: tip + ":", each: func(path string, line int, text []byte) error {
		if large[path] || !s.filter.Match(path) {
			return nil
		}
		return s.add(ctx, path, blobs[path], text, line)
	}}
	err := s.run(ctx, "git grep over the search's commit", func(deadline time.Duration) (gitproc.Result, error) {
		return r.s.o.Git.Run(ctx, gitproc.Cmd{Dir: r.synced.Dir, Dropping: r.synced.Dropping,
			Config: r.s.o.Cache.ServeConfig(),
			Args:   append([]string{"grep", "-I", "-n", "-z", "--no-color", "--full-name"}, tail...),
			Stdout: out, Class: gitproc.ClassRead, Deadline: deadline})
	}, out.close)
	var exit *gitproc.ExitError
	if errors.As(err, &exit) && exit.Result.Exit == 1 {
		return nil // git grep's "no line matched"
	}
	return err
}

// run runs one git of the search over the copy — git, which spells its own
// Cmd where it meets the pool, so the writers gate reads every argument
// list a search hands git — its deadline what is left of the read's budget
// of git; done, where set, is what its stdout's reader says of the end of
// the stream. A run the search stopped itself (errSearchFull) answered all
// it needs.
func (s *searching) run(ctx context.Context, what string, git func(deadline time.Duration) (gitproc.Result, error),
	done func() error) error {

	r := s.r
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok {
		deadline = min(deadline, time.Until(d))
	}
	res, err := git(deadline)
	if s.answer.Truncated && res.Damage == nil {
		r.charge(res)
		return nil
	}
	if err == nil && done != nil {
		err = done()
		if err != nil {
			r.charge(res)
			return r.unparsed(ctx, what, err)
		}
	}
	if ctx.Err() != nil && res.Damage == nil {
		r.charge(res)
		return ctx.Err()
	}
	return r.failed(ctx, what, res, err, nil)
}

// catStream reads `cat-file --batch`'s answer, calling each with every
// object's id and bytes, whole.
type catStream struct {
	each func(oid string, body []byte) error
	buf  []byte
}

func (c *catStream) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	for {
		nl := bytes.IndexByte(c.buf, '\n')
		if nl < 0 {
			return len(p), nil
		}
		f := strings.Fields(string(c.buf[:nl]))
		if len(f) != 3 || f[1] != "blob" {
			return 0, fmt.Errorf("%w: cat-file answered %q", errUnparsed, c.buf[:nl])
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size > code.MaxIndexedFileBytes {
			return 0, fmt.Errorf("%w: cat-file answered %q", errUnparsed, c.buf[:nl])
		}
		if len(c.buf) < nl+1+size+1 {
			return len(p), nil
		}
		body := c.buf[nl+1 : nl+1+size]
		if err := c.each(f[0], body); err != nil {
			return 0, err
		}
		c.buf = c.buf[nl+1+size+1:]
	}
}

// close is nil where the stream ended between objects.
func (c *catStream) close() error {
	if len(c.buf) != 0 {
		return fmt.Errorf("%w: cat-file's answer ended inside an object", errUnparsed)
	}
	return nil
}

// grepStream reads `git grep -n -z`'s lines — `<tip>:<path>\0<line>\0<text>\n`
// — calling each with every one. READ BY ITS NULS FIRST: a path holds any
// byte but NUL, a newline among them, so a record is its name up to the
// first NUL, its number up to the next, and its text up to the newline after
// that — which a line's text, git's own split, never holds.
type grepStream struct {
	prefix string
	each   func(path string, line int, text []byte) error
	buf    []byte
}

func (g *grepStream) Write(p []byte) (int, error) {
	g.buf = append(g.buf, p...)
	for {
		name, rest, ok := bytes.Cut(g.buf, []byte{0})
		if !ok {
			return len(p), nil
		}
		num, rest, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return len(p), nil
		}
		text, _, ok := bytes.Cut(rest, []byte{'\n'})
		if !ok {
			return len(p), nil
		}
		line, err := strconv.Atoi(string(num))
		if err != nil || line < 1 || !bytes.HasPrefix(name, []byte(g.prefix)) {
			return 0, fmt.Errorf("%w: git grep answered %q", errUnparsed, name)
		}
		if err := g.each(string(name[len(g.prefix):]), line, text); err != nil {
			return 0, err
		}
		g.buf = g.buf[len(name)+1+len(num)+1+len(text)+1:]
	}
}

// close is nil where the stream ended on a line's end.
func (g *grepStream) close() error {
	if len(g.buf) != 0 {
		return fmt.Errorf("%w: git grep's answer ended inside a line", errUnparsed)
	}
	return nil
}
