package host_test

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // git names a blob by the SHA-1 of its object, which a case spells
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
)

// THE BUILD'S CASES: what an engine write's commit holds, measured against
// git's own commands where git has one.

// pick is a revert or a cherry-pick of commit by who onto ref at head.
func pick(kind code.EngineWriteKind, repo string, who code.Principal, ref, head, commit string) code.EngineWrite {
	return code.EngineWrite{Kind: kind, RepoID: repo, Principal: who, OpID: opNow(), Fresh: linear, Ref: ref,
		ExpectedHead: head, Commit: commit}
}

// branch is alice's create of ref at commit.
func branch(repo, ref, commit string) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteCreateBranch, RepoID: repo, Principal: alice(), OpID: opNow(),
		Fresh: linear, Ref: ref, Commit: commit}
}

// messageAt is a commit's message as git stores it, in dir.
func messageAt(t *testing.T, c *gittest.Client, dir, rev string) string {
	t.Helper()
	_, msg, _ := strings.Cut(c.Run(t, dir, "cat-file", "commit", rev), "\n\n")
	return msg
}

// blobOf is git's id of a blob holding text.
func blobOf(text string) string {
	h := sha1.New() //nolint:gosec // see the import
	_, _ = fmt.Fprintf(h, "blob %d\x00%s", len(text), text)
	return hex.EncodeToString(h.Sum(nil))
}

// rawCommit is a commit on parent of parent's tree with files added, carrying
// header lines extra and message exactly as given — hash-object writes a
// commit's bytes with no rewrite, where commit-tree would rewrite a message
// that is no UTF-8 — under a branch pushed to the repository as name.
func rawCommit(t *testing.T, c *writeCase, name, parent, extra, message string, files map[string]string) string {
	t.Helper()
	git := c.p.Git()
	git.Run(t, c.work, "read-tree", parent)
	for path, content := range files {
		blob, stderr, err := git.Try(t, c.work, strings.NewReader(content), "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatalf("hash-object: %v\n%s", err, stderr)
		}
		git.Run(t, c.work, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+","+path)
	}
	tree := strings.TrimSpace(git.Run(t, c.work, "write-tree"))
	raw := fmt.Sprintf("tree %s\nparent %s\nauthor A U Thor <author@example.com> 1700000000 +0000\n"+
		"committer C O Mitter <committer@example.com> 1700000000 +0000\n%s\n%s", tree, parent, extra, message)
	out, stderr, err := git.Try(t, c.work, strings.NewReader(raw), "hash-object", "-t", "commit", "-w",
		"--literally", "--stdin")
	if err != nil {
		t.Fatalf("hash-object of a commit: %v\n%s", err, stderr)
	}
	commit := strings.TrimSpace(out)
	git.Run(t, c.work, "branch", "-f", name, commit)
	c.MustPush(c.p, c.work, name)
	git.Run(t, c.work, "read-tree", "HEAD")
	return commit
}

// A REVERT IS GIT'S REVERT: its tree and its message are what `git revert
// --no-edit` makes of the same commit at the same head — a root commit's
// among them, and a revert of a revert, which git calls a reapply — while a
// merge commit is refused, merge_commit, since reverting one needs a mainline;
// and a commit written in an encoding other than UTF-8 gives no message of
// git's this engine can write, so its revert is refused asking for one, and
// lands with one given.
func TestARevertIsGitsRevert(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"root.txt": "root\n"})
	git := c.p.Git()
	root := c.head
	one := c.p.Commit(c.work, map[string]string{"a.txt": "one\n"}, "add a")
	two := c.p.Commit(c.work, map[string]string{"b.txt": "two\n"}, "Revert \"something older\"")
	git.Run(t, c.work, "checkout", "-q", "-b", "side", one)
	c.p.Commit(c.work, map[string]string{"c.txt": "side\n"}, "a side change")
	git.Run(t, c.work, "checkout", "-q", "main")
	git.Run(t, c.work, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	head := strings.TrimSpace(git.Run(t, c.work, "rev-parse", "HEAD"))
	merge := head
	c.MustPush(c.p, c.work, "main")
	encoded := rawCommit(t, c, "encoded", head, "encoding ISO-8859-1\n", "caf\xe9\n",
		map[string]string{"l.txt": "l\n"})
	for i, commit := range []string{one, two, root} {
		ref := fmt.Sprintf("refs/heads/seats/alice/revert-%d", i)
		c.land(branch(c.repo, ref, head))
		res := c.land(pick(code.WriteRevert, c.repo, alice(), ref, head, commit))
		git.Run(t, c.work, "checkout", "-q", "-f", "-B", fmt.Sprintf("oracle-%d", i), head)
		if _, stderr, err := git.Try(t, c.work, nil, "revert", "--no-edit", commit); err != nil {
			t.Fatalf("git revert %s: %v\n%s", commit, err, stderr)
		}
		clone := c.clone()
		if got, want := c.show(clone, "%T", res.Commit), c.show(c.work, "%T", "HEAD"); got != want {
			t.Errorf("the revert of %s holds tree %s, git's %s", commit, got, want)
		}
		if got, want := messageAt(t, git, clone, res.Commit), messageAt(t, git, c.work, "HEAD"); got != want {
			t.Errorf("the revert of %s says %q, git's %q", commit, got, want)
		}
		if got := c.show(clone, "%P", res.Commit); got != head {
			t.Errorf("the revert of %s sits on %s, want the head %s", commit, got, head)
		}
	}

	ref := "refs/heads/seats/alice/merge"
	c.land(branch(c.repo, ref, head))
	res, err := c.write(pick(code.WriteRevert, c.repo, alice(), ref, head, merge))
	if err != nil || res.Refusal != code.WriteMergeCommit {
		t.Errorf("a revert of a merge: %+v %v, want merge_commit", res, err)
	}
	encRef := "refs/heads/seats/alice/encoded"
	c.land(branch(c.repo, encRef, encoded))
	_, err = c.write(pick(code.WriteRevert, c.repo, alice(), encRef, encoded, encoded))
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "message") {
		t.Errorf("a revert of an ISO-8859-1 commit with no message = %v, want ErrInvalid naming message", err)
	}
	given := pick(code.WriteRevert, c.repo, alice(), encRef, encoded, encoded)
	given.Message = "revert the latin-1 commit"
	landed := c.land(given)
	if got := messageAt(t, git, c.clone(), landed.Commit); got != "revert the latin-1 commit\n" {
		t.Errorf("a revert given its message says %q", got)
	}
}

// A CHERRY-PICK IS GIT'S CHERRY-PICK -x: its tree and its message are what
// `git cherry-pick -x` makes of the same commit at the same head — the footer
// joined to a trailer block git recognises and set apart by a blank line from
// anything else, the message cleaned of trailing white space, and a message
// that is no UTF-8 written as git writes it.
func TestACherryPickIsGitsCherryPick(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"base.txt": "base\n"})
	git := c.p.Git()
	base := c.head
	var picks []string
	for i, msg := range []string{
		"plain subject\n\nbody\n",
		"signed\n\nSigned-off-by: a <a@example.com>\n",
		"picked\n\n(cherry picked from commit 1111111111111111111111111111111111111111)\n",
		"spaces   \n\n\n\ntrailing  \n",
		"caf\xe9\n",
	} {
		picks = append(picks, rawCommit(t, c, fmt.Sprintf("pick-%d", i), base, "", msg,
			map[string]string{fmt.Sprintf("p%d.txt", i): "p\n"}))
	}
	head := c.p.Commit(c.work, map[string]string{"main.txt": "main\n"}, "main moves on")
	c.MustPush(c.p, c.work, "main")
	for i, p := range picks {
		ref := fmt.Sprintf("refs/heads/seats/alice/pick-%d", i)
		c.land(branch(c.repo, ref, head))
		res := c.land(pick(code.WriteCherryPick, c.repo, alice(), ref, head, p))
		git.Run(t, c.work, "checkout", "-q", "-f", "-B", fmt.Sprintf("oracle-%d", i), head)
		if _, stderr, err := git.Try(t, c.work, nil, "cherry-pick", "-x", p); err != nil {
			t.Fatalf("git cherry-pick -x %s: %v\n%s", p, err, stderr)
		}
		clone := c.clone()
		if got, want := c.show(clone, "%T", res.Commit), c.show(c.work, "%T", "HEAD"); got != want {
			t.Errorf("the pick of %s holds tree %s, git's %s", p, got, want)
		}
		if got, want := messageAt(t, git, clone, res.Commit), messageAt(t, git, c.work, "HEAD"); got != want {
			t.Errorf("the pick of %s says %q, git's %q", p, got, want)
		}
	}
}

// A CONFLICT ANSWERS ITS STAGES AND STORES NOTHING: a revert that conflicts in
// three hundred and one files answers as many as fit code.MaxGitAnswerBytes
// encoded — each with its three stage blobs, merge-tree's word for it and the
// start of its file as merge-tree wrote it, markers in, cut at
// code.ConflictMarkedBytes — and counts the rest; nothing is uploaded and
// nothing proposed.
func TestAConflictAnswersItsStagesAndStoresNothing(t *testing.T) {
	t.Parallel()
	e := &recording{}
	const conflicted = 300
	version := func(v string) map[string]string {
		out := map[string]string{"a-long.txt": longText(v)}
		for i := range conflicted {
			out[fmt.Sprintf("f%03d.txt", i)] = v + "\n"
		}
		return out
	}
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, version("1"))
	reverted := c.p.Commit(c.work, version("2"), "two")
	head := c.p.Commit(c.work, version("3"), "three")
	c.MustPush(c.p, c.work, "main")
	ref := "refs/heads/seats/alice/conflict"
	c.land(branch(c.repo, ref, head))
	before, proposals := c.objects(), len(e.seen("propose"))
	res, err := c.write(pick(code.WriteRevert, c.repo, alice(), ref, head, reverted))
	if err != nil || res.Refusal != code.WriteConflict || res.Outcome != "" {
		t.Fatalf("a revert that conflicts: %+v %v", res, err)
	}
	if got := len(res.Conflicts) + res.ConflictsOmitted; got != conflicted+1 || len(res.Conflicts) == 0 ||
		res.ConflictsOmitted == 0 {
		t.Errorf("answered %d conflicts and counted %d more, want %d, some of each", len(res.Conflicts),
			res.ConflictsOmitted, conflicted+1)
	}
	if encoded, err := json.Marshal(res); err != nil || len(encoded) > code.MaxGitAnswerBytes {
		t.Errorf("the answer is %d bytes encoded (%v), past %d", len(encoded), err, code.MaxGitAnswerBytes)
	}
	long := res.Conflicts[0]
	if long.Path != "a-long.txt" || !long.Truncated || len(long.Marked) > code.ConflictMarkedBytes ||
		!strings.Contains(long.Marked, "line 0") {
		t.Errorf("the long file's conflict is %q (truncated %t, %d bytes), want its start, cut", long.Path,
			long.Truncated, len(long.Marked))
	}
	for _, cf := range res.Conflicts[1:] {
		if cf.Base != blobOf("2\n") || cf.Ours != blobOf("3\n") || cf.Theirs != blobOf("1\n") ||
			!strings.HasPrefix(cf.Kind, "CONFLICT") || cf.Truncated || !strings.Contains(cf.Marked, "<<<<<<<") {
			t.Errorf("the conflict of %s is %+v, want the reverted blob its base, the head's ours, the "+
				"parent's theirs and its marked text", cf.Path, cf)
		}
	}
	if after := c.objects(); !slices.Equal(after, before) {
		t.Errorf("a revert that conflicted stored %v", after)
	}
	if n := len(e.seen("propose")); n != proposals {
		t.Errorf("a revert that conflicted proposed %d times", n-proposals)
	}
}

// longText is a file of a hundred lines whose middle one says v, so a change
// to it conflicts in a file past code.ConflictMarkedBytes.
func longText(v string) string {
	var b strings.Builder
	for i := range 100 {
		if i == 50 {
			fmt.Fprintf(&b, "line %d says %s\n", i, v)
			continue
		}
		fmt.Fprintf(&b, "line %d is the same on every side of the change, %s\n", i, strings.Repeat("-", 30))
	}
	return b.String()
}

// AN ENGINE WRITE IS HELD TO THE RELAY'S FSCK: the objects it makes are
// checked as receive-pack checks a person's push of them (index-pack --strict,
// receive.fsckObjects) — a .gitmodules whose url git refuses, written as text
// or named by an existing blob, and a .gitattributes past git's line bound are
// rejected_object naming git's own message, which the relay refuses a person's
// push of too; a component git reads as `.git` on NTFS is refused by name
// before anything is read. None stores a byte or proposes anything.
//
// Mutations: index the pack with pack-objects' own index and no index-pack
// --strict — the content rows land, red; drop the `:` rule from
// ValidTreePath — the stream row reaches index-pack and is answered
// rejected_object hasDotgit instead of the named refusal, red.
func TestAnEngineWriteIsHeldToTheRelaysFsck(t *testing.T) {
	t.Parallel()
	e := &recording{}
	modules := "[submodule \"x\"]\n\tpath = x\n\turl = --upload-pack=touch x\n"
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n", "notes.txt": modules})
	before, proposals := c.objects(), len(e.seen("propose"))
	for _, tc := range []struct {
		name string
		op   code.FileOp
		says string
	}{
		{"a .gitmodules written", text(".gitmodules", modules), "gitmodulesUrl"},
		{"a .gitmodules naming a blob", code.FileOp{Op: code.FileOpPut, Path: ".gitmodules",
			BlobOID: blobOf(modules)}, "gitmodulesUrl"},
		{"a .gitattributes past its line bound", text(".gitattributes", strings.Repeat("a", 3000)+" text\n"),
			"gitattributesLineLength"},
	} {
		res, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/fsck", "", c.head, tc.op))
		if err != nil || res.Refusal != code.WriteRejectedObject || !strings.Contains(res.Detail, tc.says) {
			t.Errorf("%s: %+v %v, want rejected_object naming %s", tc.name, res, err, tc.says)
		}
	}
	_, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/fsck", "", c.head,
		text("a/.GIT::$INDEX_ALLOCATION/x", "x\n")))
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "files[0].path") {
		t.Errorf("an NTFS stream of .git = %v, want ErrInvalid naming files[0].path", err)
	}
	if after := c.objects(); !slices.Equal(after, before) {
		t.Errorf("refused writes stored %v", after)
	}
	if n := len(e.seen("propose")); n != proposals {
		t.Errorf("refused writes proposed %d times", n-proposals)
	}
	c.p.Commit(c.work, map[string]string{".gitmodules": modules}, "a person's .gitmodules")
	if res := c.p.Push(c.Ctx, c.work, c.URL(0), "main"); res.Err == nil {
		t.Errorf("the relay took a person's push of the .gitmodules the write was refused: %s", res)
	}
}

// A LINK TAKES ITS TARGET AS TEXT: a put of mode 120000 naming a blob is
// refused naming the field — a blob_oid would carry any blob, unread, as a
// target — and one carrying its target as content lands a link whose blob is
// that text.
//
// Mutation: admit blob_oid for a link — the first write lands, red.
func TestALinkTakesItsTargetAsText(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	_, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/link", "", c.head,
		code.FileOp{Op: code.FileOpPut, Path: "link", BlobOID: blobOf("one\n"), Mode: code.ModeLink}))
	if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "files[0].blob_oid") {
		t.Errorf("a link naming a blob = %v, want ErrInvalid naming files[0].blob_oid", err)
	}
	target := "docs/README.md"
	res := c.land(files(c.repo, alice(), "refs/heads/seats/alice/link", "", c.head,
		code.FileOp{Op: code.FileOpPut, Path: "link", Content: &target, Mode: code.ModeLink}))
	entry := strings.Fields(c.p.Git().Run(t, c.clone(), "ls-tree", res.Commit, "link"))
	if len(entry) != 4 || entry[0] != code.ModeLink || entry[2] != blobOf(target) {
		t.Errorf("the link is %v, want mode %s and the blob of %q", entry, code.ModeLink, target)
	}
}

// hex40 is a whole object id, as a request names one.
var hex40 = regexp.MustCompile(`[0-9a-f]{40}`)

// AN ENGINE COMMIT READS NO WHOLE TREE (ADR-0085's anchor): a commit_files of
// three files at depth three, in a repository whose sibling directories hold
// ten files each and in one whose siblings hold ten thousand, runs the same
// processes — never read-tree, write-tree, update-index, ls-tree or diff-tree
// — handing them the same requests but for the ids: the touched prefixes, the
// touched directories and the edit's file, nothing about a sibling.
//
// Mutation: list the base tree with ls-tree -r — the forbidden process runs,
// and its listing grows with the siblings, red.
func TestAnEngineCommitReadsNoWholeTree(t *testing.T) {
	t.Parallel()
	var runs [][]string
	for _, siblings := range []int{10, 10_000} {
		c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
		git := c.p.Git()
		gittest.FastImport(t, git, c.work, func(w *bufio.Writer) error {
			fmt.Fprintf(w, "blob\nmark :1\ndata 2\nx\n\n")
			fmt.Fprintf(w, "commit refs/heads/main\nmark :2\ncommitter A <a@example.com> 1700000000 +0000\n"+
				"data 4\nwide\nfrom %s\n", c.head)
			for _, p := range []string{"a/b/c/edit.txt", "a/b/c/gone.txt", "a/b/c/keep.txt"} {
				fmt.Fprintf(w, "M 100644 :1 %s\n", p)
			}
			for _, dir := range []string{"a/b/sib", "a/sib", "sib"} {
				for i := range siblings {
					fmt.Fprintf(w, "M 100644 :1 %s/f%05d\n", dir, i)
				}
			}
			_, err := fmt.Fprintf(w, "\n")
			return err
		})
		head := strings.TrimSpace(git.Run(t, c.work, "rev-parse", "refs/heads/main"))
		c.MustPush(c.p, c.work, "main")
		inputs := host.RecordGitInputs(c.server())
		c.land(files(c.repo, alice(), "refs/heads/seats/alice/narrow", "", head,
			text("a/b/c/new.txt", "new\n"),
			code.FileOp{Op: code.FileOpEdit, Path: "a/b/c/edit.txt", Old: "x", New: "y"},
			code.FileOp{Op: code.FileOpDelete, Path: "a/b/c/gone.txt"}))
		var run []string
		for _, in := range inputs() {
			switch in.Args[0] {
			case "read-tree", "write-tree", "update-index", "ls-tree", "diff-tree":
				t.Errorf("the write ran %v, a read or a write of a whole tree", in.Args)
			}
			if !slices.Contains([]string{"for-each-ref", "cat-file", "hash-object", "mktree", "commit-tree",
				"pack-objects", "index-pack", "rev-list", "merge-base"}, in.Args[0]) {
				t.Errorf("the write ran %v, no process a commit's build or its landing runs", in.Args)
			}
			// The scratch's own path, which names the push and the fleet's
			// directory, is no request about the repository.
			stdin := strings.ReplaceAll(string(in.Stdin), in.Dir, "<scratch>")
			if in.Args[0] == "pack-objects" || in.Args[0] == "index-pack" {
				stdin = "" // its objects, as many in either repository
			}
			run = append(run, hex40.ReplaceAllString(strings.Join(in.Args, " ")+"\x00"+stdin, "<id>"))
		}
		runs = append(runs, run)
	}
	if !slices.Equal(runs[0], runs[1]) {
		t.Errorf("a commit beside ten files a sibling and beside ten thousand ran different requests:\n%q\n%q",
			runs[0], runs[1])
	}
}

// AN ENGINE WRITE NEVER WRITES IN A CACHE: every process of a build that
// writes objects — hash-object, mktree, commit-tree, merge-tree, pack-objects,
// index-pack — runs in the write's own scratch, named its push id, with the
// copy as its alternate, for a commit and for a revert alike.
//
// Mutation: run any of them in the copy itself — its directory is the
// incarnation's, red.
func TestAnEngineWriteNeverWritesInACache(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"f.txt": "1\n"})
	reverted := c.p.Commit(c.work, map[string]string{"f.txt": "2\n", "g.txt": "g\n"}, "two")
	head := c.p.Commit(c.work, map[string]string{"h.txt": "h\n"}, "three")
	c.MustPush(c.p, c.work, "main")
	inputs := host.RecordGitInputs(c.server())
	commit := files(c.repo, alice(), "refs/heads/seats/alice/w", "", head, text("w.md", "w\n"))
	c.land(commit)
	ref := "refs/heads/seats/alice/r"
	c.land(branch(c.repo, ref, head))
	revert := pick(code.WriteRevert, c.repo, alice(), ref, head, reverted)
	c.land(revert)
	scratches := []string{code.WritePushID(commit.OpID) + ".git", code.WritePushID(revert.OpID) + ".git"}
	writers := 0
	for _, in := range inputs() {
		if !slices.Contains([]string{"hash-object", "mktree", "commit-tree", "merge-tree", "pack-objects",
			"index-pack"}, in.Args[0]) {
			continue
		}
		writers++
		if !slices.Contains(scratches, filepath.Base(in.Dir)) || filepath.Base(filepath.Dir(in.Dir)) != "scratch" ||
			in.Alternate == "" {
			t.Errorf("%s ran in %s (alternate %q), not in its write's scratch", in.Args[0], in.Dir, in.Alternate)
		}
	}
	if writers < 8 {
		t.Errorf("the two writes ran %d writers, want every step of both builds recorded", writers)
	}
}
