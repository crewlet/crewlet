package engine

import (
	"context"
	"errors"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// noEstate is the guard of a key no estate derives a value under — which is
// what the race cases' test key is.
func noEstate(context.Context) error { return nil }

// A COMPANY KEY WITH NO GUARD IS NOT MINTED AT ALL.
//
// The chart's key was minted with a nil guard, which read as "mint whatever
// happens" — so the day somebody deleted it, the next call would have minted
// over it. A nil guard is refused by name now, and nothing is stored.
func TestACompanyKeyWithNoGuardIsNotMinted(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	node := keyNode(t, "node-a", fleet, coordmem.New())
	if _, err := node.companyKey(t.Context(), racedKey, "test", nil); err == nil {
		t.Fatal("a company key was minted with no guard")
	}
	if _, err := fleetsecrets.New(fleet, node.cipher).Estate().Get(t.Context(),
		racedKey); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("an unguarded mint stored the key (%v)", err)
	}
}

// A NODE BEHIND THE IDENTITY LOG DOES NOT JUDGE A MISSING KEY, AND DOES NOT
// ASK ITS ROWS.
//
// Whether a missing blind-index key was deleted or never minted is answered by
// the rows — an estate holding a blind had a key — and a node that has not
// applied the whole log may simply not have those rows yet. Its "no blind
// here" is the answer that mints over a deleted key and orphans every address
// in the directory, so behind is a refusal of its own, and the rows are not
// consulted at all until the node has caught up.
func TestANodeBehindTheIdentityLogDoesNotJudgeAMissingKey(t *testing.T) {
	t.Parallel()
	asked := false
	holds := func(held bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) {
			asked = true
			return held, err
		}
	}
	behind := func(context.Context) error { return caughtUp(5, 7) }
	current := func(context.Context) error { return caughtUp(7, 7) }

	err := judgeBlindKeyMint(t.Context(), behind, holds(false, nil))
	if !errors.Is(err, errIdentityBehind) {
		t.Fatalf("a node two records behind answered %v, want errIdentityBehind", err)
	}
	if asked {
		t.Error("a node behind the log asked its rows whether a key was in use, " +
			"and rows that have not arrived answer no")
	}

	for _, tc := range []struct {
		name    string
		held    bool
		readErr error
		want    error
	}{
		{"an estate holding a blind", true, nil, iamdomain.ErrNoBlindKey},
		{"an estate holding none", false, nil, nil},
		{"rows that could not be read", false, errors.New("disk"), nil},
	} {
		err := judgeBlindKeyMint(t.Context(), current, holds(tc.held, tc.readErr))
		switch {
		case tc.readErr != nil:
			if err == nil {
				t.Errorf("%s: the mint was allowed on rows nobody read", tc.name)
			}
		case tc.want == nil:
			if err != nil {
				t.Errorf("%s: a caught-up node refused a fresh company's key: %v",
					tc.name, err)
			}
		case !errors.Is(err, tc.want):
			t.Errorf("%s: answered %v, want %v", tc.name, err, tc.want)
		}
	}

	// THE ARITHMETIC: at the end, and past it, is caught up.
	for _, tc := range []struct{ applied, end uint64 }{{7, 7}, {8, 7}, {0, 0}} {
		if err := caughtUp(tc.applied, tc.end); err != nil {
			t.Errorf("applied %d of %d answered %v", tc.applied, tc.end, err)
		}
	}
}
