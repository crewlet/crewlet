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
	"github.com/crewlet/crewlet/internal/tools/toolargs"
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

// fakeAudit is the access log: every step a product records, and a held
// call's one answer, after which another is not its first.
type fakeAudit struct {
	mu       sync.Mutex
	steps    []growth.Step
	recs     []growth.Record
	answered map[string]bool
}

func (a *fakeAudit) add(s growth.Step, r growth.Record) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s == growth.StepAllowed || s == growth.StepDenied || s == growth.StepLapsed {
		if a.answered[r.HeldCall] {
			return false, nil
		}
		if a.answered == nil {
			a.answered = map[string]bool{}
		}
		a.answered[r.HeldCall] = true
	}
	a.steps, a.recs = append(a.steps, s), append(a.recs, r)
	return true, nil
}

func (a *fakeAudit) Made(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepMade, r)
}
func (a *fakeAudit) Verdict(_ context.Context, s growth.Step, r growth.Record) (bool, error) {
	return a.add(s, r)
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
// paused one on a 30.00 budget, in EUR, and a batch job. trace notes each
// search, write and lease, in order.
type fakeAds struct {
	mu        sync.Mutex
	campaigns map[string][2]string // status, budget
	budgets   map[string]int64
	calls     []string
	managers  []string
	writes    int
	trace     []string
	// totals are the budgets that are total budgets, by their whole amount.
	totals map[string]int64
	// portfolio is the portfolio strategy each campaign runs on, if any.
	portfolio map[string]string
	// recs are the recommendations, each a type and the budget it targets.
	recs map[string][2]string
	// batchOps is how many operations the batch job holds.
	batchOps int
	// brokenCustomer answers the customer's own search in a shape no search
	// answer has.
	brokenCustomer bool
}

// note adds what happened to the trace.
func (f *fakeAds) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trace = append(f.trace, s)
}

// leases is the engine's growth act lease, in process, noting each hold and
// release on the fake's trace.
type leases struct {
	locks spendguardtest.Locks
	f     *fakeAds
}

func (l *leases) Hold(ctx context.Context, resource string) (func(), error) {
	release, err := l.locks.Hold(ctx, resource)
	if err != nil {
		return nil, err
	}
	l.f.note("hold " + resource)
	return func() { l.f.note("release " + resource); release() }, nil
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
	if strings.Contains(r.URL.Path, "/customers/") && !strings.Contains(r.URL.Path, "/customers/"+customer+"/") {
		w.WriteHeader(http.StatusForbidden)
		answer(map[string]any{"error": map[string]any{"code": 403, "status": "PERMISSION_DENIED", "message": "no access"}})
		return
	}
	query := fmt.Sprint(body["query"])
	if from := strings.Index(query, " FROM "); from >= 0 {
		f.trace = append(f.trace, "search"+query[from:])
	} else if r.Method == http.MethodPost {
		f.trace = append(f.trace, "write "+r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/googleAds:search") && f.brokenCustomer && strings.Contains(query, "FROM customer"):
		_, _ = w.Write([]byte(`{"results": "no rows"}`))
	case strings.HasSuffix(r.URL.Path, "/googleAds:search"):
		answer(map[string]any{"results": f.search(query)})
	case strings.HasSuffix(r.URL.Path, ":listPlannableUserLists"):
		answer(map[string]any{"plannableUserLists": []any{}})
	case strings.HasSuffix(r.URL.Path, ":run"):
		f.writes++
		answer(map[string]any{"name": "customers/" + customer + "/operations/1"})
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
			if status, ok := u["status"]; ok {
				c[0] = fmt.Sprint(status)
			}
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
			b := map[string]any{"resourceName": n, "amountMicros": fmt.Sprint(f.budgets[n]),
				"referenceCount": fmt.Sprint(f.uses(n))}
			if total, ok := f.totals[n]; ok {
				b["totalAmountMicros"] = fmt.Sprint(total)
			}
			rows = append(rows, map[string]any{"campaignBudget": b})
		}
	case strings.Contains(q, "FROM recommendation"):
		for _, n := range named() {
			rows = append(rows, map[string]any{"recommendation": map[string]any{"resourceName": n,
				"type": f.recs[n][0], "campaignBudget": f.recs[n][1]}})
		}
	case strings.Contains(q, "FROM batch_job"):
		for _, n := range named() {
			rows = append(rows, map[string]any{"batchJob": map[string]any{"resourceName": n, "status": "PENDING",
				"metadata": map[string]any{"operationCount": fmt.Sprint(f.batchOps)}}})
		}
	case strings.Contains(q, "FROM campaign "):
		for _, n := range named() {
			c := f.campaigns[n]
			campaign := map[string]any{"resourceName": n, "status": c[0], "campaignBudget": c[1],
				"biddingStrategyType": "MANUAL_CPC"}
			if p := f.portfolio[n]; p != "" {
				campaign["biddingStrategyType"], campaign["biddingStrategy"] = "TARGET_CPA", p
			}
			rows = append(rows, map[string]any{"campaign": campaign})
		}
	}
	return rows
}

// uses is how many campaigns run on budget, as Google's reference_count
// counts them.
func (f *fakeAds) uses(budget string) int {
	n := 0
	for _, c := range f.campaigns {
		if c[1] == budget {
			n++
		}
	}
	return n
}

// world is a product on a company whose seat growth-lead is at administer on
// one account, reached through its manager, capped at 50.00 a change and
// 200.00 a day.
type world struct {
	p       *googleads.Product
	f       *fakeAds
	audit   *fakeAudit
	held    int
	company *config.Company
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
      identity_project: acme-crewlet-id
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
		fakeTokens{}, audit, &leases{f: f}, &http.Client{Transport: httpxtest.Rewrite(t, srv)})
	return &world{p: p, f: f, audit: audit, company: company}
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
		w.held++
		call.ID, call.HeldCall, call.Approver = "", fmt.Sprintf("held-%d", w.held), "founder"
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

// taken is the access log's steps, as words.
func (a *fakeAudit) taken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.steps))
	for i, s := range a.steps {
		out[i] = string(s)
	}
	return strings.Join(out, ",")
}

// A HELD ACT RECORDS ITS ONE ANSWER AND ACTS ONLY ON IT: a lapse and a deny
// are recorded as the act's answer, an allow arriving after the lapse sends
// nothing, and an allow is recorded before its write.
//
// Mutation: record nothing on Denied, or apply without recording the allow
// first, and the log holds no answer or the lapsed call is made.
func TestAHeldActRecordsItsOneAnswerAndActsOnlyOnIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	call := growth.Call{Handle: "growth-lead", Tool: "google_ads_set_budget",
		Args: map[string]any{"budget": budget1, "amount": "140.00"}}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	answered := func(held, by string) growth.Call {
		c := call
		c.HeldCall, c.Approver = held, by
		return c
	}
	if err := w.p.Denied(t.Context(), answered("held-1", growth.EngineActor)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.p.Apply(t.Context(), answered("held-1", "founder"), hold.Binding); !refused(err) {
		t.Fatalf("an allow after the lapse answered %v", err)
	}
	if err := w.p.Denied(t.Context(), answered("held-2", "founder")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.p.Apply(t.Context(), answered("held-3", "founder"), hold.Binding); err != nil {
		t.Fatal(err)
	}
	if got := w.audit.taken(); got != "lapsed,denied,allowed,applied" {
		t.Errorf("the log holds %s, want lapsed,denied,allowed,applied", got)
	}
	w.audit.mu.Lock()
	denier, allower := w.audit.recs[1].Answerer, w.audit.recs[2].Answerer
	w.audit.mu.Unlock()
	if denier != "founder" || allower != "founder" {
		t.Errorf("the answers name %q and %q, want the administrator", denier, allower)
	}
	if w.f.writes != 1 || w.f.budgets[budget1] != 140_000_000 {
		t.Errorf("Google was written %d times, the budget is %d", w.f.writes, w.f.budgets[budget1])
	}
}

// EVERY WRITE HOLDS THE ACCOUNT'S ONE LEASE, AND A SPEND IS HELD TO THE CAPS
// INSIDE IT: an approved raise reads its target, the account's capacity and
// writes under the write lease alone, as an approved publish and a pause do.
//
// Mutation: apply a spend through the guard's own lease, or outside the
// client's, and it holds two leases or reads capacity unleased.
func TestEveryWriteHoldsTheAccountsOneLease(t *testing.T) {
	t.Parallel()
	const lease = "write/google_ads/customers/" + customer
	w := newWorld(t)
	if _, _, err := w.call(t, "google_ads_set_budget", map[string]any{"budget": budget1, "amount": "120.00"}); err != nil {
		t.Fatal(err)
	}
	publish := map[string]any{"operation": "googleads.customers.campaigns.mutate",
		"path": map[string]any{"customersId": customer},
		"body": map[string]any{"operations": []any{map[string]any{"update": map[string]any{"resourceName": pausedC,
			"brandGuidelines": map[string]any{"mainColor": "#000000"}}, "updateMask": "brandGuidelines"}}}}
	if _, hold, err := w.call(t, "google_ads_publish", publish); err != nil || hold.Class != growth.ClassPublic {
		t.Fatalf("a publish answered %v as %s", err, hold.Class)
	}
	if _, _, err := w.call(t, "google_ads_pause", map[string]any{"resources": []any{enabledC}}); err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	trace := strings.Join(w.f.trace, "\n")
	w.f.mu.Unlock()
	acts := strings.Split(trace, "release "+lease)
	if len(acts) != 4 || strings.Count(trace, "hold ") != 3 {
		t.Fatalf("three writes held %d leases:\n%s", strings.Count(trace, "hold "), trace)
	}
	for i, act := range acts[:3] {
		under := act[strings.LastIndex(act, "hold "+lease):]
		if !strings.Contains(under, "\nwrite ") || !strings.Contains(under, "\nsearch FROM campaign") {
			t.Errorf("write %d read its target or wrote outside its lease:\n%s", i+1, act)
		}
	}
	if !strings.Contains(acts[0][strings.LastIndex(acts[0], "hold "+lease):], "campaign.status != 'REMOVED'") {
		t.Errorf("the raise's caps were not read again under its lease:\n%s", acts[0])
	}
}

// RUNNING A BATCH JOB IS ADMIN, AND AN APPROVED RUN IS MADE ONLY ON WHAT WAS
// STAGED WHEN IT WAS ASKED: nothing the engine can read says what its staged
// operations do against the account as it stands at the run, so it is asked
// of an administrator, and an operation staged after the approval moves the
// binding, so nothing runs.
//
// Mutation: bind a run to its request alone, or class it below admin, and the
// job runs what nobody was asked about.
func TestRunningABatchJobIsAdminAndMadeOnlyOnItsStaging(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.f.batchOps = 2
	args := map[string]any{"operation": "googleads.customers.batchJobs.run",
		"path": map[string]any{"customersId": customer, "batchJobsId": "5"}}
	if _, _, err := w.call(t, "google_ads_spend", args); !refused(err) {
		t.Fatalf("a batch run on the spend tool answered %v", err)
	}
	call := growth.Call{Handle: "growth-lead", Tool: "google_ads_admin", Args: args}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil || hold.Class != growth.ClassAdmin || hold.Ask.Money != nil {
		t.Fatalf("a batch run is held as %s (money %+v), %v", hold.Class, hold.Ask.Money, err)
	}
	w.f.mu.Lock()
	w.f.batchOps = 3
	w.f.mu.Unlock()
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := w.p.Apply(t.Context(), call, hold.Binding); !errors.Is(err, growth.ErrMoved) || w.f.writes != 0 {
		t.Fatalf("a run staged further after its approval answered %v (writes %d)", err, w.f.writes)
	}
}

// A CALL ACTS ONLY IN THE ACCOUNT IT NAMES: a resource another account holds,
// named in the body of a method that is no mutate, a customer_id naming
// another account, alone or beside the path's, and a path parameter the
// method does not name, are each refused before Google; a customer_id naming
// the seat's account is called through its manager.
//
// Mutation: walk the mutate's operations alone, or read where a call acts
// from its path alone, and another account is reached.
func TestACallActsOnlyInTheAccountItNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for name, args := range map[string]map[string]any{
		"a recommendation of another account": {"operation": "googleads.customers.recommendations.apply",
			"path": map[string]any{"customersId": customer},
			"body": map[string]any{"operations": []any{map[string]any{
				"resource_name": "customers/9999999999/recommendations/7"}}}},
		"another account's user lists": {"operation": "googleads.listPlannableUserLists",
			"body": map[string]any{"customer_id": "9999999999"}},
		"two accounts": {"operation": "googleads.customers.generateReachForecast",
			"path": map[string]any{"customersId": customer}, "body": map[string]any{"customerId": "9999999999"}},
		"a resource with no account": {"operation": "googleads.geoTargetConstants.suggest",
			"body": map[string]any{"campaign": "customers/9999999999/campaigns/7"}},
		"a stray path parameter": {"operation": "googleads.customers.campaigns.mutate",
			"path": map[string]any{"customersId": customer, "campaignsId": "1"},
			"body": map[string]any{"operations": []any{}}},
	} {
		tool := "google_ads_read"
		if strings.Contains(name, "recommendation") || strings.Contains(name, "stray") {
			tool = "google_ads_spend"
		}
		if _, _, err := w.call(t, tool, args); !refused(err) {
			t.Errorf("%s answered %v", name, err)
		}
	}
	for _, c := range pathsCalled(w.f) {
		t.Errorf("Google was asked %s", c)
	}
	if _, _, err := w.call(t, "google_ads_read", map[string]any{"operation": "googleads.listPlannableUserLists",
		"body": map[string]any{"customerId": customer}}); err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	if len(w.f.managers) != 1 || w.f.managers[0] != manager {
		t.Errorf("the seat's account was called with managers %v, want %s", w.f.managers, manager)
	}
}

// A WRITE IS CLASSED BY WHAT GOOGLE READS OF IT: a status given by its enum's
// number, an operation list the method does not take beside the one it does,
// and a field named in both spellings with two values never pass as the
// pause they could be read as.
//
// Mutation: compare a status by its name alone, read a mutate's
// mutateOperations whatever its method, or drop the spelling check, and
// enabling the paused campaign is made at once as operate.
func TestAWriteIsClassedByWhatGoogleReads(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	update := func(fields map[string]any) map[string]any {
		obj := map[string]any{"resourceName": pausedC}
		for k, v := range fields {
			obj[k] = v
		}
		return map[string]any{"update": obj, "updateMask": "status"}
	}
	mutate := func(body map[string]any) map[string]any {
		return map[string]any{"operation": "googleads.customers.campaigns.mutate",
			"path": map[string]any{"customersId": customer}, "body": body}
	}
	for name, args := range map[string]map[string]any{
		"a status by its number": mutate(map[string]any{"operations": []any{update(map[string]any{"status": 2})}}),
		"a decoy operation list": mutate(map[string]any{
			"mutateOperations": []any{map[string]any{"campaignOperation": update(map[string]any{"status": "PAUSED"})}},
			"operations":       []any{update(map[string]any{"status": "ENABLED"})}}),
		"a status in two spellings": mutate(map[string]any{"operations": []any{map[string]any{
			"update":      map[string]any{"resourceName": pausedC, "status": "ENABLED"},
			"updateMask":  "status",
			"update_mask": "name"}}}),
		"a snake_case mask": mutate(map[string]any{"operations": []any{map[string]any{
			"update": map[string]any{"resource_name": pausedC, "status": "ENABLED"}, "update_mask": "status"}}}),
	} {
		_, _, err := w.call(t, "google_ads_operate", args)
		if !refused(err) && !errors.As(err, new(*aboveTool)) {
			t.Errorf("%s answered %v on the operate tool", name, err)
		}
	}
	if w.f.campaigns[pausedC][0] != "PAUSED" || w.f.writes != 0 {
		t.Errorf("the paused campaign is %s after %d writes", w.f.campaigns[pausedC][0], w.f.writes)
	}
}

// A SEARCH SERVING AN APPLY THAT GOOGLE ANSWERS UNREADABLY FAILS THE ACT AS
// GOOGLE'S, never as an outcome not known: nothing was written.
//
// Mutation: fail an unreadable search as unknown_outcome, and the log says a
// write may have happened that was never sent.
func TestAnUnreadableSearchIsNoUnknownOutcome(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	call := growth.Call{Handle: "growth-lead", Tool: "google_ads_set_budget",
		Args: map[string]any{"budget": budget1, "amount": "120.00"}}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	w.f.mu.Lock()
	w.f.brokenCustomer = true
	w.f.mu.Unlock()
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := w.p.Apply(t.Context(), call, hold.Binding); err == nil {
		t.Fatal("an apply whose caps could not be read was made")
	}
	w.audit.mu.Lock()
	defer w.audit.mu.Unlock()
	last := w.audit.recs[len(w.audit.recs)-1]
	if w.audit.steps[len(w.audit.steps)-1] != growth.StepFailed || last.Failure != growth.FailureVendorUnavailable ||
		w.f.writes != 0 {
		t.Errorf("the apply is recorded %s %q after %d writes", w.audit.steps[len(w.audit.steps)-1], last.Failure,
			w.f.writes)
	}
}

// A SEAT'S ACCOUNTS ARE HELD AGAIN UNDER THE LEASE: an approved raise on an
// account taken off the seat since it was asked sends nothing, and fails as
// the seat's access lowered.
//
// Mutation: hold the call to the seat's accounts only when it is asked, and
// the approval is made on an account the seat no longer reaches.
func TestASeatsAccountsAreHeldAgainWhenAnApprovalIsMade(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	call := growth.Call{Handle: "growth-lead", Tool: "google_ads_set_budget",
		Args: map[string]any{"budget": budget1, "amount": "120.00"}}
	hold, err := w.p.Prepare(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	w.company.Roles[0].Integrations.GoogleAds.Customers = []string{"9999999999"}
	call.HeldCall, call.Approver = "held-1", "founder"
	if _, err := w.p.Apply(t.Context(), call, hold.Binding); !refused(err) {
		t.Fatalf("an approval on an account the seat lost answered %v", err)
	}
	w.audit.mu.Lock()
	defer w.audit.mu.Unlock()
	last := w.audit.recs[len(w.audit.recs)-1]
	if w.audit.steps[len(w.audit.steps)-1] != growth.StepFailed || last.Failure != growth.FailureAccessLowered ||
		w.f.writes != 0 {
		t.Errorf("the apply is recorded %s %q after %d writes", w.audit.steps[len(w.audit.steps)-1], last.Failure,
			w.f.writes)
	}
}

// moveTo is a campaigns.mutate moving campaign to budget.
func moveTo(campaign, budget string) map[string]any {
	return map[string]any{"operation": "googleads.customers.campaigns.mutate",
		"path": map[string]any{"customersId": customer},
		"body": map[string]any{"operations": []any{map[string]any{"update": map[string]any{"resourceName": campaign,
			"campaignBudget": budget}, "updateMask": "campaignBudget"}}}}
}

// A BUDGET MOVE AND A BIDDING SWITCH ARE CLASSED ON WHAT THE ENGINE READS OF
// THEM: a campaign moved off its own daily budget onto a smaller one, or onto
// a total budget whose whole is smaller, operates; onto a larger one, a
// larger total, off a budget another campaign keeps, or off a total budget,
// spends; and a campaign given a standard scheme of its own, or one on a
// portfolio, spends.
//
// Mutation: compare against the budget the campaign leaves without reading
// it, a total budget by its daily amount, a budget another campaign keeps as
// freed, or a campaign's scheme by its type alone, and a raise is made at once.
func TestABudgetMoveAndABiddingSwitchAreClassedOnWhatTheEngineReads(t *testing.T) {
	t.Parallel()
	const (
		small, big           = "customers/1234567890/campaignBudgets/31", "customers/1234567890/campaignBudgets/32"
		total, bigTotal      = "customers/1234567890/campaignBudgets/33", "customers/1234567890/campaignBudgets/34"
		shared, totalLeft    = "customers/1234567890/campaignBudgets/35", "customers/1234567890/campaignBudgets/36"
		onShared, onTotalOff = "customers/1234567890/campaigns/4", "customers/1234567890/campaigns/5"
	)
	w := newWorld(t)
	w.f.mu.Lock()
	for name, amount := range map[string]int64{small: 1_000_000, big: 120_000_000, shared: 100_000_000,
		totalLeft: 200_000_000} {
		w.f.budgets[name] = amount
	}
	w.f.totals = map[string]int64{total: 50_000_000, bigTotal: 140_000_000, totalLeft: 60_000_000}
	w.f.campaigns["customers/1234567890/campaigns/3"] = [2]string{"PAUSED", shared}
	w.f.campaigns[onShared] = [2]string{"PAUSED", shared}
	w.f.campaigns[onTotalOff] = [2]string{"PAUSED", totalLeft}
	w.f.portfolio = map[string]string{pausedC: "customers/1234567890/biddingStrategies/9"}
	w.f.mu.Unlock()
	targetCpa := func(campaign string) map[string]any {
		return map[string]any{"operation": "googleads.customers.campaigns.mutate",
			"path": map[string]any{"customersId": customer},
			"body": map[string]any{"operations": []any{map[string]any{"update": map[string]any{
				"resourceName": campaign, "targetCpa": map[string]any{"targetCpaMicros": "1"}},
				"updateMask": "targetCpa.targetCpaMicros"}}}}
	}
	for name, tc := range map[string]struct {
		args map[string]any
		want growth.Class
	}{
		"moved onto a smaller budget":       {moveTo(enabledC, small), growth.ClassOperate},
		"moved onto a smaller total budget": {moveTo(enabledC, total), growth.ClassOperate},
		"moved onto a larger budget":        {moveTo(enabledC, big), growth.ClassSpend},
		"moved onto a larger total budget":  {moveTo(enabledC, bigTotal), growth.ClassSpend},
		"moved off a budget another keeps":  {moveTo(onShared, small), growth.ClassSpend},
		"moved off a total budget":          {moveTo(onTotalOff, small), growth.ClassSpend},
		"given a target CPA of its own":     {targetCpa(enabledC), growth.ClassSpend},
		"on a portfolio given its own CPA":  {targetCpa(pausedC), growth.ClassSpend},
	} {
		if _, hold, err := w.call(t, "google_ads_operate", tc.args); hold.Class != tc.want ||
			(err != nil && !errors.As(err, new(*aboveTool))) {
			t.Errorf("a campaign %s is %s (%v), want %s", name, hold.Class, err, tc.want)
		}
	}
}

// A SPEND BESIDE AN ADMIN ACT IS HELD TO THE CAPS: a budget raised past the
// change cap in the same Mutate as an ad group removed is refused before
// anyone is asked, and one within it is asked as admin with its figures.
//
// Mutation: hold a call to the caps only when its top class is spend, and the
// raise is asked as an account change with no figures and no cap.
func TestASpendBesideAnAdminActIsHeldToTheCaps(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mutate := func(amount string) growth.Call {
		return growth.Call{Handle: "growth-lead", Tool: "google_ads_admin", Args: map[string]any{
			"operation": "googleads.customers.googleAds.mutate", "path": map[string]any{"customersId": customer},
			"body": map[string]any{"mutateOperations": []any{
				map[string]any{"campaignBudgetOperation": map[string]any{"update": map[string]any{
					"resourceName": budget1, "amountMicros": amount}, "updateMask": "amountMicros"}},
				map[string]any{"adGroupOperation": map[string]any{"remove": "customers/1234567890/adGroups/2"}},
			}}}}
	}
	if _, err := w.p.Prepare(t.Context(), mutate("900000000000")); !refused(err) ||
		!strings.Contains(err.Error(), "per-change") {
		t.Fatalf("a raise past the cap beside a removal answered %v", err)
	}
	hold, err := w.p.Prepare(t.Context(), mutate("120000000"))
	if err != nil || hold.Class != growth.ClassAdmin || hold.Ask.Money == nil || hold.Ask.Money.DeltaMicros != 20_000_000 {
		t.Fatalf("a raise within the cap beside a removal is held as %s (money %+v), %v", hold.Class,
			hold.Ask.Money, err)
	}
}

// AN AMOUNT AS A TOOL'S ARGUMENTS CARRY IT IS HELD TO THE CAPS: every
// transport decodes a call's arguments with json.Number, and a budget raised
// by a JSON number past the change cap is refused before anyone is asked.
//
// Mutation: read a json.Number amount as unchanged in capacity, and the
// raise adds nothing the caps refuse.
func TestAnAmountAsAToolsArgumentsCarryItIsHeldToTheCaps(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	args, err := toolargs.Decode([]byte(`{"operation": "googleads.customers.campaignBudgets.mutate",
		"path": {"customersId": "` + customer + `"}, "body": {"operations": [{"update": {"resourceName": "` +
		budget1 + `", "amountMicros": 900000000000}, "updateMask": "amountMicros"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.p.Prepare(t.Context(), growth.Call{Handle: "growth-lead", Tool: "google_ads_spend", Args: args})
	if !refused(err) || !strings.Contains(err.Error(), "per-change") {
		t.Fatalf("a raise by a JSON number past the cap answered %v", err)
	}
}

// A BUDGET RECOMMENDATION APPLIED IS HELD TO THE CAPS BY ITS NEW AMOUNT, on
// the budget Google says it targets: one past the change cap is refused
// before anyone is asked, and one at Google's own amount is admin.
//
// Mutation: hand the caps no operation for an apply, and the new amount
// passes both caps.
func TestABudgetRecommendationAppliedIsHeldToTheCaps(t *testing.T) {
	t.Parallel()
	const rec = "customers/1234567890/recommendations/abc"
	w := newWorld(t)
	w.f.recs = map[string][2]string{rec: {"CAMPAIGN_BUDGET", budget1}}
	apply := func(params map[string]any) growth.Call {
		item := map[string]any{"resourceName": rec}
		for k, v := range params {
			item[k] = v
		}
		return growth.Call{Handle: "growth-lead", Tool: "google_ads_admin", Args: map[string]any{
			"operation": "googleads.customers.recommendations.apply", "path": map[string]any{"customersId": customer},
			"body": map[string]any{"operations": []any{item}}}}
	}
	if _, err := w.p.Prepare(t.Context(), apply(map[string]any{"campaignBudget": map[string]any{
		"newBudgetAmountMicros": "900000000000"}})); !refused(err) || !strings.Contains(err.Error(), "per-change") {
		t.Fatalf("a budget recommendation past the cap answered %v", err)
	}
	if hold, err := w.p.Prepare(t.Context(), apply(nil)); err != nil || hold.Class != growth.ClassAdmin {
		t.Fatalf("a budget recommendation at Google's amount is held as %s, %v", hold.Class, err)
	}
}

// A CUSTOMER NAMED ALONE IS HELD TO THE CALL'S ACCOUNT, except where the
// method's purpose is to name another: another account named as a resource
// of the account itself is refused before Google, while a manager linking a
// client, or a client moving to a new manager, names it.
//
// Mutation: match full resource names alone, or allow another account at any
// path of an exempt method, and the call reaches another account.
func TestACustomerNamedAloneIsHeldToTheCallsAccount(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	call := func(id string, body map[string]any) growth.Call {
		return growth.Call{Handle: "growth-lead", Tool: "google_ads_admin", Args: map[string]any{"operation": id,
			"path": map[string]any{"customersId": customer}, "body": body}}
	}
	for name, c := range map[string]growth.Call{
		"another account renamed": call("googleads.customers.mutate", map[string]any{"operation": map[string]any{
			"update":     map[string]any{"resourceName": "customers/9999999999", "descriptiveName": "x"},
			"updateMask": "descriptiveName"}}),
		"another account outside the field": call("googleads.customers.customerClientLinks.mutate",
			map[string]any{"operation": map[string]any{"create": map[string]any{"clientCustomer": "customers/1",
				"hidden": "customers/9999999999"}}}),
	} {
		if _, err := w.p.Prepare(t.Context(), c); !refused(err) || !strings.Contains(err.Error(), "9999999999") {
			t.Errorf("%s answered %v", name, err)
		}
	}
	for name, c := range map[string]growth.Call{
		"a client linked": call("googleads.customers.customerClientLinks.mutate", map[string]any{
			"operation": map[string]any{"create": map[string]any{"clientCustomer": "customers/9999999999"}}}),
		"a manager moved to": call("googleads.customers.customerManagerLinks.moveManagerLink", map[string]any{
			"customerId": customer, "newManager": "customers/9999999999",
			"previousCustomerManagerLink": "customers/1234567890/customerManagerLinks/1112223334~5"}),
	} {
		if hold, err := w.p.Prepare(t.Context(), c); err != nil || hold.Class != growth.ClassAdmin {
			t.Errorf("%s is held as %s, %v", name, hold.Class, err)
		}
	}
	for _, c := range pathsCalled(w.f) {
		if strings.Contains(c, "9999999999") || !strings.HasSuffix(c, "googleAds:search") {
			t.Errorf("Google was asked %s", c)
		}
	}
}
