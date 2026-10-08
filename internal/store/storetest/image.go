package storetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// A MIGRATED ESTATE, BUILT ONCE PER TEST BINARY.
//
// Opening a store on a fresh file runs both estates' whole migration sequence —
// eighty-odd files of DDL, each its own fsync'd transaction — and a test
// package that opens a store per case ran that sequence hundreds of times: the
// audit that found it counted 1,210 runs in tracker's suite alone, 307 s of its
// 354 s wall clock inside them. None of those runs was the subject of the test
// that paid for it. A migration is deterministic and inserts nothing that
// identifies the file it ran in, so the second run on a fresh file produces
// exactly what the first did.
//
// So the first fixture in a binary that asks builds the pair once — through
// [store.OpenNode] and [store.DB.OpenReplicated], the production sequence, so
// the image is precisely what THIS binary's migrations produce and a broken
// migration still fails every package at its first fixture — folds each
// file's write-ahead log back into it with [store.QuiesceCopy], and keeps the
// two files' bytes. Every fixture after that writes them where a fresh file
// would have been created, and opens the copy through the same production
// path, which finds every version applied and applies nothing.
//
// NO FILE IS EVER COMMITTED for it: an image checked into the tree would be a
// second copy of the schema that a new migration could disagree with.
//
// WHAT STAYS FRESH is whatever has migrating as its subject: internal/store's
// own suites — the contract's schema cases, Pending, adoption, the ledger — and
// every test that calls [store.OpenNode] directly.
var image = sync.OnceValues(buildImage)

// estateImage is the two estates' files as a fresh pair is left by this
// binary's migrations, each folded into one self-contained file.
type estateImage struct {
	node, replicated []byte
}

// buildImage migrates a fresh pair in a directory of its own and keeps its
// bytes. The directory is removed before it returns, so nothing is left behind
// in a binary that has no TestMain to clean up after it.
//
// ITS OWN CONTEXT, not a caller's: the image is built once for every fixture in
// the binary, and a failure is remembered for all of them — so the first
// test's context ending must not become every later test's failure.
func buildImage() (estateImage, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "crewlet-storetest-image-")
	if err != nil {
		return estateImage{}, fmt.Errorf("a directory to build it in: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	nodePath := filepath.Join(dir, "node.db")
	node, err := store.OpenNode(ctx, nodePath, store.Options{})
	if err != nil {
		return estateImage{}, fmt.Errorf("migrate a fresh node estate: %w", err)
	}
	if _, err := node.OpenReplicated(ctx, 1); err != nil {
		_ = node.Close()
		return estateImage{}, fmt.Errorf("migrate a fresh replicated estate: %w", err)
	}
	replicatedPath := node.ReplicatedFile()
	if err := node.Close(); err != nil {
		return estateImage{}, fmt.Errorf("close the migrated estates: %w", err)
	}

	// FOLDED, and only the main files kept. A clean close leaves the whole
	// schema in each file's -wal — the main file is one page — and a -wal
	// carried beside a copy would be applied to whatever file is there.
	var img estateImage
	for _, f := range []struct {
		path string
		into *[]byte
	}{{nodePath, &img.node}, {replicatedPath, &img.replicated}} {
		if err := store.QuiesceCopy(ctx, f.path); err != nil {
			return estateImage{}, fmt.Errorf("fold the migrated file: %w", err)
		}
		if *f.into, err = os.ReadFile(f.path); err != nil {
			return estateImage{}, fmt.Errorf("read the migrated file: %w", err)
		}
	}
	return img, nil
}

// Seed writes estate's migrated image at path, when no database is there yet,
// so the store a test opens there next is migrated already. A database that IS
// there is left exactly as it is: a test reopening its own file, or handing in
// a copy it prepared, gets that file and nothing else.
//
// For a test that opens a store or boots an engine on a path of its own —
// [OpenEstate] and [OpenNode] seed for themselves. Seed only the estates the
// caller will open: a replicated file beside a node opened alone is a file a
// fresh node would not have, and Pending, a backup or an estate-presence check
// would see it.
//
// An in-memory path is not seeded, since there is no file to write; it
// migrates on open as it always has.
func Seed(t testing.TB, estate store.Estate, path string) {
	t.Helper()
	if path == "" || strings.HasPrefix(path, ":memory:") {
		return
	}
	img, err := image()
	if err != nil {
		t.Fatalf("storetest: build the migrated estate image: %v", err)
	}
	var content []byte
	switch estate {
	case store.EstateNode:
		content = img.node
	case store.EstateReplicated:
		content = img.replicated
	default:
		t.Fatalf("storetest: seed %s: %q is not an estate; the estates are %v",
			path, estate, store.Estates)
	}
	if err := writeImage(path, content); err != nil {
		t.Fatalf("storetest: seed the %s estate at %s: %v", estate, path, err)
	}
}

// writeImage creates path holding content, and leaves a file already there
// untouched. CREATE-EXCLUSIVE rather than a check followed by a write, so a
// file that appears in between is never overwritten either.
//
// 0644 because that is the mode the driver creates a database with, measured
// under the default umask, so a seeded file is the file a fresh open would
// have made.
func writeImage(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// OpenNode opens a node's own store at path — seeded from the migrated image
// when no database is there, and never with a replicated estate beside it —
// and fails the test when it cannot. The caller closes it, as it would a
// handle [store.OpenNode] returned.
//
// A SCRATCH store is not seeded: [store.Options.Scratch] discards whatever is
// at the path before it opens, so a seed would only be deleted.
func OpenNode(t testing.TB, path string, opts store.Options) *store.DB {
	t.Helper()
	if !opts.Scratch {
		Seed(t, store.EstateNode, path)
	}
	db, err := store.OpenNode(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("open the node's store at %s: %v", path, err)
	}
	return db
}
