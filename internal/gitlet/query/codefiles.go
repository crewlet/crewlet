package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// A COMMIT'S DEPLOY AND DEPENDENCY FILES are the files of its tree the closed
// list admits (internal/deployfiles), read for the atlas by the engine alone.
// A LISTING is one `ls-tree -r -z -l` of the commit, judged record by record
// HERE: only a regular file the list admits is kept, so a symbolic link, a
// submodule and every file off the list never leave this node, whatever the
// tree holds. ONE FILE is read as a checks file is, from the batch's lookup of
// its path, and only a path the list admits as a file that is read — the
// query's own validation refuses any other before git runs.

// codeFilesRead answers a code files read: the commit's listing, or the one
// file the query names.
func (r *read) codeFilesRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	q := r.q.CodeFiles
	rev := objs.revs[q.Commit]
	if rev.peeledKind != "commit" {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: code_files.commit: %s names a %s; the files are read at a "+
			"commit", gitlet.ErrInvalid, q.Commit, rev.peeledKind)
	}
	ans := &gitlet.CodeFilesAnswer{Commit: rev.peeled}
	a := gitlet.GitAnswer{Kind: gitlet.GitCodeFiles, Rev: rev.peeled, CodeFiles: ans}
	if q.File == "" {
		return a, r.codeFilesList(ctx, rev.peeled, ans)
	}
	// THE LIST IS ASKED AGAIN HERE, beside the query's validation: this is
	// the node that reads the bytes, and it reads none the list refuses.
	if f, ok := deployfiles.Classify(q.File); !ok || !f.Parsed() {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: code_files.file: not a deploy or dependency file the engine "+
			"reads", gitlet.ErrInvalid)
	}
	ans.File = &gitlet.CodeFile{Path: q.File}
	target := objs.target
	// A LINK IS REFUSED BY THE LISTING, which reads modes; the lookup here
	// reads none, and a link read by its path answers the path it points
	// at, never the file there.
	if target.kind != "blob" {
		return a, nil
	}
	ans.File.OID, ans.File.Size = target.id, target.size
	if target.size == 0 || target.size > deployfiles.MaxFileBytes {
		return a, nil
	}
	deadline, err := r.budget()
	if err != nil {
		return gitlet.GitAnswer{}, err
	}
	out := &cappedBuffer{max: int(target.size)}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args: []string{"cat-file", "blob", target.id}, Config: r.s.o.Cache.ServeConfig(),
		Stdout: out, Class: gitproc.ClassRead, Deadline: deadline,
	})
	narrow := fmt.Errorf("%w: reading %s did not finish in the read's %s of git", gitlet.ErrSliceTooLarge,
		target.id, r.s.gitBudget)
	if err = r.failed(ctx, "read of a deploy or dependency file", res, err, narrow); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if out.buf.Len() != int(target.size) {
		return gitlet.GitAnswer{}, fmt.Errorf("query: cat-file read %d bytes of %s, which the lookup sized at %d",
			out.buf.Len(), target.id, target.size)
	}
	ans.Raw = out.buf.Bytes()
	return a, nil
}

// modeSymlink is git's mode of a symbolic link, whose blob is a path and
// never a manifest.
const modeSymlink = "120000"

// codeFilesList fills ans with the listed files of commit's tree: the first
// deployfiles.MaxFiles by path, and how many more it holds.
func (r *read) codeFilesList(ctx context.Context, commit string, ans *gitlet.CodeFilesAnswer) error {
	var files []gitlet.CodeFile
	recs := newRecords(0, gitproc.MaxLine+gitlet.MaxGitPathBytes, func(rec []byte, long bool) error {
		if long {
			return nil // a path past a record's bound is past the list's own
		}
		e, err := parseEntry(rec, "")
		if err != nil {
			return err
		}
		if e.Type != "blob" || e.Mode == modeSymlink || e.PathB64 != "" {
			return nil
		}
		if _, ok := deployfiles.Classify(e.Path); ok {
			files = append(files, gitlet.CodeFile{Path: e.Path, OID: e.OID, Size: e.Size})
		}
		return nil
	})
	deadline, err := r.budget()
	if err != nil {
		return err
	}
	res, err := r.s.o.Git.Run(ctx, gitproc.Cmd{
		Dir: r.synced.Dir, Dropping: r.synced.Dropping,
		Args:   []string{"ls-tree", "-r", "-z", "-l", commit},
		Config: r.s.o.Cache.ServeConfig(), Stdout: recs, Class: gitproc.ClassRead, Deadline: deadline,
	})
	if err == nil {
		err = recs.close()
	}
	narrow := fmt.Errorf("%w: listing the files of %s did not finish in the read's %s of git",
		gitlet.ErrSliceTooLarge, commit, r.s.gitBudget)
	if err = r.failed(ctx, "listing of a commit's deploy and dependency files", res, err, narrow); err != nil {
		return err
	}
	// BY PATH, so the files past the bound are the same ones on every node
	// and at every read of the commit.
	slices.SortFunc(files, func(a, b gitlet.CodeFile) int { return cmp.Compare(a.Path, b.Path) })
	if len(files) > deployfiles.MaxFiles {
		ans.More, files = len(files)-deployfiles.MaxFiles, files[:deployfiles.MaxFiles]
	}
	ans.Files = files
	return nil
}
