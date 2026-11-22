package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// THE OUTPUT IS A FUNCTION OF THE INPUT ALONE: two runs over the same bytes
// write the same file, its header names no file and no time, and it gunzips
// to the input, so a rebuild on any machine diffs clean against the commit.
func TestTheOutputIsAFunctionOfTheInputAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := filepath.Join(dir, "notehelper")
	data := bytes.Repeat([]byte("\x7fELF a build of the helper "), 4096)
	if err := os.WriteFile(in, data, 0o755); err != nil {
		t.Fatal(err)
	}
	var outs [][]byte
	for _, name := range []string{"one.gz", "two.gz"} {
		out := filepath.Join(dir, name)
		if err := write(in, out); err != nil {
			t.Fatal(err)
		}
		gz, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, gz)
	}
	if !bytes.Equal(outs[0], outs[1]) {
		t.Error("two runs over the same input wrote different files")
	}
	r, err := gzip.NewReader(bytes.NewReader(outs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "" || !r.ModTime.IsZero() || r.OS != 255 {
		t.Errorf("header name %q time %v OS %d, want no name, no time, OS 255", r.Name, r.ModTime, r.OS)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("gunzipped %d bytes (%v), want the input's %d", len(got), err, len(data))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("the directory holds %d files, want the input and two outputs, no temporary left", len(entries))
	}
}
