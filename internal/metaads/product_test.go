package metaads_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/metaads"
)

// The fake app's credentials. The tokens are Meta's shape, so the redaction
// rule would also catch them; each test still checks for the exact values.
const (
	appID        = "987654321012345"
	appSecret    = "0123456789abcdef0123456789abcdef"
	readToken    = "EAAGreadTOKENaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	operateToken = "EAAGoperateTOKENbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	adminToken   = "EAAGadminTOKENcccccccccccccccccccccccccccccccccccc"
	renewedToken = "EAAGrenewedTOKENddddddddddddddddddddddddddddddddd"
)

// meta is a fake Graph API over two ad accounts, act_111 the seat's and
// act_222 another's, with a campaign holding its budget, one whose ad sets
// hold theirs, ads, a custom audience and a Page, and what was asked of it.
type meta struct {
	mu       sync.Mutex
	t        *testing.T
	scopes   map[string][]string
	expires  map[string]int64
	usage    int
	requests []*http.Request
	bodies   []string
	writes   []string
	objects  map[string]map[string]any
	audience []map[string]any
	exchange int
	noRenew  bool
	noProof  bool
	nextID   int
}

func newMeta(t *testing.T) *meta {
	return &meta{t: t, nextID: 900, audience: []map[string]any{},
		scopes: map[string][]string{readToken: {"ads_read", "pages_show_list"},
			operateToken: {"ads_management", "pages_read_engagement", "pages_show_list"},
			adminToken:   {"business_management", "ads_management"}, renewedToken: {"ads_read"}},
		expires: map[string]int64{},
		objects: map[string]map[string]any{
			"act_111": {"id": "act_111", "account_id": "111", "name": "Notes", "currency": "USD",
				"timezone_name": "America/New_York", "account_status": json.Number("1"), "min_daily_budget": json.Number("100")},
			"act_222": {"id": "act_222", "account_id": "222", "name": "Other", "currency": "JPY",
				"timezone_name": "Asia/Tokyo", "account_status": json.Number("1")},
			"501": {"id": "501", "account_id": "111", "name": "Notes US", "status": "ACTIVE", "daily_budget": "10000"},
			"502": {"id": "502", "account_id": "111", "name": "Notes JP", "status": "ACTIVE", "daily_budget": "0"},
			"601": {"id": "601", "account_id": "111", "campaign_id": "502", "status": "ACTIVE", "daily_budget": "3000",
				"bid_amount": "150", "targeting": map[string]any{"geo_locations": map[string]any{"countries": []any{"US"}}}},
			"602": {"id": "602", "account_id": "111", "campaign_id": "502", "status": "PAUSED", "daily_budget": "2000"},
			"701": {"id": "701", "account_id": "111", "status": "PAUSED", "adset_id": "601"},
			"702": {"id": "702", "account_id": "111", "status": "ACTIVE", "adset_id": "601"},
			"801": {"id": "801", "account_id": "111", "name": "Spare"},
			"901": {"id": "901", "account_id": "111", "name": "Buyers", "lookalike_audience_ids": []any{}},
			"5":   {"id": "5", "account_id": "222", "name": "Theirs", "status": "ACTIVE", "daily_budget": "500"},
		},
	}
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func proof(token string) string {
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
}

func (f *meta) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	q := r.URL.Query()
	f.requests, f.bodies = append(f.requests, r), append(f.bodies, string(body))
	path := strings.TrimPrefix(r.URL.Path, "/"+metaads.Version)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if path == "/oauth/access_token" {
		f.exchange++
		if f.noRenew || form.Get("client_secret") != appSecret || form.Get("grant_type") != "fb_exchange_token" {
			write(w, 400, map[string]any{"error": map[string]any{"code": 101, "message": "bad exchange"}})
			return
		}
		write(w, 200, map[string]any{"access_token": renewedToken, "expires_in": 5184000})
		return
	}
	got := q.Get("appsecret_proof") + form.Get("appsecret_proof")
	if path == "/debug_token" {
		input := q.Get("input_token")
		write(w, 200, map[string]any{"data": map[string]any{"app_id": appID, "type": "SYSTEM_USER",
			"is_valid": f.scopes[input] != nil, "scopes": f.scopes[input], "user_id": "42", "expires_at": f.expires[input]}})
		return
	}
	if got != proof(token) {
		if got == "" && f.noProof {
			write(w, 200, map[string]any{"id": "42"})
			return
		}
		write(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "API calls from the server require an appsecret_proof argument"}})
		return
	}
	if f.scopes[token] == nil {
		write(w, 400, map[string]any{"error": map[string]any{"code": 190, "error_subcode": 463, "message": "Error validating access token: " + token}})
		return
	}
	if f.usage > 0 {
		w.Header().Set("X-Business-Use-Case-Usage", fmt.Sprintf(`{"111":[{"type":"ads_management","call_count":%d,"total_cputime":1,"total_time":1,"estimated_time_to_regain_access":0,"ads_api_access_tier":"standard_access"}]}`, f.usage))
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/me/adaccounts":
		write(w, 200, map[string]any{"data": []any{f.objects["act_111"], f.objects["act_222"]}})
	case len(parts) == 2 && parts[1] == "campaigns" && r.Method == "GET" && q.Get("after") == "c1":
		write(w, 200, map[string]any{"data": []any{}})
	case len(parts) == 2 && parts[1] == "campaigns" && r.Method == "GET":
		page := map[string]any{"data": f.ofAccount(parts[0], "campaign")}
		if q.Get("after") == "" {
			// A FIRST PAGE names the next, as Meta does, with the token in it.
			page["paging"] = map[string]any{"cursors": map[string]any{"after": "c1"},
				"next": "https://graph.facebook.com/v27.0/x?access_token=" + token}
		}
		write(w, 200, page)
	case len(parts) == 2 && parts[1] == "adsets" && r.Method == "GET":
		write(w, 200, map[string]any{"data": f.ofAccount(parts[0], "adset")})
	case len(parts) == 2 && parts[1] == "ads" && r.Method == "GET" && parts[0] == "901":
		write(w, 200, map[string]any{"data": f.audience})
	case len(parts) == 2 && parts[1] == "promote_pages":
		write(w, 200, map[string]any{"data": []any{map[string]any{"id": "3001"}}})
	case len(parts) == 2 && r.Method == "POST":
		f.nextID++
		id := fmt.Sprint(f.nextID)
		o := map[string]any{"id": id, "account_id": strings.TrimPrefix(parts[0], "act_")}
		for k := range form {
			if k != "appsecret_proof" {
				o[k] = form.Get(k)
			}
		}
		f.objects[id] = o
		f.writes = append(f.writes, "POST "+path)
		write(w, 200, map[string]any{"id": id})
	case len(parts) == 1 && f.objects[parts[0]] != nil:
		o := f.objects[parts[0]]
		switch r.Method {
		case "POST":
			for k := range form {
				if k != "appsecret_proof" {
					o[k] = form.Get(k)
				}
			}
			f.writes = append(f.writes, "POST "+path)
			write(w, 200, map[string]any{"success": true})
		case "DELETE":
			f.writes = append(f.writes, "DELETE "+path)
			write(w, 200, map[string]any{"success": true})
		default:
			out := map[string]any{"id": o["id"]}
			for _, k := range strings.Split(q.Get("fields"), ",") {
				if v, ok := o[k]; ok {
					out[k] = v
				}
			}
			if q.Get("fields") == "" {
				out = o
			}
			write(w, 200, out)
		}
	default:
		write(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "Unsupported request " + path}})
	}
}

// ofAccount are account's objects of kind: campaigns hold no campaign_id,
// ad sets one.
func (f *meta) ofAccount(account, kind string) []any {
	var out []any
	for _, o := range f.objects {
		if "act_"+fmt.Sprint(o["account_id"]) != account || o["status"] == nil {
			continue
		}
		_, adset := o["campaign_id"]
		_, ad := o["adset_id"]
		if (kind == "campaign" && !adset && !ad) || (kind == "adset" && adset) {
			out = append(out, o)
		}
	}
	return out
}

func (f *meta) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// leases is an in-process lease per resource.
type leases struct {
	mu   sync.Mutex
	held map[string]bool
}

func (l *leases) Hold(_ context.Context, resource string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	if l.held[resource] {
		return nil, growth.ErrActHeld
	}
	l.held[resource] = true
	return func() { l.mu.Lock(); delete(l.held, resource); l.mu.Unlock() }, nil
}

type step struct {
	step growth.Step
	rec  growth.Record
}

// audit keeps every step as the access log would: it refuses a record the
// log's own validation refuses, failing the test, keeps one answer and one
// outcome per act, answers first=false for an act already answered, and
// counts changes as the budget limit reads them.
type audit struct {
	t       *testing.T
	mu      sync.Mutex
	steps   []step
	phases  map[string]bool
	fail    bool
	changed int
}

func (a *audit) add(s growth.Step, r growth.Record) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := accessdomain.GrowthOf(s, r).Validate(); err != nil {
		a.t.Errorf("the access log refuses the %s record %+v: %v", s, r, err)
		return false, err
	}
	if a.fail {
		return false, errors.New("the log is unreachable")
	}
	phase := r.Call + r.HeldCall + "/outcome"
	switch s {
	case growth.StepAllowed, growth.StepDenied, growth.StepLapsed:
		phase = r.Call + r.HeldCall + "/answer"
	}
	if a.phases == nil {
		a.phases = map[string]bool{}
	}
	if a.phases[phase] {
		return false, nil
	}
	a.phases[phase] = true
	a.steps = append(a.steps, step{s, r})
	return true, nil
}

func (a *audit) Made(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepMade, r)
}
func (a *audit) Verdict(_ context.Context, s growth.Step, r growth.Record) (bool, error) {
	return a.add(s, r)
}
func (a *audit) Applied(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepApplied, r)
}
func (a *audit) Void(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepVoid, r)
}
func (a *audit) Failed(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepFailed, r)
}
func (a *audit) Changed(context.Context, string, string, string, string, time.Time) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.changed, nil
}

func (a *audit) last() step {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steps[len(a.steps)-1]
}

// renewals keeps each slot's renewal in memory, with its version.
type renewals struct {
	mu   sync.Mutex
	vals map[string]string
	vers map[string]uint64
}

func (r *renewals) Held(_ context.Context, slot string) (string, uint64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vals[slot]
	return v, r.vers[slot], ok, nil
}

func (r *renewals) Put(_ context.Context, slot, value string, version uint64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vals == nil {
		r.vals, r.vers = map[string]string{}, map[string]uint64{}
	}
	if r.vers[slot] != version {
		return false, nil
	}
	r.vals[slot], r.vers[slot] = value, version+1
	return true, nil
}

func (r *renewals) Delete(_ context.Context, slot string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.vals, slot)
	delete(r.vers, slot)
	return nil
}

// now is the fake clock.
var now = time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)

// company is a document connecting the app, a seat at level on act_111, and
// caps on act_111 where caps is set.
func company(level string, caps bool) *config.Company {
	on := true
	c := &config.Company{
		Integrations: config.Integrations{MetaAds: &config.MetaAds{Enabled: true, AppID: appID,
			AppSecret: "${S}", ReadToken: "${R}", OperateToken: "${O}", AdminToken: "${A}"}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			MetaAds: &config.RoleMetaAds{Enabled: &on, Level: level, AdAccounts: []string{"act_111"}}}}},
	}
	if caps {
		c.Integrations.MetaAds.Spend = &config.GrowthSpend{Accounts: []config.GrowthCap{{Account: "act_111",
			Currency: "USD", PerChange: "50.00", Daily: "200.00"}}}
	}
	return c
}

// resolver reads a ${VAR} from values, and any other value as written, as
// the engine's resolver does.
func resolver(values map[string]string) func(string) string {
	return func(ref string) string {
		if v, ok := values[ref]; ok {
			return v
		}
		if strings.HasPrefix(ref, "${") {
			return ""
		}
		return ref
	}
}

// fixture is a product over a fake Meta.
type fixture struct {
	f     *meta
	p     *metaads.Product
	audit *audit
	ren   *renewals
	doc   *config.Company
}

func newFixture(t *testing.T, level string, caps bool) *fixture {
	t.Helper()
	f := newMeta(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	fx := &fixture{f: f, audit: &audit{t: t}, ren: &renewals{}, doc: company(level, caps)}
	values := map[string]string{"${S}": appSecret, "${R}": readToken, "${O}": operateToken, "${A}": adminToken}
	fx.p = metaads.New(metaads.Deps{Company: func() *config.Company { return fx.doc },
		Resolve: resolver(values), HTTP: srv.Client(), Base: srv.URL, Audit: fx.audit,
		Leases: &leases{}, Renewals: fx.ren, Now: func() time.Time { return now }})
	return fx
}

func (fx *fixture) call(tool string, args map[string]any) growth.Call {
	return growth.Call{ID: "0190c4a2-0000-7000-8000-00000000000" + fmt.Sprint(len(fx.audit.steps)%10), Handle: "growth",
		Turn: "turn-1", Tool: tool, Args: args}
}

// operate makes an operate call as the engine does: prepared, then applied
// on the binding it was classed on.
func (fx *fixture) operate(t *testing.T, tool string, args map[string]any) (growth.Hold, any, error) {
	t.Helper()
	c := fx.call(tool, args)
	hold, err := fx.p.Prepare(context.Background(), c)
	if err != nil {
		return hold, nil, err
	}
	answer, err := fx.p.Apply(context.Background(), c, hold.Binding)
	return hold, answer, err
}

// refusedAs fails unless err is a refusal of kind whose words hold want.
func refusedAs(t *testing.T, err error, kind growth.RefusalKind, want string) {
	t.Helper()
	var r *growth.Refusal
	if !errors.As(err, &r) || r.Kind != kind || !strings.Contains(r.Said, want) {
		t.Fatalf("got %v, want a %s refusal saying %q", err, kind, want)
	}
}

// The catalog holds every operation of the matrix in its class, with the
// review's decisions: archiving admin, a label's removal operate, program
// operations and unconfirmed specs left out with a reason, an act only an
// admin-level task may make held, and the Page reads a creative needs.
//
// Mutation: drop a decision from the table, and its case fails.
func TestTheCatalogHoldsTheMatrixAndItsDecisions(t *testing.T) {
	t.Parallel()
	c := metaads.Catalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	counts := map[growth.Class]int{}
	for _, op := range c.Operations {
		counts[op.Class]++
	}
	if len(c.Operations) != 985 || counts[growth.ClassNever] != 42 || counts[growth.ClassSpend] != 27 {
		t.Errorf("the catalog holds %d operations, %v", len(c.Operations), counts)
	}
	class := func(id string) growth.Class {
		op, ok := c.Operation(id)
		if !ok {
			t.Fatalf("no operation %s", id)
		}
		return op.Class
	}
	for id, want := range map[string]growth.Class{
		"ad_set.adlabels.delete":                      growth.ClassOperate,
		"ad_account.reachfrequencypredictions.create": growth.ClassNever,
		"ad_account.message_campaign.create":          growth.ClassNever,
		"ad_account.assigned_users.list":              growth.ClassNever,
		"ad_account.update":                           growth.ClassAdmin,
		"page.videos.list":                            growth.ClassRead,
		"page.video_reels.list":                       growth.ClassRead,
		"ads_subscription.subscriptions.list":         growth.ClassNever,
	} {
		if got := class(id); got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}
	update, _ := c.Operation("campaign.update")
	for name, tc := range map[string]struct {
		ch   []growth.Change
		want growth.Class
	}{
		"an archive":    {[]growth.Change{{Field: "status", After: "ARCHIVED"}}, growth.ClassAdmin},
		"a pause":       {[]growth.Change{{Field: "status", After: "PAUSED"}}, growth.ClassOperate},
		"a rename":      {[]growth.Change{{Field: "name", After: "x"}}, growth.ClassOperate},
		"an enable":     {[]growth.Change{{Field: "status", After: "ACTIVE"}}, growth.ClassSpend},
		"a lower":       {[]growth.Change{{Field: "daily_budget", Before: "100", After: "50"}}, growth.ClassOperate},
		"a raise":       {[]growth.Change{{Field: "daily_budget", Before: "50", After: "100"}}, growth.ClassSpend},
		"no rule names": {[]growth.Change{{Field: "smart_promotion_type", After: "x"}}, growth.ClassSpend},
		"a cap removed": {[]growth.Change{{Field: "spend_cap", Before: "100", After: "0"}}, growth.ClassSpend},
	} {
		if got := growth.Classify(update, tc.ch); got != tc.want {
			t.Errorf("campaign.update %s is %s, want %s", name, got, tc.want)
		}
	}
	for _, op := range c.Operations {
		if op.Class == growth.ClassNever && op.Reason == "" {
			t.Errorf("%s is never offered and says no reason", op.ID)
		}
	}
}

// No token, app secret or appsecret_proof reaches a URL that is logged, an
// error, a record or an answer: the bearer is a header, every call carries
// the app secret's proof of its token (a write's in its body), Meta's paging
// links are dropped, and an error says the method and route alone.
//
// Mutation: send the token as access_token, keep paging links, or let a
// url.Error through, and a case fails.
func TestNoSecretReachesAURLAnErrorARecordOrAnAnswer(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", true)
	answer, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "ad_account.campaigns.list", "path": map[string]any{"ad_account_id": "act_111"},
		"params": map[string]any{"fields": "name,status"}}))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(answer)
	for _, secret := range []string{readToken, operateToken, appSecret, proof(readToken), "access_token="} {
		if strings.Contains(string(doc), secret) {
			t.Errorf("the answer holds %q: %s", secret, doc)
		}
	}
	fx.f.mu.Lock()
	for i, r := range fx.f.requests {
		if r.URL.Query().Get("access_token") != "" || strings.Contains(fx.f.bodies[i], "access_token=EAA") {
			t.Errorf("a request carried the token as a parameter: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Path != "/"+metaads.Version+"/debug_token" && r.Header.Get("Authorization") == "" {
			t.Errorf("%s %s carried no bearer", r.Method, r.URL.Path)
		}
	}
	fx.f.mu.Unlock()
	if _, _, err := fx.operate(t, metaads.ToolPause, map[string]any{"level": "campaign", "ids": []any{"501"},
		"reason": "spend runs away"}); err != nil {
		t.Fatal(err)
	}
	fx.f.mu.Lock()
	last := fx.f.requests[len(fx.f.requests)-1]
	if last.Method != "POST" || last.URL.RawQuery != "" || !strings.Contains(fx.f.bodies[len(fx.f.bodies)-1], "appsecret_proof="+proof(operateToken)) {
		t.Errorf("a write sent %s ?%q body %q: its proof belongs in its body", last.Method, last.URL.RawQuery,
			fx.f.bodies[len(fx.f.bodies)-1])
	}
	fx.f.mu.Unlock()
	for _, s := range fx.audit.steps {
		rec, _ := json.Marshal(s.rec)
		for _, secret := range []string{readToken, operateToken, appSecret} {
			if strings.Contains(string(rec), secret) {
				t.Errorf("a record holds a secret: %s", rec)
			}
		}
	}
	// AN ERROR NAMES THE ROUTE ALONE: a closed server's dial error is a
	// url.Error whose text is the whole URL, appsecret_proof and all.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	values := map[string]string{"${S}": appSecret, "${R}": readToken}
	p := metaads.New(metaads.Deps{Company: func() *config.Company { return company("read", false) },
		Resolve: resolver(values), Base: closed.URL, Now: func() time.Time { return now }})
	_, err = p.Run(context.Background(), growth.Call{Handle: "growth", Tool: metaads.ToolAccounts, Args: map[string]any{}})
	if err == nil || strings.Contains(err.Error(), proof(readToken)) || strings.Contains(err.Error(), readToken) ||
		strings.Contains(err.Error(), "input_token") {
		t.Errorf("a failed call answered %v", err)
	}
}

// Every object a request names is held to the seat's ad accounts: the path's
// object by the account_id Meta answers for it, an ad account or an object
// named in a parameter or a filter, a Page by the ad account's own list, and
// what no ad account owns only for a seat given business_resources. A list is
// answered with the seat's objects alone, and a field expansion, a method
// override or a token field is refused.
//
// Mutation: skip the parameter check, the filter's IDs, the Page's list or
// the answer's filter, and a case reaches act_222.
func TestEveryObjectARequestNamesIsHeldToTheSeat(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", true)
	run := func(op string, path, params map[string]any) error {
		_, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{"operation": op,
			"path": path, "params": params}))
		return err
	}
	refusedAs(t, run("campaign.get", map[string]any{"campaign_id": "5"}, nil), growth.RefusedForbidden, "act_222")
	refusedAs(t, run("ad_account.campaigns.list", map[string]any{"ad_account_id": "act_222"}, nil),
		growth.RefusedForbidden, "act_222")
	refusedAs(t, run("ad_account.insights.list", map[string]any{"ad_account_id": "act_111"}, map[string]any{
		"filtering": `[{"field":"campaign.id","operator":"IN","value":["5"]}]`}), growth.RefusedForbidden, "act_222")
	refusedAs(t, run("ad_account.insights.list", map[string]any{"ad_account_id": "act_111"}, map[string]any{
		"filtering": `[{"field":"ad.account_id","operator":"EQUAL","value":"act_222"}]`}), growth.RefusedForbidden, "act_222")
	refusedAs(t, run("ad_account.campaigns.list", map[string]any{"ad_account_id": "act_111"},
		map[string]any{"fields": "name,adsets{name}"}), growth.RefusedInvalid, "expansion")
	refusedAs(t, run("ad_account.campaigns.list", map[string]any{"ad_account_id": "act_111"},
		map[string]any{"method": "delete"}), growth.RefusedInvalid, "set by the engine")
	refusedAs(t, run("page.get", map[string]any{"page_id": "3001"}, map[string]any{"fields": "access_token"}),
		growth.RefusedInvalid, "access_token")
	refusedAs(t, run("page.get", map[string]any{"page_id": "3001"}, nil), growth.RefusedForbidden, "business_resources")
	_, _, err := fx.operate(t, metaads.ToolOperate, map[string]any{"operation": "ad_set.update",
		"path": map[string]any{"ad_set_id": "601"}, "params": map[string]any{"campaign_id": "5"}, "reason": "x"})
	refusedAs(t, err, growth.RefusedForbidden, "act_222")
	_, err = fx.p.Prepare(context.Background(), fx.call(metaads.ToolCreateAd, map[string]any{"ad_set": "601",
		"name": "New", "image_hash": "abc", "page_id": "3999", "message": "Hi", "link": "https://example.com",
		"reason": "x"}))
	refusedAs(t, err, growth.RefusedForbidden, "promote_pages")
	_, err = fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "user.adaccounts.list", "path": map[string]any{"user_id": "me"}}))
	refusedAs(t, err, growth.RefusedForbidden, "business_resources")
	fx.f.mu.Lock()
	fx.f.audience = []map[string]any{{"id": "702", "account_id": "111", "effective_status": "PAUSED"},
		{"id": "999", "account_id": "222", "effective_status": "ACTIVE"}}
	fx.f.mu.Unlock()
	answer, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "custom_audience.ads.list", "path": map[string]any{"custom_audience_id": "901"}}))
	if err != nil {
		t.Fatal(err)
	}
	a := answer.(metaads.Answer)
	if strings.Contains(a.Document, "999") || !strings.Contains(a.Document, "702") || a.Withheld != 1 {
		t.Errorf("an audience's ads across ad accounts answered %+v", a)
	}
}

// A write is classed against its target as read now: a pause is made at once,
// an enable is spend and refused through an operate tool, an archive is
// admin, a creative swapped on a paused ad is operate and on a live one
// public, a dry run is read, a campaign created PAUSED is operate, and an
// audience no active ad set uses changes at operate, one in use at spend.
//
// Mutation: drop a state's reading, or the dry run, and a case is misclassed.
func TestAWriteIsClassedAgainstItsTargetAsReadNow(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "administer", true)
	prepare := func(tool, op string, path, params map[string]any) growth.Class {
		t.Helper()
		hold, err := fx.p.Prepare(context.Background(), fx.call(tool, map[string]any{"operation": op, "path": path,
			"params": params, "reason": "x"}))
		if err != nil {
			t.Fatalf("%s %v: %v", op, params, err)
		}
		return hold.Class
	}
	for name, tc := range map[string]struct {
		tool, op string
		path     map[string]any
		params   map[string]any
		want     growth.Class
	}{
		"a pause":                  {metaads.ToolOperate, "campaign.update", map[string]any{"campaign_id": "501"}, map[string]any{"status": "PAUSED"}, growth.ClassOperate},
		"an enable":                {metaads.ToolSpend, "ad_set.update", map[string]any{"ad_set_id": "602"}, map[string]any{"status": "ACTIVE"}, growth.ClassSpend},
		"an archive":               {metaads.ToolAdmin, "campaign.update", map[string]any{"campaign_id": "501"}, map[string]any{"status": "ARCHIVED"}, growth.ClassAdmin},
		"a creative, paused ad":    {metaads.ToolOperate, "ad.update", map[string]any{"ad_id": "701"}, map[string]any{"creative": `{"creative_id":"801"}`}, growth.ClassOperate},
		"a creative, live ad":      {metaads.ToolPublish, "ad.update", map[string]any{"ad_id": "702"}, map[string]any{"creative": `{"creative_id":"801"}`}, growth.ClassPublic},
		"a dry run":                {metaads.ToolOperate, "campaign.update", map[string]any{"campaign_id": "501"}, map[string]any{"status": "ACTIVE", "execution_options": []any{"validate_only"}}, growth.ClassRead},
		"a paused create":          {metaads.ToolOperate, "ad_account.campaigns.create", map[string]any{"ad_account_id": "act_111"}, map[string]any{"name": "x", "status": "PAUSED", "daily_budget": "9000"}, growth.ClassOperate},
		"an active create":         {metaads.ToolSpend, "ad_account.campaigns.create", map[string]any{"ad_account_id": "act_111"}, map[string]any{"name": "x", "status": "ACTIVE", "daily_budget": "1000"}, growth.ClassSpend},
		"an unused audience":       {metaads.ToolOperate, "custom_audience.update", map[string]any{"custom_audience_id": "901"}, map[string]any{"rule": `{"inclusions":{}}`}, growth.ClassOperate},
		"a later end, as a number": {metaads.ToolOperate, "ad_set.update", map[string]any{"ad_set_id": "601"}, map[string]any{"name": "y"}, growth.ClassOperate},
	} {
		if got := prepare(tc.tool, tc.op, tc.path, tc.params); got != tc.want {
			t.Errorf("%s is %s, want %s", name, got, tc.want)
		}
	}
	fx.f.mu.Lock()
	fx.f.audience = []map[string]any{{"id": "702", "effective_status": "ACTIVE"}}
	fx.f.mu.Unlock()
	if got := prepare(metaads.ToolSpend, "custom_audience.update", map[string]any{"custom_audience_id": "901"},
		map[string]any{"rule": `{"inclusions":{}}`}); got != growth.ClassSpend {
		t.Errorf("an audience an active ad uses changes at %s, want spend", got)
	}
}

// Spend is held to the account's caps before a person is asked and again
// under the account's lease at the apply: an enable counts the ad set's
// budget, a raise its difference, unset caps refuse, and an allowed raise is
// made inside the guard's apply with its figures recorded.
//
// Mutation: skip the check at the ask, or apply outside the guard, and a
// raise over the cap is asked or made.
func TestSpendIsHeldToTheCapsAtTheAskAndTheApply(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", true)
	hold, err := fx.p.Prepare(context.Background(), fx.call(metaads.ToolEnable, map[string]any{"level": "ad_set",
		"ids": []any{"602"}, "reason": "launch"}))
	if err != nil {
		t.Fatal(err)
	}
	if hold.Class != growth.ClassSpend || hold.Ask.Money == nil || hold.Ask.Money.DeltaMicros != 20_000_000 ||
		hold.Ask.Money.DailyMicros != 130_000_000 {
		t.Fatalf("enabling a $20 ad set asks %+v money %+v", hold.Class, hold.Ask.Money)
	}
	_, err = fx.p.Prepare(context.Background(), fx.call(metaads.ToolRaiseBudget, map[string]any{"level": "campaign",
		"id": "501", "amount": "160.00", "reason": "scale"}))
	refusedAs(t, err, growth.RefusedForbidden, "per-change cap")
	fx.doc.Integrations.MetaAds.Spend = nil
	_, err = fx.p.Prepare(context.Background(), fx.call(metaads.ToolRaiseBudget, map[string]any{"level": "campaign",
		"id": "501", "amount": "120.00", "reason": "scale"}))
	refusedAs(t, err, growth.RefusedForbidden, "no spending caps")
	fx.doc = company("operate", true)
	raise := fx.call(metaads.ToolRaiseBudget, map[string]any{"level": "campaign", "id": "501", "amount": "120.00",
		"reason": "scale"})
	hold, err = fx.p.Prepare(context.Background(), raise)
	if err != nil || hold.Ask.Money.DeltaMicros != 20_000_000 {
		t.Fatalf("a $20 raise asks %+v (%v)", hold.Ask.Money, err)
	}
	raise.HeldCall, raise.Approver, raise.ID = "0190c4a2-0000-7000-8000-0000000000aa", "founder", ""
	if _, err := fx.p.Apply(context.Background(), raise, hold.Binding); err != nil {
		t.Fatal(err)
	}
	if got := fx.f.objects["501"]["daily_budget"]; got != "12000" {
		t.Errorf("the campaign's budget is %v, want 12000 cents", got)
	}
	last := fx.audit.last()
	if last.step != growth.StepApplied || last.rec.Money == nil || last.rec.Money.DailyAfterMicros != 150_000_000 {
		t.Errorf("the apply recorded %+v", last)
	}
}

// A held act records its allow before it acts and honours an earlier answer:
// a lapse already recorded stands and nothing is sent, a log that cannot
// record sends nothing, a target that moved is void, and an operate call is
// never made as approved nor a held one without an approval.
//
// Mutation: act before the allow lands, or ignore first=false, and a write
// reaches Meta.
func TestAHeldActRecordsItsAllowFirstAndHonoursAnEarlierAnswer(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", true)
	ask := fx.call(metaads.ToolEnable, map[string]any{"level": "ad_set", "ids": []any{"602"}, "reason": "launch"})
	hold, err := fx.p.Prepare(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	ask.HeldCall, ask.ID = "0190c4a2-0000-7000-8000-0000000000bb", ""
	lapse := ask
	lapse.Approver = "engine"
	if err := fx.p.Denied(context.Background(), lapse); err != nil {
		t.Fatal(err)
	}
	ask.Approver = "founder"
	_, err = fx.p.Apply(context.Background(), ask, hold.Binding)
	refusedAs(t, err, growth.RefusedForbidden, "already holds an answer")
	if w := fx.f.wrote(); len(w) != 0 {
		t.Fatalf("an act lapsed first was made: %v", w)
	}
	ask.HeldCall = "0190c4a2-0000-7000-8000-0000000000cc"
	fx.audit.fail = true
	_, err = fx.p.Apply(context.Background(), ask, hold.Binding)
	refusedAs(t, err, growth.RefusedUnavailable, "did not record")
	fx.audit.fail = false
	fx.f.mu.Lock()
	fx.f.objects["602"]["daily_budget"] = "2500"
	fx.f.mu.Unlock()
	ask.HeldCall = "0190c4a2-0000-7000-8000-0000000000dd"
	if _, err := fx.p.Apply(context.Background(), ask, hold.Binding); !errors.Is(err, growth.ErrMoved) {
		t.Fatalf("a moved target answered %v", err)
	}
	if w := fx.f.wrote(); len(w) != 0 || fx.audit.last().step != growth.StepVoid {
		t.Fatalf("a moved target was made: %v, last step %s", w, fx.audit.last().step)
	}
	pause := fx.call(metaads.ToolPause, map[string]any{"level": "campaign", "ids": []any{"501"}})
	ph, err := fx.p.Prepare(context.Background(), pause)
	if err != nil {
		t.Fatal(err)
	}
	pause.HeldCall, pause.Approver, pause.ID = "0190c4a2-0000-7000-8000-0000000000ee", "founder", ""
	_, err = fx.p.Apply(context.Background(), pause, ph.Binding)
	refusedAs(t, err, growth.RefusedForbidden, "never as an approved call")
}

// Meta's four budget changes an hour per ad set are counted from the access
// log before anybody is asked: a fifth is refused.
//
// Mutation: skip the count, and the fifth is asked.
func TestTheFifthBudgetChangeAnHourIsRefusedBeforeTheAsk(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", true)
	fx.audit.changed = 4
	_, err := fx.p.Prepare(context.Background(), fx.call(metaads.ToolRaiseBudget, map[string]any{"level": "ad_set",
		"id": "601", "amount": "31.00", "reason": "scale"}))
	refusedAs(t, err, growth.RefusedConflict, "fifth budget change")
}

// Each call goes through the least privileged token its class and operation
// allow: a read through the read token, a change through the operate token,
// never the admin token for a read or a change made at once; a token holding
// more than its slot allows is refused.
//
// Mutation: let a read through the admin token, or skip the slot's policy, and
// a case passes.
func TestEachCallGoesThroughTheLeastPrivilegedToken(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "administer", true)
	fx.doc.Roles[0].Integrations.MetaAds.BusinessResources = true
	_, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "business.owned_ad_accounts.list", "path": map[string]any{"business_id": "77"}}))
	refusedAs(t, err, growth.RefusedForbidden, "business_management")
	answer, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
	if err != nil || answer.(metaads.Answer).Token != "read" {
		t.Fatalf("a read went through %+v (%v)", answer, err)
	}
	if _, _, err := fx.operate(t, metaads.ToolPause, map[string]any{"level": "ad", "ids": []any{"702"}}); err != nil {
		t.Fatal(err)
	}
	if via := fx.audit.last().rec.Via; via != "operate" {
		t.Errorf("a pause went through %q", via)
	}
	fresh := newFixture(t, "read", false)
	fresh.f.scopes[readToken] = []string{"ads_read", "ads_management"}
	fresh.doc.Integrations.MetaAds.OperateToken = ""
	_, err = fresh.p.Run(context.Background(), fresh.call(metaads.ToolRead, map[string]any{
		"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
	if err == nil || !strings.Contains(err.Error(), "writes") {
		t.Errorf("a read token that writes answered %v", err)
	}
}

// A renewed token is tied to the pasted token it descends from: used while
// the document names that paste, deleted once it names another, renewed at
// fifty days of sixty once a day, and forgotten when the integration is off.
//
// Mutation: drop the digest check, or renew every pass, and a case fails.
func TestARenewalIsTiedToThePasteItDescendsFrom(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "read", false)
	fx.f.expires[readToken] = now.Add(5 * 24 * time.Hour).Unix()
	if _, err := fx.p.Prove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.f.exchange != 1 {
		t.Fatalf("a token five days from its end was renewed %d times", fx.f.exchange)
	}
	raw, _, ok, _ := fx.ren.Held(context.Background(), "read")
	var r metaads.Renewal
	_ = json.Unmarshal([]byte(raw), &r)
	sum := sha256.Sum256([]byte(readToken))
	if !ok || r.Token != renewedToken || r.From != hex.EncodeToString(sum[:]) {
		t.Fatalf("the renewal is %+v (%v)", r, ok)
	}
	if strings.Contains(raw, readToken) {
		t.Error("the renewal keeps the pasted token")
	}
	if _, err := fx.p.Prove(context.Background()); err != nil || fx.f.exchange != 1 {
		t.Errorf("a second pass the same day renewed again (%d, %v)", fx.f.exchange, err)
	}
	stale := metaads.Renewal{Token: renewedToken, From: "another-paste"}
	data, _ := json.Marshal(stale)
	fx.ren.vals["read"] = string(data)
	if _, err := fx.p.Prove(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _, _, _ = fx.ren.Held(context.Background(), "read")
	_ = json.Unmarshal([]byte(raw), &r)
	if r.From != hex.EncodeToString(sum[:]) {
		t.Errorf("a renewal of another paste outlived it: %+v", r)
	}
	fx.doc.Integrations.MetaAds.Enabled = false
	fx.ren.vals["read"] = string(data)
	if _, err := fx.p.Prove(context.Background()); !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("a pass turned off answered %v", err)
	}
	if _, _, ok, _ := fx.ren.Held(context.Background(), "read"); ok {
		t.Error("turning the integration off kept a renewal")
	}
}

// A renewal Meta refuses is tried once a day, and says the date the token
// ends as an expiring credential.
//
// Mutation: drop the day's mark, and the second pass tries again.
func TestAFailedRenewalIsTriedOnceADay(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "read", false)
	fx.f.noRenew = true
	fx.f.expires[readToken] = now.Add(5 * 24 * time.Hour).Unix()
	findings, err := fx.p.Prove(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expiring := false
	for _, f := range findings {
		expiring = expiring || (f.Kind == integration.FindingCredentialExpiring && !f.ExpiresAt.IsZero())
	}
	if fx.f.exchange != 1 || !expiring {
		t.Fatalf("a refused renewal was tried %d times and raised %+v", fx.f.exchange, findings)
	}
	if _, err := fx.p.Prove(context.Background()); err != nil || fx.f.exchange != 1 {
		t.Errorf("a second pass the same day tried again (%d, %v)", fx.f.exchange, err)
	}
}

// A pass raises what the design names: Require App Secret off, an ad account
// named here the system user does not reach, and an operate seat with no
// operate token.
//
// Mutation: skip the proofless read or the reach check, and its finding goes.
func TestThePassRaisesWhatAPersonMustFix(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "operate", false)
	fx.f.noProof = true
	fx.doc.Integrations.MetaAds.AdAccounts = []string{"act_111", "act_333"}
	fx.doc.Integrations.MetaAds.OperateToken = ""
	findings, err := fx.p.Prove(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[integration.FindingKind]string{}
	for _, f := range findings {
		kinds[f.Kind] += f.Detail + f.Title
	}
	if !strings.Contains(kinds[integration.FindingGrantExcess], "Require App Secret") ||
		!strings.Contains(kinds[integration.FindingGrantShort], "act_333") ||
		!strings.Contains(kinds[integration.FindingGrantShort], "operate") {
		t.Errorf("the pass raised %v", kinds)
	}
}
