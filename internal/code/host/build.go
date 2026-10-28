package host

import (
	"bufio"
	"bytes"
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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// AN ENGINE WRITE'S BUILD, AT A COST THAT FOLLOWS THE CHANGE (ADR-0062): git's
// own plumbing over exactly the trees the change touches — never read-tree,
// write-tree, update-index or a checkout, never ls-tree -r or diff-tree -r of
// the base: no index of the whole tree, ever (TestAnEngineCommitReadsNoWholeTree
// holds it). Every process is in the read lane (gitproc.ClassRead), and each is
// ONE for the whole write, run one after another, each with its input known
// before it starts: a process that answers line by line to a caller feeding it
// what its answers decide can leave the pool's copy of its input waiting on
// the caller after the process has died, for as long as the caller's deadline
// (gitproc's Stdin contract), so no process here is held open across another.
//
// FIRST, A CHECK of what the write names — outside the build, a short read
// like any (code.GitShare: no unit of the share): ONE `for-each-ref` of the
// branch's EXACT name, never cat-file's reading of a name, which tries
// refs/<name>, refs/tags/<name>, refs/heads/<name>… when no ref of exactly
// that name exists — measured on 2.56.0, `refs/heads/foo` answered the commit
// of a branch NAMED refs/heads/foo while refs/heads/foo itself did not
// exist, which would refuse a create as stale for good; and ONE `cat-file
// --batch-check -Z` of the objects it names and, for a commit_files, of every
// path an operation names and every directory above one at the parent.
//
// THEN THE BUILD, under ONE unit of the share and ONE budget of git run time
// (code.WriteBuildDeadline) that its processes share, each given what the ones
// before it left and charged what it ran — never its wait for a slot, which
// is the node's load and no fact about the change. A commit_files reads, in
// the copy, ONE batch (`cat-file --batch -Z`) of the parent commit — its
// committer's date — every directory the change touches, staged as git
// stores it, and every file an edit rewrites, streamed through its edit into
// a staged file; then, in its scratch: ONE `hash-object -w` of every new
// file, ONE `mktree -z --batch` a LEVEL of the touched directories, deepest
// first — a parent names its children's new ids, which only the level below's
// mktree answers — passing every entry the change does not touch through as
// git reads it (treeEntry.canon); and ONE `commit-tree`. A revert or a
// cherry-pick reads its two commits in ONE batch, runs ONE `merge-tree
// --write-tree -z` — whose cost follows the trees' DIFFERENCES, since it skips
// every subtree the three sides share — and commits the same way. Every kind
// that commits then packs: ONE `pack-objects` of the objects the build wrote,
// self-contained, into a staged file, and ONE `index-pack --strict` of it into
// the scratch, the relay's own check of every object a push stores
// (receive.fsckObjects).
//
// WHAT A STEP WRITES IT WRITES IN THE SCRATCH (cache.NewScratch, the write's
// alone): its objects, loose, and what the build stages under write/. An
// object the copy already holds is not written again: git FRESHENS the
// copy's pack that holds it — its modification time, not a byte (measured on
// 2.56.0 for hash-object, mktree and merge-tree alike) — which is no part of
// a copy's image and no part of a pack's identity (internal/code/cache's
// fileID). So the scratch's loose objects are exactly what the build made
// new, and the pack is exactly them.

// emptyTreeID is git's id of the tree with nothing in it: a root commit's
// revert's other side and its cherry-pick's merge base, which git knows
// whether or not a repository stores it.
const emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// lookupFormat is what the write's check asks of every name: the object, its
// type and size, and — for a tree entry named by path — its mode as git reads
// it.
const lookupFormat = "--batch-check=%(objectname) %(objecttype) %(objectsize) %(objectmode)"

// object is what the copy holds under one name: its id, its type ("" for
// nothing, "submodule" for a gitlink a path names), its size and — for a path
// — its tree entry's mode as git reads it ("40000" for a directory).
type object struct {
	id, kind, mode string
	size           int64
}

// present reports a name the copy holds something under.
func (o object) present() bool { return o.kind != "" }

// file reports a regular or an executable file: what an edit and a mode
// change.
func (o object) file() bool {
	return o.kind == "blob" && (o.mode == code.ModeFile || o.mode == code.ModeExecutable)
}

// what is the object as a sentence names it.
func (o object) what() string {
	switch {
	case o.kind == "tree":
		return "a directory"
	case o.kind == "submodule":
		return "a submodule"
	case o.mode == code.ModeLink:
		return "a link"
	}
	return "a file"
}

// judgement is what a write's check read of the copy: where the branch is, the
// commit the write's commit sits on, an object the write names that the copy
// lacks, and — for a commit_files — what each path it names and each
// directory above one is at that commit.
type judgement struct {
	// head is the branch's commit, "" where the branch does not exist.
	head string
	// parent is the commit the write's commit sits on: the head, a new
	// branch's start, or "" for a root commit.
	parent string
	// absent is an object the write names that the copy does not hold, ""
	// where it holds every one.
	absent string
	// paths is a commit_files' every named path and every directory above
	// one, at parent: a path it does not hold is a zero object.
	paths map[string]object
}

// check is the write's check of the copy: where its branch is, then — for a
// write onto the head it expects — what every object it names is, and every
// path a commit_files names. A head that is not the one the caller read is a
// stale verdict; an object of the wrong type is code.ErrInvalid naming its
// field; an object the copy lacks is judgement.absent, looked for again at
// the log's head.
func (wr *writing) check(ctx context.Context) (judgement, writeVerdict, error) {
	w := wr.w
	head, err := wr.head(ctx)
	if err != nil {
		return judgement{}, writeVerdict{}, err
	}
	j := judgement{head: head, parent: w.ExpectedHead, paths: map[string]object{}}
	if j.parent == "" {
		j.parent = w.Base
	}
	creates := w.Kind == code.WriteCreateBranch || (w.Kind == code.WriteCommitFiles && w.ExpectedHead == "")
	if (creates && head != "") || (!creates && head != w.ExpectedHead) {
		return j, writeVerdict{reason: code.RefusalStale, detail: wr.headSentence(head), stale: true}, nil
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
	ids := slices.Sorted(maps.Keys(want))
	var paths []string
	if w.Kind == code.WriteCommitFiles && j.parent != "" {
		paths = namedPaths(w.Files)
	}
	names := slices.Clone(ids)
	for _, p := range paths {
		names = append(names, j.parent+":"+p)
	}
	if len(names) == 0 {
		return j, writeVerdict{}, nil
	}
	got, err := wr.lookup(ctx, names)
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
			return judgement{}, writeVerdict{}, fmt.Errorf("%w: %s: %s is a %s, not a %s", code.ErrInvalid, n.field, id,
				o.kind, n.kind)
		}
	}
	for i, p := range paths {
		j.paths[p] = got[len(ids)+i]
	}
	return j, writeVerdict{}, nil
}

// head is where the write's branch is in the copy, "" where it does not exist:
// ONE `for-each-ref` of its exact name, which lists that ref and the refs
// beneath it as a directory — one of which may exist where the branch does
// not — so only the line naming it exactly is read. A branch naming anything
// but a commit is code.ErrInvalid: no engine write moves one.
func (wr *writing) head(ctx context.Context) (string, error) {
	var head, kind string
	lines := gitproc.NewLines(func(line []byte) error {
		id, rest, ok := bytes.Cut(line, []byte{' '})
		typ, ref, ok2 := bytes.Cut(rest, []byte{' '})
		if !ok || !ok2 || !gitref.ValidOID(id) {
			return fmt.Errorf("host: for-each-ref answered %q, which names no ref", line)
		}
		if string(ref) == wr.w.Ref {
			head, kind = string(id), string(typ)
		}
		return nil
	})
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Args: []string{"for-each-ref", "--format=%(objectname) %(objecttype) %(refname)", wr.w.Ref},
		Config: wr.s.o.Cache.ServeConfig(), Stdout: lines, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err == nil {
		err = lines.Close()
	}
	if err := wr.failed(ctx, "read of the branch", res, err); err != nil {
		return "", err
	}
	if head != "" && kind != "commit" {
		return "", fmt.Errorf("%w: ref: %s names a %s, not a commit, which no engine write moves", code.ErrInvalid,
			wr.w.Ref, kind)
	}
	return head, nil
}

// lookup is ONE `cat-file --batch-check -Z` of names in the copy — NUL-ended
// both ways, so a path is one record whatever it holds — and is what each
// name is, in order: a zero object for a name the copy holds nothing under.
func (wr *writing) lookup(ctx context.Context, names []string) ([]object, error) {
	var stdin bytes.Buffer
	for _, n := range names {
		stdin.WriteString(n)
		stdin.WriteByte(0)
	}
	out := make([]object, 0, len(names))
	recs := newRecords(func(rec []byte) error {
		i := len(out)
		if i >= len(names) {
			return fmt.Errorf("host: cat-file answered %d records for %d names", i+1, len(names))
		}
		if string(rec) == names[i]+" missing" {
			out = append(out, object{})
			return nil
		}
		o, ok := parseLookup(rec)
		if !ok {
			return fmt.Errorf("host: cat-file answered %q for %q, which is no object", rec, names[i])
		}
		out = append(out, o)
		return nil
	})
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: wr.synced.Dir, Args: []string{"cat-file", lookupFormat, "-Z"}, Config: wr.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: recs, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if err == nil {
		err = recs.close()
	}
	if err := wr.failed(ctx, "check of what the write names", res, err); err != nil {
		return nil, err
	}
	if len(out) != len(names) {
		return nil, fmt.Errorf("host: cat-file answered %d records for %d names", len(out), len(names))
	}
	return out, nil
}

// parseLookup reads one record of lookupFormat: `<id> <type> <size> <mode>`,
// the mode empty for an object no path named, or `<id> submodule` for a
// gitlink whose commit the copy does not hold — and a gitlink whose commit it
// does hold is a submodule all the same.
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

// namedPaths is every path a commit_files names — each operation's path, a
// move's destination — and every directory above one, the root left out:
// sorted, each once.
func namedPaths(files []code.FileOp) []string {
	seen := map[string]bool{}
	add := func(p string) {
		for ; p != "." && p != ""; p = path.Dir(p) {
			seen[p] = true
		}
	}
	for _, f := range files {
		add(f.Path)
		if f.Op == code.FileOpMove {
			add(f.To)
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// failed is what a git process's end means for a write, once its run is
// charged to the build where the build has begun (writing.metered): nil for
// one that answered; unready, syncing, for one that reported damage — failed
// or not, since git answers some questions over damage and exits 0 — or that
// the pool refused a directory's shape for: the cache repairs both, as it
// does a read's (no process here names a gitproc.Repairer, so the cache's
// observer acts on its report at once), and a repair under the write would
// hold it for a round no answer waits on. Unready, share, for one the write's
// answer fell due during — waiting for a slot or running — and for a read lane
// busy past gitproc.SlotWait; errBuildSpent for a process of the build that
// ran out the whole of its budget, which another node would take as long
// over (the build answers it too_large); and unready, node_failed, for one of
// the short checks before it running its whole bound, which no healthy node
// takes over a few names. Anything else is the process's own failure.
//
// A RUN THE ANSWER'S DUE INSTANT ENDED IS NEVER TOO LARGE, as no read's is: it
// had less than its budget, the rest spent on the waits before it.
func (wr *writing) failed(ctx context.Context, what string, res gitproc.Result, err error) error {
	wr.charge(res)
	s := wr.s
	if res.Damage != nil {
		return wr.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s met damage the node is "+
			"repairing (%v); another git node builds the write meanwhile", s.o.NodeID, what, wr.repoName(),
			res.Damage))
	}
	switch {
	case err == nil:
		return nil
	case code.AnswerDue(ctx):
		return wr.unready(code.UnreadyShare, fmt.Sprintf("%s could not finish the %s over repository %s before "+
			"the write's answer fell due — its asker's attempt spent on the waits before it; another git node "+
			"builds it", s.o.NodeID, what, wr.repoName()))
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.Is(err, gitproc.ErrBusy):
		return wr.unready(code.UnreadyShare, fmt.Sprintf("%s's read lane was busy past %s; another git node "+
			"builds the write meanwhile", s.o.NodeID, gitproc.SlotWait))
	case errors.Is(err, gitproc.ErrDeadline) && wr.metered:
		return errBuildSpent
	case errors.Is(err, gitproc.ErrDeadline):
		return wr.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s's %s over repository %s ran its whole bound, "+
			"which no healthy git node takes over it: %v; another git node builds the write", s.o.NodeID, what,
			wr.repoName(), err))
	case errors.Is(err, gitproc.ErrRepoShape):
		return wr.unready(code.UnreadySyncing, fmt.Sprintf("%s's %s over repository %s did not complete: %v; "+
			"another git node builds the write meanwhile", s.o.NodeID, what, wr.repoName(), err))
	}
	return fmt.Errorf("host: %s over repository %s: %w", what, wr.repoName(), err)
}

// budget is the deadline the build's next process is given: what is left of
// its budget of git run time, which every process of the build shares. A
// budget already run out is errBuildSpent: a run is charged after it ends, so
// a later process finds it spent only where the one before it ran out its
// last instant.
func (wr *writing) budget() (time.Duration, error) {
	if wr.left <= 0 {
		return 0, errBuildSpent
	}
	return wr.left, nil
}

// charge counts a process's run against the build's budget once the build
// has begun (writing.metered): its run alone, never its wait for a slot.
func (wr *writing) charge(res gitproc.Result) {
	if wr.metered {
		wr.left -= res.Took
	}
}

// built is what a build made: its commit.
type built struct {
	commit string
}

// writeStage is the directory under a write's scratch its build stages files
// in: new files' text, edited files, the touched directories as git stores
// them, each level's input to mktree, and the pack. Removed with the scratch.
const writeStage = "write"

// building is one build in progress.
type building struct {
	wr    *writing
	sc    *cache.Scratch
	stage string
	files int // staged files so far, which name the next
}

// build is a commit's build, by its kind, then its pack: the commit and its
// one pack in the scratch, or a refusal of the change (done, with the answer).
func (wr *writing) build(ctx context.Context, sc *cache.Scratch, j judgement) (built, code.EngineWriteResult,
	bool, error) {

	stage := filepath.Join(sc.Dir, writeStage)
	if err := os.Mkdir(stage, hostbox.DirMode); err != nil {
		return built{}, code.EngineWriteResult{}, true, fmt.Errorf("host: the write's staging in %s: %w", sc.Dir,
			err)
	}
	b := &building{wr: wr, sc: sc, stage: stage}
	var (
		commit string
		res    code.EngineWriteResult
		done   bool
		err    error
	)
	if wr.w.Kind == code.WriteCommitFiles {
		commit, res, done, err = b.commitFiles(ctx, j)
	} else {
		commit, res, done, err = b.pick(ctx)
	}
	if done || err != nil {
		return built{}, res, true, err
	}
	if res, done, err := b.pack(ctx); done || err != nil {
		return built{}, res, true, err
	}
	return built{commit: commit}, code.EngineWriteResult{}, false, nil
}

// create makes the build's next staged file, of kind.
func (b *building) create(kind string) (*os.File, error) {
	b.files++
	return os.OpenFile(filepath.Join(b.stage, kind+"-"+strconv.Itoa(b.files)), os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		hostbox.FileMode)
}

// commitFiles is a commit_files' build: the change planned on the parent's
// paths, its edits and touched directories read and staged, its new files
// written and hashed, its directories written a level at a time, and its
// commit.
func (b *building) commitFiles(ctx context.Context, j judgement) (string, code.EngineWriteResult, bool, error) {
	wr := b.wr
	p, refusal, detail := planFiles(wr.w.Files, j)
	if refusal != "" {
		return "", wr.changeRefused(refusal, detail), true, nil
	}
	parent, refusal, detail, err := b.contents(ctx, j, p)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case refusal != "":
		return "", wr.changeRefused(refusal, detail), true, nil
	}
	if err := b.stagePuts(p); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if res, done, err := b.blobLimit(p); done || err != nil {
		return "", res, true, err
	}
	if err := b.hash(ctx, p); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	root, err := b.trees(ctx, p)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if j.parent != "" && root == parent.tree {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("the change leaves %s's tree as it "+
			"is: no commit is made", shortID(j.parent))), true, nil
	}
	message := wr.w.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commit, err := b.commit(ctx, root, j.parent, parent.committed, message)
	return commit, code.EngineWriteResult{}, false, err
}

// contents is the build's ONE read of the copy's contents (`cat-file --batch
// -Z`, in the incarnation): the parent commit — its tree and its committer's
// date, answered — every touched directory the parent holds, staged as git
// stores it, and every file an edit rewrites, streamed through its edit into
// a staged file. An edit's refusal is answered by name.
func (b *building) contents(ctx context.Context, j judgement, p *plan) (*commitHeader, code.WriteRefusal, string,
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
			e.stream = newEditStream(e.old, e.new, f)
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
	if err := b.wr.failed(ctx, "read of the files and directories the change touches", res, err); err != nil {
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

// blobLimit refuses a change one of whose new files is past
// code.limits.max_blob_bytes, before anything is hashed: the landing would
// refuse it after its listing, and this spares the build. A refusal of the
// push, blob_too_large, as a push's own is.
func (b *building) blobLimit(p *plan) (code.EngineWriteResult, bool, error) {
	limit := b.wr.s.limits().MaxBlobBytes
	for _, nb := range p.newBlobs() {
		info, err := os.Stat(nb.staged)
		if err != nil {
			return code.EngineWriteResult{}, true, err
		}
		if info.Size() > limit {
			return b.wr.refused(code.RefusalBlobTooLarge, fmt.Sprintf("%s would be %s, past "+
				"code.limits.max_blob_bytes (%s)", nb.path, humanBytes(info.Size()), humanBytes(limit))), true, nil
		}
	}
	return code.EngineWriteResult{}, false, nil
}

// hash is ONE `hash-object -w --no-filters --stdin-paths` over every new file
// the build staged, each set to the id git answers, in order.
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
	if err := b.wr.failed(ctx, "hash of the change's new files", res, err); err != nil {
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

// trees writes every touched directory, deepest first, one level at a time,
// and answers the root's new id: a directory left empty is dropped from its
// parent, and an empty root is the empty tree, written like any.
func (b *building) trees(ctx context.Context, p *plan) (string, error) {
	ids := map[string]string{} // a directory's new id, "" where it is left empty
	order := p.order()
	for depth := p.depth; depth >= 0; depth-- {
		var level []string
		for _, d := range order {
			if dirDepth(d) == depth {
				level = append(level, d)
			}
		}
		if err := b.level(ctx, p, level, ids); err != nil {
			return "", err
		}
	}
	return ids[""], nil
}

// level is ONE `mktree -z --batch` for the directories of one depth: each
// one's entries — its parent's as git reads them, every one the change does
// not touch passed through, then what the change sets — written into a staged
// file a directory at a time, each ended by the empty record that ends a tree
// in a batch, and the file handed to mktree, which answers each tree's id on
// its own line. A directory left empty is no record at all, so mktree never
// writes it — but the root, whose empty record writes the empty tree.
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
	if err := b.wr.failed(ctx, "write of the change's directories", res, err); err != nil {
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

// writeDir writes one directory's entries as mktree reads them, and how many:
// none for a directory the change leaves empty.
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

// commit is ONE `commit-tree` of tree onto parent — "" for a root commit — its
// message on its standard input as it is given, its identity the write's
// signature and its dates the write's (commitDates).
func (b *building) commit(ctx context.Context, tree, parent string, parentDate time.Time, message string) (string,
	error) {

	wr := b.wr
	author, committer := commitDates(wr.w.Instant(), parentDate)
	id := &gitproc.Identity{
		AuthorName: wr.sig.AuthorName, AuthorEmail: wr.sig.AuthorEmail,
		CommitterName: wr.sig.CommitterName, CommitterEmail: wr.sig.CommitterEmail,
		AuthorWhen: author, CommitterWhen: committer,
	}
	left, err := wr.budget()
	if err != nil {
		return "", err
	}
	var res gitproc.Result
	if parent == "" {
		res, err = wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"commit-tree", tree}, Identity: id,
			Stdin: strings.NewReader(message), Class: gitproc.ClassRead, Deadline: left,
		})
	} else {
		res, err = wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"commit-tree", tree, "-p", parent}, Identity: id,
			Stdin: strings.NewReader(message), Class: gitproc.ClassRead, Deadline: left,
		})
	}
	if err := wr.failed(ctx, "commit of the change", res, err); err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(res.Stdout))
	if !gitref.ValidOID(commit) {
		return "", fmt.Errorf("host: commit-tree answered %q, which is no commit id", commit)
	}
	return commit, nil
}

// commitDates are an engine-built commit's two dates (M3-decisions U18, the
// user's answer (c); ADR-0062): its AUTHOR date the instant the principal
// acted — the operation's own, to the second — and its COMMITTER date that
// instant or its parent's committer date, whichever is later. A parent dated
// after the operation — a person's clock ahead — would otherwise give a child
// dated before it, which every walk by date pays for (M2's O18 measured 6.9 s
// a push over a gigabyte of history with no commit-graph), while git's
// date-ordered walks, merge-base and the commit-graph's corrected dates read
// the committer date alone — as git's own rebase and cherry-pick keep the
// author date and set the committer date of their own. Both are pure
// functions of the operation and the parent, so a retry builds a
// byte-identical commit, and one operation's writes share one author date.
func commitDates(instant, parentDate time.Time) (author, committer time.Time) {
	author = instant.UTC().Truncate(time.Second)
	committer = author
	if parentDate.After(committer) {
		committer = parentDate.UTC().Truncate(time.Second)
	}
	return author, committer
}

// pick is a revert's or a cherry-pick's build: its two commits read, the
// subject held to having one parent, ONE three-way merge onto the head, and
// its commit — or the merge's conflicts, answered with nothing stored.
func (b *building) pick(ctx context.Context) (string, code.EngineWriteResult, bool, error) {
	wr := b.wr
	w := wr.w
	head := &commitHeader{}
	subject := &commitHeader{keep: code.MaxCommitPayloadBytes}
	if err := b.readCommits(ctx, []string{w.ExpectedHead, w.Commit}, []*commitHeader{head, subject}); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if len(subject.parents) > 1 {
		return "", wr.changeRefused(code.WriteMergeCommit, fmt.Sprintf("%s merges %d commits: reverting or picking "+
			"a merge needs a mainline; do it in a coding box (run_sandbox)", shortID(w.Commit),
			len(subject.parents))), true, nil
	}
	message := w.Message
	if message == "" {
		var (
			res  code.EngineWriteResult
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
	if w.Kind == code.WriteRevert {
		base, other = w.Commit, parent
	}
	m, err := b.merge(ctx, base, w.ExpectedHead, other)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if !m.clean {
		res, err := b.conflicted(ctx, m)
		return "", res, true, err
	}
	if m.tree == head.tree {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s changes nothing on %s: no commit "+
			"is made", shortID(w.Commit), shortID(w.ExpectedHead))), true, nil
	}
	commit, err := b.commit(ctx, m.tree, w.ExpectedHead, head.committed, message)
	return commit, code.EngineWriteResult{}, false, err
}

// defaultMessage is a revert's or a cherry-pick's message where the caller
// gave none: git's own (message.go), as git writes it into a commit
// (gitref.WrittenCommitText). A subject written in an encoding other than
// UTF-8 — its `encoding` header, which git converts through iconv — is
// code.ErrInvalid asking for a message; a message past what one write
// carries is too_large.
func (wr *writing) defaultMessage(subject *commitHeader) (string, code.EngineWriteResult, bool, error) {
	w := wr.w
	if enc := subject.encoding; enc != "" && !utf8Encoding(enc) {
		return "", code.EngineWriteResult{}, true, fmt.Errorf("%w: message: %s's message is written in %q, "+
			"which this engine does not convert to UTF-8: give the write its message", code.ErrInvalid,
			shortID(w.Commit), enc)
	}
	text := string(subject.message)
	cut := subject.cut
	if i := strings.IndexByte(text, 0); i >= 0 {
		text, cut = text[:i], false
	}
	if cut && (w.Kind == code.WriteCherryPick || !subjectEnds(text)) {
		return "", wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("%s's message is past the %s one engine write "+
			"carries (code.MaxCommitPayloadBytes): give the write its message, or %s it in a coding box "+
			"(run_sandbox)", shortID(w.Commit), humanBytes(code.MaxCommitPayloadBytes),
			strings.ReplaceAll(string(w.Kind), "_", "-"))), true, nil
	}
	msg := pickMessage(w.Commit, text)
	if w.Kind == code.WriteRevert {
		msg = revertMessage(w.Commit, text)
	}
	return gitref.WrittenCommitText(msg), code.EngineWriteResult{}, false, nil
}

// subjectEnds reports a message whose subject line ends within it: what a
// revert, which reads the subject alone, needs of a message cut short.
func subjectEnds(text string) bool {
	_, rest := commitSubject(text)
	return strings.IndexByte(rest, '\n') >= 0
}

// utf8Encoding is git's is_encoding_utf8: "utf-8" or "utf8" in any case.
func utf8Encoding(enc string) bool {
	return strings.EqualFold(enc, "utf-8") || strings.EqualFold(enc, "utf8")
}

// readCommits is ONE `cat-file --batch -Z` of commits in the copy, each
// streamed into its header.
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
		Dir: b.wr.synced.Dir, Args: []string{"cat-file", "--batch", "-Z"}, Config: b.wr.s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: reader, Class: gitproc.ClassRead, Deadline: left,
	})
	if err == nil {
		err = reader.Close()
	}
	return b.wr.failed(ctx, "read of the commits", res, err)
}

// commitHeader is what a build reads of a commit object as it streams: its
// tree, its parents, its committer's date and its encoding, and its message —
// kept up to keep bytes, cut marking one longer.
type commitHeader struct {
	tree      string
	parents   []string
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

// identDate is the instant an identity line states — `<who> <<email>>
// <seconds> <zone>`, the seconds since the epoch whatever the zone — or the
// zero time where it states none git would read.
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

// maxConflictsHeld is how many conflicted paths a build holds as merge-tree
// names them: as many as could fit one answer — each at least a path and a
// kind, some two dozen bytes encoded — so a merge of a hundred thousand
// conflicts holds what it could answer and counts the rest.
const maxConflictsHeld = code.MaxGitAnswerBytes / 24

// mergeOutput is what `merge-tree --write-tree -z` answered: the merged tree
// and, for a merge that conflicts, each conflicted path with its stages and
// merge-tree's word for it, read as its records stream.
type mergeOutput struct {
	tree      string
	clean     bool
	conflicts []code.Conflict
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

// record reads one record: the tree, a stage record `<mode> <id> <stage>
// TAB <path>` — consecutive for one path — up to an empty record, then each
// message's `<count>`, its paths, its type and its words.
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
				m.conflicts = append(m.conflicts, code.Conflict{Path: p})
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

// merge is ONE `merge-tree --write-tree -z --merge-base=<base> <ours>
// <theirs>` in the scratch, which writes the merged tree — and, for a merge
// that conflicts, its files with their conflict markers — there and nowhere
// else: exit 0 is a clean merge, 1 one that conflicts.
func (b *building) merge(ctx context.Context, base, ours, theirs string) (*mergeOutput, error) {
	m := &mergeOutput{at: map[string]int{}, firstType: map[string]string{}}
	recs := newRecords(m.record)
	left, err := b.wr.budget()
	if err != nil {
		return nil, err
	}
	res, err := b.wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate,
		Args:   []string{"merge-tree", "--write-tree", "-z", "--merge-base=" + base, ours, theirs},
		Stdout: recs, Class: gitproc.ClassRead, Deadline: left, Answers: []int{1},
	})
	if err == nil {
		err = recs.close()
	}
	if err := b.wr.failed(ctx, "merge of the change", res, err); err != nil {
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

// conflicted is a merge's conflicts as the write answers them: as many as fit
// code.MaxGitAnswerBytes encoded, each with the start of its file as
// merge-tree wrote it — read in ONE `cat-file --batch -Z` of the scratch,
// which alone holds that tree — and the rest counted. Nothing is stored.
func (b *building) conflicted(ctx context.Context, m *mergeOutput) (code.EngineWriteResult, error) {
	wr := b.wr
	res := code.EngineWriteResult{Refusal: code.WriteConflict, PushID: wr.pushID, Detail: fmt.Sprintf("%s does not "+
		"apply cleanly on %s: %d paths conflict; resolve them in a coding box (run_sandbox), or write the "+
		"result yourself", shortID(wr.w.Commit), shortID(wr.w.ExpectedHead), len(m.conflicts)+m.omitted)}
	held := m.conflicts[:fitConflicts(res, m.conflicts, m.omitted)]
	marked := make([]prefixWriter, len(held))
	var (
		names []string
		which []int
	)
	for i, c := range held {
		marked[i].n = code.ConflictMarkedBytes
		// A path git would read as relative to a working tree it does not
		// have (object-name.c's resolve_relative_path) is no name cat-file
		// takes — and no path a repository the relay's fsck admitted holds.
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
			return code.EngineWriteResult{}, err
		}
		run, err := wr.s.runGit(ctx, gitproc.Cmd{
			Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"cat-file", "--batch", "-Z"}, Stdin: &stdin,
			Stdout: reader, Class: gitproc.ClassRead, Deadline: left,
		})
		if err == nil {
			err = reader.Close()
		}
		if err := wr.failed(ctx, "read of the conflicted files", run, err); err != nil {
			return code.EngineWriteResult{}, err
		}
	}
	answered := make([]code.Conflict, len(held))
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
// code.MaxGitAnswerBytes encoded, the ones left out counted beside omitted.
func fitConflicts(res code.EngineWriteResult, conflicts []code.Conflict, omitted int) int {
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
		if size > code.MaxGitAnswerBytes {
			return k
		}
	}
	return len(conflicts)
}

// pack is ONE `pack-objects` of every object the build wrote — the scratch's
// loose objects, exactly what was new — into a staged file, and ONE
// `index-pack --strict --stdin` of it into the scratch, writing its .idx
// (version 2) and .rev beside it: the relay's own check of every object a
// push stores, so an engine write stores nothing a push of the same objects is
// refused at the door. A pack past code.MaxWritePackBytes is too_large; an
// object the check refuses is rejected_object, in git's own words.
func (b *building) pack(ctx context.Context) (code.EngineWriteResult, bool, error) {
	wr := b.wr
	ids, err := looseObjects(filepath.Join(b.sc.Dir, "objects"))
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	if len(ids) == 0 {
		return code.EngineWriteResult{}, false, nil
	}
	out, err := b.create("pack")
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	packPath := out.Name()
	var stdin bytes.Buffer
	for _, id := range ids {
		stdin.WriteString(id)
		stdin.WriteByte('\n')
	}
	left, err := wr.budget()
	if err != nil {
		_ = out.Close()
		return code.EngineWriteResult{}, true, err
	}
	// AN OBJECT LIST, never revs and never --thin, so no delta leans on an
	// object outside the pack; one thread and no reuse, so two nodes building
	// one write pack it alike.
	res, err := wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate,
		Args:  []string{"pack-objects", "--threads=1", "--no-reuse-object", "-q", "--stdout"},
		Stdin: &stdin, Stdout: out, Class: gitproc.ClassRead, Deadline: left,
	})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err := wr.failed(ctx, "pack of the change's objects", res, err); err != nil {
		return code.EngineWriteResult{}, true, err
	}
	info, err := os.Stat(packPath)
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	if info.Size() > code.MaxWritePackBytes {
		return wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("this change's pack is %s, past the %s one engine "+
			"write may store (code.MaxWritePackBytes); make it in a coding box (run_sandbox)",
			humanBytes(info.Size()), humanBytes(code.MaxWritePackBytes))), true, nil
	}
	in, err := os.Open(packPath)
	if err != nil {
		return code.EngineWriteResult{}, true, err
	}
	defer func() { _ = in.Close() }()
	if left, err = wr.budget(); err != nil {
		return code.EngineWriteResult{}, true, err
	}
	res, err = wr.s.runGit(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Alternate: b.sc.Alternate, Args: []string{"index-pack", "--strict", "--stdin"},
		Config: []gitproc.KV{{Key: "pack.indexVersion", Value: "2"}, {Key: "pack.writeReverseIndex", Value: "true"}},
		Stdin:  in, Class: gitproc.ClassRead, Deadline: left,
	})
	if err != nil && res.Damage == nil && refusedByFsck(res) {
		wr.charge(res)
		return wr.changeRefused(code.WriteRejectedObject, fsckWords(res)), true, nil
	}
	if err := wr.failed(ctx, "check of the change's objects", res, err); err != nil {
		return code.EngineWriteResult{}, true, err
	}
	return code.EngineWriteResult{}, false, nil
}

// refusedByFsck reports an index-pack the relay's own check stopped: git's
// two formats for it (index-pack.c), read from the run's trace — never from
// its words, which carry the objects' own text.
func refusedByFsck(res gitproc.Result) bool {
	for _, f := range res.Trace.Formats {
		if f == "fsck error in packed object" || f == "fsck error in pack objects" {
			return true
		}
	}
	return false
}

// fsckWords is what git said of the first object its check refused — `<id>:
// <message id>: <message>` — as the refusal's sentence: the line index-pack
// wrote for it, its "error: object " cut.
func fsckWords(res gitproc.Result) string {
	for line := range strings.Lines(res.Stderr) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "error: object "); ok {
			return "an object this change makes is one git's own check refuses: " + rest
		}
	}
	return "an object this change makes is one git's own check refuses: " + res.LastLine()
}

// looseObjects is every loose object under objects — the scratch's own, which
// the build wrote — sorted.
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
