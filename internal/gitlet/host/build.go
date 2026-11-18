package host

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/toolname"
)

// AN ENGINE WRITE'S BUILD (package doc, "An engine write is built beside the
// copy and lands as a push"): git's plumbing over exactly the trees the change
// touches, never an index of the whole tree (TestAnEngineCommitReadsNoWholeTree).
// Each process runs alone in the read lane, its input known before it starts:
// one fed by a caller reading its answers can leave the pool's copy of its
// input waiting on the caller after it dies (gitproc's Stdin contract).
// The build's processes share one unit of the share and one budget of git run
// time (gitlet.WriteBuildDeadline), each charged its run, never its slot wait.
// Each writes only in the write's scratch (cache.NewScratch); an object the
// copy already holds is not written but FRESHENED in it — its pack's mtime, no
// part of a copy's image nor a pack's identity (cache's fileID) — so the
// scratch's loose objects, and the pack, are exactly what the build made new.

// emptyTreeID is git's empty tree, which git knows whether or not a repository
// stores it: a root commit's revert's other side and cherry-pick's merge base.
const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// lookupFormat is what a lookup asks of every name; the mode is set only for a
// name given by path.
const lookupFormat = "--batch-check=%(objectname) %(objecttype) %(objectsize) %(objectmode)"

// object is what the copy holds under one name: kind is "" for nothing and
// "submodule" for a gitlink; mode, for a path only, is as git reads it
// ("40000" for a directory).
type object struct {
	id, kind, mode string
	size           int64
}

// present reports a name the copy holds something under.
func (o object) present() bool { return o.kind != "" }

// file reports a regular or executable file: what an edit or a mode change
// takes.
func (o object) file() bool {
	return o.kind == "blob" && (o.mode == gitlet.ModeFile || o.mode == gitlet.ModeExecutable)
}

// what is the object as a sentence names it.
func (o object) what() string {
	switch {
	case o.kind == "tree":
		return "a directory"
	case o.kind == "submodule":
		return "a submodule"
	case o.mode == gitlet.ModeLink:
		return "a link"
	}
	return "a file"
}

// judgement is what a write's check read of the copy.
type judgement struct {
	// head is the branch's commit, "" where the branch does not exist.
	head string
	// parent is the commit the write's commit sits on: the head, a new
	// branch's start, or "" for a root commit.
	parent string
	// absent is the first object the write names that the copy lacks, "" for
	// none.
	absent string
}

// check is the write's short read of the copy, outside the build: its branch,
// then every object it names by id. A head other than the caller's is a stale
// verdict; an object of the wrong type is gitlet.ErrInvalid naming its field;
// one the copy lacks is judgement.absent, looked for again at the log's head.
// A commit_files' paths are the build's to read (building.paths).
func (wr *writing) check(ctx context.Context) (judgement, writeVerdict, error) {
	w := wr.w
	head, err := wr.head(ctx)
	if err != nil {
		return judgement{}, writeVerdict{}, err
	}
	j := judgement{head: head, parent: w.ExpectedHead}
	if j.parent == "" {
		j.parent = w.Base
	}
	creates := w.Kind == gitlet.WriteCreateBranch || w.Kind == gitlet.WriteCreateTag ||
		w.Kind == gitlet.WriteRevertChange || (w.Kind == gitlet.WriteCommitFiles && w.ExpectedHead == "")
	if (creates && head != "") || (!creates && head != w.ExpectedHead) {
		return j, writeVerdict{reason: gitlet.RefusalStale, detail: wr.headSentence(head), stale: true}, nil
	}
	type need struct{ kind, field string }
	want := map[string]need{}
	name := func(id, kind, field string) {
		if _, named := want[id]; !named && id != "" {
			want[id] = need{kind, field}
		}
	}
	name(w.Commit, "commit", "commit")
	name(w.Base, "commit", "base")
	for i, f := range w.Files {
		name(f.BlobOID, "blob", fmt.Sprintf("files[%d].blob_oid", i))
	}
	for i, s := range w.Suggestions {
		name(s.BlobOID, "blob", fmt.Sprintf("suggestions[%d].blob_oid", i))
	}
	ids := slices.Sorted(maps.Keys(want))
	if len(ids) == 0 {
		return j, writeVerdict{}, nil
	}
	got, err := wr.lookup(ctx, lookups{ids: ids}, "check of what the write names", gitproc.ReadDeadline)
	if err != nil {
		return judgement{}, writeVerdict{}, err
	}
	for i, id := range ids {
		switch o, n := got[i], want[id]; {
		case !o.present():
			if j.absent == "" {
				j.absent = id
			}
		case o.kind != n.kind:
			return judgement{}, writeVerdict{}, fmt.Errorf("%w: %s: %s is a %s, not a %s", gitlet.ErrInvalid, n.field, id,
				o.kind, n.kind)
		}
	}
	return j, writeVerdict{}, nil
}

// head is where the write's branch is in the copy, "" where it does not exist:
// ONE `for-each-ref` of its exact name, never cat-file, which falls back to
// refs/heads/<name> and the like when no ref of exactly that name exists and
// so would refuse a create as stale for good. for-each-ref also lists refs
// beneath the name, so only the exact line is read. A branch naming anything
// but a commit is gitlet.ErrInvalid: no engine write moves one. A tag names
// any object, an annotated tag's own first.
func (wr *writing) head(ctx context.Context) (string, error) { return wr.headOf(ctx, wr.w.Ref) }

// headOf is [writing.head] for any branch, a rebase's target among them.
func (wr *writing) headOf(ctx context.Context, branch string) (string, error) {
	var head, kind string
	lines := gitproc.NewLines(func(line []byte) error {
		id, rest, ok := bytes.Cut(line, []byte{' '})
		typ, ref, ok2 := bytes.Cut(rest, []byte{' '})
		if !ok || !ok2 || !gitref.ValidOID(id) {
			return fmt.Errorf("host: for-each-ref answered %q, which names no ref", line)
		}
		if string(ref) == branch {
			head, kind = string(id), string(typ)
		}
		return nil
	})
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Args: []string{"for-each-ref", "--format=%(objectname) %(objecttype) %(refname)", branch},
		Config: wr.s.o.Cache.ServeConfig(), Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err == nil {
		err = lines.Close()
	}
	if err = wr.failed(ctx, "read of the branch", res, err); err != nil {
		return "", err
	}
	if head != "" && kind != "commit" && !strings.HasPrefix(branch, "refs/tags/") {
		return "", fmt.Errorf("%w: ref: %s names a %s, not a commit, which no engine write moves", gitlet.ErrInvalid,
			branch, kind)
	}
	return head, nil
}

// lookup is ONE `cat-file --batch-check -Z` in the copy of q's asks — -Z so a
// path is one record whatever it holds — answering each in order, a zero
// object for a name the copy lacks. what names the read in a failure.
func (wr *writing) lookup(ctx context.Context, q lookups, what string, deadline time.Duration) ([]object, error) {
	n := q.len()
	out := make([]object, 0, n)
	recs := newRecords(func(rec []byte) error {
		i := len(out)
		if i >= n {
			return fmt.Errorf("host: cat-file answered %d records for %d names", i+1, n)
		}
		if q.missing(i, rec) {
			out = append(out, object{})
			return nil
		}
		o, ok := parseLookup(rec)
		if !ok {
			return fmt.Errorf("host: cat-file answered %q for %q, which is no object", rec, q.name(i))
		}
		out = append(out, o)
		return nil
	})
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Args: []string{"cat-file", lookupFormat, "-Z"}, Config: wr.s.o.Cache.ServeConfig(),
		Stdin: q.reader(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = recs.close()
	}
	if err = wr.failed(ctx, what, res, err); err != nil {
		return nil, err
	}
	if len(out) != n {
		return nil, fmt.Errorf("host: cat-file answered %d records for %d names", len(out), n)
	}
	return out, nil
}

// lookups is what one lookup asks, in order: object ids, then paths at one
// commit, each `<commit>:<path>`. NEVER SPELLED OUT AHEAD: every directory
// above a path is asked too, so the spelled asks are every prefix — 133 KB for
// one 4 KiB path 64 deep (gitlet.MaxTreePathDepth), 34 MB for a mebibyte of
// such paths — so they are spelled as git reads (reader) and matched in place
// (missing).
type lookups struct {
	ids   []string
	at    string
	paths []string
}

func (q lookups) len() int { return len(q.ids) + len(q.paths) }

// appendAsk appends ask i, unterminated.
func (q lookups) appendAsk(b []byte, i int) []byte {
	if i < len(q.ids) {
		return append(b, q.ids[i]...)
	}
	b = append(b, q.at...)
	b = append(b, ':')
	return append(b, q.paths[i-len(q.ids)]...)
}

// name is ask i spelled out, for a failure's sentence only.
func (q lookups) name(i int) string { return string(q.appendAsk(nil, i)) }

// missingSuffix ends git's answer that an ask names nothing: `<ask> missing`.
const missingSuffix = " missing"

// missing reports whether rec is git's answer that ask i names nothing.
func (q lookups) missing(i int, rec []byte) bool {
	rest, ok := bytes.CutSuffix(rec, []byte(missingSuffix))
	if !ok {
		return false
	}
	if i < len(q.ids) {
		return string(rest) == q.ids[i]
	}
	p := q.paths[i-len(q.ids)]
	at, colon := len(q.at), len(q.at)+1
	return len(rest) == colon+len(p) && string(rest[:at]) == q.at && rest[at] == ':' && string(rest[colon:]) == p
}

// reader is every ask NUL-ended, spelled as git reads.
func (q lookups) reader() io.Reader { return &lookupsReader{q: q} }

// lookupsReader is a Cmd.Stdin holding one ask at a time. Every Read returns
// at once and the asks are fixed before the run, never decided by git's
// answers (gitproc's Stdin contract).
type lookupsReader struct {
	q       lookups
	next    int    // the next ask to spell
	spelled []byte // the ask being fed, and its NUL
	pending []byte // what of it is not yet fed
}

func (r *lookupsReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(r.pending) == 0 {
			if r.next == r.q.len() {
				break
			}
			r.spelled = append(r.q.appendAsk(r.spelled[:0], r.next), 0)
			r.pending = r.spelled
			r.next++
		}
		c := copy(p[n:], r.pending)
		n += c
		r.pending = r.pending[c:]
	}
	if n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, nil
}

// parseLookup reads one record of lookupFormat: `<id> <type> <size> <mode>`
// (mode empty for an id), or `<id> submodule` for a gitlink whose commit the
// copy lacks. A gitlink whose commit it holds is a submodule all the same.
func parseLookup(rec []byte) (object, bool) {
	fields := strings.Split(string(rec), " ")
	if len(fields) == 2 && fields[1] == "submodule" && gitref.ValidOID(fields[0]) {
		return object{id: fields[0], kind: "submodule", mode: modeSubmodule}, true
	}
	if len(fields) != 4 || !gitref.ValidOID(fields[0]) {
		return object{}, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return object{}, false
	}
	switch fields[1] {
	case "commit", "tree", "blob", "tag":
	default:
		return object{}, false
	}
	mode := strings.TrimLeft(fields[3], "0")
	if fields[3] != "" && mode == "" {
		return object{}, false
	}
	o := object{id: fields[0], kind: fields[1], size: size, mode: mode}
	if mode == modeSubmodule {
		o.kind = "submodule"
	}
	return o, true
}

// namedPaths is every path a commit_files names (a move's destination too) and
// every directory above one but the root, sorted, each once. A walk up stops
// at the first path already seen, whose ancestors are seen too.
func namedPaths(files []gitlet.FileOp) []string {
	seen := map[string]bool{}
	add := func(p string) {
		for ; p != "." && p != "" && !seen[p]; p = path.Dir(p) {
			seen[p] = true
		}
	}
	for _, f := range files {
		add(f.Path)
		if f.Op == gitlet.FileOpMove {
			add(f.To)
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// failed is what a git process's end means for a write, its run first charged
// (writing.charge). Damage, even with exit 0, or a directory shape the pool
// refused, is unready syncing: the cache repairs it at once, as for a read (no
// process here names a gitproc.Repairer), and a repair under the write would
// hold it for a round nobody waits on. The answer falling due, or a read lane
// busy past gitproc.SlotWait, is unready share — never too_large, the budget
// having gone on waits. A build process out of budget is errBuildSpent
// (too_large: another node would take as long); a short check out of its bound
// is unready node_failed, which no healthy node takes over a few names.
func (wr *writing) failed(ctx context.Context, what string, res gitproc.Result, err error) error {
	wr.charge(res)
	s := wr.s
	if res.Damage != nil {
		return wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s met damage the node is "+
			"repairing (%v); another Gitlet node builds the write meanwhile", s.o.NodeID, what, wr.repoName(),
			res.Damage))
	}
	switch {
	case err == nil:
		return nil
	case gitlet.AnswerDue(ctx):
		return wr.unready(gitlet.UnreadyShare, fmt.Sprintf("%s could not finish the %s over repository %s before "+
			"the write's answer fell due — its asker's attempt spent on the waits before it; another Gitlet node "+
			"builds it", s.o.NodeID, what, wr.repoName()))
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.Is(err, gitproc.ErrBusy):
		return wr.unready(gitlet.UnreadyShare, fmt.Sprintf("%s's read lane was busy past %s; another Gitlet node "+
			"builds the write meanwhile", s.o.NodeID, gitproc.SlotWait))
	case errors.Is(err, gitproc.ErrDeadline) && wr.metered:
		return errBuildSpent
	case errors.Is(err, gitproc.ErrDeadline):
		return wr.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s's %s over repository %s ran its whole bound, "+
			"which no healthy Gitlet node takes over it: %v; another Gitlet node builds the write", s.o.NodeID, what,
			wr.repoName(), err))
	case errors.Is(err, gitproc.ErrRepoShape):
		return wr.unready(gitlet.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s did not complete: %v; "+
			"another Gitlet node builds the write meanwhile", s.o.NodeID, what, wr.repoName(), err))
	}
	return fmt.Errorf("host: %s over repository %s: %w", what, wr.repoName(), err)
}

// budget is the deadline the build's next process is given: what is left of
// the build's shared run time, or errBuildSpent once the one before ran it out.
func (wr *writing) budget() (time.Duration, error) {
	if wr.left <= 0 {
		return 0, errBuildSpent
	}
	return wr.left, nil
}

// charge counts a process's run, never its slot wait, against the budget once
// the build has begun (writing.metered).
func (wr *writing) charge(res gitproc.Result) {
	if wr.metered {
		wr.left -= res.Took
	}
}

// built is what a build made.
type built struct {
	commit string
}

// writeStage is the directory under a write's scratch its build stages files
// in; it goes with the scratch.
const writeStage = "write"

// building is one build in progress.
type building struct {
	wr    *writing
	sc    *cache.Scratch
	stage string
	files int // staged files so far, which name the next
}

// build is a write's commit, by its kind, and its pack, both in the scratch,
// or a refusal of the change (done, with the answer).
func (wr *writing) build(ctx context.Context, sc *cache.Scratch, j judgement) (built, gitlet.EngineWriteResult,
	bool, error) {

	b, err := wr.newBuilding(sc)
	if err != nil {
		return built{}, gitlet.EngineWriteResult{}, true, err
	}
	var (
		commit string
		res    gitlet.EngineWriteResult
		done   bool
	)
	switch wr.w.Kind {
	case gitlet.WriteCommitFiles:
		commit, res, done, err = b.commitFiles(ctx, j)
	case gitlet.WriteApplySuggestions:
		commit, res, done, err = b.applySuggestions(ctx, j)
	case gitlet.WriteRebaseChange:
		commit, res, done, err = b.rebase(ctx)
	case gitlet.WriteRevertChange:
		commit, res, done, err = b.revertChange(ctx)
	case gitlet.WriteCreateTag:
		commit, err = b.tag(ctx)
	default:
		commit, res, done, err = b.pick(ctx)
	}
	if done || err != nil {
		return built{}, res, true, err
	}
	if res, done, err := b.pack(ctx); done || err != nil {
		return built{}, res, true, err
	}
	return built{commit: commit}, gitlet.EngineWriteResult{}, false, nil
}

// newBuilding is a build in sc, its staging directory made under it.
func (wr *writing) newBuilding(sc *cache.Scratch) (*building, error) {
	stage := filepath.Join(sc.Dir, writeStage)
	if err := os.Mkdir(stage, hostbox.DirMode); err != nil {
		return nil, fmt.Errorf("host: the write's staging in %s: %w", sc.Dir, err)
	}
	return &building{wr: wr, sc: sc, stage: stage}, nil
}

// ancestor reports whether commit a is an ancestor of commit b, or b itself:
// ONE `merge-base --is-ancestor` in the scratch, within the build's budget,
// since without a commit-graph it walks history by date.
func (b *building) ancestor(ctx context.Context, a, of string) (bool, error) {
	left, err := b.wr.budget()
	if err != nil {
		return false, err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"merge-base", "--is-ancestor", a, of},
		Class: gitproc.ClassRead, Answers: []int{1}, Deadline: left,
	})
	if err = b.wr.failed(ctx, "ancestry of the merge", res, err); err != nil {
		return false, err
	}
	return res.Exit == 0, nil
}

// create makes the build's next staged file, of kind.
func (b *building) create(kind string) (*os.File, error) {
	b.files++
	return os.OpenFile(filepath.Join(b.stage, kind+"-"+strconv.Itoa(b.files)), os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		hostbox.FileMode)
}

// commitFiles is a commit_files' build: its paths read, then committed on
// ([building.commitPlan]).
func (b *building) commitFiles(ctx context.Context, j judgement) (string, gitlet.EngineWriteResult, bool, error) {
	files := b.wr.w.Files
	paths, err := b.paths(ctx, j.parent, files)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	return b.commitPlan(ctx, j, files, paths, nil)
}

// commitPlan commits files on what paths holds at the parent. An
// apply_suggestions' files are one edit a file, each read through its splices
// ([splice]) in place of an edit's old and new text.
func (b *building) commitPlan(ctx context.Context, j judgement, files []gitlet.FileOp, paths map[string]object,
	spliced map[string][]splice) (string, gitlet.EngineWriteResult, bool, error) {

	wr := b.wr
	p, refusal, detail := planFiles(files, j.parent, paths)
	if refusal != "" {
		return "", wr.changeRefused(refusal, detail), true, nil
	}
	for _, e := range p.edits {
		e.splices = spliced[e.blob.path]
	}
	parent, refusal, detail, err := b.contents(ctx, j, p)
	switch {
	case err != nil:
		return "", gitlet.EngineWriteResult{}, true, err
	case refusal != "":
		return "", wr.changeRefused(refusal, detail), true, nil
	}
	if err = b.stagePuts(p); err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	if res, done, err := b.blobLimit(p); done || err != nil { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		return "", res, true, err
	}
	if err = b.hash(ctx, p); err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	root, err := b.trees(ctx, p)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	if j.parent != "" && root == parent.tree {
		return "", wr.changeRefused(gitlet.WriteNothingChanged, fmt.Sprintf("the change leaves %s's tree as it "+
			"is: no commit is made", shortID(j.parent))), true, nil
	}
	message := wr.w.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commit, err := b.commit(ctx, root, j.parent, parent.committed, message)
	return commit, gitlet.EngineWriteResult{}, false, err
}

// paths is what every path a commit_files names, and every directory above
// one (namedPaths), is at parent: ONE lookup in the copy, within the build's
// budget. A root commit has none; a path the parent lacks is absent.
//
// IT IS THE BUILD'S, never the check's: git walks each path from the root, so
// one n deep costs some n(n+1)/2 tree reads — 1 000 paths with their
// directories took 19 s of git 64 deep (1.8 s 16 deep) on git 2.56.0. As a
// short read, running out is a node failing, which sends the write to the
// next node to fail alike; in the build it is the change's too_large.
func (b *building) paths(ctx context.Context, parent string, files []gitlet.FileOp) (map[string]object, error) {
	if parent == "" {
		return nil, nil
	}
	named := namedPaths(files)
	left, err := b.wr.budget()
	if err != nil {
		return nil, err
	}
	got, err := b.wr.lookup(ctx, lookups{at: parent, paths: named}, "read of the paths the change names", left)
	if err != nil {
		return nil, err
	}
	out := make(map[string]object, len(named))
	for i, p := range named {
		if got[i].present() {
			out[p] = got[i]
		}
	}
	return out, nil
}

// contents is the build's ONE `cat-file --batch -Z` of the copy: the parent
// commit's header, every touched directory the parent holds staged as git
// stores it, and every file an edit rewrites streamed through its edit into a
// staged file. An edit's refusal is answered by name.
func (b *building) contents(ctx context.Context, j judgement, p *plan) (*commitHeader, gitlet.WriteRefusal, string,
	error) {

	parent := &commitHeader{}
	var (
		files []*os.File
		names []string
		kinds []string
		to    []func() (io.Writer, error)
	)
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	staging := func(kind string, staged *string) (*os.File, error) {
		f, err := b.create(kind)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		*staged = f.Name()
		return f, nil
	}
	ask := func(name, kind string, w func() (io.Writer, error)) {
		names, kinds, to = append(names, name), append(kinds, kind), append(to, w)
	}
	if j.parent != "" {
		ask(j.parent, "commit", func() (io.Writer, error) { return parent, nil })
	}
	for _, d := range p.order() {
		dc := p.dirs[d]
		if dc.base != "" {
			ask(dc.base, "tree", func() (io.Writer, error) { return staging("tree", &dc.staged) })
		}
	}
	for _, e := range p.edits {
		ask(e.base, "blob", func() (io.Writer, error) {
			f, err := staging("edit", &e.blob.staged)
			if err != nil {
				return nil, err
			}
			// Either stream holds only a bounded window of the file.
			if e.splices != nil {
				e.stream = newSpliceStream(e.splices, f)
			} else {
				e.stream = newEditStream(e.old, e.new, f)
			}
			return e.stream, nil
		})
	}
	if len(names) == 0 {
		return parent, "", "", nil
	}
	reader := newBatchReader(names, batchSink{
		object: func(i int, _, kind string, _ int64) (io.Writer, error) {
			if kind != kinds[i] {
				return nil, fmt.Errorf("host: %s is a %s in the copy, not the %s it was checked as", names[i], kind,
					kinds[i])
			}
			return to[i]()
		},
		missing: func(i int) error {
			return fmt.Errorf("host: %s is gone from the copy it was checked in", names[i])
		},
	})
	var stdin bytes.Buffer
	for _, n := range names {
		stdin.WriteString(n)
		stdin.WriteByte(0)
	}
	left, err := b.wr.budget()
	if err != nil {
		return nil, "", "", err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.wr.synced.Dir, Args: []string{"cat-file", "--batch", "-Z"}, Config: b.wr.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: reader, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = reader.Close()
	}
	if err = b.wr.failed(ctx, "read of the files and directories the change touches", res, err); err != nil {
		return nil, "", "", err
	}
	for _, e := range p.edits {
		refusal, detail, err := e.stream.Close(e.blob.path)
		if err != nil || refusal != "" {
			return nil, refusal, detail, err
		}
	}
	for _, f := range files {
		if err := f.Close(); err != nil {
			return nil, "", "", fmt.Errorf("host: staging %s: %w", f.Name(), err)
		}
	}
	files = nil
	return parent, "", "", nil
}

// stagePuts writes every put's text into a staged file.
func (b *building) stagePuts(p *plan) error {
	for _, put := range p.puts {
		f, err := b.create("put")
		if err != nil {
			return err
		}
		put.blob.staged = f.Name()
		_, err = io.WriteString(f, put.content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("host: staging %s: %w", put.blob.path, err)
		}
	}
	return nil
}

// blobLimit refuses, blob_too_large as a push is, a change with a new file
// past code.limits.max_blob_bytes before anything is hashed: the landing would
// refuse it anyway, after the whole build.
func (b *building) blobLimit(p *plan) (gitlet.EngineWriteResult, bool, error) {
	limit := b.wr.s.limits().MaxBlobBytes
	for _, nb := range p.newBlobs() {
		info, err := os.Stat(nb.staged)
		if err != nil {
			return gitlet.EngineWriteResult{}, true, err
		}
		if info.Size() > limit {
			return b.wr.refused(gitlet.RefusalBlobTooLarge, fmt.Sprintf("%s would be %s, past "+
				"code.limits.max_blob_bytes (%s)", nb.path, humanBytes(info.Size()), humanBytes(limit))), true, nil
		}
	}
	return gitlet.EngineWriteResult{}, false, nil
}

// hash is ONE `hash-object -w` over every new file the build staged, setting
// each one's id.
func (b *building) hash(ctx context.Context, p *plan) error {
	blobs := p.newBlobs()
	if len(blobs) == 0 {
		return nil
	}
	var stdin bytes.Buffer
	for _, nb := range blobs {
		stdin.WriteString(nb.staged)
		stdin.WriteByte('\n')
	}
	var ids []string
	lines := gitproc.NewLines(func(line []byte) error {
		if !gitref.ValidOID(line) {
			return fmt.Errorf("host: hash-object answered %q, which is no object id", line)
		}
		ids = append(ids, string(line))
		return nil
	})
	left, err := b.wr.budget()
	if err != nil {
		return err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"hash-object", "-w", "--no-filters", "--stdin-paths"},
		Stdin: &stdin, Stdout: lines, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = lines.Close()
	}
	if err = b.wr.failed(ctx, "hash of the change's new files", res, err); err != nil {
		return err
	}
	if len(ids) != len(blobs) {
		return fmt.Errorf("host: hash-object answered %d ids for %d files", len(ids), len(blobs))
	}
	for i, nb := range blobs {
		nb.id = ids[i]
	}
	return nil
}

// trees writes every touched directory a level at a time, deepest first since
// a parent names its children's new ids, and answers the root's id. A
// directory left empty is dropped from its parent; an empty root is the empty
// tree.
func (b *building) trees(ctx context.Context, p *plan) (string, error) {
	ids := map[string]string{} // a directory's new id, "" where it is left empty
	levels := p.levels()
	for depth := len(levels) - 1; depth >= 0; depth-- {
		if err := b.level(ctx, p, levels[depth], ids); err != nil {
			return "", err
		}
	}
	return ids[""], nil
}

// level is ONE `mktree -z --batch` for the directories of one depth, staged as
// a file: each one's untouched entries passed through as git reads them
// (treeEntry.canon), then what the change sets, ended by an empty record. A
// directory left empty sends no record, so mktree never writes it — but the
// root, whose empty record writes the empty tree.
func (b *building) level(ctx context.Context, p *plan, level []string, ids map[string]string) error {
	if len(level) == 0 {
		return nil
	}
	f, err := b.create("level")
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, gitproc.CopyBuffer)
	var sent []string
	for _, d := range level {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		n, err := writeDir(bw, p.dirs[d], ids)
		if err != nil {
			_ = f.Close()
			return err
		}
		if n == 0 && d != "" {
			ids[d] = ""
			continue
		}
		if err := bw.WriteByte(0); err != nil {
			_ = f.Close()
			return err
		}
		sent = append(sent, d)
	}
	err = bw.Flush()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("host: staging the directories to write: %w", err)
	}
	if len(sent) == 0 {
		return nil
	}
	in, err := os.Open(f.Name())
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	var answered []string
	lines := gitproc.NewLines(func(line []byte) error {
		if !gitref.ValidOID(line) {
			return fmt.Errorf("host: mktree answered %q, which is no tree id", line)
		}
		answered = append(answered, string(line))
		return nil
	})
	left, err := b.wr.budget()
	if err != nil {
		return err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"mktree", "-z", "--batch"}, Stdin: in,
		Stdout: lines, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = lines.Close()
	}
	if err = b.wr.failed(ctx, "write of the change's directories", res, err); err != nil {
		return err
	}
	if len(answered) != len(sent) {
		return fmt.Errorf("host: mktree answered %d trees for %d directories", len(answered), len(sent))
	}
	for i, d := range sent {
		ids[d] = answered[i]
	}
	return nil
}

// writeDir writes one directory's entries for mktree, answering how many.
func writeDir(w io.Writer, dc *dirChange, ids map[string]string) (int, error) {
	n := 0
	if dc.staged != "" {
		f, err := os.Open(dc.staged)
		if err != nil {
			return 0, err
		}
		err = parseTree(f, func(e treeEntry) error {
			if dc.touches(e.name) {
				return nil
			}
			n++
			return mktreeLine(w, e.canon(), e.idHex(), e.name)
		})
		_ = f.Close()
		if err != nil {
			return 0, fmt.Errorf("host: the directory %q: %w", dc.path, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(dc.set)) {
		e := dc.set[name]
		id := e.id
		if e.blob != nil {
			id = e.blob.id
		}
		n++
		if err := mktreeLine(w, e.mode, id, name); err != nil {
			return 0, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(dc.sub)) {
		id := ids[joinPath(dc.path, name)]
		if id == "" {
			continue
		}
		n++
		if err := mktreeLine(w, modeTree, id, name); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// commit is ONE `commit-tree` of tree onto parent ("" for a root commit) as
// the write's signature, dated by commitDates.
func (b *building) commit(ctx context.Context, tree, parent string, parentDate time.Time, message string) (string,
	error) {

	wr := b.wr
	author, committer := commitDates(wr.w.Instant(), parentDate)
	id := &gitproc.Identity{
		AuthorName: wr.sig.AuthorName, AuthorEmail: wr.sig.AuthorEmail,
		CommitterName: wr.sig.CommitterName, CommitterEmail: wr.sig.CommitterEmail,
		AuthorWhen: author, CommitterWhen: committer,
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	return b.commitOf(ctx, tree, parents, id, message)
}

// commitOf is ONE `commit-tree` of tree onto parents in order (none for a
// root commit, two for a merge) as id, message verbatim on its stdin.
func (b *building) commitOf(ctx context.Context, tree string, parents []string, id *gitproc.Identity,
	message string) (string, error) {

	wr := b.wr
	left, err := wr.budget()
	if err != nil {
		return "", err
	}
	// Each argument list is spelled in the Cmd that runs it, so the cache's
	// writers gate reads every one (cache's TestNothingButSyncWritesACache).
	var res gitproc.Result
	switch len(parents) {
	case 0:
		res, err = wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"commit-tree", tree}, Identity: id,
			Stdin: strings.NewReader(message), Class: gitproc.ClassRead, Deadline: left,
		})
	case 1:
		res, err = wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"commit-tree", tree, "-p", parents[0]},
			Identity: id, Stdin: strings.NewReader(message), Class: gitproc.ClassRead, Deadline: left,
		})
	case 2:
		res, err = wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate,
			Args:     []string{"commit-tree", tree, "-p", parents[0], "-p", parents[1]},
			Identity: id, Stdin: strings.NewReader(message), Class: gitproc.ClassRead, Deadline: left,
		})
	default:
		return "", fmt.Errorf("host: a commit of %d parents, and an engine write makes one of two at most",
			len(parents))
	}
	if err = wr.failed(ctx, "commit of the change", res, err); err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(res.Stdout))
	if !gitref.ValidOID(commit) {
		return "", fmt.Errorf("host: commit-tree answered %q, which is no commit id", commit)
	}
	return commit, nil
}

// tag is an annotated create_tag's object: one `mktag` (git's own fsck) of what git tag -a writes,
// its tagger the write's author (ADR-0092) at the operation's second, so a retry builds the same
// object.
func (b *building) tag(ctx context.Context) (string, error) {
	wr := b.wr
	left, err := wr.budget()
	if err != nil {
		return "", err
	}
	message := wr.w.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	at := wr.w.Instant().UTC()
	body := fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger %s <%s> %d +0000\n\n%s", wr.w.Commit,
		strings.TrimPrefix(wr.w.Ref, "refs/tags/"), wr.sig.AuthorName, wr.sig.AuthorEmail, at.Unix(), message)
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"mktag"}, Stdin: strings.NewReader(body),
		Class: gitproc.ClassRead, Deadline: left,
	})
	if err = wr.failed(ctx, "tag object of the write", res, err); err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(res.Stdout))
	if !gitref.ValidOID(id) {
		return "", fmt.Errorf("host: mktag answered %q, which is no object id", id)
	}
	return id, nil
}

// commitDates are an engine-built commit's dates (U18 c; ADR-0092): AUTHOR the
// operation's instant to the second, COMMITTER the later of that and the
// parent's committer date. A parent dated ahead (a person's clock) would
// otherwise give a child dated before it, which every walk by date pays for
// (package doc, "Measured"); git's walks read the committer date alone, as
// its own rebase and cherry-pick set it. Both are pure functions of the
// operation and parent, so a retry builds a byte-identical commit and one
// operation's writes share one author date.
func commitDates(instant, parentDate time.Time) (author, committer time.Time) {
	author = instant.UTC().Truncate(time.Second)
	committer = author
	if parentDate.After(committer) {
		committer = parentDate.UTC().Truncate(time.Second)
	}
	return author, committer
}

// later is the later of a and b: a merge commit's committer date is held at or
// past BOTH its parents' (commitDates).
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// pick is a revert's or a cherry-pick's build: ONE three-way merge onto the
// head of a subject with at most one parent, then its commit, or the
// conflicts answered with nothing stored.
func (b *building) pick(ctx context.Context) (string, gitlet.EngineWriteResult, bool, error) {
	wr := b.wr
	w := wr.w
	head := &commitHeader{}
	subject := &commitHeader{keep: gitlet.MaxCommitPayloadBytes}
	if err := b.readCommits(ctx, []string{w.ExpectedHead, w.Commit}, []*commitHeader{head, subject}); err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	if len(subject.parents) > 1 {
		return "", wr.changeRefused(gitlet.WriteMergeCommit, fmt.Sprintf("%s merges %d commits: reverting or picking "+
			"a merge needs a mainline; do it in a checkout of the repository ("+toolname.CodeCheckout+")", shortID(w.Commit),
			len(subject.parents))), true, nil
	}
	message := w.Message
	if message == "" {
		var (
			res  gitlet.EngineWriteResult
			done bool
			err  error
		)
		if message, res, done, err = wr.defaultMessage(subject); done || err != nil {
			return "", res, true, err
		}
	} else if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	parent := emptyTreeID
	if len(subject.parents) == 1 {
		parent = subject.parents[0]
	}
	base, other := parent, w.Commit
	if w.Kind == gitlet.WriteRevert {
		base, other = w.Commit, parent
	}
	m, err := b.merge(ctx, base, w.ExpectedHead, other)
	if err != nil {
		return "", gitlet.EngineWriteResult{}, true, err
	}
	if !m.clean {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := b.conflicted(ctx, m, fmt.Sprintf("%s does not apply cleanly on %s: %d paths conflict; resolve "+
			"them in a checkout of the repository ("+toolname.CodeCheckout+"), or write the result yourself", shortID(w.Commit),
			shortID(w.ExpectedHead), len(m.conflicts)+m.omitted))
		return "", res, true, err
	}
	if m.tree == head.tree {
		return "", wr.changeRefused(gitlet.WriteNothingChanged, fmt.Sprintf("%s changes nothing on %s: no commit "+
			"is made", shortID(w.Commit), shortID(w.ExpectedHead))), true, nil
	}
	commit, err := b.commit(ctx, m.tree, w.ExpectedHead, head.committed, message)
	return commit, gitlet.EngineWriteResult{}, false, err
}

// defaultMessage is git's own revert or cherry-pick message (message.go) where
// the caller gave none, as git writes it (gitref.WrittenCommitText). A subject
// in a non-UTF-8 `encoding`, which git would convert through iconv, is
// gitlet.ErrInvalid asking for a message; one past what a write carries is
// too_large.
func (wr *writing) defaultMessage(subject *commitHeader) (string, gitlet.EngineWriteResult, bool, error) {
	w := wr.w
	if enc := subject.encoding; enc != "" && !utf8Encoding(enc) {
		return "", gitlet.EngineWriteResult{}, true, fmt.Errorf("%w: message: %s's message is written in %q, "+
			"which this engine does not convert to UTF-8: give the write its message", gitlet.ErrInvalid,
			shortID(w.Commit), enc)
	}
	text := string(subject.message)
	cut := subject.cut
	if i := strings.IndexByte(text, 0); i >= 0 {
		text, cut = text[:i], false
	}
	if cut && (w.Kind == gitlet.WriteCherryPick || !subjectEnds(text)) {
		return "", wr.changeRefused(gitlet.WriteTooLarge, fmt.Sprintf("%s's message is past the %s one engine write "+
			"carries (gitlet.MaxCommitPayloadBytes): give the write its message, or %s it in a checkout of the repository "+
			"("+toolname.CodeCheckout+")", shortID(w.Commit), humanBytes(gitlet.MaxCommitPayloadBytes),
			strings.ReplaceAll(string(w.Kind), "_", "-"))), true, nil
	}
	msg := pickMessage(w.Commit, text)
	if w.Kind == gitlet.WriteRevert {
		msg = revertMessage(w.Commit, text)
	}
	return gitref.WrittenCommitText(msg), gitlet.EngineWriteResult{}, false, nil
}

// subjectEnds reports whether text's subject line ends within it: all a
// revert, which reads the subject alone, needs of a message cut short.
func subjectEnds(text string) bool {
	_, rest := commitSubject(text)
	return strings.IndexByte(rest, '\n') >= 0
}

// utf8Encoding is git's is_encoding_utf8: "utf-8" or "utf8" in any case.
func utf8Encoding(enc string) bool {
	return strings.EqualFold(enc, "utf-8") || strings.EqualFold(enc, "utf8")
}

// readCommits is ONE `cat-file --batch -Z` of commits into their headers, in
// the scratch, never the copy alone: a chain's next entry merges onto a commit
// the build made (chain.go), which only the scratch holds.
func (b *building) readCommits(ctx context.Context, ids []string, into []*commitHeader) error {
	var stdin bytes.Buffer
	for _, id := range ids {
		stdin.WriteString(id)
		stdin.WriteByte(0)
	}
	reader := newBatchReader(ids, batchSink{
		object: func(i int, _, kind string, _ int64) (io.Writer, error) {
			if kind != "commit" {
				return nil, fmt.Errorf("host: %s is a %s in the copy, not the commit it was checked as", ids[i], kind)
			}
			return into[i], nil
		},
		missing: func(i int) error {
			return fmt.Errorf("host: %s is gone from the copy it was checked in", ids[i])
		},
	})
	left, err := b.wr.budget()
	if err != nil {
		return err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"cat-file", "--batch", "-Z"},
		Stdin: &stdin, Stdout: reader, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = reader.Close()
	}
	return b.wr.failed(ctx, "read of the commits", res, err)
}

// commitHeader is what a build reads of a commit as it streams. author is the
// line as git wrote it, which a rebase replays; message is kept up to keep
// bytes, cut marking one longer.
type commitHeader struct {
	tree      string
	parents   []string
	author    string
	committed time.Time
	encoding  string
	message   []byte
	cut       bool
	keep      int

	buf  []byte
	body bool
}

func (h *commitHeader) Write(p []byte) (int, error) {
	n := len(p)
	if h.body {
		h.keepMessage(p)
		return n, nil
	}
	h.buf = append(h.buf, p...)
	for !h.body {
		i := bytes.IndexByte(h.buf, '\n')
		if i < 0 {
			if len(h.buf) > gitproc.MaxLine {
				return 0, gitproc.ErrLineTooLong
			}
			return n, nil
		}
		line := string(h.buf[:i])
		h.buf = h.buf[i+1:]
		switch {
		case line == "":
			h.body = true
			h.keepMessage(h.buf)
			h.buf = nil
		case strings.HasPrefix(line, "tree "):
			h.tree = strings.TrimPrefix(line, "tree ")
		case strings.HasPrefix(line, "parent "):
			h.parents = append(h.parents, strings.TrimPrefix(line, "parent "))
		case strings.HasPrefix(line, "author "):
			h.author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "committer "):
			h.committed = identDate(line)
		case strings.HasPrefix(line, "encoding "):
			h.encoding = strings.TrimPrefix(line, "encoding ")
		}
	}
	return n, nil
}

// keepMessage keeps what of p the message's bound leaves room for.
func (h *commitHeader) keepMessage(p []byte) {
	room := h.keep - len(h.message)
	if room >= len(p) {
		h.message = append(h.message, p...)
		return
	}
	h.message = append(h.message, p[:max(room, 0)]...)
	h.cut = true
}

// identDate is the instant an identity line `<who> <<email>> <seconds> <zone>`
// states (epoch seconds, whatever the zone), or zero where it states none.
func identDate(line string) time.Time {
	end := strings.LastIndexByte(line, '>')
	if end < 0 {
		return time.Time{}
	}
	fields := strings.Fields(line[end+1:])
	if len(fields) < 1 {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || secs < 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
}

// maxConflictsHeld bounds the conflicted paths a build holds to as many as
// could fit one answer, each at least ~24 bytes encoded; the rest are counted.
const maxConflictsHeld = gitlet.MaxGitAnswerBytes / 24

// mergeOutput is what `merge-tree --write-tree -z` answered, read as its
// records stream: the merged tree and each conflicted path's stages and kind.
type mergeOutput struct {
	tree      string
	clean     bool
	conflicts []gitlet.Conflict
	at        map[string]int // a held conflict's index, by its path
	omitted   int            // conflicted paths past those held
	firstType map[string]string

	state    int      // what the next record is (mergeTree, mergeStages, …)
	last     string   // the path of the last stage record
	want     int      // a message's paths still to come
	paths    []string // a message's paths
	typeWord string
}

// The records of merge-tree's -z output, in order.
const (
	mergeTree = iota
	mergeStages
	mergeCount
	mergePaths
	mergeType
	mergeMessage
)

// record reads one record: the tree, stage records `<mode> <id> <stage> TAB
// <path>` (one path's consecutive) up to an empty record, then each message's
// `<count>`, paths, type and words.
func (m *mergeOutput) record(rec []byte) error {
	switch m.state {
	case mergeTree:
		if !gitref.ValidOID(rec) {
			return fmt.Errorf("host: merge-tree answered %q, which is no tree id", rec)
		}
		m.tree, m.state = string(rec), mergeStages
	case mergeStages:
		if len(rec) == 0 {
			m.state = mergeCount
			return nil
		}
		head, p, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(head)
		if !ok || len(fields) != 3 || !gitref.ValidOID(fields[1]) {
			return fmt.Errorf("host: merge-tree answered %q, which is no stage record", rec)
		}
		if p != m.last {
			m.last = p
			if len(m.conflicts) >= maxConflictsHeld {
				m.omitted++
			} else {
				m.at[p] = len(m.conflicts)
				m.conflicts = append(m.conflicts, gitlet.Conflict{Path: p})
			}
		}
		i, held := m.at[p]
		if !held {
			return nil
		}
		switch fields[2] {
		case "1":
			m.conflicts[i].Base = fields[1]
		case "2":
			m.conflicts[i].Ours = fields[1]
		case "3":
			m.conflicts[i].Theirs = fields[1]
		default:
			return fmt.Errorf("host: merge-tree answered the stage %q for %q", fields[2], p)
		}
	case mergeCount:
		n, err := strconv.Atoi(string(rec))
		if err != nil || n < 0 {
			return fmt.Errorf("host: merge-tree answered %q where a message's count of paths is", rec)
		}
		m.want, m.paths, m.state = n, m.paths[:0], mergePaths
		if n == 0 {
			m.state = mergeType
		}
	case mergePaths:
		m.paths = append(m.paths, string(rec))
		if m.want--; m.want == 0 {
			m.state = mergeType
		}
	case mergeType:
		m.typeWord, m.state = string(rec), mergeMessage
	case mergeMessage:
		for _, p := range m.paths {
			i, held := m.at[p]
			if !held {
				continue
			}
			if _, typed := m.firstType[p]; !typed {
				m.firstType[p] = m.typeWord
			}
			if m.conflicts[i].Kind == "" && strings.HasPrefix(m.typeWord, "CONFLICT") {
				m.conflicts[i].Kind = m.typeWord
			}
		}
		m.state = mergeCount
	}
	return nil
}

// merge is ONE `merge-tree --write-tree -z` in the scratch, writing the merged
// tree (with conflict markers) there only; its cost follows the trees'
// differences, as it skips subtrees all three sides share. A base of "" is a
// change's merge into its target: git finds the base, and two unrelated
// histories merge (`--allow-unrelated-histories`), as the change's
// mergeability does (internal/gitlet/home), so the two never disagree.
func (b *building) merge(ctx context.Context, base, ours, theirs string) (*mergeOutput, error) {
	m := &mergeOutput{at: map[string]int{}, firstType: map[string]string{}}
	recs := newRecords(m.record)
	left, err := b.wr.budget()
	if err != nil {
		return nil, err
	}
	// Each argument list spelled in the Cmd that runs it, as commitOf's.
	var res gitproc.Result
	if base == "" {
		res, err = b.wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate,
			Args:   []string{"merge-tree", "--write-tree", "-z", "--allow-unrelated-histories", ours, theirs},
			Stdout: recs, Class: gitproc.ClassRead, Deadline: left, Answers: []int{1},
		})
	} else {
		res, err = b.wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate,
			Args:   []string{"merge-tree", "--write-tree", "-z", "--merge-base=" + base, ours, theirs},
			Stdout: recs, Class: gitproc.ClassRead, Deadline: left, Answers: []int{1},
		})
	}
	if err == nil {
		err = recs.close()
	}
	if err = b.wr.failed(ctx, "merge of the change", res, err); err != nil {
		return nil, err
	}
	if m.tree == "" {
		return nil, errors.New("host: merge-tree answered no tree")
	}
	m.clean = res.Exit == 0
	for i := range m.conflicts {
		c := &m.conflicts[i]
		if c.Kind == "" {
			c.Kind = cmpOr(m.firstType[c.Path], "CONFLICT")
		}
	}
	return m, nil
}

// cmpOr is the first of a and b that is not empty.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// conflicted answers a merge's conflicts, as many as fit
// gitlet.MaxGitAnswerBytes, each with the start of its marked file read in ONE
// `cat-file --batch -Z` of the scratch, which alone holds that tree; the rest
// are counted, detail the refusal's sentence. Nothing is stored.
func (b *building) conflicted(ctx context.Context, m *mergeOutput, detail string) (gitlet.EngineWriteResult,
	error) {

	wr := b.wr
	res := gitlet.EngineWriteResult{Refusal: gitlet.WriteConflict, PushID: wr.pushID, Detail: detail}
	held := m.conflicts[:fitConflicts(res, m.conflicts, m.omitted)]
	marked := make([]prefixWriter, len(held))
	var (
		names []string
		which []int
	)
	for i, c := range held {
		marked[i].n = gitlet.ConflictMarkedBytes
		// cat-file reads these as relative to a working tree it lacks
		// (object-name.c's resolve_relative_path); fsck admits no such path.
		if strings.HasPrefix(c.Path, "./") || strings.HasPrefix(c.Path, "../") {
			continue
		}
		names, which = append(names, m.tree+":"+c.Path), append(which, i)
	}
	if len(names) > 0 {
		var stdin bytes.Buffer
		for _, n := range names {
			stdin.WriteString(n)
			stdin.WriteByte(0)
		}
		reader := newBatchReader(names, batchSink{
			object:  func(i int, _, _ string, _ int64) (io.Writer, error) { return &marked[which[i]], nil },
			missing: func(int) error { return nil },
		})
		left, err := wr.budget()
		if err != nil {
			return gitlet.EngineWriteResult{}, err
		}
		run, err := wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"cat-file", "--batch", "-Z"}, Stdin: &stdin,
			Stdout: reader, Class: gitproc.ClassRead, Deadline: left,
		})
		if err == nil {
			err = reader.Close()
		}
		if err = wr.failed(ctx, "read of the conflicted files", run, err); err != nil {
			return gitlet.EngineWriteResult{}, err
		}
	}
	answered := make([]gitlet.Conflict, len(held))
	for i, c := range held {
		c.Marked = marked[i].text()
		c.Truncated = c.Marked != "" && marked[i].truncated()
		answered[i] = c
	}
	n := fitConflicts(res, answered, m.omitted+len(m.conflicts)-len(held))
	res.Conflicts = answered[:n]
	res.ConflictsOmitted = m.omitted + len(m.conflicts) - n
	return res, nil
}

// fitConflicts is how many of conflicts, in order, res answers within
// gitlet.MaxGitAnswerBytes encoded, those left out counted with omitted.
func fitConflicts(res gitlet.EngineWriteResult, conflicts []gitlet.Conflict, omitted int) int {
	res.Conflicts = nil
	res.ConflictsOmitted = omitted + len(conflicts) // its widest
	envelope, err := json.Marshal(res)
	if err != nil {
		return 0
	}
	size := len(envelope) + len(`,"conflicts":[]`)
	for k, c := range conflicts {
		one, err := json.Marshal(c)
		if err != nil {
			return k
		}
		size += len(one)
		if k > 0 {
			size++
		}
		if size > gitlet.MaxGitAnswerBytes {
			return k
		}
	}
	return len(conflicts)
}

// pack is ONE `pack-objects` of the scratch's loose objects into a staged
// file, then ONE `index-pack --strict` of it into the scratch: the relay's
// check of a push (receive.fsckObjects), so an engine write stores nothing a
// push would be refused. A pack past gitlet.MaxWritePackBytes is too_large; an
// object the check refuses is rejected_object, in git's words.
func (b *building) pack(ctx context.Context) (gitlet.EngineWriteResult, bool, error) {
	res, _, done, err := b.packWithin(ctx, gitlet.MaxWritePackBytes, writePackTooLarge)
	return res, done, err
}

// writePackTooLarge is the refusal of an engine write's pack past
// gitlet.MaxWritePackBytes, a merge_change's and a chain entry's (chain.go)
// alike.
func writePackTooLarge(size int64) string {
	return fmt.Sprintf("this change's pack is %s, past the %s one engine write may store "+
		"(gitlet.MaxWritePackBytes); make it in a checkout of the repository ("+toolname.CodeCheckout+")",
		humanBytes(size),
		humanBytes(gitlet.MaxWritePackBytes))
}

// packWithin is pack with its own bound and tooLarge sentence (a chain's is
// gitlet.MaxChainPackBytes, chain.go). With rejected_object it answers the
// object git's check named first, or "", so a chain skips the entry that
// wrote it.
func (b *building) packWithin(ctx context.Context, bound int64, tooLarge func(size int64) string) (
	gitlet.EngineWriteResult, string, bool, error) {

	wr := b.wr
	ids, err := looseObjects(filepath.Join(b.sc.Dir, "objects"))
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	if len(ids) == 0 {
		return gitlet.EngineWriteResult{}, "", false, nil
	}
	out, err := b.create("pack")
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	packPath := out.Name()
	err = b.packObjects(ctx, ids, out, "pack of the change's objects")
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	info, err := os.Stat(packPath)
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	if info.Size() > bound {
		return wr.changeRefused(gitlet.WriteTooLarge, tooLarge(info.Size())), "", true, nil
	}
	in, err := os.Open(packPath)
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	defer func() { _ = in.Close() }()
	left, err := wr.budget()
	if err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"index-pack", "--strict", "--stdin"},
		Config: []gitproc.KV{{Key: "pack.indexVersion", Value: "2"}, {Key: "pack.writeReverseIndex", Value: "true"}},
		Stdin:  in, Class: gitproc.ClassRead, Deadline: left,
	})
	if err != nil && res.Damage == nil && refusedByFsck(res) {
		wr.charge(res)
		return wr.changeRefused(gitlet.WriteRejectedObject, fsckWords(res)), fsckObject(res), true, nil
	}
	if err = wr.failed(ctx, "check of the change's objects", res, err); err != nil {
		return gitlet.EngineWriteResult{}, "", true, err
	}
	return gitlet.EngineWriteResult{}, "", false, nil
}

// packObjects is ONE `pack-objects` of ids in the scratch into out, within the
// build's budget; what names it in a failure.
func (b *building) packObjects(ctx context.Context, ids []string, out io.Writer, what string) error {
	wr := b.wr
	var stdin bytes.Buffer
	for _, id := range ids {
		stdin.WriteString(id)
		stdin.WriteByte('\n')
	}
	left, err := wr.budget()
	if err != nil {
		return err
	}
	// An object list, never revs or --thin, so no delta leans on an object
	// outside the pack; one thread and no reuse, so every node packs alike.
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate,
		Args:  []string{"pack-objects", "--threads=1", "--no-reuse-object", "-q", "--stdout"},
		Stdin: &stdin, Stdout: out, Class: gitproc.ClassRead, Deadline: left,
	})
	return wr.failed(ctx, what, res, err)
}

// counted is an io.Writer that keeps nothing and counts what it was given.
type counted int64

func (c *counted) Write(p []byte) (int, error) {
	*c += counted(len(p))
	return len(p), nil
}

// refusedByFsck reports an index-pack the fsck check stopped, by git's two
// formats for it (index-pack.c) in the trace — never by its stderr, which
// carries the objects' own text.
func refusedByFsck(res gitproc.Result) bool {
	for _, f := range res.Trace.Formats {
		if f == "fsck error in packed object" || f == "fsck error in pack objects" {
			return true
		}
	}
	return false
}

// fsckWords is the refusal's sentence: index-pack's line for the first object
// it refused, `<id>: <message id>: <message>`.
func fsckWords(res gitproc.Result) string {
	for line := range strings.Lines(res.Stderr) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "error: object "); ok {
			return "an object this change makes is one git's own check refuses: " + rest
		}
	}
	return "an object this change makes is one git's own check refuses: " + res.LastLine()
}

// fsckObject is the id on fsckWords' line, "" where no line names one.
func fsckObject(res gitproc.Result) string {
	for line := range strings.Lines(res.Stderr) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "error: object "); ok {
			if id, _, _ := strings.Cut(rest, ":"); gitref.ValidOID(id) {
				return id
			}
		}
	}
	return ""
}

// looseSize is the uncompressed size the loose object at path states in its
// header, so a chain weighs an entry's objects without inflating any whole.
func looseSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	z, err := zlib.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("host: loose object %s: %w", path, err)
	}
	defer func() { _ = z.Close() }()
	// At most 32 bytes: "commit", a space, twenty digits and the NUL.
	header, err := bufio.NewReaderSize(z, 32).ReadSlice(0)
	if err != nil {
		return 0, fmt.Errorf("host: loose object %s has no header: %w", path, err)
	}
	_, size, ok := strings.Cut(string(header[:len(header)-1]), " ")
	n, perr := strconv.ParseInt(size, 10, 64)
	if !ok || perr != nil || n < 0 {
		return 0, fmt.Errorf("host: loose object %s has the header %q", path, header)
	}
	return n, nil
}

// looseObjects is every loose object under objects, sorted.
func looseObjects(objects string) ([]string, error) {
	dirs, err := os.ReadDir(objects)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(objects, d.Name()))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range entries {
			if id := d.Name() + e.Name(); gitref.ValidOID(id) {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// prune removes every loose object in the scratch that kept does not name —
// what a skipped chain merge wrote — so the chain's pack is exactly its
// commits' objects, the same bytes a rebuild without that entry packs. An
// object in kept is safe whatever the skipped merge did: git never rewrites a
// loose object it holds, only freshens it.
func (b *building) prune(kept []string) error {
	objects := filepath.Join(b.sc.Dir, "objects")
	ids, err := looseObjects(objects)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, found := slices.BinarySearch(kept, id); found {
			continue
		}
		if err := os.Remove(filepath.Join(objects, id[:2], id[2:])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("host: dropping object %s a skipped merge wrote: %w", id, err)
		}
	}
	return nil
}
