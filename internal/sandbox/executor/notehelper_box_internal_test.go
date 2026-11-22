package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// helperBox is a fake box of an x86_64 machine whose `sha256sum` reads its
// copy of the note helper (none where sums is false), counting each removal
// the writer makes before it writes.
type helperBox struct {
	*sandbox.FakeSandbox
	removed int
}

func newHelperBox(machine string, sums bool) *helperBox {
	b := &helperBox{FakeSandbox: sandbox.NewFakeSandbox("box-1")}
	b.ExecFunc = func(ctx context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		switch {
		case strings.HasPrefix(cmd, "uname -m;"):
			out := machine + "\n"
			if held, _ := b.ReadFile(ctx, PathsFor(b).NoteHelper()); sums && len(held) > 0 {
				sum := sha256.Sum256(held)
				out += hex.EncodeToString(sum[:]) + "  " + PathsFor(b).NoteHelper() + "\n"
			}
			return sandbox.ExecResult{Stdout: out}, nil
		case strings.HasPrefix(cmd, "rm -f "):
			b.removed++
		}
		return sandbox.ExecResult{}, nil
	}
	return b
}

// THE NOTE HELPER IS WRITTEN ONLY WHERE THE BOX'S COPY IS NOT THIS ENGINE'S
// BUILD: a reused box holding it takes no upload, a copy that differs (another
// build's, or anything else) is removed and replaced, a box with no
// `sha256sum` takes it at every launch, and a machine with no build is
// [errNoNoteHelper].
//
// Mutation: write at every launch, keep any copy, or compare another sum, and
// this goes red.
func TestTheNoteHelperIsWrittenOnlyWhereTheBoxsCopyIsNotThisBuild(t *testing.T) {
	t.Parallel()
	want, err := noteHelperBinary("amd64")
	if err != nil {
		t.Fatal(err)
	}
	b := newHelperBox("x86_64", true)
	path := PathsFor(b).NoteHelper()
	launch := func(t *testing.T, b *helperBox, wantRemoved int) {
		t.Helper()
		if err := writeNoteHelper(t.Context(), b, path); err != nil {
			t.Fatalf("writeNoteHelper: %v", err)
		}
		if got, _ := b.ReadFile(t.Context(), path); !bytes.Equal(got, want) {
			t.Fatal("the box does not hold this engine's build")
		}
		if b.removed != wantRemoved {
			t.Errorf("the box's copy was replaced %d times, want %d", b.removed, wantRemoved)
		}
	}
	launch(t, b, 1)
	launch(t, b, 1)
	if err = b.WriteFile(t.Context(), path, []byte("another build")); err != nil {
		t.Fatal(err)
	}
	launch(t, b, 2)

	plain := newHelperBox("aarch64", false)
	if want, err = noteHelperBinary("arm64"); err != nil {
		t.Fatal(err)
	}
	launch(t, plain, 1)
	launch(t, plain, 2)

	if err = writeNoteHelper(t.Context(), newHelperBox("riscv64", true), path); !errors.Is(err, errNoNoteHelper) ||
		!strings.Contains(err.Error(), `"riscv64"`) {
		t.Errorf("a machine with no build is %v, want errNoNoteHelper naming it", err)
	}
}
