// Command gzipbuild gzips one note helper build for the engine to embed, as
// `make notehelper` and ci.yml's build job run it:
//
//	go run ./internal/notehelper/gzipbuild IN OUT
//
// compress/gzip at its best compression under an empty header (no name, a zero
// time, the OS byte "unknown"), so the bytes depend on IN and go.mod's
// toolchain alone. The gzip program would make them depend on the machine as
// well: its output is its version's. A package of its own, so the helper's
// binary carries none of it.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gzipbuild IN OUT")
		os.Exit(2)
	}
	if err := write(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "gzipbuild:", err)
		os.Exit(1)
	}
}

// write gzips in to out, by a rename, so a failed run leaves out as it was.
func write(in, out string) error {
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	gz, err := compress(data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".gzipbuild-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(gz); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

// compress is data gzipped under the empty header.
func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	w.OS = 255
	if _, err = io.Copy(w, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
