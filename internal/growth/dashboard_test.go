package growth_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
)

// THE DASHBOARD READS THE GROWTH WIRE AS THE ENGINE WRITES IT: the classes,
// acts, levels and account states it draws, and every object it reads (an
// account, a connection, a vendor role, a held call's card and a spending
// product's caps), held both ways: every field the dashboard declares is sent,
// and every field the engine sends is declared. Each value is written with
// every field set, so an optional field is in the answer and in the check.
//
// Mutation: drop `moved` from GrowthCard, add a field to growth.Account that
// GrowthAccount does not declare, or send a card's empty findings as null,
// and this is red.
func TestTheDashboardReadsTheGrowthWire(t *testing.T) {
	t.Parallel()
	holdUnion(t, "GrowthClass", growth.Classes())
	var acts []string
	for _, c := range growth.Classes() {
		if c.Held() {
			acts = append(acts, string(c.Act()))
		}
	}
	holdUnion(t, "GrowthAct", acts)
	holdUnion(t, "GrowthLevel", growth.Levels())
	holdUnion(t, "GrowthAccountState", growth.AccountStates())

	account := growth.Account{Kind: "google_ads", Connection: "main", ID: "1234567890", Name: "Nimbus",
		Currency: "EUR", TimeZone: "Europe/Berlin", Detail: "com.example.nimbus", State: growth.AccountServing,
		Roles: []string{"standard"}}
	holdObject(t, "GrowthAccount", account)
	holdObject(t, "GrowthConnection", growth.Connection{ID: "main", Name: "Nimbus", Identity: "reader@example.com",
		State: "connected", RolesHeld: []string{"standard"}, Accounts: []growth.Account{account}, Open: true,
		Unread: "Add a key that lists accounts"})
	holdObject(t, "GrowthRoleOffer", growth.RoleOffer{ID: "standard", Name: "Standard", Note: "Changes campaigns.",
		Classes: []growth.Class{growth.ClassRead}, Levels: []growth.Level{growth.LevelRead}})
	change := growth.CardChange{Field: "amount_micros", Label: "Daily budget", Before: "1", After: "2", Money: true}
	act := growth.CardAct{Target: "Spring sale", Summary: "Raise the daily budget", Changes: []growth.CardChange{change},
		Danger: true, Text: "Thanks", TextLimit: 350, TargetExcerpt: "Great", TargetRating: 5, TargetAuthor: "A reader"}
	money := growth.CardMoney{Currency: "EUR", DeltaMicros: 1, DailyMicros: 2, DailyAfterMicros: 3,
		PerChangeCapMicros: 4, DailyCapMicros: 5, Overdelivery: "Vendors may spend past a daily budget."}
	holdObject(t, "GrowthCardChange", change)
	holdObject(t, "GrowthCardAct", act)
	holdObject(t, "GrowthCardMoney", money)
	holdObject(t, "GrowthCard", growth.Card{Act: growth.ClassSpend.Act(), Product: "google_ads", Tool: "google_ads_budget",
		Account: account, Acts: []growth.CardAct{act}, Money: &money, Findings: []string{"policy"}, Reason: "why",
		LapsesAt: time.Unix(0, 0).UTC(), Moved: true, OtherOpenAsks: 1})
	// THE CARD IS READ AS SENT, so a card built with nothing in a list (no
	// findings, an act with no fields) still sends the list, never null.
	holdZero(t, "GrowthCard", growth.Card{})
	holdZero(t, "GrowthCardAct", growth.CardAct{})
	limit := config.GrowthCap{Account: "1234567890", Currency: "EUR", PerChange: "50.00", Daily: "200.00"}
	holdObject(t, "GrowthSpendAccount", limit)
	holdObject(t, "GrowthSpend", config.GrowthSpend{Accounts: []config.GrowthCap{limit}})
}

// holdUnion holds a dashboard union to the engine's closed set.
func holdUnion[S ~string](t *testing.T, union string, want []S) {
	t.Helper()
	got, err := clientsource.Union(clientsource.Tree(t), union)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	words := make([]string, 0, len(want))
	for _, w := range want {
		words = append(words, string(w))
	}
	if slices.Sort(got); !slices.Equal(got, slices.Sorted(slices.Values(words))) {
		t.Errorf("%s declares %v, the engine sends %v", union, got, words)
	}
}

// holdObject holds a dashboard interface to a value as the engine writes it,
// both ways; a required member must be in the answer.
func holdObject(t *testing.T, iface string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	members, err := clientsource.Interface(clientsource.Tree(t), iface)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	declared := map[string]bool{}
	for _, m := range members {
		declared[m.Name] = true
	}
	for _, field := range slices.Sorted(maps.Keys(declared)) {
		if _, ok := sent[field]; !ok {
			t.Errorf("%s declares %s and the engine does not send it", iface, field)
		}
	}
	for _, field := range slices.Sorted(maps.Keys(sent)) {
		if !declared[field] {
			t.Errorf("the engine sends %s.%s and %s does not declare it", iface, field, iface)
		}
	}
}

// holdZero holds the zero value of a type the dashboard reads as sent: every
// required member is in the answer, and null only where it is declared so.
func holdZero(t *testing.T, iface string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	members, err := clientsource.Interface(clientsource.Tree(t), iface)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	for _, m := range members {
		if m.Optional {
			continue
		}
		v, ok := sent[m.Name]
		switch {
		case !ok:
			t.Errorf("an empty %s leaves out %s, which the dashboard requires", iface, m.Name)
		case v == nil && !m.Nullable:
			t.Errorf("an empty %s sends %s as null, which the dashboard reads as a value", iface, m.Name)
		}
	}
}
