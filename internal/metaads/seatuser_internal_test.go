package metaads

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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// The fake business's app and tokens, in Meta's shape.
const (
	fakeApp      = "987654321012345"
	fakeSecret   = "0123456789abcdef0123456789abcdef"
	fakeBusiness = "5550001"
	fakeAdmin    = "EAAGadminSYSTEMUSERaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// fakeToken is one token as Meta knows it.
type fakeToken struct {
	user, app, kind string
	scopes          []string
	expires         int64
	dead            bool
}

// graphFake is a fake Graph API for one business: its system users, the
// tasks each holds on each ad account, its tokens, and every request.
type graphFake struct {
	mu       sync.Mutex
	users    []systemUser
	tasks    map[string]map[string][]string // user -> account -> tasks
	installs map[string]bool
	tokens   map[string]*fakeToken
	minted   int
	refuse   map[string]map[string]any // "METHOD path" -> Meta's error
	tier     string
	calls    []string
	bodies   []string
}

func newGraphFake() *graphFake {
	f := &graphFake{
		users: []systemUser{{ID: "100", Name: "admin", Role: roleAdmin}, {ID: "101", Name: "Crewlet", Role: "EMPLOYEE"}},
		tasks: map[string]map[string][]string{}, installs: map[string]bool{}, refuse: map[string]map[string]any{},
		tokens: map[string]*fakeToken{fakeAdmin: {user: "100", app: fakeApp, kind: "SYSTEM_USER",
			scopes: []string{"business_management"}}},
	}
	return f
}

func fakeProof(token string) string {
	m := hmac.New(sha256.New, []byte(fakeSecret))
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *graphFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	q := r.URL.Query()
	path := strings.TrimPrefix(r.URL.Path, "/"+Version)
	f.calls, f.bodies = append(f.calls, r.Method+" "+path), append(f.bodies, string(raw)+"?"+r.URL.RawQuery)
	if f.tier != "" {
		w.Header().Set("X-Ad-Account-Usage", `{"acc_id_util_pct":1,"ads_api_access_tier":"`+f.tier+`"}`)
	}
	if e, ok := f.refuse[r.Method+" "+path]; ok {
		status := 400
		if n, ok := e["status"].(int); ok {
			status = n
		}
		answer(w, status, map[string]any{"error": e})
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if path == "/debug_token" {
		if bearer != fakeApp+"|"+fakeSecret {
			answer(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "app token required"}})
			return
		}
		t := f.tokens[q.Get("input_token")]
		if t == nil {
			answer(w, 200, map[string]any{"data": map[string]any{"is_valid": false, "error": map[string]any{"subcode": 467}}})
			return
		}
		answer(w, 200, map[string]any{"data": map[string]any{"app_id": t.app, "type": t.kind, "is_valid": !t.dead,
			"scopes": t.scopes, "user_id": t.user, "expires_at": t.expires}})
		return
	}
	if q.Get("appsecret_proof")+form.Get("appsecret_proof") != fakeProof(bearer) {
		answer(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "appsecret_proof required"}})
		return
	}
	caller := f.tokens[bearer]
	if caller == nil || caller.dead {
		answer(w, 400, map[string]any{"error": map[string]any{"code": 190, "message": "Invalid token " + bearer}})
		return
	}
	admin := caller.user == "100"
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/oauth/revoke" && r.Method == "POST":
		if form.Get("client_secret") != fakeSecret || form.Get("client_id") != fakeApp {
			answer(w, 400, map[string]any{"error": map[string]any{"code": 101, "message": "bad client"}})
			return
		}
		if t := f.tokens[form.Get("revoke_token")]; t != nil {
			t.dead = true
		}
		answer(w, 200, map[string]any{"success": true})
	case path == "/me":
		name := ""
		for _, u := range f.users {
			if u.ID == caller.user {
				name = u.Name
			}
		}
		answer(w, 200, map[string]any{"id": caller.user, "name": name})
	case path == "/me/adaccounts":
		var data []any
		for a, tasks := range f.tasks[caller.user] {
			data = append(data, map[string]any{"id": a, "user_tasks": tasks})
		}
		answer(w, 200, map[string]any{"data": data})
	case path == "/"+fakeBusiness+"/system_users" && r.Method == "GET" && admin:
		answer(w, 200, map[string]any{"data": f.users})
	case path == "/"+fakeBusiness+"/system_users" && r.Method == "POST" && admin:
		id := fmt.Sprint(200 + len(f.users))
		f.users = append(f.users, systemUser{ID: id, Name: form.Get("name"), Role: form.Get("role")})
		answer(w, 200, map[string]any{"id": id})
	case len(parts) == 1 && strings.HasPrefix(parts[0], "act_") && r.Method == "GET":
		answer(w, 200, map[string]any{"id": parts[0]})
	case len(parts) == 2 && parts[1] == "applications" && r.Method == "POST" && admin:
		if form.Get("business_app") != fakeApp {
			answer(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "no such app"}})
			return
		}
		if f.installs[parts[0]] {
			// A SECOND INSTALL is refused here, the stricter of what Meta may answer.
			answer(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "already installed"}})
			return
		}
		f.installs[parts[0]] = true
		answer(w, 200, map[string]any{"success": true})
	case len(parts) == 2 && parts[1] == "assigned_users" && admin:
		user := form.Get("user") + q.Get("user")
		if f.tasks[user] == nil {
			f.tasks[user] = map[string][]string{}
		}
		if r.Method == "DELETE" {
			delete(f.tasks[user], parts[0])
		} else {
			var tasks []string
			_ = json.Unmarshal([]byte(form.Get("tasks")), &tasks)
			f.tasks[user][parts[0]] = tasks
		}
		answer(w, 200, map[string]any{"success": true})
	case len(parts) == 2 && parts[1] == "access_tokens" && r.Method == "POST" && admin:
		if !f.installs[parts[0]] {
			answer(w, 400, map[string]any{"error": map[string]any{"code": 100, "message": "app not installed"}})
			return
		}
		f.minted++
		v := fmt.Sprintf("EAAGseat%sTOKEN%dzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", parts[0], f.minted)
		exp := int64(0)
		if form.Get("set_token_expires_in_60_days") == "true" {
			exp = 1_900_000_000
		}
		f.tokens[v] = &fakeToken{user: parts[0], app: fakeApp, kind: "SYSTEM_USER",
			scopes: strings.Split(form.Get("scope"), ","), expires: exp}
		answer(w, 200, map[string]any{"access_token": v})
	case len(parts) == 2 && parts[1] == "access_tokens" && r.Method == "DELETE" && admin:
		for _, t := range f.tokens {
			if t.user == parts[0] {
				t.dead = true
			}
		}
		answer(w, 200, map[string]any{"success": true})
	default:
		answer(w, 400, map[string]any{"error": map[string]any{"code": 200, "message": "Permissions error " + path}})
	}
}

// fakeGraph is a graph over a fresh fake, with the admin token where admin.
func fakeGraph(t *testing.T, admin bool) (*graph, *graphFake) {
	t.Helper()
	f := newGraphFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	g := &graph{client: NewClient(srv.URL, fakeSecret, srv.Client()), appID: fakeApp, business: fakeBusiness}
	if admin {
		g.admin = fakeAdmin
	}
	return g, f
}

// handToken gives the fake a token of user holding scopes, as a person
// generating one in Business settings would.
func (f *graphFake) handToken(user string, scopes ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := "EAAGhand" + user + strings.Repeat("h", 40)
	f.tokens[v] = &fakeToken{user: user, app: fakeApp, kind: "SYSTEM_USER", scopes: scopes}
	return v
}

func TestASeatsSystemUserIsMadeAssignedAndMintedWithTheAdminToken(t *testing.T) {
	g, f := fakeGraph(t, true)
	scopes := seatScopes(growth.LevelOperate, map[string][]string{slotRead: {"ads_read", "pages_show_list"},
		slotOperate: {"ads_management", "pages_read_engagement"}})
	made, err := g.make(context.Background(), "crewlet-growth", growth.LevelOperate, []string{"act_111"}, nil, scopes)
	if err != nil {
		t.Fatal(err)
	}
	if made.Name != "crewlet-growth" || made.Token == "" {
		t.Fatalf("made %+v", made)
	}
	u := f.users[len(f.users)-1]
	if u.Name != "crewlet-growth" || u.Role != "EMPLOYEE" || u.ID != made.UserID {
		t.Fatalf("Meta holds %+v, want an Employee system user named crewlet-growth", u)
	}
	if got := f.tasks[u.ID]["act_111"]; !slices.Equal(got, []string{taskAdvertise, taskAnalyze}) {
		t.Fatalf("act_111 tasks %v, want the advertiser set", got)
	}
	tok := f.tokens[made.Token]
	if tok.user != u.ID || tok.expires == 0 {
		t.Fatalf("minted %+v, want a sixty-day token of the new system user", tok)
	}
	if !slices.Equal(tok.scopes, []string{"ads_management", "ads_read", "pages_read_engagement", "pages_show_list"}) {
		t.Fatalf("scopes %v, want the level's base and the company's within it", tok.scopes)
	}
	read, err := g.check(context.Background(), made.Token, growth.LevelOperate, []string{"act_111"})
	if err != nil || read.Name != "crewlet-growth" || read.Role != "EMPLOYEE" {
		t.Fatalf("check of the minted token: %+v, %v", read, err)
	}
}

func TestASeatThatComesBackReusesItsSystemUser(t *testing.T) {
	g, f := fakeGraph(t, true)
	f.users = append(f.users, systemUser{ID: "150", Name: "crewlet-growth", Role: "EMPLOYEE"})
	f.tasks["150"] = map[string][]string{"act_222": {taskAnalyze}}
	f.installs["150"] = true
	made, err := g.make(context.Background(), "crewlet-growth", growth.LevelRead, []string{"act_111"},
		[]string{"act_222"}, seatScopes(growth.LevelRead, nil))
	if err != nil {
		t.Fatal(err)
	}
	if made.UserID != "150" || slices.Contains(f.calls, "POST /"+fakeBusiness+"/system_users") {
		t.Fatalf("made %+v with calls %v, want system user 150 reused and none made", made, f.calls)
	}
	if _, ok := f.tasks["150"]["act_222"]; ok {
		t.Fatalf("act_222, which the seat no longer names, is still assigned: %v", f.tasks["150"])
	}
}

func TestTheSystemUserCapIsHeldBeforeMetaIsAsked(t *testing.T) {
	cases := []struct {
		name       string
		employees  int
		tier       string
		refuse     bool
		used, capN int
	}{
		{name: "full access holds ten", employees: 10, used: 10, capN: 10},
		{name: "limited access holds one", employees: 1, tier: "limited_access", used: 1, capN: 1},
		{name: "Meta's own refusal is the cap", employees: 4, refuse: true, used: 4, capN: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, f := fakeGraph(t, true)
			f.users = f.users[:1]
			for i := range tc.employees {
				f.users = append(f.users, systemUser{ID: fmt.Sprint(300 + i), Name: fmt.Sprint("other-", i), Role: "EMPLOYEE"})
			}
			// THE TIER IS HEARD on an ad account answer's usage header, as Meta says it.
			f.tier = tc.tier
			if tc.refuse {
				f.refuse["POST /"+fakeBusiness+"/system_users"] = map[string]any{"code": 100, "error_subcode": errCodeCap,
					"message": "This Business Manager has reached maximum number of system user limit."}
			}
			_, err := g.make(context.Background(), "crewlet-growth", growth.LevelRead, []string{"act_111"}, nil,
				[]string{"ads_read"})
			var capErr *CapError
			if !errors.As(err, &capErr) || capErr.Used != tc.used || capErr.Cap != tc.capN {
				t.Fatalf("err %v, want the cap %d of %d", err, tc.used, tc.capN)
			}
			if !tc.refuse && slices.Contains(f.calls, "POST /"+fakeBusiness+"/system_users") {
				t.Fatalf("Meta was asked past the cap: %v", f.calls)
			}
		})
	}
}

func TestAMakeThatFailsTakesBackWhatItAssigned(t *testing.T) {
	g, f := fakeGraph(t, true)
	f.refuse["POST /202/access_tokens"] = map[string]any{"code": 200, "message": "scope not approved"}
	_, err := g.make(context.Background(), "crewlet-growth", growth.LevelOperate, []string{"act_111", "act_333"}, nil,
		[]string{"ads_read", "ads_management"})
	if err == nil {
		t.Fatal("a failed mint made an identity")
	}
	if len(f.tasks["201"]) != 0 {
		t.Fatalf("the failed make left assignments %v", f.tasks["201"])
	}
}

func TestTheCheckHoldsASeatsTokenToItsLevel(t *testing.T) {
	cases := []struct {
		name     string
		level    growth.Level
		setup    func(f *graphFake) string
		admin    bool
		problem  string
		said     string
		warnings []string
	}{
		{name: "an operate token assigned its account works", level: growth.LevelOperate, admin: true,
			setup: func(f *graphFake) string {
				f.tasks["101"] = map[string][]string{"act_111": {taskAdvertise, taskAnalyze}}
				return f.handToken("101", "ads_management", "ads_read")
			}},
		{name: "a revoked token is refused", level: growth.LevelRead, problem: ProblemRevoked, said: "no longer accepts",
			setup: func(f *graphFake) string {
				v := f.handToken("101", "ads_read")
				f.tokens[v].dead = true
				return v
			}},
		{name: "another app's token is another business's", level: growth.LevelRead, problem: ProblemWrongTeam,
			said: "another business", setup: func(f *graphFake) string {
				v := f.handToken("101", "ads_read")
				f.tokens[v].app = "111222333"
				return v
			}},
		{name: "a person's token is not a system user's", level: growth.LevelRead, problem: ProblemWrongRole,
			said: "user token", setup: func(f *graphFake) string {
				v := f.handToken("101", "ads_read")
				f.tokens[v].kind = "USER"
				return v
			}},
		{name: "a read agent's token that writes is refused", level: growth.LevelRead, problem: ProblemWrongRole,
			said: "only reads", setup: func(f *graphFake) string {
				f.tasks["101"] = map[string][]string{"act_111": {taskAnalyze}}
				return f.handToken("101", "ads_read", "ads_management")
			}},
		{name: "business_management below administer is refused", level: growth.LevelOperate, problem: ProblemWrongRole,
			said: "business_management", setup: func(f *graphFake) string {
				return f.handToken("101", "ads_read", "ads_management", "business_management")
			}},
		{name: "an unassigned account is a wrong role", level: growth.LevelRead, problem: ProblemWrongRole,
			said: "act_111 is not assigned", setup: func(f *graphFake) string { return f.handToken("101", "ads_read") }},
		{name: "an analyst cannot operate", level: growth.LevelOperate, problem: ProblemWrongRole,
			said: "act_111 holds View performance", setup: func(f *graphFake) string {
				f.tasks["101"] = map[string][]string{"act_111": {taskAnalyze}}
				return f.handToken("101", "ads_read", "ads_management")
			}},
		{name: "an admin system user is refused", level: growth.LevelRead, admin: true, problem: ProblemWrongRole,
			said: "Admin system user", setup: func(f *graphFake) string {
				f.tasks["100"] = map[string][]string{"act_111": {taskAnalyze}}
				return f.handToken("100", "ads_read")
			}},
		{name: "another business's system user is a wrong team", level: growth.LevelRead, admin: true,
			problem: ProblemWrongTeam, said: "not one of business", setup: func(f *graphFake) string {
				f.tasks["777"] = map[string][]string{"act_111": {taskAnalyze}}
				return f.handToken("777", "ads_read")
			}},
		{name: "without the admin token the role is said unread", level: growth.LevelRead,
			warnings: []string{"cannot read the system user's role"}, setup: func(f *graphFake) string {
				f.tasks["101"] = map[string][]string{"act_111": {taskAnalyze}}
				return f.handToken("101", "ads_read")
			}},
		{name: "more than the level needs is allowed and said", level: growth.LevelOperate,
			warnings: []string{`Manage ad account on "act_111"`, `also reaches "act_999"`},
			setup: func(f *graphFake) string {
				f.tasks["101"] = map[string][]string{"act_111": {taskManage, taskAdvertise, taskAnalyze},
					"act_999": {taskAnalyze}}
				return f.handToken("101", "ads_read", "ads_management")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, f := fakeGraph(t, tc.admin)
			value := tc.setup(f)
			read, err := g.check(context.Background(), value, tc.level, []string{"act_111"})
			var problem *SeatProblem
			switch {
			case tc.problem == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.problem != "" && (!errors.As(err, &problem) || problem.Kind != tc.problem ||
				!strings.Contains(problem.Said, tc.said)):
				t.Fatalf("err %v, want %s saying %q", err, tc.problem, tc.said)
			}
			for _, w := range tc.warnings {
				if !strings.Contains(strings.Join(read.Warnings, " "), w) {
					t.Fatalf("warnings %v, want one saying %q", read.Warnings, w)
				}
			}
			if strings.Contains(fmt.Sprint(err), value) {
				t.Fatalf("the refusal repeats the token: %v", err)
			}
		})
	}
}

func TestASeatsScopesStayWithinItsLevelAndTheApp(t *testing.T) {
	company := map[string][]string{slotRead: {"ads_read", "pages_show_list", "read_insights"},
		slotOperate: {"ads_management", "pages_manage_ads", "pages_read_engagement"},
		slotAdmin:   {"business_management", "ads_management", "catalog_management"}}
	for _, tc := range []struct {
		level growth.Level
		want  []string
	}{
		{growth.LevelRead, []string{"ads_read", "pages_show_list", "read_insights"}},
		{growth.LevelOperate, []string{"ads_management", "ads_read", "pages_manage_ads", "pages_read_engagement",
			"pages_show_list", "read_insights"}},
		{growth.LevelAdminister, []string{"ads_management", "ads_read", "business_management", "catalog_management",
			"pages_manage_ads", "pages_read_engagement", "pages_show_list", "read_insights"}},
	} {
		got := seatScopes(tc.level, company)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.level, got, tc.want)
		}
		if why := seatPolicy(tc.level, got); why != "" {
			t.Errorf("%s: the minted scopes fail the check: %s", tc.level, why)
		}
	}
}

func TestRetiringASeatTakesItsAccountsAndRevokesEveryToken(t *testing.T) {
	g, f := fakeGraph(t, true)
	made, err := g.make(context.Background(), "crewlet-growth", growth.LevelRead, []string{"act_111"}, nil,
		[]string{"ads_read"})
	if err != nil {
		t.Fatal(err)
	}
	f.refuse["DELETE /act_999/assigned_users"] = map[string]any{"code": 100, "message": "not assigned"}
	if err := g.retire(context.Background(), made.UserID, made.Token, []string{"act_111", "act_999"}); err != nil {
		t.Fatal(err)
	}
	if len(f.tasks[made.UserID]) != 0 || !f.tokens[made.Token].dead {
		t.Fatalf("after retire: tasks %v, token dead %v", f.tasks[made.UserID], f.tokens[made.Token].dead)
	}
	if slices.Contains(f.calls, "DELETE /"+made.UserID) {
		t.Fatal("the system user was deleted; Meta deletes none, so it is kept for reuse")
	}
}

func TestARenewalIsMintedWithTheAdminTokenAndExchangedWithout(t *testing.T) {
	g, f := fakeGraph(t, true)
	made, err := g.make(context.Background(), "crewlet-growth", growth.LevelRead, []string{"act_111"}, nil,
		[]string{"ads_read"})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := g.renewal(context.Background(), made.UserID, made.Token, []string{"ads_read"})
	if err != nil || fresh == made.Token || f.tokens[fresh].user != made.UserID {
		t.Fatalf("renewal %q, %v", fresh, err)
	}
	if err := g.revoke(context.Background(), made.Token, fresh); err != nil || !f.tokens[made.Token].dead {
		t.Fatalf("revoke the old token: %v, dead %v", err, f.tokens[made.Token].dead)
	}
	if strings.Contains(strings.Join(f.calls, " "), fakeSecret) {
		t.Fatal("the app secret reached a URL")
	}
	for i, call := range f.calls {
		if call == "POST /oauth/revoke" && strings.Contains(f.bodies[i], "?client_secret") {
			t.Fatal("the app secret travelled in the query")
		}
	}
}

// THE PASS PROVES THE ADMIN SYSTEM USER TOKEN before anybody presses Make it
// for me: one Meta refuses, of another app, without business_management, of
// an Employee, or about to end is a finding; an Admin's is none.
func TestThePassProvesTheAdminSystemUserToken(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		setup func(f *graphFake)
		kind  integration.FindingKind
		said  string
	}{
		{name: "an admin system user's token is none"},
		{name: "a revoked token", kind: integration.FindingCredentialRejected, said: "no longer accepts",
			setup: func(f *graphFake) { f.tokens[fakeAdmin].dead = true }},
		{name: "another app's", kind: integration.FindingCredentialRejected, said: "another Meta app",
			setup: func(f *graphFake) { f.tokens[fakeAdmin].app = "111" }},
		{name: "no business_management", kind: integration.FindingCredentialRejected, said: "lacks business_management",
			setup: func(f *graphFake) { f.tokens[fakeAdmin].scopes = []string{"ads_read"} }},
		{name: "an employee's", kind: integration.FindingCredentialRejected, said: "not an Admin system user",
			setup: func(f *graphFake) { f.users[0].Role = "EMPLOYEE" }},
		{name: "ending within ten days", kind: integration.FindingCredentialExpiring, said: "expires on",
			setup: func(f *graphFake) { f.tokens[fakeAdmin].expires = now.Add(5 * 24 * time.Hour).Unix() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, f := fakeGraph(t, true)
			if tc.setup != nil {
				tc.setup(f)
			}
			got, err := g.proveAdmin(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.kind == "" && got != nil:
				t.Fatalf("finding %+v for a working admin token", got)
			case tc.kind != "" && (got == nil || got.Kind != tc.kind || !strings.Contains(got.Detail, tc.said)):
				t.Fatalf("finding %+v, want %s saying %q", got, tc.kind, tc.said)
			}
		})
	}
}
