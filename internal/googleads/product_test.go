package googleads_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/spendguard/spendguardtest"
)

const (
	customer = "1234567890"
	manager  = "1112223334"
	enabledC = "customers/1234567890/campaigns/1"
	pausedC  = "customers/1234567890/campaigns/2"
	budget1  = "customers/1234567890/campaignBudgets/10"
	budget2  = "customers/1234567890/campaignBudgets/20"
)

// fakeTokens mints a token naming its scopes, as the engine's issuer would.
type fakeTokens struct{}

func (fakeTokens) GoogleGrowthToken(_ context.Context, handle string, scopes []string, _ time.Duration,
	_ *http.Client) (googleauth.SeatToken, error) {
	return googleauth.SeatToken{Token: "ya29.seat-" + handle, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// fakeAudit is the access log: every step a product records.
type fakeAudit struct {
	mu    sync.Mutex
	steps []growth.Step
	recs  []growth.Record
}

func (a *fakeAudit) add(s growth.Step, r growth.Record) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steps, a.recs = append(a.steps, s), append(a.recs, r)
	return true, nil
}

func (a *fakeAudit) Made(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepMade, r)
}
func (a *fakeAudit) Applied(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepApplied, r)
}
func (a *fakeAudit) Void(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepVoid, r)
}
func (a *fakeAudit) Failed(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepFailed, r)
}

// fakeAds is one Google Ads account: an enabled campaign on a 100.00 budget, a
// paused one on a 30.00 budget, in EUR.
type fakeAds struct {
	mu        sync.Mutex
	campaigns map[string][2]string // status, budget
	budgets   map[string]int64
	calls     []string
	managers  []string
	writes    int
}

func newFakeAds() *fakeAds {
	return &fakeAds{
		campaigns: map[string][2]string{enabledC: {"ENABLED", budget1}, pausedC: {"PAUSED", budget2}},
		budgets:   map[string]int64{budget1: 100_000_000, budget2: 30_000_000},
	}
}

var inList = regexp.MustCompile(`IN \(([^)]*)\)`)

func (f *fakeAds) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	f.managers = append(f.managers, r.Header.Get("Login-Customer-Id"))
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	answer := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if !strings.Contains(r.URL.Path, "/customers/"+customer+"/") {
		w.WriteHeader(http.StatusForbidden)
		answer(map[string]any{"error": map[string]any{"code": 403, "status": "PERMISSION_DENIED", "message": "no access"}})
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/googleAds:search"):
		answer(map[string]any{"results": f.search(fmt.Sprint(body["query"]))})
	case strings.HasSuffix(r.URL.Path, "/campaignBudgets:mutate"):
		for _, op := range body["operations"].([]any) {
			u := op.(map[string]any)["update"].(map[string]any)
			var amount int64
			_, _ = fmt.Sscan(fmt.Sprint(u["amountMicros"]), &amount)
			f.budgets[u["resourceName"].(string)] = amount
		}
		f.writes++
		answer(map[string]any{"results": []any{}})
	case strings.HasSuffix(r.URL.Path, "/campaigns:mutate"):
		for _, op := range body["operations"].([]any) {
			u := op.(map[string]any)["update"].(map[string]any)
			c := f.campaigns[u["resourceName"].(string)]
			c[0] = fmt.Sprint(u["status"])
			f.campaigns[u["resourceName"].(string)] = c
		}
		f.writes++
		answer(map[string]any{"results": []any{}})
	default:
		w.WriteHeader(http.StatusNotFound)
		answer(map[string]any{"error": map[string]any{"code": 404, "status": "NOT_FOUND", "message": r.URL.Path}})
	}
}

// search answers the queries the product makes: the customer, the account's
// campaigns and budgets for capacity, and resources read by name.
func (f *fakeAds) search(q string) []any {
	named := func() []string {
		m := inList.FindStringSubmatch(q)
		if m == nil {
			return nil
		}
		var out []string
		for _, n := range strings.Split(m[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(n), "'"))
		}
		return out
	}
	var rows []any
	switch {
	case strings.Contains(q, "FROM customer"):
		rows = append(rows, map[string]any{"customer": map[string]any{"resourceName": "customers/" + customer,
			"descriptiveName": "Acme", "currencyCode": "EUR", "timeZone": "Europe/Berlin", "status": "ENABLED"}})
	case strings.Contains(q, "FROM campaign WHERE campaign.status != 'REMOVED'"):
		for name, c := range f.campaigns {
			rows = append(rows, map[string]any{
				"campaign":       map[string]any{"resourceName": name, "status": c[0], "campaignBudget": c[1]},
				"campaignBudget": map[string]any{"resourceName": c[1], "amountMicros": fmt.Sprint(f.budgets[c[1]])},
				"metrics":        map[string]any{"costMicros": "0"},
			})
		}
	case strings.Contains(q, "FROM campaign_budget"):
		for _, n := range named() {
			rows = append(rows, map[string]any{"campaignBudget": map[string]any{"resourceName": n,
				"amountMicros": fmt.Sprint(f.budgets[n])}})
		}
	case strings.Contains(q, "FROM campaign "):
		for _, n := range named() {
			c := f.campaigns[n]
			rows = append(rows, map[string]any{"campaign": map[string]any{"resourceName": n, "status": c[0],
				"campaignBudget": c[1], "biddingStrategyType": "MANUAL_CPC"}})
		}
	}
	return rows
}

// world is a product on a company whose seat growth-lead is at administer on
// one account, reached through its manager, capped at 50.00 a change and
// 200.00 a day.
type world struct {
	p     *googleads.Product
	f     *fakeAds
	audit *fakeAudit
}

func newWorld(t *testing.T) *world {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	doc := `
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
  google_ads:
    enabled: true
    customers: [{id: "` + customer + `", manager: "` + manager + `"}]
    spend:
      accounts: [{account: "` + customer + `", currency: EUR, per_change: "50.00", daily: "200.00"}]
roles:
  - name: Growth lead
    handle: growth-lead
    integrations:
      google_ads: {enabled: true, level: administer, customers: ["` + customer + `"]}
`
	company, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	env := config.NewResolver(config.MapSource{
		"GOOGLE_CLOUD_FEDERATION_KEY": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	f := newFakeAds()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	audit := &fakeAudit{}
	p := googleads.New(func() *config.Company { return company }, func() *config.Resolver { return env },
		fakeTokens{}, audit, &spendguardtest.Locks{}, nil, &http.Client{Transport: httpxtest.Rewrite(t, srv)})
	return &world{p: p, f: f, audit: audit}
}

// aboveTool is the framework's refusal of a change above its tool's class.
type aboveTool struct{ class growth.Class }

func (e *aboveTool) Error() string { return "the change is " + string(e.class) }

// call makes tool's call as the framework does (internal/agent/builtin): a
// read is run; any other call is prepared, its class held to its tool's and
// its binding to a digest, and applied on that binding, a held one as its
// approved held call.
func (w *world) call(t *testing.T, tool string, args map[string]any) (any, growth.Hold, error) {
	t.Helper()
	call := growth.Call{ID: "call-1", Handle: "growth-lead", Turn: "turn-1", Tool: tool, Args: args}
	def, ok := w.p.Catalog().Tool(tool)
	if !ok {
		t.Fatalf("no tool %s", tool)
	}
	if def.Class == growth.ClassRead {
		out, err := w.p.Run(t.Context(), call)
		return out, growth.Hold{}, err
	}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil {
		return nil, hold, err
	}
	if !hold.Class.Valid() || len(hold.Binding) != 64 {
		t.Fatalf("%s prepared a hold the framework refuses: %+v", tool, hold)
	}
	if hold.Class.Rank() > def.Class.Rank() {
		return nil, hold, &aboveTool{class: hold.Class}
	}
	if def.Class.Held() {
		call.ID, call.HeldCall = "", "held-1"
	}
	out, err := w.p.Apply(t.Context(), call, hold.Binding)
	return out, hold, err
}

// refused reports err as a refusal the agent is told.
func refused(err error) bool {
	var r *growth.Refusal
	return errors.As(err, &r)
}

func pathsCalled(f *fakeAds) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// A CALL OUTSIDE THE SEAT'S ACCOUNTS REACHES NO VENDOR: a query on an account
// the seat does not name, and a mutate on its own account whose operation
// names another account's resource, are refused before Google.
//
// Mutation: drop the resource walk from the scope check, and the operation
// reaching into another account is sent.
func TestACallOutsideTheSeatsAccountsReachesNoVendor(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if _, _, err := w.call(t, "google_ads_query", map[string]any{"customer": "9999999999",
		"query": "SELECT campaign.name FROM campaign"}); !refused(err) {
		t.Fatalf("a query on another account answered %v", err)
	}
	reach := map[string]any{"operation": "googleads.customers.adGroups.mutate",
		"path": map[string]any{"customersId": customer},
		"body": map[string]any{"operations": []any{map[string]any{"create": map[string]any{
			"campaign": "customers/9999999999/campaigns/7", "name": "x", "status": "PAUSED"}}}}}
	if _, _, err := w.call(t, "google_ads_operate", reach); !refused(err) || !strings.Contains(err.Error(), "9999999999") {
		t.Fatalf("an operation naming another account's campaign answered %v", err)
	}
	for _, c := range pathsCalled(w.f) {
		if strings.Contains(c, "9999999999") || strings.Contains(c, "adGroups:mutate") {
			t.Errorf("Google was asked %s", c)
		}
	}
}

// AN ACCOUNT REACHED THROUGH A MANAGER IS CALLED WITH ITS MANAGER, in the
// login-customer-id header the company's block names.
func TestAnAccountReachedThroughAManagerIsCalledWithIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if _, _, err := w.call(t, "google_ads_query", map[string]any{"customer": customer,
		"query": "SELECT customer.descriptive_name FROM customer"}); err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	if len(w.f.managers) == 0 || w.f.managers[len(w.f.managers)-1] != manager {
		t.Errorf("Google was called with managers %v, want %s", w.f.managers, manager)
	}
}

// PAUSING IS MADE AT ONCE AND ENABLING IS HELD: a pause comes to operate and
// is made and recorded; enabling the paused campaign comes to spend, which the
// operate tool may not make, with the 30.00 its budget adds to the day.
func TestPausingIsMadeAtOnceAndEnablingIsHeld(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if _, hold, err := w.call(t, "google_ads_pause", map[string]any{"resources": []any{enabledC}}); err != nil ||
		hold.Class != growth.ClassOperate {
		t.Fatalf("a pause answered %v as %s", err, hold.Class)
	}
	if w.f.campaigns[enabledC][0] != "PAUSED" {
		t.Fatal("the campaign was not paused")
	}
	enable := map[string]any{"operation": "googleads.customers.campaigns.mutate",
		"path": map[string]any{"customersId": customer},
		"body": map[string]any{"operations": []any{map[string]any{"update": map[string]any{"resourceName": pausedC,
			"status": "ENABLED"}, "updateMask": "status"}}}}
	if _, _, err := w.call(t, "google_ads_operate", enable); !errors.As(err, new(*aboveTool)) {
		t.Fatalf("enabling on the operate tool answered %v", err)
	}
	hold, err := w.p.Prepare(t.Context(), growth.Call{Handle: "growth-lead", Tool: "google_ads_spend", Args: enable})
	if err != nil || hold.Class != growth.ClassSpend || hold.Ask.Money == nil || hold.Ask.Money.DeltaMicros != 30_000_000 {
		t.Fatalf("enabling is held as %+v (money %+v), %v", hold.Class, hold.Ask.Money, err)
	}
}

// A SPEND OVER ITS CAPS IS REFUSED BEFORE ANYONE IS ASKED: a budget raised by
// more than the 50.00 a change allows is refused at Prepare.
//
// Mutation: skip the spend check at Prepare, and a person is asked to allow
// what the apply would refuse.
func TestASpendOverItsCapsIsRefusedBeforeAnyoneIsAsked(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, err := w.p.Prepare(t.Context(), growth.Call{Handle: "growth-lead", Tool: "google_ads_set_budget",
		Args: map[string]any{"budget": budget1, "amount": "160.00"}})
	if !refused(err) || !strings.Contains(err.Error(), "per-change") || w.f.writes != 0 {
		t.Fatalf("preparing a raise past the per-change cap answered %v (writes %d)", err, w.f.writes)
	}
}

// AN ALLOWED SPEND IS HELD TO THE CAPS AGAIN WHEN IT IS MADE: a raise within
// the caps when asked is refused at apply, and recorded as over its caps,
// once another budget has taken the day past the daily cap meanwhile.
//
// Mutation: apply outside the guard, and the day ends over its cap.
func TestAnAllowedSpendIsHeldToTheCapsAgainWhenItIsMade(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	args := map[string]any{"budget": budget1, "amount": "140.00"}
	call := growth.Call{Handle: "growth-lead", Tool: "google_ads_set_budget", Args: args}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil || hold.Ask.Money == nil || hold.Ask.Money.DailyAfterMicros != 140_000_000 {
		t.Fatalf("a raise within the caps is held as %+v, %v", hold.Ask.Money, err)
	}
	w.f.mu.Lock()
	c := w.f.campaigns[pausedC]
	c[0] = "ENABLED"
	w.f.campaigns[pausedC] = c
	w.f.budgets[budget2] = 90_000_000
	w.f.mu.Unlock()
	call.HeldCall = "held-1"
	if _, err := w.p.Apply(t.Context(), call, hold.Binding); !refused(err) || w.f.budgets[budget1] != 100_000_000 {
		t.Fatalf("an apply over the daily cap answered %v (budget %d)", err, w.f.budgets[budget1])
	}
	w.audit.mu.Lock()
	defer w.audit.mu.Unlock()
	last := w.audit.recs[len(w.audit.recs)-1]
	if w.audit.steps[len(w.audit.steps)-1] != growth.StepFailed || last.Failure != growth.FailureCapExceeded {
		t.Errorf("the refused apply is recorded as %s %q", w.audit.steps[len(w.audit.steps)-1], last.Failure)
	}
}
