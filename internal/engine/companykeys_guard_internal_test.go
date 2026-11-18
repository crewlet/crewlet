package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// noEstate guards a key no estate derives values under, as the race cases'
// test key is.
func noEstate(context.Context) error { return nil }

// A COMPANY KEY WITH NO GUARD IS NOT MINTED AT ALL.
//
// A nil guard would mint over a deleted key, so it is refused by name and
// nothing is stored.
func TestACompanyKeyWithNoGuardIsNotMinted(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	node := keyNode(t, "node-a", fleet, coordmem.New())
	if _, err := node.companyKey(t.Context(), racedKey, secrets.SourceIAM, nil); err == nil {
		t.Fatal("a company key was minted with no guard")
	}
	if _, err := fleetsecrets.New(fleet, node.cipher).Estate().Get(t.Context(),
		racedKey); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("an unguarded mint stored the key (%v)", err)
	}
}

// A MISSING KEY IS JUDGED ON ROWS PROVED AGAINST THE LOG'S END, READ FIRST.
//
// "No blind here" proves nothing on rows that have not applied the whole log,
// so the end is read BEFORE the rows and handed to them: an unreadable end
// refuses without asking, rows that cannot vouch for it (behind, or holding a
// retained record) refuse like an unreadable store, and only a proved "none"
// mints.
func TestAMissingKeyIsJudgedOnRowsProvedAgainstTheLogsEnd(t *testing.T) {
	t.Parallel()
	var order []string
	var provedAgainst uint64
	logEnd := func(end uint64, err error) func(context.Context) (uint64, error) {
		return func(context.Context) (uint64, error) {
			order = append(order, "end")
			return end, err
		}
	}
	holds := func(held bool, err error) func(context.Context, uint64) (bool, error) {
		return func(_ context.Context, end uint64) (bool, error) {
			order = append(order, "rows")
			provedAgainst = end
			return held, err
		}
	}

	order = nil
	err := judgeBlindKeyMint(t.Context(), logEnd(0, errors.New("no responders")),
		holds(false, nil))
	if err == nil {
		t.Fatal("a node that could not read the log's end allowed the mint")
	}
	if slices.Contains(order, "rows") {
		t.Error("a node that could not read the log's end asked its rows, " +
			"and rows with nothing to be proved against answer no")
	}

	for _, tc := range []struct {
		name    string
		held    bool
		readErr error
		want    error
		allowed bool
	}{
		{"an estate holding a blind", true, nil, iamdomain.ErrNoBlindKey, false},
		{"an estate proved to hold none", false, nil, nil, true},
		{"rows that cannot vouch for the log's end", false,
			fmt.Errorf("%w: it holds a record it could not apply", iamdomain.ErrNotCurrent),
			iamdomain.ErrNotCurrent, false},
		{"rows that could not be read", false, errors.New("disk"), nil, false},
	} {
		order, provedAgainst = nil, 0
		err := judgeBlindKeyMint(t.Context(), logEnd(7, nil), holds(tc.held, tc.readErr))
		if !slices.Equal(order, []string{"end", "rows"}) {
			t.Errorf("%s: asked %v, want the log's end first and the rows after "+
				"— rows asked first can be proved against an end they never saw",
				tc.name, order)
		}
		if provedAgainst != 7 {
			t.Errorf("%s: the rows were proved against %d, want the end just read (7)",
				tc.name, provedAgainst)
		}
		switch {
		case tc.allowed:
			if err != nil {
				t.Errorf("%s: a fresh company's key was refused: %v", tc.name, err)
			}
		case err == nil:
			t.Errorf("%s: the mint was allowed", tc.name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: answered %v, want %v", tc.name, err, tc.want)
		}
	}
}
