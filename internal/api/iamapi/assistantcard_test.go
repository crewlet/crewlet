package iamapi_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
)

// THE SEATS CARD READS THE SEATS ANSWER: get_org_chart's seats view is drawn
// from GET /iam/seats, which this surface serves, so its card is held here to
// the answer this handler gives an administrator, as internal/api's
// TestEveryReadCardReadsItsRoutesOwnAnswer holds every other read to its own.
// A member the handler leaves out for this seat (`omitempty`) is held to the
// row's own type, which must hold every key a real row carries. Mutation:
// point the card's rows or a column at a key the answer does not hold, and
// this names it.
func TestTheSeatsCardReadsTheSeatsAnswer(t *testing.T) {
	t.Parallel()
	tool, ok := catalog.Lookup("get_org_chart")
	if !ok {
		t.Fatal("the catalogue holds no get_org_chart")
	}
	args, reached := assistanttest.CardVariants(t, tool)["seats"]
	if !reached {
		t.Fatal("no example arguments reach get_org_chart's seats card")
	}
	req, err := tool.Request(args)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodGet || req.Path != "/iam/seats" {
		t.Fatalf("get_org_chart's seats view sends %s %s, not GET /iam/seats", req.Method,
			req.Path)
	}
	got := newRig(t).as(administrator(), req.Method, req.Path, nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/seats answered %d %v", got.status, got.body)
	}
	held, _ := assistanttest.HoldCard(t, "get_org_chart/seats", tool.Cards["seats"],
		got.body,
		assistanttest.ValueTypes{"row": reflect.TypeFor[iamapi.SeatRow]()})
	if held == 0 {
		t.Error("the seats card reads no path; the walk is not reading its spec")
	}
}
