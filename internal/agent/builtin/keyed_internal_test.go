package builtin

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A REQUEST'S KEY GOES ON ITS ACTOR AT ITS OWN INSTANT, in one place: every
// tracker id a request's writes derive carries that instant, and the ledger
// vouches for a retry by it. A key with no instant is refused, or every id
// would date at the epoch, older than any loss the ledger had.
//
// Mutations: Keyed dating the key at time.Now() moves the ids off the key's
// instant; dropping its refusal takes a key with no instant.
func TestARequestKeyGoesOnItsActorAtItsOwnInstant(t *testing.T) {
	t.Parallel()
	minted := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	key := statelog.NewOpID(minted, "")
	actor, err := Actor{Handle: "ana"}.Keyed(key)
	if err != nil {
		t.Fatalf("Keyed(%q): %v", key, err)
	}
	if actor.WorkKey != key || !actor.OperationSince().Equal(minted) {
		t.Errorf("the keyed actor carries %q at %v, want %q at %v",
			actor.WorkKey, actor.OperationSince(), key, minted)
	}
	opID := opIDFor(actor, "update_work_item", "update", "t-1",
		map[string]any{"item": "ENG-1", "status": "done"})
	if at, ok := statelog.OpMintedAt(opID); !ok || !at.Equal(minted) {
		t.Errorf("a write under the key derives %q at %v, want the key's own %v",
			opID, at, minted)
	}

	if got, err := (Actor{Handle: "ana"}).Keyed("not-an-op-id"); err == nil {
		t.Errorf("a key with no instant was put on the actor: %+v", got)
	}
	plain, err := Actor{Handle: "ana"}.Keyed("")
	if err != nil || plain.WorkKey != "" || !plain.WorkSince.IsZero() {
		t.Errorf("no key changed the actor to %+v (%v)", plain, err)
	}
}
