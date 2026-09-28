package api_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The broker panel's lists the dashboard keeps its own copy of, held against
// the engine's — see internal/clientsource for why a copy exists at all and why
// this side checks it.

// EVERY BROKER KIND A ROW CAN CARRY HAS A RENDERING, AND EVERY RENDERING A KIND —
// `unknown` among them, which is what the engine renders a row that did not
// say as, and the one reading the panel marks.
func TestTheDashboardKnowsEveryBrokerKind(t *testing.T) {
	t.Parallel()
	want := []string{placement.BrokerUnknown.String()}
	for _, kind := range placement.BrokerKinds() {
		want = append(want, kind.String())
	}
	holdStrings(t, `export const BROKER_KINDS = \[([^\]]*)\] as const;`, want, "broker kind")
}

// AND EVERY DISAGREEMENT THE ENGINE NAMES. A kind the panel does not know is
// shown by its bare name, which tells an operator nothing about which of the
// two records is wrong; the other direction is a branch nothing reaches.
func TestTheDashboardKnowsEveryBrokerFinding(t *testing.T) {
	t.Parallel()
	var want []string
	for _, kind := range engine.BrokerFindingKinds() {
		want = append(want, string(kind))
	}
	holdStrings(t, `export const BROKER_FINDING_KINDS = \[([^\]]*)\] as const;`, want,
		"broker finding")
}

// THE REMOVAL DIALOG WAITS PAST THE NODE'S OWN WAIT ON THE MEMBER CARRYING IT:
// a dialog that gave up first would report a removal the metadata group went
// on to commit as failed.
func TestTheDashboardWaitsPastABrokerRemoval(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`export const BROKER_REMOVE_TIMEOUT_MS = ([0-9_]+);`)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("BROKER_REMOVE_TIMEOUT_MS = %q is not a number: %v", body, err)
	}
	if wait := time.Duration(ms) * time.Millisecond; wait <= engine.BrokerRemoveWait() {
		t.Errorf("the dashboard waits %s for a removal the node waits %s on", wait,
			engine.BrokerRemoveWait())
	}
}

// AND THE SOCKET'S QUERY WAITS PAST THE NODE'S READ OF THE GROUP. The panel
// asks the fleet_broker question over the socket, and the node spends up to
// [engine.BrokerReadWait] asking members for the metadata group before it can
// answer: a query that gave up first would draw an unread group as a failed
// panel on exactly the fleet — a member wedged inside its lease — the panel
// exists for.
func TestTheDashboardWaitsPastAReadOfTheGroup(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`const QUERY_TIMEOUT_MS = ([0-9_]+);`)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("QUERY_TIMEOUT_MS = %q is not a number: %v", body, err)
	}
	if wait := time.Duration(ms) * time.Millisecond; wait <= engine.BrokerReadWait() {
		t.Errorf("the dashboard waits %s for a query the node may spend %s reading "+
			"the group for", wait, engine.BrokerReadWait())
	}
}

// holdStrings compares the dashboard's declared list with the engine's, both ways.
func holdStrings(t *testing.T, pattern string, want []string, what string) {
	t.Helper()
	body, err := clientsource.Declaration(clientsource.Tree(t), pattern)
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatalf("the dashboard declares no %s at all, so this gate certifies nothing", what)
	}
	for _, v := range want {
		if !slices.Contains(client, v) {
			t.Errorf("the engine sends the %s %q and the dashboard's list (%v) does not "+
				"name it", what, v, client)
		}
	}
	for _, v := range client {
		if !slices.Contains(want, v) {
			t.Errorf("the dashboard names the %s %q, which the engine never sends (it "+
				"sends %v)", what, v, want)
		}
	}
}
