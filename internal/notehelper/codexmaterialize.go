package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// THE CODEX MATERIALIZE ADAPTER writes plain every rollout of a thread that a
// person's Codex compressed, as Codex itself does before it appends to one:
// the package doc's "A rollout a person's Codex compressed" says why the
// runner needs the plain file before Codex starts.

// codexCompressed is the suffix Codex 0.171.0 adds to a rollout it compresses
// (`rollout-<ts>-<thread>.jsonl.zst`, beside where the plain file was).
const codexCompressed = ".zst"

// runCodexMaterialize writes plain the compressed rollouts of -thread under
// -sessions. Exit 0 once none of the thread's is left compressed, 1 where one
// is, saying which.
func runCodexMaterialize(_ context.Context, f flags, _ io.Reader, _, stderr io.Writer, _ func(string) string) int {
	if f.sessions == "" || !codexThreadID(f.thread) {
		fmt.Fprintln(stderr, "notehelper: codex-materialize takes -sessions, Codex's sessions directory, and "+
			"-thread, a thread id of letters, digits and hyphens")
		return exitUsage
	}
	if err := codexMaterialize(f.sessions, f.thread); err != nil {
		fmt.Fprintf(stderr, "notehelper: writing the session's compressed rollout plain: %v\n", err)
		return exitError
	}
	return exitDone
}

// codexThreadID reports whether id is a thread id a file name pattern reads
// as itself: letters, digits and hyphens, as the runner's scripts hold one.
func codexThreadID(id string) bool {
	return id != "" && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") == ""
}

// codexMaterialize writes plain each regular file under sessions named
// `rollout-*-<thread>.jsonl.zst`.
func codexMaterialize(sessions, thread string) error {
	suffix := "-" + thread + ".jsonl" + codexCompressed
	return filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if !d.Type().IsRegular() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, suffix) {
			return nil
		}
		return materializeRollout(path)
	})
}

// materializeRollout replaces the compressed rollout z with the plain file
// beside it, under its name less `.zst`, its mode and mtime kept: decoded to
// a temporary file, then linked in, so the plain name never holds part of
// it. A plain file already there is never overwritten and wins: Codex writes
// it before removing the `.zst`, so it is the newer, and the stale `.zst`
// is removed, since a resume would take it over the plain one (measured on
// 0.171.0).
func materializeRollout(z string) error {
	plain := strings.TrimSuffix(z, codexCompressed)
	switch _, err := os.Lstat(plain); {
	case err == nil:
		return os.Remove(z)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	info, err := os.Stat(z)
	if err != nil {
		return err
	}
	tmp, err := decodeBeside(z, plain)
	if tmp != "" {
		defer func() { _ = os.Remove(tmp) }()
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp, info.Mode().Perm()); err != nil {
		return err
	}
	if err = os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err = os.Link(tmp, plain); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Remove(z)
}

// decodeBeside decodes the zstd file z into a new temporary file in plain's
// directory, synced, and is its name ("" where none was made). One decoder
// goroutine: the file is read once, in order, and more would each hold
// buffers in a box's memory for no gain.
func decodeBeside(z, plain string) (string, error) {
	in, err := os.Open(z) //nolint:gosec // the session's own rollout
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	dec, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return "", err
	}
	defer dec.Close()
	out, err := os.CreateTemp(filepath.Dir(plain), ".materialize-*")
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(out, dec); err != nil {
		_ = out.Close()
		return out.Name(), fmt.Errorf("%s does not decode: %w", z, err)
	}
	if err = out.Sync(); err != nil {
		_ = out.Close()
		return out.Name(), err
	}
	return out.Name(), out.Close()
}
