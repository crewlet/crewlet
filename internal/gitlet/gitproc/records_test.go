package gitproc_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
)

// GIT'S RECORDS ARE READ ONCE, here, from what the pinned git prints: a
// batch-check answer for every type, a held submodule's `commit` and a missing
// one's `submodule`; an ls-tree -l entry for a file, an executable, a link, a
// tree, a submodule, a blob the repository lacks (`BAD`) and a path holding a
// tab and a newline; and NewRecords splits on NUL under NewLines' bounds.
// Malformed records are refused.
//
// Mutation: drop ParseCheck's submodule arm, or read BAD as a size.
func TestGitsRecordsAreReadOnce(t *testing.T) {
	t.Parallel()
	c := gittest.NewClientWith(t, gittest.RequireNode(t))
	work := c.Init(t)
	first := c.Commit(t, work, map[string]string{"f.txt": "one\n", "run.sh": "#!/bin/sh\n", "d/x": "x\n",
		"tab\tand\nnl": "odd\n"}, "first")
	if err := os.Symlink("f.txt", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	c.Run(t, work, "add", "-A")
	c.Run(t, work, "update-index", "--chmod=+x", "run.sh")
	const lost = "1234567890123456789012345678901234567890"
	c.Run(t, work, "update-index", "--add", "--cacheinfo", "160000,"+first+",held")
	c.Run(t, work, "update-index", "--add", "--cacheinfo", "160000,"+lost+",lost")
	c.Run(t, work, "commit", "-q", "-m", "second")
	c.Run(t, work, "tag", "-a", "-m", "v1", "v1")
	rev := func(spec string) string { return strings.TrimSpace(c.Run(t, work, "rev-parse", spec)) }
	head, tree, blob, tag := rev("HEAD"), rev("HEAD^{tree}"), rev("HEAD:f.txt"), rev("v1")

	t.Run("batch-check", func(t *testing.T) {
		in := []string{head, tree, blob, tag, head + ":held", head + ":lost"}
		var stdin bytes.Buffer
		for _, line := range in {
			stdin.WriteString(line + "\x00")
		}
		out, _, err := c.Try(t, work, &stdin, "cat-file", "--batch-check", "-Z")
		if err != nil {
			t.Fatal(err)
		}
		recs := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
		want := []gitproc.Object{{ID: head, Type: "commit"}, {ID: tree, Type: "tree"}, {ID: blob, Type: "blob", Size: 4},
			{ID: tag, Type: "tag"}, {ID: first, Type: "commit"}, {ID: lost, Type: "submodule"}}
		if len(recs) != len(want) {
			t.Fatalf("cat-file answered %q, want %d records", recs, len(want))
		}
		for i, rec := range recs {
			got, ok := gitproc.ParseCheck([]byte(rec))
			if w := want[i]; !ok || got.ID != w.ID || got.Type != w.Type || (w.Size != 0 && got.Size != w.Size) ||
				(w.Type == "submodule" && got.Size != 0) {
				t.Errorf("ParseCheck(%q) = %+v, %v, want %+v", rec, got, ok, w)
			}
		}
	})

	t.Run("ls-tree", func(t *testing.T) {
		var got []gitproc.TreeEntry
		recs := gitproc.NewRecords(func(rec []byte) error {
			e, err := gitproc.ParseTreeEntry(rec)
			if err != nil {
				return err
			}
			e.Path = slices.Clone(e.Path)
			got = append(got, e)
			return nil
		})
		out := c.Run(t, work, "ls-tree", "-r", "-t", "-l", "-z", head)
		if _, err := recs.Write([]byte(out)); err != nil {
			t.Fatal(err)
		}
		if err := recs.Close(); err != nil {
			t.Fatal(err)
		}
		want := map[string]gitproc.TreeEntry{
			"d":            {Mode: "040000", Type: "tree", ID: rev("HEAD:d"), Size: -1},
			"d/x":          {Mode: "100644", Type: "blob", ID: rev("HEAD:d/x"), Size: 2},
			"f.txt":        {Mode: "100644", Type: "blob", ID: blob, Size: 4},
			"held":         {Mode: "160000", Type: "commit", ID: first, Size: -1},
			"link":         {Mode: "120000", Type: "blob", ID: rev("HEAD:link"), Size: 5},
			"lost":         {Mode: "160000", Type: "commit", ID: lost, Size: -1},
			"run.sh":       {Mode: "100755", Type: "blob", ID: rev("HEAD:run.sh"), Size: 10},
			"tab\tand\nnl": {Mode: "100644", Type: "blob", ID: rev("HEAD:tab\tand\nnl"), Size: 4},
		}
		if len(got) != len(want) {
			t.Fatalf("ls-tree listed %d entries, want %d: %+v", len(got), len(want), got)
		}
		for _, e := range got {
			w, ok := want[string(e.Path)]
			w.Path = e.Path
			if !ok || e.Mode != w.Mode || e.Type != w.Type || e.ID != w.ID || e.Size != w.Size {
				t.Errorf("entry %q = %+v, want %+v", e.Path, e, w)
			}
		}
	})

	t.Run("BAD is a missing object", func(t *testing.T) {
		stdin := strings.NewReader("100644 blob " + lost + "\tgone\n")
		tid, _, err := c.Try(t, work, stdin, "mktree", "--missing")
		if err != nil {
			t.Fatal(err)
		}
		rec := strings.TrimSuffix(c.Run(t, work, "ls-tree", "-l", "-z", strings.TrimSpace(tid)), "\x00")
		if !strings.Contains(rec, "BAD") {
			t.Fatalf("the premise: ls-tree -l of a tree naming a lost blob answered %q, no BAD", rec)
		}
		if _, err := gitproc.ParseTreeEntry([]byte(rec)); !errors.Is(err, gitproc.ErrMissingObject) {
			t.Errorf("ParseTreeEntry(%q) = %v, want ErrMissingObject", rec, err)
		}
	})

	t.Run("malformed records are refused", func(t *testing.T) {
		for _, rec := range []string{"xyz blob 4", head + " blob -1", head + " widget 4", head + " submodule 4",
			head, head + " blob"} {
			if got, ok := gitproc.ParseCheck([]byte(rec)); ok {
				t.Errorf("ParseCheck(%q) = %+v, want refused", rec, got)
			}
		}
		for _, rec := range []string{"100644 blob " + blob + " 4 f.txt", "100644 blob " + blob + "\tf.txt",
			"100644 blob " + blob + " x\tf.txt", "100644 blob nothex 4\tf.txt"} {
			if got, err := gitproc.ParseTreeEntry([]byte(rec)); err == nil || errors.Is(err, gitproc.ErrMissingObject) {
				t.Errorf("ParseTreeEntry(%q) = %+v, %v, want refused as no entry", rec, got, err)
			}
		}
	})
}

// RECORDS SPLIT ON NUL under every bound of a line: across writes, a record
// past MaxLine, and a stream ending inside one.
func TestRecordsSplitOnNUL(t *testing.T) {
	t.Parallel()
	var got []string
	r := gitproc.NewRecords(func(rec []byte) error {
		got = append(got, string(rec))
		return nil
	})
	for _, w := range []string{"a\nb", "\x00c", "\x00\x00"} {
		if _, err := r.Write([]byte(w)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil || !slices.Equal(got, []string{"a\nb", "c", ""}) {
		t.Fatalf("records %q, %v; want a newline kept inside a record", got, err)
	}
	long := gitproc.NewRecords(func([]byte) error { return nil })
	if _, err := long.Write(bytes.Repeat([]byte("x"), gitproc.MaxLine+1)); !errors.Is(err, gitproc.ErrLineTooLong) {
		t.Errorf("a record past MaxLine = %v, want ErrLineTooLong", err)
	}
	part := gitproc.NewRecords(func([]byte) error { return nil })
	_, _ = part.Write([]byte("whole\x00part"))
	if err := part.Close(); !errors.Is(err, gitproc.ErrPartialLine) {
		t.Errorf("a stream ending inside a record = %v, want ErrPartialLine", err)
	}
}
