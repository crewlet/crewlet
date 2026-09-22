package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// TestASeatIsReadAndWrittenThroughTheChart is the seam a provisioning pass
// reaches the company through, and the one that broke silently when the chart
// stopped being part of the stored revision.
//
// The three writers that implement it — the reconcile loop's, the setup
// surface's and the command line's — all went through /config's entity route,
// where a post-split revision carries no seats at all. Every one of them
// found nothing and reported "no such seat" about a seat the company plainly
// has.
func TestASeatIsReadAndWrittenThroughTheChart(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	handle := anyAgentHandle(t, e)

	body, err := e.SeatDocument(t.Context(), handle)
	if err != nil {
		t.Fatalf("SeatDocument: %v", err)
	}
	var seat map[string]any
	if err := json.Unmarshal(body, &seat); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}

	// A VENDOR BLOCK AT THE TOP LEVEL, which is the whole shape change: the
	// chart's blob holds the RUNTIME seat, so a pass edits `slack` rather
	// than `integrations.slack`. A pass written against the authored shape
	// writes a key nothing reads.
	seat["slack"] = map[string]any{"bot_token": "${SLACK_BOT_TOKEN_TEST}"}
	updated, err := json.Marshal(seat)
	if err != nil {
		t.Fatal(err)
	}
	at, err := e.SetSeatDocument(t.Context(), handle, updated,
		"record a bot token", "test-operator")
	if err != nil {
		t.Fatalf("SetSeatDocument: %v", err)
	}
	// A POSITION RATHER THAN A REVISION, because this write makes no
	// revision: a caller reporting one would name a document it did not
	// touch.
	if at.Stream == "" {
		t.Errorf("the write reported no position: %+v", at)
	}

	// AND IT READS BACK. A write that landed on the log and not in the rows
	// is one this node cannot see, which is exactly what a pass's next
	// round would rediscover as missing.
	back, err := e.SeatDocument(t.Context(), handle)
	if err != nil {
		t.Fatalf("SeatDocument after the write: %v", err)
	}
	if !strings.Contains(string(back), "${SLACK_BOT_TOKEN_TEST}") {
		t.Fatalf("the seat does not carry what was written: %s", back)
	}
	// AND THE PUBLIC HALF SURVIVED IT. A content record is full
	// post-state, so a write that carried only the block it edited would
	// have cleared the seat's name, its goal and everything else.
	if name, _ := seat["name"].(string); name != "" &&
		!strings.Contains(string(back), name) {

		t.Errorf("the write cleared the seat's public half: %s", back)
	}
}

// A SEAT NOBODY HOLDS IS NAMED, rather than answered with an empty document.
//
// An empty document written back would CLEAR the seat it named, because a
// content record is full post-state — so a read that answered one for a typo
// would turn a misspelled handle into a wiped seat.
func TestAnUnknownSeatIsNamedRatherThanAnsweredEmpty(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	_, err := e.SeatDocument(t.Context(), "nobody-called-this")
	if err == nil {
		t.Fatal("a seat nobody holds was answered")
	}
	if !strings.Contains(err.Error(), "nobody-called-this") {
		t.Errorf("the refusal does not name the seat: %v", err)
	}
	if _, err := e.SetSeatDocument(t.Context(), "nobody-called-this",
		[]byte(`{"name":"X"}`), "s", "o"); err == nil {

		t.Fatal("a write to a seat nobody holds was accepted")
	}
}

// anyAgentHandle is one seat this engine's company actually runs.
func anyAgentHandle(t *testing.T, e *engine.Engine) string {
	t.Helper()
	company := e.Company()
	if company == nil || company.Org == nil {
		t.Fatal("this engine runs no company")
	}
	for role := range company.Org.AllRoles() {
		return role.Handle()
	}
	t.Fatal("this company has no seats, so the case would certify nothing")
	return ""
}
