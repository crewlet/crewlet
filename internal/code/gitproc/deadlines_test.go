package gitproc_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE DEADLINES ARE THEIR FORMULAS: local pack work is paced per mebibyte of
// work, a transfer is one client leg in series with that work, both
// directions share one formula, and the leg a fetch from a cache does not
// have is not paced.
func TestTheDeadlinesAreTheirFormulas(t *testing.T) {
	t.Parallel()
	for bytes, want := range map[int64]time.Duration{
		0:                gitproc.ReadDeadline,
		1:                gitproc.ReadDeadline + time.Second,
		objstore.MiB:     gitproc.ReadDeadline + time.Second,
		objstore.MiB + 1: gitproc.ReadDeadline + 2*time.Second,
		1 << 30:          gitproc.ReadDeadline + 1024*time.Second,
	} {
		if got := gitproc.PackWorkDeadline(bytes); got != want {
			t.Errorf("PackWorkDeadline(%d) = %v, want %v", bytes, got, want)
		}
	}
	for _, n := range []int64{0, 1, 1 << 30, 20 << 30} {
		if got, want := gitproc.UploadPackDeadline(n), objstore.ReadBase+objstore.PaceFor(n)+gitproc.PackWorkDeadline(n); got != want {
			t.Errorf("UploadPackDeadline(%d) = %v, want %v", n, got, want)
		}
	}
	if up, budget := gitproc.UploadPackDeadline(1<<30), objstore.ReadBudget(1<<30); up >= budget {
		t.Errorf("UploadPackDeadline(1 GiB) = %v is not under objstore.ReadBudget's %v: it paces a second network leg a fetch from a cache does not have", up, budget)
	}
	if got, want := gitproc.ReceivePackDeadline(1<<30, 0, 2*time.Minute), gitproc.UploadPackDeadline(1<<30)+2*time.Minute; got != want {
		t.Errorf("ReceivePackDeadline(1 GiB, no objects, 2m) = %v, want the upload formula plus the hook's %v", got, want)
	}
	// RECEIVE-PACK'S OWN CONNECTIVITY CHECK WALKS THE HISTORY: the bound
	// grows with what the repository holds, beside what the push sends.
	if got, want := gitproc.ReceivePackDeadline(1<<30, 3_000_000, 2*time.Minute),
		gitproc.UploadPackDeadline(1<<30)+gitproc.HistoryWalk(3_000_000)+2*time.Minute; got != want {
		t.Errorf("ReceivePackDeadline(1 GiB, 3M objects, 2m) = %v, want the upload formula, a walk of the "+
			"history and the hook's: %v", got, want)
	}
	// A REMOTE RUN IS A TRANSFER WITH THE ENGINE AS THE CLIENT: the transfer
	// formula over what it moves, and a walk of the history it fetches into
	// or pushes from — none for a listing, which walks nothing.
	if got, want := gitproc.RemoteDeadline(1<<30, 3_000_000),
		gitproc.UploadPackDeadline(1<<30)+gitproc.HistoryWalk(3_000_000); got != want {
		t.Errorf("RemoteDeadline(1 GiB, 3M objects) = %v, want the transfer formula and a walk of the history: %v", got, want)
	}
	if got, want := gitproc.RemoteDeadline(0, 0), gitproc.UploadPackDeadline(0); got != want {
		t.Errorf("RemoteDeadline(0, 0), a listing's, = %v, want the transfer formula's floor %v", got, want)
	}
	for objects, want := range map[int64]time.Duration{
		-1: 0, 0: 0, 1: gitproc.WalkPerObject, 3_000_000: 3_000_000 * gitproc.WalkPerObject,
	} {
		if got := gitproc.HistoryWalk(objects); got != want {
			t.Errorf("HistoryWalk(%d) = %v, want %v", objects, got, want)
		}
	}
	if gitproc.TransferStall != objstore.Stall {
		t.Errorf("TransferStall = %v, want objstore.Stall %v", gitproc.TransferStall, objstore.Stall)
	}
	if gitproc.ReadDeadline != gitproc.SlotWait {
		t.Errorf("ReadDeadline %v and SlotWait %v are both one read attempt", gitproc.ReadDeadline, gitproc.SlotWait)
	}
}
