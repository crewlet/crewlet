package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// A CHECKS FILE is what a commit holds at gitlet.ChecksFilePath, read whole: the
// one read a repository's home makes of a target's tip to queue a change's
// checks (internal/gitlet/home). The batch the read's step 7 runs already
// looked the path up, so a commit holding nothing there, or something that is
// no file, or a file the parse refuses by its size alone, is answered from
// that lookup with no git of its own; only a file within the bound is read,
// by one `cat-file blob` into a buffer that refuses a byte past the size the
// lookup reported — the copy's object cannot grow under a read, so a stream
// longer than that is git misbehaving, never a file to keep.

// checksFileRead answers a checks file read: the commit it names, which must
// be one, and what that commit holds at the path.
func (r *read) checksFileRead(ctx context.Context, objs found) (gitlet.GitAnswer, error) {
	rev := objs.revs[r.q.ChecksFile.Commit]
	if rev.peeledKind != "commit" {
		return gitlet.GitAnswer{}, fmt.Errorf("%w: checks_file.commit: %s names a %s; a checks file is read at a "+
			"commit", gitlet.ErrInvalid, r.q.ChecksFile.Commit, rev.peeledKind)
	}
	ans := &gitlet.ChecksFileAnswer{Commit: rev.peeled}
	a := gitlet.GitAnswer{Kind: gitlet.GitChecksFile, Rev: rev.peeled, ChecksFile: ans}
	target := objs.target
	if target.id == "" {
		return a, nil
	}
	ans.OID, ans.Type = target.id, target.kind
	if target.kind != "blob" {
		return a, nil
	}
	ans.Size = target.size
	if target.size == 0 || target.size > gitlet.MaxChecksFileBytes {
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
	if err = r.failed(ctx, "read of a checks file", res, err, narrow); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if out.buf.Len() != int(target.size) {
		return gitlet.GitAnswer{}, fmt.Errorf("query: cat-file read %d bytes of %s, which the lookup sized at %d",
			out.buf.Len(), target.id, target.size)
	}
	ans.Raw = out.buf.Bytes()
	return a, nil
}

// errPastSize is a stream longer than the object it reads was sized at.
var errPastSize = errors.New("query: git wrote more of an object than its size")

// cappedBuffer holds at most max bytes, refusing a write past them — every
// Write bounded, as gitproc's contract asks of a Stdout.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

// Write keeps p, or refuses it whole where it would pass max.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		return 0, errPastSize
	}
	return c.buf.Write(p)
}
