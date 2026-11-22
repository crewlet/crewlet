package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// growthLog is the access log's growth records, keeping what it was asked.
type growthLog struct {
	rows  []accessdomain.GrowthRow
	asked []accessdomain.GrowthQuery
}

func (l *growthLog) Changes(_ context.Context, q accessdomain.GrowthQuery,
	_ statelog.Freshness) (accessdomain.GrowthPage, error) {
	l.asked = append(l.asked, q)
	if q.Product == "Fake Ads" {
		return accessdomain.GrowthPage{}, accessdomain.ErrInvalid
	}
	return accessdomain.GrowthPage{Rows: l.rows, Next: "41", Complete: true}, nil
}

// THE RECENT AGENT CHANGES ARE AN ADMINISTRATOR'S, as the company-wide trail
// is: a person holding no admin is refused before the log is read, an
// administrator is answered the page narrowed as asked, money as exact decimal
// strings and a published text as its digest, and a product that is no kind is
// a bad parameter.
//
// Mutation: register the question as everybody's and a person without admin
// reads every change an agent made to the company's ad accounts.
func TestTheRecentAgentChangesAreAnAdministratorsRead(t *testing.T) {
	t.Parallel()
	log := &growthLog{rows: []accessdomain.GrowthRow{{Handle: "growth", HeldCall: "h-1", Product: "fakeads",
		Connection: "eu", Account: "act_1", Target: "c1", Tool: "fakeads_raise_budget", Class: growth.ClassSpend,
		Step: growth.StepApplied, Operation: "ab",
		Changes: []accessdomain.GrowthChange{{Field: "daily_budget", Label: "Daily budget", Before: "50000000",
			After: "60000000", Money: true}, {Field: "reply", Text: true, AfterSHA256: "cd", AfterBytes: 12}},
		Money: &accessdomain.GrowthMoney{Currency: "EUR", DeltaMicros: 9_007_199_254_740_993},
		At:    time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Growth: log, Chart: authz.NoChart{}})
	if _, err := r.Answer(seated(t, "bob", iam.GrantWorklet), "growth_changes", nil); !errors.Is(err,
		queries.ErrUnauthorized) {
		t.Errorf("a person without admin asking the changes: %v, want refused", err)
	}
	if len(log.asked) != 0 {
		t.Fatalf("a refused read reached the log: %+v", log.asked)
	}
	got, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "growth_changes",
		map[string]any{"product": "fakeads", "cursor": "99", "limit": "10"})
	if err != nil {
		t.Fatalf("an administrator asking the changes: %v", err)
	}
	if want := (accessdomain.GrowthQuery{Product: "fakeads", Cursor: "99", Limit: 10}); len(log.asked) != 1 ||
		log.asked[0] != want {
		t.Errorf("the log was asked %+v, want %+v", log.asked, want)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Rows []struct {
			Act, Seat, Step string
			Held            bool
			Changes         []map[string]any
			Money           map[string]string
		}
		Next     string
		Complete bool
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Next != "41" || !page.Complete {
		t.Fatalf("the page answered %s", raw)
	}
	row := page.Rows[0]
	if row.Act != "h-1" || !row.Held || row.Seat != "growth" || row.Step != "applied" ||
		row.Money["delta_micros"] != "9007199254740993" || row.Changes[0]["after"] != "60000000" ||
		row.Changes[1]["after"] != nil || row.Changes[1]["after_sha256"] != "cd" {
		t.Errorf("the row answered %s", raw)
	}
	if _, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "growth_changes",
		map[string]any{"product": "Fake Ads"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a product that is no kind: %v, want bad params", err)
	}
}
