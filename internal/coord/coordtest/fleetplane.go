package coordtest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- the config plane --------------------------------------------------- //

var planeCases = []fleetCase{{
	// "Nothing activated" and "the store cannot be read" send a node down
	// opposite paths, so the read reports the two separately.
	name: "an unset pointer is reported as unset rather than empty",
	fn: func(h *fleetHarness) {
		_, found, err := h.f.Target(h.ctx)
		if err != nil {
			h.t.Fatalf("Target: %v", err)
		}
		if found {
			h.t.Fatal("a store with no activation reported one")
		}
	},
}, {
	name: "an activation is readable with the epoch the store assigned",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"name":"Acme"}`),
			At: h.now(), Origin: anOrigin,
		})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if published.Epoch <= 0 {
			h.t.Fatalf("epoch = %d, want a positive sequence", published.Epoch)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if got.Epoch != published.Epoch || got.RevisionID != "rev-1" {
			h.t.Fatalf("target = %+v, want the published %+v", got, published)
		}
	},
}, {
	// THE AUTHOR TRAVELS WITH THE POINTER, whole. A peer meets a revision
	// for the first time when the pointer names it, so what the pointer
	// drops, every node but the origin records as unknown — which is how
	// every adopted revision came to read "peer" on the audit screen. The
	// credential is part of it: a person writing through their own machine
	// token and the same person signed in are one author and two
	// credentials, and a backend that dropped the second would have every
	// other node's history unable to say which.
	name: "a pointer carries the revision's origin to every reader",
	fn: func(h *fleetHarness) {
		written := h.now().Add(-time.Hour)
		origin := coord.RevisionOrigin{
			Author: "maya", AuthorKind: iam.ActorOperator,
			OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a",
			Source:     "api", CreatedAt: written,
		}
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Summary: "first", Payload: []byte("{}"),
			At: h.now(), Origin: origin,
		})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		for name, read := range map[string]coord.RevisionOrigin{
			"the activation answered": published.Origin, "the pointer read back": got.Origin,
		} {
			if read.Author != "maya" || read.AuthorKind != iam.ActorOperator ||
				read.OperatorID != origin.OperatorID || read.Source != "api" ||
				!read.CreatedAt.Equal(written) {
				h.t.Fatalf("%s carries origin %+v, want %+v", name, read, origin)
			}
		}
	},
}, {
	// AN AUTHOR KIND THIS BUILD CANNOT NAME IS CARRIED AS THE VALUE IT IS.
	// The kind is an enum read off the wire, so a newer peer's kind arrives
	// as a value nobody here has a constant for — and a backend that mapped
	// it onto a kind that exists would have every node's history attribute
	// the revision to the wrong sort of author, with nothing to say so.
	name: "an author kind this build does not know round-trips unchanged",
	fn: func(h *fleetHarness) {
		unknown := iam.ActorKind("a_kind_from_a_newer_build")
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Origin: coord.RevisionOrigin{
				Author: "x", AuthorKind: unknown, Source: "api", CreatedAt: h.now(),
			},
		})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		for name, read := range map[string]coord.RevisionOrigin{
			"the activation answered": published.Origin, "the pointer read back": got.Origin,
		} {
			if read.AuthorKind != unknown || read.AuthorKind.Valid() {
				h.t.Fatalf("%s carries kind %q, want %q unchanged", name, read.AuthorKind, unknown)
			}
		}
	},
}, {
	// A POINTER NAMING NOBODY IS REFUSED, and leaves nothing behind. Every
	// writer knows who it is, how it made the revision and when; a pointer
	// that left any of them out would put the revision on every peer with
	// an author, a source or an instant somebody would have to guess.
	name: "an activation without its revision's origin is refused",
	fn: func(h *fleetHarness) {
		for name, origin := range map[string]coord.RevisionOrigin{
			"no origin at all": {},
			"no author kind":   {Author: "maya", Source: "api", CreatedAt: h.now()},
			"no source":        {Author: "maya", AuthorKind: iam.ActorOperator, CreatedAt: h.now()},
			"no creation":      {Author: "maya", AuthorKind: iam.ActorOperator, Source: "api"},
		} {
			_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: origin,
			})
			if !errors.Is(err, coord.ErrIncompleteOrigin) {
				h.t.Fatalf("%s: Activate = %v, want coord.ErrIncompleteOrigin", name, err)
			}
		}
		if got, found, err := h.f.Target(h.ctx); err != nil || found {
			h.t.Fatalf("Target = (%+v, %v, %v) after refused activations, want unset",
				got, found, err)
		}
		if h.stored("{}") {
			h.t.Fatal("a refused activation left its body")
		}
		// The author's LABEL alone may be empty: the kind still says what
		// wrote it.
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Origin: coord.RevisionOrigin{AuthorKind: iam.ActorOperator, Source: "api", CreatedAt: h.now()},
		}); err != nil {
			h.t.Fatalf("an origin with no author label: %v", err)
		}
	},
}, {
	// The epoch is a FENCING token: a counter that went backwards would
	// hand a node a number an older revision already used.
	name: "the epoch only ever moves forward",
	fn: func(h *fleetHarness) {
		var last int64
		for i := range 4 {
			got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
			if err != nil {
				h.t.Fatalf("Activate: %v", err)
			}
			if got.Epoch <= last {
				h.t.Fatalf("epoch %d did not advance past %d", got.Epoch, last)
			}
			last = got.Epoch
		}
	},
}, {
	// THE LOSER OF A RACE IS TOLD. There is no leader here — any node's
	// API may write the config — so without a compare-and-set two
	// operators editing at the same moment both succeed and the later
	// write silently wins. That is a lost edit with a 201 in the operator's
	// hand, and nothing anywhere to find it by.
	name: "an activation expecting a revision the fleet has left is refused",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		// Somebody else moves the pointer, the way a second node's API
		// would: expecting what this caller also read.
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte(`{"v":2}`), At: h.now(),
			Expect: "rev-1", Origin: anOrigin}); err != nil {
			h.t.Fatalf("the second write was refused: %v", err)
		}
		// And now the first caller's edit, built on rev-1, arrives.
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-3", Payload: []byte(`{"v":3}`), At: h.now(),
			Expect: "rev-1", Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced", err)
		}
		// AND IT CHANGED NOTHING. A refusal that had already moved the
		// pointer, or left rev-3's body behind for the next reader, is
		// the same lost edit wearing an error.
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if target.RevisionID != "rev-2" {
			h.t.Fatalf("the fleet is on %s, want the write that won", target.RevisionID)
		}
		if _, body := h.current(); string(body) != `{"v":2}` {
			h.t.Fatalf("body = %q, want the winner's", body)
		}
		if h.stored(`{"v":3}`) {
			h.t.Fatal("the refused write left its body")
		}
	},
}, {
	// The matching expectation SUCCEEDS, or the guard above is just a way
	// of refusing every conditional write.
	name: "an activation expecting the current revision succeeds",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`),
			At: h.now(), Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("a matching expectation was refused: %v", err)
		}
		if got.RevisionID != "rev-2" {
			h.t.Fatalf("published %+v", got)
		}
	},
}, {
	// EXPECTING A REVISION WHEN NOTHING IS ACTIVE IS A RACE: the write
	// was built on a pointer that is gone, so it edits a company the fleet
	// no longer runs. Letting it land is how a node whose own copy named
	// a revision the fleet had dropped published that copy's successor
	// over the fleet's loss — every config write named the node's local
	// revision, whatever the fleet ran. A writer that read no pointer
	// passes ExpectAbsent, which the case below lands.
	name: "an activation expecting a revision lands only while the pointer names it",
	fn: func(h *fleetHarness) {
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-0", Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced over no pointer", err)
		}
		// AND IT LEFT NOTHING BEHIND: no pointer, and no body for one.
		if target, found, err := h.f.Target(h.ctx); err != nil || found {
			h.t.Fatalf("target = %+v found=%v err=%v, want no activation", target, found, err)
		}
		if h.stored("{}") {
			h.t.Fatal("the refused body was published")
		}
	},
}, {
	// AN EXPECTED EPOCH PINS THE VERY ACTIVATION THE WRITER READ. With the
	// revision alone, another writer re-activating it in between lets the
	// write land a millisecond after that one — at an instant the writer
	// never planned — and the boot publish of an offline offer, which
	// records the instant it will land at before it publishes, then reads
	// its own landed publish as never made. Pinned, the re-activation is the
	// race it is, and a write at the epoch it read lands at exactly
	// ActivationAt(its instant, that activation's).
	name: "an activation expecting an epoch lands only on that activation",
	fn: func(h *fleetHarness) {
		read, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		again, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("re-activate: %v", err)
		}
		planned := coord.ActivationAt(h.now().Add(time.Minute), read.At)
		_, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: planned,
			Expect: "rev-1", ExpectEpoch: read.Epoch, Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("an epoch the pointer has moved past: err = %v, want coord.ErrActivationRaced", err)
		}
		if target, _, _ := h.f.Target(h.ctx); target.Epoch != again.Epoch {
			h.t.Fatalf("the refused write moved the pointer to %+v", target)
		}
		planned = coord.ActivationAt(h.now().Add(time.Minute), again.At)
		landed, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: planned,
			Expect: "rev-1", ExpectEpoch: again.Epoch, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("the epoch the pointer is at was refused: %v", err)
		}
		if !landed.At.Equal(planned) {
			h.t.Fatalf("landed at %s, want exactly the planned %s", landed.At, planned)
		}
		if _, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-3", Payload: []byte("{}"), At: h.now(),
			ExpectEpoch: landed.Epoch, Origin: anOrigin}); err == nil ||
			errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("an epoch with no revision: err = %v, want a refusal that is no race", err)
		}
	},
}, {
	// A RESTORE SAYS SO, AND ONLY UNTIL THE NEXT WRITE. A peer that knows the
	// fleet moved on replaces a restored pointer and never a chosen one, so a
	// mark that outlived its write would let it replace the next choice, and
	// one the backend dropped would leave a stale restore standing.
	name: "a restored pointer says so until the next write",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			ExpectAbsent: true, Restore: true, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("restore: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || !target.Restored || !published.Restored {
			h.t.Fatalf("restored pointer reads %+v (published %+v, found=%v err=%v), "+
				"want it marked restored", target, published, found, err)
		}
		if _, err = h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-1", Origin: anOrigin}); err != nil {
			h.t.Fatalf("choose: %v", err)
		}
		if target, _, _ = h.f.Target(h.ctx); target.Restored {
			h.t.Fatalf("a chosen pointer reads %+v, want the restore's mark gone", target)
		}
	},
}, {
	// AN EMPTY EXPECTATION IS UNCONDITIONAL, which is `crewlet run
	// -import-company`: an operator asserting a company outright at one
	// node's start rather than editing what it read. It has nothing to have
	// raced with, so a pointer that moved under it is replaced, not a
	// refusal. Every other writer — the boot publish included — read the
	// pointer, or its absence, and passes what it read.
	name: "an activation with no expectation overwrites whatever is there",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-2", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("an unconditional activation was refused: %v", err)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-2" {
			h.t.Fatalf("target = %+v found=%v err=%v", target, found, err)
		}
	},
}, {
	// A CREATE-ONLY ACTIVATION LANDS ON AN EMPTY STORE. It is what a first
	// import is: a company built on nothing, asking to be the first.
	name: "an activation expecting no activation lands on an empty store",
	fn: func(h *fleetHarness) {
		got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(),
			ExpectAbsent: true, Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("a create-only activation on an empty store was refused: %v", err)
		}
		if got.Epoch == 0 || got.RevisionID != "rev-1" {
			h.t.Fatalf("published %+v", got)
		}
		if _, body := h.current(); string(body) != `{"v":1}` {
			h.t.Fatalf("body = %q, want the created revision's", body)
		}
	},
}, {
	// AND IT IS REFUSED WHEN THE FLEET HAS ONE, whatever this node's own
	// store says. A node that joined a fleet and has not reconciled yet
	// believes nothing is configured; without this, its first import
	// replaces a running company, renaming it and orphaning the memory of
	// every seat, and nothing refuses it.
	name: "an activation expecting no activation is refused when one exists",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "the-fleets", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "a-second-company", Payload: []byte(`{"v":2}`), At: h.now(),
			ExpectAbsent: true, Origin: anOrigin})
		if !errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want coord.ErrActivationRaced", err)
		}
		// AND IT CHANGED NOTHING: the pointer still names the company the
		// fleet is running, and its payload is still there for every peer
		// that has yet to apply it. A refusal that had already published
		// the loser's body leaves the fleet pointed at a revision no peer
		// can read.
		target, body := h.current()
		if target.RevisionID != "the-fleets" || string(body) != `{"v":1}` {
			h.t.Fatalf("the fleet is on %s with body %q, want the fleet's own", target.RevisionID, body)
		}
		if h.stored(`{"v":2}`) {
			h.t.Fatal("the refused write published its body")
		}
	},
}, {
	// EXACTLY ONE OF N CONCURRENT CREATIONS LANDS. Two operators pressing
	// Create on two nodes at the same moment is the shape this is for, and
	// it cannot be read off the single-threaded cases: they prove the
	// comparison happens, this proves it is atomic.
	name: "concurrent create-only activations leave exactly one winner",
	fn: func(h *fleetHarness) {
		var wg sync.WaitGroup
		won := make(chan string, 8)
		raced := make(chan error, 8)
		for i := range 8 {
			wg.Go(func() {
				id := fmt.Sprintf("company-%d", i)
				_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: id, Payload: []byte("{}"), At: h.now(),
					ExpectAbsent: true, Origin: anOrigin})
				switch {
				case err == nil:
					won <- id
				case errors.Is(err, coord.ErrActivationRaced):
					raced <- err
				default:
					h.t.Errorf("Activate: %v", err)
				}
			})
		}
		wg.Wait()
		close(won)
		close(raced)
		if got := len(won); got != 1 {
			h.t.Fatalf("%d of 8 creations were accepted, want exactly 1", got)
		}
		if got := len(raced); got != 7 {
			h.t.Fatalf("%d creations were told they lost, want 7", got)
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("Target: %v found=%v", err, found)
		}
		if winner := <-won; target.RevisionID != winner {
			h.t.Fatalf("the fleet is on %s, want the creation that won, %s",
				target.RevisionID, winner)
		}
	},
}, {
	// A WRITE CANNOT HAVE BEEN BUILT ON A REVISION AND ON NOTHING AT ONCE,
	// so asking for both is refused as the programming error it is rather
	// than resolved in favour of one.
	name: "an activation expecting both a revision and none is refused",
	fn: func(h *fleetHarness) {
		_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("{}"), At: h.now(),
			Expect: "rev-0", ExpectAbsent: true, Origin: anOrigin})
		if err == nil {
			h.t.Fatal("an activation expecting a revision and no revision was accepted")
		}
		if errors.Is(err, coord.ErrActivationRaced) {
			h.t.Fatalf("err = %v, want a refusal of the request rather than a race", err)
		}
		if _, found, ferr := h.f.Target(h.ctx); ferr != nil || found {
			h.t.Fatalf("a refused request published an activation (found=%v err=%v)", found, ferr)
		}
	},
}, {
	// EXACTLY ONE OF N CONCURRENT EDITS ON ONE BASE LANDS. This is the
	// property the whole mechanism exists for, and it cannot be read off
	// the single-threaded cases: they prove the comparison happens, this
	// proves it is atomic.
	name: "concurrent edits on one base leave exactly one winner",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "base", Payload: []byte("{}"), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		var wg sync.WaitGroup
		won := make(chan string, 8)
		raced := make(chan error, 8)
		for i := range 8 {
			wg.Go(func() {
				id := fmt.Sprintf("edit-%d", i)
				_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: id, Payload: []byte("{}"), At: h.now(),
					Expect: "base", Origin: anOrigin})
				switch {
				case err == nil:
					won <- id
				case errors.Is(err, coord.ErrActivationRaced):
					raced <- err
				default:
					h.t.Errorf("Activate: %v", err)
				}
			})
		}
		wg.Wait()
		close(won)
		close(raced)
		if got := len(won); got != 1 {
			h.t.Fatalf("%d of 8 edits on one base were accepted, want exactly 1", got)
		}
		if got := len(raced); got != 7 {
			h.t.Fatalf("%d edits were told they lost, want 7", got)
		}
	},
}, {
	// Two nodes activating at once must be handed two epochs, not one:
	// that is the whole reason the store assigns it.
	name: "concurrent activations get distinct epochs",
	fn: func(h *fleetHarness) {
		var wg sync.WaitGroup
		epochs := make(chan int64, 8)
		for i := range 8 {
			wg.Go(func() {
				got, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
				if err == nil {
					epochs <- got.Epoch
				}
			})
		}
		wg.Wait()
		close(epochs)
		seen := map[int64]bool{}
		for epoch := range epochs {
			if seen[epoch] {
				h.t.Fatalf("epoch %d was handed out twice", epoch)
			}
			seen[epoch] = true
		}
		if len(seen) == 0 {
			h.t.Fatal("no activation succeeded at all")
		}
	},
}, {
	// A LATER ACTIVATION CARRIES A LATER INSTANT, whatever the activating
	// node's clock said. The instant is how a node tells which of two
	// activations is newer — a booting node that remembers the fleet on a
	// revision activated later than a RESTORED pointer's publishes that one
	// over it, by compare-and-set — so an activation from a node whose clock
	// runs behind, or a republish of an older `activated_at`, left a node
	// holding the replaced revision reading its own copy as the newer. See
	// [coord.ActivationAt].
	name: "an activation no later than the one it replaces is published after it",
	fn: func(h *fleetHarness) {
		first := h.now()
		activate := func(req coord.ActivationRequest) coord.Activation {
			h.t.Helper()
			req.Payload = []byte("{}")
			got, err := h.f.Activate(h.ctx, req)
			if err != nil {
				h.t.Fatalf("Activate(%s): %v", req.RevisionID, err)
			}
			target, found, err := h.f.Target(h.ctx)
			if err != nil || !found || !target.At.Equal(got.At) {
				h.t.Fatalf("the pointer carries %+v (found=%v err=%v), and the "+
					"activation reported %s", target, found, err, got.At)
			}
			return got
		}
		if got := activate(coord.ActivationRequest{RevisionID: "rev-1", At: first, Origin: anOrigin}); !got.At.Equal(first) {
			h.t.Fatalf("the first activation is at %s, want the %s it asked for", got.At, first)
		}
		for _, c := range []struct {
			name string
			req  coord.ActivationRequest
		}{
			{"an unconditional one from a clock behind",
				coord.ActivationRequest{RevisionID: "rev-2", At: first.Add(-time.Hour), Origin: anOrigin}},
			{"an expecting one at the same instant",
				coord.ActivationRequest{RevisionID: "rev-3", At: first, Expect: "rev-2", Origin: anOrigin}},
			{"one later by less than the stamp's resolution",
				coord.ActivationRequest{RevisionID: "rev-4", At: first.Add(2*time.Millisecond + 400*time.Microsecond), Origin: anOrigin}},
		} {
			before, _, err := h.f.Target(h.ctx)
			if err != nil {
				h.t.Fatalf("Target: %v", err)
			}
			got := activate(c.req)
			if got.At.UnixMilli() <= before.At.UnixMilli() {
				h.t.Fatalf("%s is published at %s, no later in milliseconds than "+
					"the %s it replaced", c.name, got.At, before.At)
			}
		}
		// AND A GENUINELY LATER ONE IS PUBLISHED AS IT ASKED.
		later := first.Add(time.Minute)
		if got := activate(coord.ActivationRequest{RevisionID: "rev-5", At: later, Origin: anOrigin}); !got.At.Equal(later) {
			h.t.Fatalf("a later activation is at %s, want the %s it asked for", got.At, later)
		}
	},
}, {
	// THE INSTANT IS DECIDED INSIDE THE COMPARE-AND-SET, so two activations
	// racing each other are never both told one instant: whichever lands
	// second is compared with the first.
	name: "concurrent activations are handed increasing instants",
	fn: func(h *fleetHarness) {
		type landed struct {
			epoch int64
			at    time.Time
		}
		var wg sync.WaitGroup
		results := make(chan landed, 8)
		for i := range 8 {
			wg.Go(func() {
				got, err := h.f.Activate(h.ctx, coord.ActivationRequest{
					RevisionID: fmt.Sprintf("rev-%d", i), Payload: []byte("{}"), At: h.now(), Origin: anOrigin})
				if err != nil {
					h.t.Errorf("Activate: %v", err)
					return
				}
				results <- landed{got.Epoch, got.At}
			})
		}
		wg.Wait()
		close(results)
		var all []landed
		for r := range results {
			all = append(all, r)
		}
		if len(all) != 8 {
			h.t.Fatalf("%d of 8 unconditional activations landed", len(all))
		}
		slices.SortFunc(all, func(a, b landed) int { return cmp.Compare(a.epoch, b.epoch) })
		for i := 1; i < len(all); i++ {
			if all[i].at.UnixMilli() <= all[i-1].at.UnixMilli() {
				h.t.Fatalf("epoch %d is at %s, no later than epoch %d's %s",
					all[i].epoch, all[i].at, all[i-1].epoch, all[i-1].at)
			}
		}
	},
}, {
	// The fleet view was each node reading its own row and drawing a fleet
	// of one.
	name: "every node's apply status is in the fleet view",
	fn: func(h *fleetHarness) {
		at := h.now()
		for i, node := range []string{"node-a", "node-b", "node-c"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: node, Epoch: 7, Status: "ok",
				UpdatedAt: at.Add(time.Duration(i) * time.Second),
			}); err != nil {
				h.t.Fatalf("RecordApply(%s): %v", node, err)
			}
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 3 {
			h.t.Fatalf("the fleet view holds %d nodes, want 3: %+v", len(got), got)
		}
		if got[0].NodeID != "node-c" {
			h.t.Fatalf("fleet[0] = %s, want the freshest report first", got[0].NodeID)
		}
	},
}, {
	// THE TIE-BREAK IS PART OF THE CONTRACT, and it was implemented twice
	// with nothing holding either to it. A rolling apply lands every node's
	// report inside the same second, so a fleet view ordered by timestamp
	// alone reshuffles between reads — and an operator watching a deploy
	// converge would see the list jump with no node having changed.
	//
	// Newest first, the node id breaking the tie ASCENDING.
	name: "one instant's reports are ordered by node id",
	fn: func(h *fleetHarness) {
		at := h.now()
		// Recorded out of order, so a backend that answered in write
		// order rather than sorting would pass a same-order assertion.
		for _, node := range []string{"node-c", "node-a", "node-b"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: node, Epoch: 7, Status: "ok", UpdatedAt: at,
			}); err != nil {
				h.t.Fatalf("RecordApply(%s): %v", node, err)
			}
		}
		// One node reporting LATER must still sort ahead of all three,
		// so the tie-break never outranks the timestamp.
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-z", Epoch: 7, Status: "ok", UpdatedAt: at.Add(time.Second),
		}); err != nil {
			h.t.Fatalf("RecordApply(node-z): %v", err)
		}

		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		ids := make([]string, len(got))
		for i, row := range got {
			ids[i] = row.NodeID
		}
		want := []string{"node-z", "node-a", "node-b", "node-c"}
		if !slices.Equal(ids, want) {
			h.t.Fatalf("fleet order = %v, want %v — freshest first, then by node id", ids, want)
		}
	},
}, {
	// Three revisions, none the one asked for. WHAT A NODE RUNS differs from
	// the epoch's when it refused that revision (the fleet view shows a node
	// still serving the previous company). WHAT IT BOOTS ON is a third while a
	// write to its boot row is retried (runs rev-6, returns on rev-5), which a
	// reader keeping a revision's credentials acts on. THE EPOCH IT LAST
	// APPLIED is not the one attempted (refused 7, applied 6), which a writer
	// judging whether its save reached the node would read as "not yet" for
	// good. A backend dropping any field reported nothing for it.
	name: "an apply record carries what its node applied, serves and boots on",
	fn: func(h *fleetHarness) {
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-a", Epoch: 7, RevisionID: "rev-7", Applied: 6, Serving: "rev-6",
			Boots: "rev-5", Status: "failed", Error: "refused", UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 || got[0].RevisionID != "rev-7" || got[0].Applied != 6 ||
			got[0].Serving != "rev-6" || got[0].Boots != "rev-5" {
			h.t.Fatalf("fleet = %+v, want node-a asked for rev-7 at epoch 7, having "+
				"applied epoch 6, serving rev-6 and booting on rev-5", got)
		}
	},
}, {
	name: "a node's later report replaces its earlier one",
	fn: func(h *fleetHarness) {
		at := h.now()
		for _, status := range []string{"applying", "ok"} {
			if err := h.f.RecordApply(h.ctx, coord.NodeApply{
				NodeID: "node-a", Epoch: 7, Status: status, UpdatedAt: at,
			}); err != nil {
				h.t.Fatalf("RecordApply: %v", err)
			}
			at = at.Add(time.Second)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 || got[0].Status != "ok" {
			h.t.Fatalf("fleet = %+v, want one node at its latest status", got)
		}
	},
}, {
	// A driver's message with a query in it can be kilobytes, and this
	// value is read by every peer on every reconcile tick.
	name: "a node's failure text is bounded",
	fn: func(h *fleetHarness) {
		huge := make([]byte, 8000)
		for i := range huge {
			huge[i] = 'x'
		}
		if err := h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-a", Epoch: 7, Status: "failed",
			Error: string(huge), UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err := h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		if len(got) != 1 {
			h.t.Fatalf("fleet = %+v", got)
		}
		if len(got[0].Error) >= len(huge) {
			h.t.Fatalf("the failure text was stored whole (%d bytes)", len(got[0].Error))
		}

		// NEVER THROUGH A RUNE. The cut was a plain byte slice, so a driver
		// error naming a non-ASCII path ended in half a character — which
		// the KV's JSON encoding replaces with U+FFFD, delivering the fleet
		// view a garbled message rather than a shortened one.
		if err = h.f.RecordApply(h.ctx, coord.NodeApply{
			NodeID: "node-b", Epoch: 7, Status: "failed",
			Error: strings.Repeat("日本語", 1000), UpdatedAt: h.now(),
		}); err != nil {
			h.t.Fatalf("RecordApply: %v", err)
		}
		got, err = h.f.Fleet(h.ctx)
		if err != nil {
			h.t.Fatalf("Fleet: %v", err)
		}
		for _, n := range got {
			if n.NodeID != "node-b" {
				continue
			}
			if !utf8.ValidString(n.Error) {
				h.t.Error("a non-ASCII failure text was cut through a rune")
			}
			if !strings.HasSuffix(n.Error, "…") {
				h.t.Errorf("the cut is unmarked: %q", n.Error[max(0, len(n.Error)-12):])
			}
		}
	},
}}

// ---- the revision payload ---------------------------------------------- //

var payloadCases = []fleetCase{{
	// The reason the payload travels with the pointer. A peer applies the
	// revision the pointer NAMES by reading it — and while the body lived
	// only in the node's own database, the peer read nothing. A live config
	// change reached exactly the node it was posted to, and every other
	// node served whatever it had booted with, reporting "no such revision"
	// once per reconcile tick for the life of the deployment.
	name: "the revision a peer activated is readable by every node",
	fn: func(h *fleetHarness) {
		body := []byte(`{"name":"Acme","agents":{}}`)
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: body, At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Body(h.ctx, published)
		if err != nil {
			h.t.Fatalf("Body: %v", err)
		}
		if !found {
			h.t.Fatal("the payload a peer activated is invisible — the node cannot converge")
		}
		if string(got) != string(body) {
			h.t.Errorf("payload = %q, want %q", got, body)
		}
	},
}, {
	// A node converging on epoch N must not be handed epoch N+1's body.
	// Applying whatever happened to be there would converge it on a
	// revision the fleet is not pointed at, and report success. The body
	// is read by the digest its activation names, so it is that
	// activation's bytes or nothing.
	name: "a superseded activation's body is its own or absent, never the current one",
	fn: func(h *fleetHarness) {
		first, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Summary: "first", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-2", Summary: "second", Payload: []byte(`{"v":2}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if got, found, err := h.f.Body(h.ctx, first); err != nil || (found && string(got) != `{"v":1}`) {
			h.t.Errorf("the superseded activation's body = %q (found=%v err=%v), want its own or none",
				got, found, err)
		}
		if target, got := h.current(); target.RevisionID != "rev-2" || string(got) != `{"v":2}` {
			h.t.Errorf("the fleet is on %s with body %q, want rev-2's", target.RevisionID, got)
		}
	},
}, {
	name: "a body nothing wrote is absent, not an error",
	fn: func(h *fleetHarness) {
		got, found, err := h.f.Body(h.ctx, coord.Activation{RevisionID: "rev-1",
			Body: coord.BodyOf([]byte(`{"v":1}`))})
		if err != nil {
			h.t.Fatalf("Body before any activation: %v", err)
		}
		if found || got != nil {
			h.t.Errorf("body = %q found=%v, want nothing", got, found)
		}
	},
}, {
	// A POINTER THAT NAMES NO BODY IS A FAULT, NOT ABSENT. Only a build from
	// before bodies had keys writes one; read as absent, a converging node
	// would wait for a body never coming.
	name: "an activation naming no body is refused as bodiless",
	fn: func(h *fleetHarness) {
		if _, _, err := h.f.Body(h.ctx, coord.Activation{RevisionID: "rev-1"}); !errors.Is(err, coord.ErrBodyless) ||
			errors.Is(err, coord.ErrUnavailable) {
			h.t.Errorf("Body of an activation naming no body = %v, want coord.ErrBodyless and no outage", err)
		}
	},
}, {
	// ONE DOCUMENT IS ONE BODY: two activations of the same bytes (a
	// revert, a boot restore) name one key, and each reads it.
	name: "two activations of one document share its body",
	fn: func(h *fleetHarness) {
		first, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1",
			Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		again, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-2",
			Payload: []byte(`{"v":1}`), At: h.now(), Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate the same document again: %v", err)
		}
		if first.Body != again.Body || first.Body != coord.BodyOf([]byte(`{"v":1}`)) {
			h.t.Errorf("the two activations name %+v and %+v, want the document's one body",
				first.Body, again.Body)
		}
		if target, got := h.current(); target.Body != again.Body || string(got) != `{"v":1}` {
			h.t.Errorf("the pointer names %+v with body %q, want the shared one", target.Body, got)
		}
	},
}, {
	// THE BODY COMES BACK AS IT WENT IN. It is a sealed envelope, and what
	// a peer adopts into its own history is what it read here, so a store
	// that re-spelled it would hand every node a different copy of one
	// revision. The real backend embeds it inside a JSON record, where
	// encoding/json's default is to escape `<`, `>` and `&` — which a
	// ciphertext never holds and a document may — so the case carries all
	// three beside an envelope-shaped value.
	name: "a sealed body round-trips byte for byte",
	fn: func(h *fleetHarness) {
		body := []byte(`{"__encrypted__":"enc:v1:2026-01:q8+/Zm9vYmFy==","note":"a<b && c>d"}`)
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: body, At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Body(h.ctx, published)
		if err != nil || !found {
			h.t.Fatalf("Body: found=%v err=%v", found, err)
		}
		if string(got) != string(body) {
			h.t.Errorf("payload = %s, want the bytes activated, %s", got, body)
		}
	},
}, {
	// ONE FORM ON BOTH BACKENDS: the broker's record compacts what it
	// embeds, so the twin must too, or a case certified on the twin
	// certifies bytes the broker never hands back.
	name: "a body is served in its compact form",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-1", Payload: []byte("  {\n  \"name\" : \"Acme\",\n  \"agents\": { }\n}\n"),
			At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, found, err := h.f.Body(h.ctx, published)
		if err != nil || !found {
			h.t.Fatalf("Body: found=%v err=%v", found, err)
		}
		if want := `{"name":"Acme","agents":{}}`; string(got) != want {
			h.t.Errorf("payload = %q, want %q", got, want)
		}
	},
}, {
	// A BODY NO NODE COULD DECODE IS NOT PUBLISHED. It is a company
	// document, sealed or not, so anything but a JSON object is a caller's
	// mistake — and a pointer naming it is a fleet that converges on
	// nothing. Refused before anything is written: the fleet stays where
	// it was.
	name: "a payload that is not a document is refused and moves nothing",
	fn: func(h *fleetHarness) {
		if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
			RevisionID: "rev-good", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin}); err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		for name, payload := range map[string][]byte{
			"nothing":     nil,
			"empty":       []byte(""),
			"not json":    []byte("name: Acme"),
			"truncated":   []byte(`{"name":`),
			"a string":    []byte(`"enc:v1:k:abc"`),
			"an array":    []byte(`[1,2]`),
			"json null":   []byte("null"),
			"blank space": []byte("   "),
		} {
			_, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: "rev-" + name, Payload: payload, At: h.now(), Origin: anOrigin})
			if !errors.Is(err, coord.ErrPayloadNotDocument) {
				h.t.Errorf("%s: Activate = %v, want ErrPayloadNotDocument", name, err)
			}
		}
		target, found, err := h.f.Target(h.ctx)
		if err != nil || !found || target.RevisionID != "rev-good" {
			h.t.Fatalf("a refused payload moved the pointer: %+v found=%v err=%v",
				target, found, err)
		}
		if _, got := h.current(); string(got) != `{"v":1}` {
			h.t.Errorf("a refused payload disturbed the body: %q", got)
		}
	},
}, {
	// A caller mutating what it read must not reach the store: the body is
	// handed to a decoder that unseals in place on some paths.
	name: "a caller mutating a payload cannot reach the store",
	fn: func(h *fleetHarness) {
		published, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		got, _, _ := h.f.Body(h.ctx, published)
		for i := range got {
			got[i] = 'x'
		}
		again, _, _ := h.f.Body(h.ctx, published)
		if string(again) != `{"v":1}` {
			h.t.Errorf("the store took a caller's mutation: %q", again)
		}
	},
}}

// bodySweepCases hold the marker sweep to the activation's bodies: it removes
// one no pointer names once it is past the cutoff, and nothing else.
var bodySweepCases = []fleetCase{{
	// A BODY IS SWEPT ONLY ONCE IT IS OLD AND UNNAMED. Young, a flip still in
	// flight may name it; named, every peer still converging reads it.
	//
	// Mutation: skip the age check, and the young sweep removes rev-1's body;
	// skip the pointer, or purge every body, and the late one removes rev-2's.
	name: "the sweep removes a body no pointer names once it is past the cutoff",
	fn: func(h *fleetHarness) {
		first, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-1",
			Payload: []byte(`{"v":1}`), At: h.now(), Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		second, err := h.f.Activate(h.ctx, coord.ActivationRequest{RevisionID: "rev-2",
			Payload: []byte(`{"v":2}`), At: h.now(), Expect: "rev-1", Origin: anOrigin})
		if err != nil {
			h.t.Fatalf("Activate: %v", err)
		}
		if _, err = h.f.SweepMarkers(h.ctx, time.Now().Add(-time.Hour)); err != nil {
			h.t.Fatalf("SweepMarkers an hour back: %v", err)
		}
		if _, found, err := h.f.Body(h.ctx, first); err != nil || !found {
			h.t.Errorf("rev-1's body after a sweep it is younger than: found=%v err=%v, want kept", found, err)
		}
		if _, err = h.f.SweepMarkers(h.ctx, time.Now().Add(time.Hour)); err != nil {
			h.t.Fatalf("SweepMarkers an hour on: %v", err)
		}
		if _, found, err := h.f.Body(h.ctx, first); err != nil || found {
			h.t.Errorf("rev-1's body after a sweep past it: found=%v err=%v, want removed", found, err)
		}
		if got, found, err := h.f.Body(h.ctx, second); err != nil || !found || string(got) != `{"v":2}` {
			h.t.Errorf("the pointer's body after every sweep = %q found=%v err=%v, want kept", got, found, err)
		}
	},
}}

// payloadRaceCases hold the body and the pointer to ONE decision under
// concurrent writers.
var payloadRaceCases = []fleetCase{{
	// THE BODY AND THE POINTER CANNOT COME APART. They were two keys written
	// in two steps — the body, then the flip — so two nodes activating at
	// once could each write a body, and the one whose flip landed FIRST was
	// then pointed at by a pointer whose body the other had overwritten.
	// Every peer read "no such revision" for the epoch the fleet was on
	// until somebody activated again. Unconditional writers do exactly
	// this — two nodes started with -import-company each assert their file —
	// and an edit's compare-and-set only decided the pointer: the loser's
	// body had already landed.
	name: "racing activations leave the pointer's own body readable",
	fn: func(h *fleetHarness) {
		const writers = 8
		for round := range 6 {
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					id := fmt.Sprintf("rev-%d-%d", round, w)
					req := coord.ActivationRequest{
						RevisionID: id, At: h.now(), Origin: anOrigin,
						Payload: []byte(fmt.Sprintf(`{"revision":%q}`, id)),
					}
					// Half race unconditionally, as two overriding nodes
					// do; half as editors naming what they read, where all
					// but one lose the compare-and-set.
					if w%2 == 1 && round > 0 {
						req.Expect = fmt.Sprintf("rev-%d-0", round-1)
					}
					_, err := h.f.Activate(h.ctx, req)
					if err != nil && !errors.Is(err, coord.ErrActivationRaced) {
						h.t.Errorf("Activate(%s): %v", id, err)
					}
				}()
			}
			wg.Wait()
			target, found, err := h.f.Target(h.ctx)
			if err != nil || !found {
				h.t.Fatalf("round %d: Target: %v found=%v", round, err, found)
			}
			body, found, err := h.f.Body(h.ctx, target)
			if err != nil {
				h.t.Fatalf("round %d: Body: %v", round, err)
			}
			if !found {
				h.t.Fatalf("round %d: the fleet points at %s and its body is "+
					"nowhere — every peer would fail to converge", round,
					target.RevisionID)
			}
			if want := fmt.Sprintf(`{"revision":%q}`, target.RevisionID); string(body) != want {
				h.t.Fatalf("round %d: the body of %s is %s", round, target.RevisionID, body)
			}
			// The next round's editors expect the round's first writer;
			// point the fleet there so half of them can win.
			if _, err := h.f.Activate(h.ctx, coord.ActivationRequest{
				RevisionID: fmt.Sprintf("rev-%d-0", round), At: h.now(), Origin: anOrigin,
				Payload: []byte(fmt.Sprintf(`{"revision":"rev-%d-0"}`, round)),
			}); err != nil {
				h.t.Fatalf("round %d: re-point: %v", round, err)
			}
		}
	},
}}

// current is the activation the pointer names and its body, read as a peer
// reads them: the pointer, then the body it names. Either missing fails the
// case.
func (h *fleetHarness) current() (coord.Activation, json.RawMessage) {
	h.t.Helper()
	target, found, err := h.f.Target(h.ctx)
	if err != nil || !found {
		h.t.Fatalf("Target: found=%v err=%v", found, err)
	}
	body, found, err := h.f.Body(h.ctx, target)
	if err != nil || !found {
		h.t.Fatalf("Body of %s: found=%v err=%v", target.RevisionID, found, err)
	}
	return target, body
}

// stored reports whether the store holds payload as an activation's body: what
// a refused activation must not have written.
func (h *fleetHarness) stored(payload string) bool {
	h.t.Helper()
	_, found, err := h.f.Body(h.ctx, coord.Activation{RevisionID: "probe", Body: coord.BodyOf([]byte(payload))})
	if err != nil {
		h.t.Fatalf("Body of %s: %v", payload, err)
	}
	return found
}
