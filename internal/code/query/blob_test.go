package query_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// A FILE IS READ BY LINE RANGE, A PAGE AT A TIME, AND NEVER CUT. Lines 10 to
// 20 of a 50-line file are exactly those lines; read page by page at a 1 KiB
// knob, every file's pages concatenate to the file byte for byte — a line
// longer than a page served in Continues pieces at rune boundaries, each
// resuming at the byte the last stopped, through text that costs six JSON
// bytes a character (`<`, a control character, U+2028, a byte that is not
// UTF-8) and text of two- and three-byte runes — each page within its bound
// and each naming the file's whole line count. A file with no last newline
// counts its last line, an empty file has none, a binary file is answered by
// its size with no text, and a blob is read by its own id as well as by a
// path. A directory named where a file was guessed is answered its listing.
func TestAFileIsReadByLineRange(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	var fifty strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&fifty, "line %d\n", i)
	}
	files := map[string]string{
		"fifty.txt":   fifty.String(),
		"long.txt":    "first\n" + strings.Repeat("abcdefghij", 300) + "\nafter\n",
		"escapes.txt": strings.Repeat("<&>\x01 \"\\", 200) + "\n" + strings.Repeat("é€", 600) + "\nend",
		"invalid.txt": "ok\n" + strings.Repeat("caf\xe9 ", 400) + "\n",
		"noeol.txt":   "a\nb\nc",
		"empty.txt":   "",
		"bin.dat":     "head\x00tail",
		"dir/f.txt":   "in a directory\n",
	}
	head := x.commit(files, "files")
	repo := x.repo(codetest.RepoName)
	s := x.server(0, query.Options{})

	t.Run("a range of lines", func(t *testing.T) {
		a := mustRead(t, s, blobQ(repo, code.BlobQuery{Rev: head, Path: "fifty.txt", StartLine: 10, EndLine: 20}))
		var want strings.Builder
		for i := 10; i <= 20; i++ {
			fmt.Fprintf(&want, "line %d\n", i)
		}
		b := a.Blob
		if b.Text != want.String() || b.StartLine != 10 || b.EndLine != 20 || b.Lines != 50 || a.Next != "" {
			t.Errorf("lines 10-20 = %+v (next %q), want those eleven lines of 50", b, a.Next)
		}
	})

	// Every blob read of head names the one commit: one decoration for all.
	limit := 1<<10 + x.decoration(blobQ(repo, code.BlobQuery{Rev: head, Path: "bin.dat"}))
	paged := x.server(0, query.Options{})
	query.SetPageBytes(paged, limit)
	for name, content := range files {
		if name == "bin.dat" {
			continue
		}
		t.Run("page by page: "+name, func(t *testing.T) {
			ps := pages(t, paged, blobQ(repo, code.BlobQuery{Rev: head, Path: name}), limit)
			if got := joined(ps); got != content {
				t.Errorf("the pages of %s concatenate to %d bytes, want its %d byte for byte", name, len(got),
					len(content))
			}
			lines := strings.Count(content, "\n")
			if content != "" && !strings.HasSuffix(content, "\n") {
				lines++
			}
			for i, p := range ps {
				if p.Blob.Lines != lines {
					t.Errorf("page %d says %d lines, want the file's %d", i, p.Blob.Lines, lines)
				}
			}
		})
	}

	t.Run("a long line in pieces", func(t *testing.T) {
		ps := pages(t, paged, blobQ(repo, code.BlobQuery{Rev: head, Path: "long.txt", StartLine: 2, EndLine: 2}),
			limit)
		if len(ps) < 3 {
			t.Fatalf("a 3 000-byte line at a 1 KiB page came in %d pages, want pieces", len(ps))
		}
		at := 0
		for i, p := range ps {
			b := p.Blob
			last := i == len(ps)-1
			if b.StartLine != 2 || b.EndLine != 2 || b.StartByte != at || b.Continues == last {
				t.Errorf("piece %d = start %d byte %d end %d continues %v, want line 2 from byte %d, continuing "+
					"but for the last", i, b.StartLine, b.StartByte, b.EndLine, b.Continues, at)
			}
			at += len(b.Text)
		}
	})

	t.Run("a blob by its own id", func(t *testing.T) {
		blob := strings.TrimSpace(x.git("rev-parse", head+":noeol.txt"))
		a := mustRead(t, s, blobQ(repo, code.BlobQuery{Rev: blob[:10]}))
		if a.Blob.OID != blob || a.Blob.Text != "a\nb\nc" || a.Blob.Lines != 3 || a.Blob.Path != "" {
			t.Errorf("blob %s read by its id = %+v", blob, a.Blob)
		}
	})

	t.Run("a binary file", func(t *testing.T) {
		a := mustRead(t, s, blobQ(repo, code.BlobQuery{Rev: head, Path: "bin.dat"}))
		if !a.Blob.Binary || a.Blob.Size != int64(len(files["bin.dat"])) || a.Blob.Text != "" {
			t.Errorf("a binary file = %+v, want its size and no text", a.Blob)
		}
	})

	t.Run("a directory", func(t *testing.T) {
		a := mustRead(t, s, blobQ(repo, code.BlobQuery{Rev: head, Path: "dir"}))
		if a.Kind != code.GitTree || a.Tree == nil || len(a.Tree.Entries) != 1 || a.Tree.Entries[0].Path != "dir/f.txt" {
			t.Errorf("a file read of a directory = %+v, want its listing", a)
		}
	})

	t.Run("past the end", func(t *testing.T) {
		_, err := s.Read(t.Context(), blobQ(repo, code.BlobQuery{Rev: head, Path: "noeol.txt", StartLine: 4}))
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "3 lines") {
			t.Errorf("line 4 of a 3-line file = %v, want ErrInvalid naming its lines", err)
		}
		_, err = s.Read(t.Context(), blobQ(repo, code.BlobQuery{Rev: head, Path: "no/such/file"}))
		if !errors.Is(err, code.ErrNotABlob) {
			t.Errorf("a path naming nothing = %v, want ErrNotABlob", err)
		}
	})
}
