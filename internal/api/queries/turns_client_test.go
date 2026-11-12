package queries_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/store"
)

// THE TURNS TAB SAYS THE WINDOW THE ENGINE READS: a seat's Turns tab asks for
// the longest window the turns list reads ([store.MaxTurnDays]) and says
// "last N days". A figure above it says more history than any read returns;
// one below hides turns the engine still holds.
//
// Mutation: move either figure and this fails.
func TestTheTurnsTabSaysTheWindowTheEngineReads(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "TURNS_MAX_DAYS")
	if err != nil {
		t.Fatal(err)
	}
	if got != strconv.Itoa(store.MaxTurnDays) {
		t.Errorf("the dashboard's TURNS_MAX_DAYS is %s and the turns list reads %d days: "+
			"change it in contract/wire.ts", got, store.MaxTurnDays)
	}
}
