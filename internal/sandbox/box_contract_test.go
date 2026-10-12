package sandbox_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// THE FILE CONTRACT, ON EVERY BACKEND. The twin first, because every runner
// and coordinator test reads a box through it, and a twin that answers what
// no real box would makes each of those tests a claim about nothing.
//
// The twin and the two local boxes run at [contractCap]: what they read is
// memory or the engine host's own files, so nothing between them and the file
// could hold a limit of its own, and every branch the cap cases reach is the
// same at any size. That each of them reads by [sandbox.MaxFileBytes] in
// production is [TestEveryBoxReadsWholeUpToMaxFileBytes]'s; E2B, whose reads
// cross envd's transport, runs at the real cap in e2b_test.go.
func TestTheFakeKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, contractCap, func(*testing.T) sandbox.Sandbox {
		return sandbox.NewFakeSandbox("box-1").CapReads(contractCap)
	})
}

func TestADirectBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, contractCap, func(t *testing.T) sandbox.Sandbox {
		return sandbox.CapReads(directBox(t), contractCap)
	})
}

// A container box reads and writes on the host side of its mount, so its
// contract is certified there; what the container itself does is local_test's.
func TestAContainerBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, contractCap, func(t *testing.T) sandbox.Sandbox {
		return sandbox.CapReads(containerBox(t), contractCap)
	})
}

// contractCap is the whole-read cap the in-process backends keep the file
// contract at: a MiB, past the largest file the suite reads whole and the
// tail it reads, and a thirty-second of what it would build at the real one.
const contractCap = 1 << 20

// ONLY ABSENCE IS EMPTY ON THE ENGINE HOST. A local box's whole read answered
// every failure to open or read a file as an empty one, so a report the
// engine could not read collected as a run that wrote none — where a remote
// box's envd answers the same failure as an error, which a collection retries
// and a person can act on. A path under a FILE cannot be resolved at all, and
// says so rather than claiming it lies outside the box.
//
// A DIRECTORY stands in for an unreadable file because it is one on every
// host and for every user, the root this suite may run as included.
func TestALocalBoxAnswersAFileItCannotReadAsAnError(t *testing.T) {
	t.Parallel()
	for name, fresh := range map[string]func(*testing.T) sandbox.Sandbox{
		"direct": directBox, "container": containerBox,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := fresh(t)
			ctx := t.Context()
			unreadable := box.Home() + "/.crewlet/findings.md"
			if err := box.WriteFile(ctx, unreadable+"/inside", []byte("x")); err != nil {
				t.Fatalf("make the path a directory: %v", err)
			}
			if got, err := box.ReadFile(ctx, unreadable); err == nil {
				t.Errorf("ReadFile = %q, nil; want the read's failure, not an empty file", got)
			}
			if tail, err := box.ReadTail(ctx, unreadable, 64); err == nil {
				t.Errorf("ReadTail = %+v, nil; want the read's failure", tail)
			}
			stream, err := box.OpenFile(ctx, unreadable)
			if err == nil {
				_, err = io.ReadAll(stream)
				_ = stream.Close()
			}
			if err == nil {
				t.Error("OpenFile read to its end without an error; want the read's failure")
			}

			marker := box.Home() + "/.crewlet/done"
			if err := box.WriteFile(ctx, marker, []byte("0")); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err = box.ReadFile(ctx, marker+"/below")
			if err == nil || strings.Contains(err.Error(), "outside") {
				t.Errorf("a path under a file = %v; want it unresolvable, and not an escape", err)
			}
		})
	}
}

func directBox(t *testing.T) sandbox.Sandbox {
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{Placement: sandbox.Direct})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })
	return box
}

func containerBox(t *testing.T) sandbox.Sandbox {
	box, _ := sandbox.NewContainerBoxAt(t.TempDir())
	return box
}
