package stream_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD KNOWS THE REACH OF EVERY KIND THIS SOCKET BROADCASTS.
//
// The person shell reads only what a member is pushed — the dashboard's
// `app/shell.test.ts` refuses a person screen that reads a store slice fed
// by a kind above `member` — and the classification it judges by is
// [stream.KindReach], declared on the client as `PUSH_REACH`
// (`contract/reach.ts`). Held here BOTH WAYS over every [stream.Kind]
// constant: a broadcast kind the table does not name is one the gate cannot
// judge, a reach that differs is the gate judging by a classification the hub
// no longer makes, and a row for a kind that is never broadcast (a query's
// answer, the pong) or no longer exists is a decision nobody made.
func TestTheDashboardKnowsEveryPushKindsReach(t *testing.T) {
	t.Parallel()
	table, err := clientsource.Literal(clientsource.Tree(t), "PUSH_REACH")
	if err != nil {
		t.Fatal(err)
	}
	named, err := clientsource.Keys(table)
	if err != nil {
		t.Fatal(err)
	}
	if len(named) < 5 {
		t.Fatalf("PUSH_REACH names %d kinds; a reader that stopped reading it "+
			"would certify nothing", len(named))
	}
	var broadcast []string
	for _, kind := range engineKinds(t) {
		reach, pushed := stream.KindReach(stream.Kind(kind))
		if !pushed {
			if slices.Contains(named, kind) {
				t.Errorf("PUSH_REACH names %q, which the hub never broadcasts — "+
					"it is sent to one client, and has no reach to judge", kind)
			}
			continue
		}
		broadcast = append(broadcast, kind)
		if !slices.Contains(named, kind) {
			t.Errorf("the hub broadcasts %q at %q and PUSH_REACH does not name it, "+
				"so the person shell's gate cannot judge a screen that reads it",
				kind, reach)
			continue
		}
		got := clientsource.Field(table, kind)
		if len(got) != 1 || got[0] != string(reach) {
			t.Errorf("PUSH_REACH says %q is pushed at %q and the hub pushes it at %q",
				kind, got, reach)
		}
	}
	if len(broadcast) == 0 {
		t.Fatal("no kind is broadcast at all, so this certifies nothing")
	}
	for _, kind := range named {
		if !slices.Contains(engineKinds(t), kind) {
			t.Errorf("PUSH_REACH names %q and no stream.Kind constant spells it — a "+
				"renamed kind leaves exactly this behind", kind)
		}
	}
}
