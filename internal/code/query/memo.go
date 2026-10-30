package query

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/gitref"
)

// THE MEMO (the cache's, its memo.go): every page a read's run cuts — the
// asked one and every one after it the same run reaches — is kept beside the
// pinned copy, keyed by everything it is a function of, so the next read that
// asks for it is answered from a file rather than from git. A page is a
// function of the object ids the read names, its arguments, its cursor and the
// page bound; and of nothing a node or a moment holds: what Read stamps on an
// answer (its repository and position, the push that introduced each commit it
// names and the index's coverage) is stamped on a page from the memo as on one
// cut, and never memoized.
//
// # Answered before any git runs
//
// A read naming whole ids asks the memo once the grant has judged it and before
// ANY git runs — before even the check of what it names, which on a warm copy
// is about half a miss's cost and would leave a hit no faster than two
// processes. That is sound because the memo is the copy's: an entry is in an
// incarnation's memo only where a read of that incarnation found every object
// it names there, and a copy loses an object only with its history, which
// drops the memo whole and shuts it until the change is visible (the cache's
// memo.go). A read naming an abbreviation asks it once the abbreviation is
// expanded from the objects — what an abbreviation names can change as the
// copy gains objects, what a whole id names never — and its check has run. A
// refusal the copy may be behind about is judged at the log's head as ever
// (objects), and only then is the memo asked.
//
// # The run that misses cuts every page
//
// git cannot seek inside an object or a listing, so a page past the first is a
// read of everything before it: the fiftieth page of a file streams the
// forty-nine before it, and the recursive listing of a 100 000-file tree is
// some 250 pages, each a whole `ls-tree -r`. So a run that misses cuts every
// page from the one asked to the end of the answer — a listing's, a file's (or
// its line range's), a stat's, a patch's — and stages each as it is cut
// (cache.MemoBatch.Put), holding one page at a time beside the asked one; then
// memoizes them all once git has answered whole and reported no damage, each
// ending with what the run learned of it only at its end (its tail): a
// listing's total, a file's line count, a diff's totals. A
// stat's page's files' counts, which follow every file, are written into the
// page's staged entry as soon as the stat has counted past the page
// (cache.MemoBatch.Extend), so a stat's run holds one page's counts at a time
// however many files it counts. Reading a file page by page costs one stream
// of it. A comparison, whose answer is one page, is memoized whole; an
// interdiff's pages as a diff's are; a blame's run cuts every page of its
// range, as a file's does; a history's page is its walk's own — the walk ends
// where the page does, since its cost is the walk — memoized where the walk
// ended it, and never where the read's budget or its due instant did, which
// is the machine's and the moment's; and a landed-in read, a revision's facts
// and a re-anchoring never (memoizable says why). A history's and a blame's
// key carries the measure their pages were cut with (costModel), since a
// fence they are measured with is a build's.
//
// # A page fits whatever it is stamped with
//
// A page is cut against the room its read's stamped fields leave, measured
// with this read's: what the push index holds of a commit may grow before the
// page is read again, so each entry carries its BUDGET — the most the stamped
// fields may encode to and the page still fit its bound: the room its items
// left, beside what those fields cost when it was cut. A page whose read's
// stamped fields would take more is cut again, and that cut memoized over it.
// Every envelope is measured with Memo set (room), so a page from the memo
// fits as the one cut did.

// memoFormat names what an entry of this build holds — a memoPage as
// appendPage writes it and its tail as appendTail does — and is the first part
// of every key, so an entry of another shape is never read as one of this:
// TestAMemoEntryHoldsEveryFieldOfItsAnswer holds the encoding to the answers'
// fields, and TestAnAnswersShapeIsItsMemoFormat holds this name to a
// fingerprint of them, so a change to either fails until this moves with it.
const memoFormat = "page/3"

// memoPage is one page as the memo keeps it.
type memoPage struct {
	// answer is the page as its kind cut it — Kind, Rev, Next and the kind's
	// own part — with no field Read stamps.
	answer code.GitAnswer
	// commits is every commit the read that cut it named before it cut it
	// (read.named), which a hit decorates as that read did.
	commits []string
	// budget is the most the stamped fields of an answer of it may encode to
	// (read.stampCost) and the page still fit its bound.
	budget int
}

// memoTail is what a run learns of a page only at its end: a listing's total,
// a file's line count, a diff's totals and each of the page's files' counts
// in their order — none known when the page is cut, since every page is
// staged as it is cut and the run reads on to the answer's end. counted: the
// files' counts are in the entry already, written as the stat counted past
// the page (memoRun.extend), so the tail carries the totals alone.
type memoTail struct {
	total, added, deleted, lines int
	files                        []fileCounts
	counted                      bool
}

// fileCounts is one diff file's lines added and deleted, or that git calls it
// binary.
type fileCounts struct {
	added, deleted int
	binary         bool
}

// errMemoPage is an entry this build cannot read as a page: what a hit that
// fails to decode is, which the read answers as a miss — its run memoizes the
// page anew, under the same key.
var errMemoPage = errors.New("query: a memo entry that is no page of this build's")

// memoKey is the key of the page at the cursor at of the read q names, its
// revisions read as whole ids (whole, by the revision as q spells it): the
// memo's format, the page bound and q's kind, then every argument the page is
// a function of, normalised — a whole id for every revision, a path as its
// bytes, a filter's paths sorted and once each, a line range from 1 — and the
// cursor, canonical as every one a node issues (parseCursor).
func (r *read) memoKey(whole map[string]string, at cursor) cache.MemoKey {
	parts := []string{memoFormat, strconv.Itoa(r.s.pageBytes), string(r.q.Kind)}
	switch q := r.q; q.Kind {
	case code.GitTree:
		parts = append(parts, whole[q.Tree.Rev], pathOf(q), strconv.FormatBool(q.Tree.Recursive))
	case code.GitBlob:
		parts = append(parts, whole[q.Blob.Rev], pathOf(q), strconv.Itoa(max(q.Blob.StartLine, 1)),
			strconv.Itoa(q.Blob.EndLine))
	case code.GitDiff:
		file, _ := code.GitPath("diff.file", q.Diff.File, q.Diff.FileB64, true)
		paths := slices.Compact(slices.Sorted(slices.Values(q.Diff.Paths)))
		parts = append(parts, whole[q.Diff.Base], whole[q.Diff.Head], strconv.FormatBool(q.Diff.Renames), file,
			strconv.Itoa(len(paths)))
		parts = append(parts, paths...)
	case code.GitCompare:
		parts = append(parts, whole[q.Compare.Base], whole[q.Compare.Head])
	case code.GitInterdiff:
		// THE FILTER AS THE READ APPLIES IT (interdiffFilter): none where
		// the bases match or either revision touches all — two queries whose
		// lists differ then read one page — and otherwise the union of the
		// touched paths, sorted and once each.
		file, _ := code.GitPath("interdiff.file", q.Interdiff.File, q.Interdiff.FileB64, true)
		f := r.interdiffFilter()
		parts = append(parts, whole[q.Interdiff.FromHead], whole[q.Interdiff.ToHead], file,
			strconv.FormatBool(f.filters()), strconv.Itoa(len(f.paths)))
		parts = append(parts, f.paths...)
	case code.GitLog:
		// Since as the second its walk stops before, the limit as the page
		// applies it, and the cost the page's fields were measured at.
		path, _ := code.GitPath("log.path", q.Log.Path, q.Log.PathB64, true)
		since := ""
		if !q.Log.Since.IsZero() {
			since = strconv.FormatInt(sinceSecond(q.Log.Since), 10)
		}
		parts = append(parts, whole[q.Log.Rev], path, q.Log.Grep, since, strconv.Itoa(logLimitOf(q.Log)),
			costModel)
	case code.GitBlame:
		start, end := q.Blame.Range()
		parts = append(parts, whole[q.Blame.Rev], pathOf(q), strconv.Itoa(start), strconv.Itoa(end), costModel)
	}
	return cache.NewMemoKey(append(parts, formatCursor(at))...)
}

// memoizable reports whether q's kind is answered from the memo at all: every
// kind but a landed-in read, whose answer moves with its target; a revision's
// facts, asked once a revision and a target by whoever records them, and
// whose patch-id a node too loaded to finish it omits — an answer no page
// should keep; a re-anchoring, a check and one diff of a few blobs, keyed
// by a list of anchors each caller makes anew; and a checks file, asked once
// a target's tip by the home that keeps what it parsed of it — a whole file
// the memo would hold for nobody else.
func memoizable(q code.GitQuery) bool {
	switch q.Kind {
	case code.GitLandedIn, code.GitRevisionFacts, code.GitReanchor, code.GitChecksFile:
		return false
	}
	return true
}

// recallWhole is the memo's answer to a read naming whole ids alone, asked
// before any git runs — and on a miss, the answer of the read cutting the same
// page now, waited for (await): none for a read that names an abbreviation, or
// whose grant this copy refuses — objects judges that at the log's head, and
// the memo is asked after it.
func (r *read) recallWhole(ctx context.Context, at cursor) (code.GitAnswer, bool, error) {
	if !memoizable(r.q) || r.grant() != nil {
		return code.GitAnswer{}, false, nil
	}
	whole := map[string]string{}
	for _, rev := range revsOf(r.q) {
		if !gitref.ValidOID(rev) {
			return code.GitAnswer{}, false, nil
		}
		whole[rev] = rev
	}
	r.recalled = true
	if a, ok := r.recall(whole, at); ok {
		return a, true, nil
	}
	return r.await(ctx, whole, at)
}

// await is a memo miss's SINGLE FLIGHT: where another read of this node is
// cutting the page this one missed — the same repository and memo key — the
// read waits for that read to answer, its pages placed, and asks the memo
// again; where none is, it LEADS, its own run the page's flight until it has
// answered (Read lands it). A read whose memo still misses after the wait —
// the leader's run failed, was too large, or memoized nothing — runs its own,
// as any miss does, leading nothing. The wait holds no unit of the share and
// no slot of the read lane, and ends with the read's answer as a wait for the
// share does: due, it answers unready (share), which another git node answers
// meanwhile.
func (r *read) await(ctx context.Context, whole map[string]string, at cursor) (code.GitAnswer, bool, error) {
	k := flightKey{repo: r.q.RepoID, key: r.memoKey(whole, at)}
	f, lead := r.s.join(k)
	if lead {
		r.landing = func() { r.s.land(k, f) }
		return code.GitAnswer{}, false, nil
	}
	r.s.waiting.Add(1)
	select {
	case <-f.done:
		r.s.waiting.Add(-1)
	case <-ctx.Done():
		r.s.waiting.Add(-1)
		if code.AnswerDue(ctx) {
			return code.GitAnswer{}, false, r.unready(code.UnreadyShare, fmt.Sprintf("%s was cutting the page "+
				"for another read until the read's answer fell due, its asker's attempt spent; another git node "+
				"answers meanwhile", r.s.o.NodeID))
		}
		return code.GitAnswer{}, false, ctx.Err()
	}
	a, ok := r.recall(whole, at)
	return a, ok, nil
}

// recall is the memo's page at at of the read, its revisions read as whole
// ids, decorated as the read that cut it was — or none: no entry, one this
// build cannot read, or one whose budget this read's stamped fields would
// overrun.
func (r *read) recall(whole map[string]string, at cursor) (code.GitAnswer, bool) {
	body, ok := r.s.o.Cache.MemoGet(r.synced, r.memoKey(whole, at))
	if !ok {
		return code.GitAnswer{}, false
	}
	p, err := decodePage(body)
	if err != nil {
		return code.GitAnswer{}, false
	}
	r.index(p.commits)
	if r.stampCost() > p.budget {
		return code.GitAnswer{}, false
	}
	a := p.answer
	a.Memo = true
	return a, true
}

// wholeOf is every revision q names, by its spelling, as the whole id objs
// found it to be.
func wholeOf(q code.GitQuery, objs found) map[string]string {
	whole := map[string]string{}
	for _, rev := range revsOf(q) {
		whole[rev] = objs.revs[rev].id
	}
	return whole
}

// stampCost is what the fields Read stamps on every answer of this read —
// its repository, its copy's position, the push that introduced each commit
// it names, the index's coverage, and Memo — encode to, beside an answer
// holding nothing else: what a page's budget is held to. The difference it
// makes to any answer is the same, since every answer encodes the same field
// before them (Kind, which is never left out), so an answer's whole size moves
// by the difference between two reads' stamp costs exactly.
func (r *read) stampCost() int {
	return jsonLen(code.GitAnswer{RepoID: r.q.RepoID, Position: r.synced.Position, Memo: true,
		PushedBy: r.pushedBy, Coverage: r.coverage})
}

// memoRun is the pages one run memoizes: a batch of the pinned copy's memo,
// and how each page is keyed. Nil memoizes nothing — a cache that keeps no
// memo, a copy whose memo is shut, a landed-in read — and its methods accept
// it.
type memoRun struct {
	r     *read
	batch *cache.MemoBatch
	whole map[string]string
}

// memoRun begins the pages of the run about to answer the read, over objs.
func (r *read) memoRun(objs found) *memoRun {
	if !memoizable(r.q) {
		return nil
	}
	b := r.s.o.Cache.Memo(r.synced)
	if b == nil {
		return nil
	}
	return &memoRun{r: r, batch: b, whole: wholeOf(r.q, objs)}
}

// put stages a, the page at at, whose items left left of the room it was cut
// against, and answers which put it was (-1 for none). Pages are put in the
// order they are cut, which is what place's i-th tail is of.
func (m *memoRun) put(at cursor, a code.GitAnswer, left int) int {
	if m == nil {
		return -1
	}
	body := appendPage(nil, memoPage{answer: a, commits: m.r.named, budget: left + m.r.stampCost()})
	return m.batch.Put(m.r.memoKey(m.whole, at), body)
}

// extend writes files, the counts of the put-th page's files in their order,
// into that page's staged entry, where its tail would carry them (a tail of
// counted).
func (m *memoRun) extend(put int, files []fileCounts) {
	if m != nil {
		m.batch.Extend(put, appendCounts(nil, files))
	}
}

// place memoizes every page put, the i-th — each a page of kind — ending with
// tail(i).
func (m *memoRun) place(kind code.GitReadKind, tail func(i int) memoTail) {
	if m == nil {
		return
	}
	m.batch.Place(func(i int) []byte { return appendTail(nil, kind, tail(i)) })
}

// discard gives back every page put and memoizes none; after place it does
// nothing.
func (m *memoRun) discard() {
	if m != nil {
		m.batch.Discard()
	}
}

// same is a tail every page of a run shares.
func same(t memoTail) func(int) memoTail { return func(int) memoTail { return t } }

// THE ENCODING of a page and its tail: unsigned varints, and each string as
// its length then its bytes, every field in its struct's order. A page's
// fields its run learns only at its end are its tail's, never its own: a
// listing's Total, a file's Lines, a diff's totals and its files' counts — a
// stat's page and a patch's alike, the patch's naming its one file. A diff's
// files' counts are a block of their own, after the page and before its
// totals, so a stat writes each page's block as it is counted
// (memoRun.extend) and a patch's tail carries its one file's: the same bytes
// either way. Its Kind says which part follows; a comparison is one answer,
// its counts in the page. Binary rather than JSON because a hit decodes a
// whole page: a page of a listing is some 400 entries of five strings, which
// encoding/json decodes at a few tens of megabytes a second, against a
// varint reader slicing each string out of one copy.

// pageWriter is an entry's bytes as they are written.
type pageWriter []byte

func (w *pageWriter) uint(n uint64) { *w = binary.AppendUvarint(*w, n) }
func (w *pageWriter) int(n int)     { w.uint(uint64(max(n, 0))) }
func (w *pageWriter) size(n int64)  { w.uint(uint64(max(n, 0))) }
func (w *pageWriter) str(s string)  { w.uint(uint64(len(s))); *w = append(*w, s...) }

// time writes an instant as its seconds since the epoch — signed, which a
// commit's dates never need and the encoding costs nothing to allow — and its
// nanoseconds, read back in UTC as every answer states one.
func (w *pageWriter) time(t time.Time) {
	*w = binary.AppendVarint(*w, t.Unix())
	w.uint(uint64(t.Nanosecond()))
}
func (w *pageWriter) flag(v bool) {
	if v {
		*w = append(*w, 1)
	} else {
		*w = append(*w, 0)
	}
}

// appendPage is buf with p's encoding appended.
func appendPage(buf []byte, p memoPage) []byte {
	w := pageWriter(buf)
	w.int(p.budget)
	w.int(len(p.commits))
	for _, c := range p.commits {
		w.str(c)
	}
	a := p.answer
	w.str(string(a.Kind))
	w.str(a.Rev)
	w.str(a.Next)
	switch a.Kind {
	case code.GitTree:
		t := a.Tree
		w.str(t.Tree)
		w.str(t.Path)
		w.str(t.PathB64)
		w.int(len(t.Entries))
		for _, e := range t.Entries {
			w.str(e.Mode)
			w.str(e.Type)
			w.str(e.OID)
			w.str(e.Path)
			w.str(e.PathB64)
			w.size(e.Size)
		}
	case code.GitBlob:
		b := a.Blob
		w.str(b.Path)
		w.str(b.PathB64)
		w.str(b.OID)
		w.size(b.Size)
		w.flag(b.Binary)
		w.int(b.StartLine)
		w.int(b.EndLine)
		w.int(b.StartByte)
		w.str(b.Text)
		w.flag(b.Continues)
	case code.GitDiff, code.GitInterdiff:
		d := a.Diff
		w.str(d.Base)
		w.str(d.Head)
		w.str(d.Patch)
		w.int(len(d.Files))
		for _, f := range d.Files {
			appendFile(&w, f, false)
		}
	case code.GitCompare:
		c := a.Compare
		w.str(c.Base)
		w.str(c.Head)
		w.str(c.MergeBase)
		w.flag(c.BaseIsAncestor)
		w.flag(c.HeadIsAncestor)
		w.int(c.Ahead)
		w.int(c.Behind)
		w.int(len(c.Files))
		for _, f := range c.Files {
			appendFile(&w, f, true)
		}
		w.int(c.Total)
		w.int(c.Added)
		w.int(c.Deleted)
	case code.GitLog:
		l := a.Log
		w.str(l.Path)
		w.str(l.PathB64)
		w.int(len(l.Commits))
		for _, c := range l.Commits {
			w.str(c.OID)
			w.int(len(c.Parents))
			for _, p := range c.Parents {
				w.str(p)
			}
			w.int(c.ParentCount)
			w.str(c.Author)
			w.int(c.AuthorBytes)
			w.str(c.AuthorEmail)
			w.int(c.AuthorEmailBytes)
			w.time(c.AuthoredAt)
			w.time(c.CommittedAt)
			w.str(c.Subject)
			w.int(c.SubjectBytes)
		}
		w.int(l.Scanned)
		w.flag(l.Complete)
	case code.GitBlame:
		b := a.Blame
		w.str(b.Path)
		w.str(b.PathB64)
		w.int(b.StartLine)
		w.int(b.EndLine)
		w.int(len(b.Lines))
		for _, l := range b.Lines {
			w.int(l.Line)
			w.str(l.Commit)
			w.int(l.OrigLine)
			w.str(l.OrigPath)
			w.str(l.OrigPathB64)
			w.str(l.Text)
			w.int(l.TextBytes)
		}
		// The commits by id, in order: one page, one encoding.
		w.int(len(b.Commits))
		for _, id := range slices.Sorted(maps.Keys(b.Commits)) {
			c := b.Commits[id]
			w.str(id)
			w.str(c.Author)
			w.int(c.AuthorBytes)
			w.str(c.AuthorEmail)
			w.int(c.AuthorEmailBytes)
			w.time(c.AuthoredAt)
			w.str(c.Subject)
			w.int(c.SubjectBytes)
		}
	}
	return w
}

// appendFile writes one diff file: its counts too where with is set (a
// comparison's), and otherwise none — they are its page's tail.
func appendFile(w *pageWriter, f code.DiffFile, with bool) {
	w.str(f.Path)
	w.str(f.PathB64)
	w.str(f.OldPath)
	w.str(f.OldPathB64)
	w.str(f.Status)
	w.int(f.Similarity)
	w.str(f.OldMode)
	w.str(f.NewMode)
	if with {
		w.int(f.Added)
		w.int(f.Deleted)
		w.flag(f.Binary)
	}
	w.str(f.OldOID)
	w.str(f.NewOID)
}

// appendTail is buf with t's encoding appended, as a page of kind ends: a
// listing's total, a file's line count, a diff's files' counts — unless they
// were counted into the entry already — and its totals, and nothing after a
// comparison.
func appendTail(buf []byte, kind code.GitReadKind, t memoTail) []byte {
	diff := kind == code.GitDiff || kind == code.GitInterdiff
	if diff && !t.counted {
		buf = appendCounts(buf, t.files)
	}
	w := pageWriter(buf)
	switch {
	case kind == code.GitTree:
		w.int(t.total)
	case kind == code.GitBlob:
		w.int(t.lines)
	case diff:
		w.int(t.total)
		w.int(t.added)
		w.int(t.deleted)
	}
	return w
}

// appendCounts is buf with a diff page's files' counts appended, in its
// files' order: how many, then each one's added, deleted and binary.
func appendCounts(buf []byte, files []fileCounts) []byte {
	w := pageWriter(buf)
	w.int(len(files))
	for _, f := range files {
		w.int(f.added)
		w.int(f.deleted)
		w.flag(f.binary)
	}
	return w
}

// pageReader reads an entry's bytes as appendPage and appendTail wrote them.
// It reads a string as a slice of the one copy of the entry it holds, so a
// page of four hundred entries costs one allocation for its strings, not two
// thousand.
type pageReader struct {
	s   string
	err error
}

func (r *pageReader) uint() uint64 {
	if r.err != nil {
		return 0
	}
	var n uint64
	for i, shift := 0, uint(0); i < len(r.s) && i < binary.MaxVarintLen64; i, shift = i+1, shift+7 {
		c := r.s[i]
		n |= uint64(c&0x7f) << shift
		if c < 0x80 {
			r.s = r.s[i+1:]
			return n
		}
	}
	r.err = errMemoPage
	return 0
}

// int is a count, which no page holds past what an int carries.
func (r *pageReader) int() int {
	n := r.uint()
	if n > uint64(maxInt) {
		r.err = errMemoPage
		return 0
	}
	return int(n)
}

// size is a file's or an entry's bytes.
func (r *pageReader) size() int64 { return int64(r.int()) }

// count is a list's length: never more than the bytes left, since every item
// is at least one.
func (r *pageReader) count() int {
	n := r.int()
	if n > len(r.s) {
		r.err = errMemoPage
		return 0
	}
	return n
}

func (r *pageReader) str() string {
	n := r.int()
	if r.err != nil || n > len(r.s) {
		r.err = errMemoPage
		return ""
	}
	s := r.s[:n]
	r.s = r.s[n:]
	return s
}

// time is an instant as pageWriter.time wrote it.
func (r *pageReader) time() time.Time {
	if r.err != nil {
		return time.Time{}
	}
	secs, n := binary.Varint([]byte(r.s[:min(len(r.s), binary.MaxVarintLen64)]))
	if n <= 0 {
		r.err = errMemoPage
		return time.Time{}
	}
	r.s = r.s[n:]
	nanos := r.uint()
	if r.err != nil || nanos >= uint64(time.Second) {
		r.err = errMemoPage
		return time.Time{}
	}
	return time.Unix(secs, int64(nanos)).UTC()
}

func (r *pageReader) flag() bool {
	if r.err != nil || len(r.s) == 0 || r.s[0] > 1 {
		r.err = errMemoPage
		return false
	}
	v := r.s[0] == 1
	r.s = r.s[1:]
	return v
}

// maxInt is the largest int, which a count read back may not pass.
const maxInt = int(^uint(0) >> 1)

// decodePage is the page an entry holds, its tail applied: every field
// appendPage and appendTail wrote, and nothing left over — or errMemoPage.
func decodePage(body []byte) (memoPage, error) {
	r := &pageReader{s: string(body)}
	var p memoPage
	p.budget = r.int()
	if n := r.count(); r.err == nil {
		p.commits = make([]string, n)
		for i := range p.commits {
			p.commits[i] = r.str()
		}
	}
	a := &p.answer
	a.Kind = code.GitReadKind(r.str())
	a.Rev = r.str()
	a.Next = r.str()
	switch a.Kind {
	case code.GitTree:
		t := &code.TreeAnswer{Tree: r.str(), Path: r.str(), PathB64: r.str(), Entries: []code.TreeEntry{}}
		if n := r.count(); r.err == nil {
			t.Entries = make([]code.TreeEntry, n)
			for i := range t.Entries {
				t.Entries[i] = code.TreeEntry{Mode: r.str(), Type: r.str(), OID: r.str(), Path: r.str(),
					PathB64: r.str(), Size: r.size()}
			}
		}
		t.Total = r.int()
		a.Tree = t
	case code.GitBlob:
		a.Blob = &code.BlobAnswer{Path: r.str(), PathB64: r.str(), OID: r.str(), Size: r.size(), Binary: r.flag(),
			StartLine: r.int(), EndLine: r.int(), StartByte: r.int(), Text: r.str(), Continues: r.flag()}
		a.Blob.Lines = r.int()
	case code.GitDiff, code.GitInterdiff:
		d := &code.DiffAnswer{Base: r.str(), Head: r.str(), Patch: r.str(), Files: []code.DiffFile{}}
		if n := r.count(); r.err == nil {
			d.Files = make([]code.DiffFile, n)
			for i := range d.Files {
				d.Files[i] = readFile(r, false)
			}
		}
		if n := r.count(); r.err == nil && n != len(d.Files) {
			r.err = errMemoPage
		}
		for i := range d.Files {
			d.Files[i].Added, d.Files[i].Deleted, d.Files[i].Binary = r.int(), r.int(), r.flag()
		}
		d.Total, d.Added, d.Deleted = r.int(), r.int(), r.int()
		a.Diff = d
	case code.GitCompare:
		c := &code.CompareAnswer{Base: r.str(), Head: r.str(), MergeBase: r.str(), BaseIsAncestor: r.flag(),
			HeadIsAncestor: r.flag(), Ahead: r.int(), Behind: r.int(), Files: []code.DiffFile{}}
		if n := r.count(); r.err == nil {
			c.Files = make([]code.DiffFile, n)
			for i := range c.Files {
				c.Files[i] = readFile(r, true)
			}
		}
		c.Total, c.Added, c.Deleted = r.int(), r.int(), r.int()
		a.Compare = c
	case code.GitLog:
		l := &code.LogAnswer{Path: r.str(), PathB64: r.str(), Commits: []code.LogCommit{}}
		if n := r.count(); r.err == nil {
			l.Commits = make([]code.LogCommit, n)
			for i := range l.Commits {
				l.Commits[i] = readLogCommit(r)
			}
		}
		l.Scanned, l.Complete = r.int(), r.flag()
		a.Log = l
	case code.GitBlame:
		b := &code.BlameAnswer{Path: r.str(), PathB64: r.str(), StartLine: r.int(), EndLine: r.int(),
			Lines: []code.BlameLine{}, Commits: map[string]code.BlameCommit{}}
		if n := r.count(); r.err == nil {
			b.Lines = make([]code.BlameLine, n)
			for i := range b.Lines {
				b.Lines[i] = code.BlameLine{Line: r.int(), Commit: r.str(), OrigLine: r.int(), OrigPath: r.str(),
					OrigPathB64: r.str(), Text: r.str(), TextBytes: r.int()}
			}
		}
		if n := r.count(); r.err == nil {
			for range n {
				id := r.str()
				b.Commits[id] = code.BlameCommit{Author: r.str(), AuthorBytes: r.int(), AuthorEmail: r.str(),
					AuthorEmailBytes: r.int(), AuthoredAt: r.time(), Subject: r.str(), SubjectBytes: r.int()}
			}
			if len(b.Commits) != n {
				r.err = errMemoPage // an id twice: no page this build writes
			}
		}
		a.Blame = b
	default:
		return memoPage{}, errMemoPage
	}
	if r.err != nil || r.s != "" {
		return memoPage{}, errMemoPage
	}
	return p, nil
}

// readLogCommit reads one commit of a history as appendPage wrote it.
func readLogCommit(r *pageReader) code.LogCommit {
	c := code.LogCommit{OID: r.str()}
	if n := r.count(); r.err == nil && n > 0 {
		c.Parents = make([]string, n)
		for i := range c.Parents {
			c.Parents[i] = r.str()
		}
	}
	c.ParentCount = r.int()
	c.Author, c.AuthorBytes = r.str(), r.int()
	c.AuthorEmail, c.AuthorEmailBytes = r.str(), r.int()
	c.AuthoredAt, c.CommittedAt = r.time(), r.time()
	c.Subject, c.SubjectBytes = r.str(), r.int()
	return c
}

// readFile reads one diff file as appendFile wrote it.
func readFile(r *pageReader, with bool) code.DiffFile {
	f := code.DiffFile{Path: r.str(), PathB64: r.str(), OldPath: r.str(), OldPathB64: r.str(), Status: r.str(),
		Similarity: r.int(), OldMode: r.str(), NewMode: r.str()}
	if with {
		f.Added, f.Deleted, f.Binary = r.int(), r.int(), r.flag()
	}
	f.OldOID, f.NewOID = r.str(), r.str()
	return f
}
