package sandbox_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// THE FILE CONTRACT, ON EVERY BACKEND. The twin first, because every runner
// and coordinator test reads a box through it, and a twin that answers what
// no real box would makes each of those tests a claim about nothing.
func TestTheFakeKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(*testing.T) sandbox.Sandbox { return sandbox.NewFakeSandbox("box-1") })
}

func TestADirectBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(t *testing.T) sandbox.Sandbox {
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
	})
}

// A container box reads and writes on the host side of its mount, so its
// contract is certified there; what the container itself does is local_test's.
func TestAContainerBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(t *testing.T) sandbox.Sandbox {
		return sandbox.NewContainerBoxAt(t.TempDir())
	})
}
