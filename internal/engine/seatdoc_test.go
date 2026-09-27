package engine_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// A PERSON THROUGH THEIR OWN MACHINE TOKEN, so the chart's author and
	// its credential are two different values and a write recording one as
	// both is caught.
	const pat = "pat:0192f00d-0000-7000-8000-00000000000a"
	by := iam.Actor{Name: "jane.doe", Kind: iam.ActorHuman, OperatorID: pat}
	at, err := e.SetSeatDocument(t.Context(), handle, updated,
		"record a bot token", by)
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
	// THE CHART RECORDS THE PERSON AS THE AUTHOR, of their kind, WITH THE
	// TOKEN BESIDE THEM. It was handed one name and recorded it as both, and
	// the name was the credential's — so the seat's history named `pat:<id>`,
	// of kind operator, and nothing in it said whose token it was once the
	// token's row was swept. Mutation: author the record as the credential
	// and this fails.
	changes, _, err := e.Chart().History(t.Context(), 10, statelog.Freshness{
		Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var found bool
	for _, change := range changes {
		if change.Object.ID != handle {
			continue
		}
		found = true
		if change.Actor != by.Name || change.ActorKind != chart.AuthorHuman ||
			change.OperatorID != pat {
			t.Errorf("the chart records the write as %q (%s) through %q, want "+
				"jane.doe (human) through %s", change.Actor, change.ActorKind,
				change.OperatorID, pat)
		}
		break
	}
	if !found {
		t.Errorf("the chart's history holds no change to %s: %+v", handle, changes)
	}
	// AND THE PUBLIC HALF SURVIVED IT. A content record is full
	// post-state, so a write that carried only the block it edited would
	// have cleared the seat's name, its goal and everything else.
	if name, _ := seat["name"].(string); name != "" &&
		!strings.Contains(string(back), name) {

		t.Errorf("the write cleared the seat's public half: %s", back)
	}
}

// A WHOLE SEAT DOCUMENT HOLDING NO RUNTIME TAKES THE SEAT'S AWAY.
//
// Slack's disconnect reads the seat's document, deletes its `slack` block and
// writes the rest back. On a seat whose runtime half held nothing else, what is
// left encodes as NO runtime at all — and a content write that leaves the half
// out KEEPS the one the seat has, which is right for a lead editing a goal and
// wrong for a caller writing the seat back whole. So SetSeatDocument clears the
// half a document does not state; carried instead, the disconnect reported
// the app's credentials removed while the seat went on holding the bot token.
// Mutation: drop that clear and the seat keeps its `slack` block.
func TestASeatDocumentHoldingNoRuntimeClearsTheSeats(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	const handle = "ceo"
	by := iam.Actor{Name: "ops", Kind: iam.ActorOperator}
	write := func(doc any, summary string) {
		t.Helper()
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.SetSeatDocument(t.Context(), handle, body, summary, by); err != nil {
			t.Fatalf("SetSeatDocument (%s): %v", summary, err)
		}
	}

	// FIRST, A SEAT WHOSE RUNTIME HALF IS A SLACK APP AND NOTHING ELSE: the
	// document the engine serves for the seat's own row, with the half
	// swapped for one holding the block alone.
	if len(seatRuntime(t, e, handle)) == 0 {
		t.Fatalf("the fixture's %s holds no runtime half, so the clear below "+
			"would prove nothing", handle)
	}
	slackOnly, err := org.SeatRuntime(&org.Role{
		Slack: org.SlackIdentity{BotToken: "${SLACK_BOT_TOKEN_CEO}"},
	})
	if err != nil || len(slackOnly) == 0 {
		t.Fatalf("encode a slack-only runtime half: %v (%s)", err, slackOnly)
	}
	detail, err := e.Chart().Seat(t.Context(), handle, statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		t.Fatalf("read the seat: %v", err)
	}
	row := detail.Seat
	row.Runtime = slackOnly
	body, err := json.Marshal(org.SeatFrom(row, detail.Manages))
	if err != nil {
		t.Fatal(err)
	}
	write(json.RawMessage(body), "connect slack")
	if got := seatRuntime(t, e, handle); !sameJSON(t, got, slackOnly) {
		t.Fatalf("the seat's runtime half is %s, want the slack block alone: %s",
			got, slackOnly)
	}

	// THEN THE DISCONNECT'S OWN EDIT: the block deleted, the rest sent back.
	doc := seatDocumentOf(t, e, handle)
	if _, held := doc["slack"]; !held {
		t.Fatalf("the seat's document carries no slack block to delete: %v", doc)
	}
	delete(doc, "slack")
	write(doc, "disconnect slack")
	if got := seatRuntime(t, e, handle); len(got) != 0 {
		t.Errorf("a document holding no runtime left the seat's own in place: %s", got)
	}
}

// sameJSON reports whether two JSON values are equal as values.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

// seatDocumentOf is one seat's whole document, decoded.
func seatDocumentOf(t *testing.T, e *engine.Engine, handle string) map[string]any {
	t.Helper()
	body, err := e.SeatDocument(t.Context(), handle)
	if err != nil {
		t.Fatalf("SeatDocument: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return doc
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
		[]byte(`{"name":"X"}`), "s", iam.Actor{Name: "o", Kind: iam.ActorOperator}); err == nil {

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
