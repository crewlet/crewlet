package setupapi_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE SEAT ACCESS CARD READS ITS ROUTE'S OWN ANSWER: get_seat_access is asked
// through this surface, which internal/api's catalogue walk does not stand up,
// and every path its card reads must be in the lane view the route answers
// (a member this company leaves empty is held to the view's Go type). Mutation:
// read the status from `status_word`, or the grant from `desired`, and this
// names the path.
func TestTheSeatAccessCardReadsItsRoutesOwnAnswer(t *testing.T) {
	t.Parallel()
	tool, ok := catalog.Lookup("get_seat_access")
	if !ok {
		t.Fatal("the catalogue holds no get_seat_access")
	}
	lane := string(seataccess.LaneDatadog)
	req, err := tool.Request(map[string]any{"handle": "sre-lead", "integration": lane})
	if err != nil {
		t.Fatal(err)
	}
	s := newAccessSurface(t)
	res := s.do(t, req.Method, req.Path, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("%s %s answered %d %s", req.Method, req.Path, res.Code, res.Body)
	}
	var answer any
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	held, _ := assistanttest.HoldCard(t, "get_seat_access/"+lane, tool.Cards[lane], answer,
		assistanttest.ValueTypes{"record": reflect.TypeFor[seataccess.LaneView](),
			"item": reflect.TypeFor[seataccess.Step]()})
	if held < 10 {
		t.Errorf("the seat access card held %d paths; the walk is not reading its spec", held)
	}
}

// THE CATALOGUE LOOKUP'S TABLE READS ITS ROUTE'S OWN ANSWER. Mutation: read a
// column from `available` and this names the path.
func TestTheCatalogueLookupReadsItsRoutesOwnAnswer(t *testing.T) {
	t.Parallel()
	tool, ok := catalog.Lookup("search_grant_catalogue")
	if !ok {
		t.Fatal("the catalogue holds no search_grant_catalogue")
	}
	req, err := tool.Request(map[string]any{"handle": "sre", "integration": "cloudflare", "q": "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Query.Get("permission") != "origin" {
		t.Fatalf("the lookup asks %s?%s, without the permission it looks up", req.Path, req.Query.Encode())
	}
	s := newCloudflareSurface(t)
	res := s.do(t, req.Method, req.Path+"?"+req.Query.Encode(), "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("%s %s answered %d %s", req.Method, req.Path, res.Code, res.Body)
	}
	var answer any
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	held, _ := assistanttest.HoldCard(t, "search_grant_catalogue", tool.Cards[""], answer,
		assistanttest.ValueTypes{"row": reflect.TypeFor[seataccess.LookupEntry]()})
	if held < 8 {
		t.Errorf("the lookup's table held %d paths; the walk is not reading its spec", held)
	}
}
