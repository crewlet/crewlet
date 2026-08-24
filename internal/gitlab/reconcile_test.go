package gitlab_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/provision"
)

// adminInstance is a GitLab that remembers what was done to it.
//
// A stand-in rather than a mock: the reconcile's whole job is a sequence of
// calls whose ORDER and IDEMPOTENCE matter, and asserting on expectations
// per-call would test the sequence this was written with rather than the
// property it has to hold.
type adminInstance struct {
	mu sync.Mutex

	users      map[string]int          // username -> id
	people     map[string]bool         // usernames the instance treats as humans
	tokens     map[int][]*gitlab.Token // user id -> its token rows
	members    map[string]int          // "group:7" / "proj:a/b" -> access level
	hooks      []gitlab.Hook
	hookBodies []map[string]any
	// signsNothing models a GitLab older than 19.1: it ACCEPTS the
	// signing_token attribute, ignores it, and answers 200 — so the write
	// succeeds and the hook still cannot sign. That is the only failure
	// mode confirmSigned exists for, and it is invisible without a fake
	// that reproduces it.
	signsNothing bool
	updatedHooks []string

	// noGroupHooks makes the GROUP hooks API answer 404, the way GitLab
	// hides an endpoint the instance's tier does not serve.
	noGroupHooks bool
	// hookStatus answers the group-hooks route with this status instead,
	// for the refusals that are NOT a tier gate.
	hookStatus int
	// projectHooks is what the per-project route holds, keyed by project
	// path.
	projectHooks map[string][]gitlab.Hook
	// missingProjects answer the existence probe with a 404, the way a
	// renamed or not-yet-created repository does.
	missingProjects map[string]bool
	// projectStatus answers the existence probe with this status instead,
	// for the refusals that are NOT absence.
	projectStatus int

	// failToken makes minting fail for this username, to reach the
	// rollback path.
	failToken string
	// failTokenRevoke makes revocation fail, to reach the "cleanup did
	// not finish" report.
	failTokenRevoke bool
	// mintEmpty answers a mint with a 200 carrying no token value, which
	// is a real GitLab response shape and the worst one.
	mintEmpty bool
	// identityFails makes the identity route answer 500 for a SEAT's
	// token, which is "cannot tell" rather than "this token is bad".
	identityFails bool

	// now is the instant token expiry is judged against, so a test can
	// age a token without waiting.
	now       time.Time
	nextID    int
	nextToken int
	revokes   int
	// mintBodies are the token-mint payloads, so a test can assert what
	// was SENT rather than what the fake chose to remember about it.
	mintBodies []map[string]any
	calls      []string
}

// adminToken is the operator credential the fixture's client presents.
const adminToken = "admin-token"

func newAdminInstance() *adminInstance {
	return &adminInstance{
		users: map[string]int{}, tokens: map[int][]*gitlab.Token{},
		people:  map[string]bool{},
		members: map[string]int{}, nextID: 100, nextToken: 1,
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *adminInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	f.calls = append(f.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && path == "/user":
		// WHOEVER PRESENTED THE TOKEN. The re-run check takes the value
		// a variable holds and asks the instance who it is, so a fake
		// answering the same account for every token would prove
		// nothing.
		presented := r.Header.Get("PRIVATE-TOKEN")
		if f.identityFails && presented != adminToken {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"500"}`))
			return
		}
		if presented == adminToken {
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "username": "root"})
			return
		}
		for id, tokens := range f.tokens {
			for _, token := range tokens {
				live := !token.Revoked &&
					(token.ExpiresAt.IsZero() || token.ExpiresAt.After(f.now))
				if token.Value == presented && live {
					json.NewEncoder(w).Encode(map[string]any{"id": id})
					return
				}
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"401 Unauthorized"}`))

	case r.Method == http.MethodGet && path == "/groups/7/members":
		out := make([]map[string]any, 0, len(f.users))
		for name, id := range f.users {
			out = append(out, map[string]any{"id": id, "username": name})
		}
		sortByUsername(out)
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/groups/7/service_accounts/"):
		id := atoi(strings.TrimPrefix(path, "/groups/7/service_accounts/"))
		for name, uid := range f.users {
			if uid != id {
				continue
			}
			if f.people[name] {
				// GitLab refuses to delete an account that is not a
				// service account, which is the guard that makes this
				// operation human-safe.
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"message":"400 Bad request - Not a service account"}`))
				return
			}
			delete(f.users, name)
			delete(f.tokens, id)
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && path == "/groups/nimbus":
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "full_path": "nimbus"})

	case r.Method == http.MethodGet && path == "/users":
		// A FILTER, NOT A LOOKUP — which is what /users?username= is on
		// several GitLab versions: it returns prefix matches, so a
		// caller that took the first row would find `crewlet-swe-old`
		// when it asked for `crewlet-swe`.
		username := r.URL.Query().Get("username")
		var out []map[string]any
		for name, id := range f.users {
			if strings.HasPrefix(name, username) {
				out = append(out, map[string]any{"id": id, "username": name})
			}
		}
		sortByUsername(out)
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == "/groups/7/service_accounts":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		f.users[body["username"]] = f.nextID
		json.NewEncoder(w).Encode(map[string]any{
			"id": f.nextID, "username": body["username"],
		})

	case r.Method == http.MethodPost && path == "/groups/7/members":
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		key := fmt.Sprintf("group:%d", body["user_id"])
		if _, already := f.members[key]; already {
			// A REAL INSTANCE 409s a second add. The reconcile has to
			// treat that as success or a second run fails on what the
			// first one did.
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.members[key] = body["access_level"]

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/members"):
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		f.members[path+fmt.Sprint(body["user_id"])] = body["access_level"]

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/personal_access_tokens"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/personal_access_tokens")
		if f.failToken != "" && f.usernameOf(id) == f.failToken {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"not permitted"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.nextToken++
		token := &gitlab.Token{
			ID: f.nextToken, Name: fmt.Sprint(body["name"]),
			Value: fmt.Sprintf("glpat-minted-%d", f.nextToken),
		}
		if raw, ok := body["expires_at"].(string); ok {
			at, _ := time.Parse(time.DateOnly, raw)
			token.ExpiresAt = gitlab.Date{Time: at}
		}
		// The token EXISTS either way — that is what makes the empty
		// response so bad — so it is recorded on the account regardless.
		f.tokens[atoi(id)] = append(f.tokens[atoi(id)], token)
		f.mintBodies = append(f.mintBodies, body)
		if f.mintEmpty {
			json.NewEncoder(w).Encode(map[string]any{"id": token.ID})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token": token.Value, "id": token.ID, "name": token.Name,
		})

	case r.Method == http.MethodGet && path == "/personal_access_tokens":
		id := atoi(r.URL.Query().Get("user_id"))
		out := make([]map[string]any, 0, len(f.tokens[id]))
		for _, t := range f.tokens[id] {
			row := map[string]any{"id": t.ID, "name": t.Name, "revoked": t.Revoked}
			if !t.ExpiresAt.IsZero() {
				row["expires_at"] = t.ExpiresAt.Format(time.DateOnly)
			}
			out = append(out, row)
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/personal_access_tokens/"):
		if f.failTokenRevoke {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.revokes++
		gone := atoi(strings.TrimPrefix(path, "/personal_access_tokens/"))
		for _, tokens := range f.tokens {
			for _, t := range tokens {
				if t.ID == gone {
					t.Revoked = true
				}
			}
		}

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/projects/") &&
		!strings.HasSuffix(path, "/members") && !strings.HasSuffix(path, "/hooks"):
		// The existence probe. A project this instance does not have
		// answers 404, which the reconcile reads as data.
		//
		// The project path arrives DECODED — net/http hands
		// r.URL.Path with %2F already turned back into a slash — so
		// "nimbus/api" is what a route sees, not "nimbus%2Fapi".
		project := strings.TrimPrefix(path, "/projects/")
		if f.projectStatus != 0 {
			w.WriteHeader(f.projectStatus)
			json.NewEncoder(w).Encode(map[string]any{"message": "refused"})
			return
		}
		if f.missingProjects[project] {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "404 Project Not Found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 99, "path_with_namespace": project})

	case strings.HasSuffix(path, "/hooks") && strings.HasPrefix(path, "/projects/"):
		project := strings.TrimSuffix(strings.TrimPrefix(path, "/projects/"), "/hooks")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(f.projectHooks[project])
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		hook := gitlab.Hook{
			ID: len(f.projectHooks[project]) + 1, URL: body["url"].(string),
			SigningTokenPresent: f.signs(body),
		}
		if f.projectHooks == nil {
			f.projectHooks = map[string][]gitlab.Hook{}
		}
		f.projectHooks[project] = append(f.projectHooks[project], hook)
		json.NewEncoder(w).Encode(hook)

	case strings.HasPrefix(path, "/groups/7/hooks") && f.hookStatus != 0:
		w.WriteHeader(f.hookStatus)
		json.NewEncoder(w).Encode(map[string]any{"message": "refused"})

	case strings.HasPrefix(path, "/groups/7/hooks") && f.noGroupHooks:
		// GitLab HIDES a licensed endpoint rather than answering 402, so
		// Free says "not found" about a feature it has.
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"message": "404 Not Found"})

	case r.Method == http.MethodGet && path == "/groups/7/hooks":
		json.NewEncoder(w).Encode(f.hooks)

	case r.Method == http.MethodPost && path == "/groups/7/hooks":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		hook := gitlab.Hook{
			ID: len(f.hooks) + 1, URL: body["url"].(string),
			SigningTokenPresent: f.signs(body),
		}
		f.hooks = append(f.hooks, hook)
		f.hookBodies = append(f.hookBodies, body)
		json.NewEncoder(w).Encode(hook)

	case r.Method == http.MethodPut && strings.Contains(path, "/hooks/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		f.updatedHooks = append(f.updatedHooks, path)
		// A PUT that carries a signing token makes the hook report one on
		// the next GET, exactly as GitLab does.
		target, _ := body["url"].(string)
		for i := range f.hooks {
			if f.hooks[i].URL == target {
				f.hooks[i].SigningTokenPresent = f.signs(body)
			}
		}
		for project := range f.projectHooks {
			for i := range f.projectHooks[project] {
				if f.projectHooks[project][i].URL == target {
					f.projectHooks[project][i].SigningTokenPresent = f.signs(body)
				}
			}
		}

	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"404 Not Found"}`))
	}
}

// signs is what GitLab reports on the next GET: a signing token is present
// when one was sent and this instance is new enough to honour it.
func (f *adminInstance) signs(body map[string]any) bool {
	if f.signsNothing {
		return false
	}
	token, _ := body["signing_token"].(string)
	return token != ""
}

func (f *adminInstance) usernameOf(id string) string {
	for name, uid := range f.users {
		if fmt.Sprint(uid) == id {
			return name
		}
	}
	return ""
}

func (f *adminInstance) liveTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, tokens := range f.tokens {
		for _, t := range tokens {
			if !t.Revoked {
				n++
			}
		}
	}
	return n
}

// forget clears the counters, so a test can measure ONE run.
func (f *adminInstance) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes, f.mintBodies, f.calls = 0, nil, nil
}

func (f *adminInstance) revoked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokes
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// recordingSink is a sink that can be made to fail, to reach the rollback.
type recordingSink struct {
	mu     sync.Mutex
	values map[string]string
	failOn string
	// holdsErr makes the sink unreadable, which must never be read as
	// "nothing is held" — that would rotate every live credential.
	holdsErr error
	discards int
}

func newRecordingSink() *recordingSink {
	return &recordingSink{values: map[string]string{}}
}

func (s *recordingSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failOn {
		return errors.New("the store is unreachable")
	}
	s.values[name] = value
	return nil
}

func (s *recordingSink) Discard(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	s.values = map[string]string{}
	return nil
}

// Holds implements the sink contract: this fixture starts empty, so
// nothing is held until this run records it.
func (s *recordingSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holdsErr != nil {
		return "", false, s.holdsErr
	}
	return s.values[name], s.values[name] != "", nil
}

func (s *recordingSink) Flush(context.Context) error { return nil }

// seed puts a value in the sink as an EARLIER run would have.
func (s *recordingSink) seed(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
}

func (s *recordingSink) value(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}
func (s *recordingSink) Describe() string { return "a test sink" }

func (s *recordingSink) recorded() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

func reconcileAgainst(t *testing.T, f *adminInstance, sink provision.TokenSink,
	seats map[string]string,
) (*gitlab.Result, error) {
	t.Helper()
	return reconcileWith(t, f, sink, seats, func(*gitlab.Options) {})
}

// reconcileWith is the same run with the options a test wants to vary.
func reconcileWith(t *testing.T, f *adminInstance, sink provision.TokenSink,
	seats map[string]string, tune func(*gitlab.Options),
) (*gitlab.Result, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	// THE SERVER'S OWN CLIENT, whose transport belongs to this server and
	// dies with it. A client over http.DefaultTransport shares one
	// connection pool with every other parallel test, so one server's
	// Close breaks a request in flight against another.
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg := enabledGitLab()
	cfg.Provisioning.Projects = []string{"nimbus/api"}
	plan := &provision.Plan{}
	for handle, tokenVar := range seats {
		plan.Add(provision.Seat{
			Handle: handle, Role: strings.ToUpper(handle),
			TokenVar: tokenVar, Email: handle + "@noreply.crewlet.invalid",
		})
	}
	opts := gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		WebhookBase: "https://crewlet.example.com", SigningSecret: "whsec-abc",
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	tune(&opts)
	return gitlab.Reconcile(context.Background(), opts)
}

// A RECONCILE CREATES WHAT IS MISSING AND RECORDS WHAT IT MINTS.
func TestAReconcileProvisionsEverySeat(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	res, err := reconcileAgainst(t, f, sink, map[string]string{
		"swe": "GITLAB_TOKEN_SWE", "cto": "GITLAB_TOKEN_CTO",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 2 || len(res.Rotated) != 2 {
		t.Fatalf("result = %+v, want two created and two rotated", res)
	}
	got := sink.recorded()
	for _, name := range []string{"GITLAB_TOKEN_SWE", "GITLAB_TOKEN_CTO"} {
		if !strings.HasPrefix(got[name], "glpat-minted-") {
			t.Errorf("%s = %q, want a minted token", name, got[name])
		}
	}
	if res.Hooked != "https://crewlet.example.com/webhooks/gitlab" {
		t.Errorf("hooked %q", res.Hooked)
	}
}

// RUNNING IT TWICE IS SAFE AND QUIET about the accounts — but it DOES rotate
// the tokens, because a personal access token's value is returned once and
// there is no "already correct" state to detect.
func TestASecondRunCreatesNothingAndRotatesEverything(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, newRecordingSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sink := newRecordingSink()
	res, err := reconcileAgainst(t, f, sink, seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("the second run created %v", res.Created)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("the second run rotated %v, want the token minted again", res.Rotated)
	}
	if sink.recorded()["GITLAB_TOKEN_SWE"] == "" {
		t.Error("the second run recorded nothing")
	}
}

// A RUN THAT CANNOT RECORD WHAT IT MINTED REVOKES IT. Between the vendor
// minting a token and the sink recording it, the only copy of a live
// credential is in this process's memory — a failure there leaves it live,
// unusable and unknown.
func TestAFailedRecordRevokesEveryMintedToken(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	sink.failOn = "GITLAB_TOKEN_SWE"

	_, err := reconcileAgainst(t, f, sink, map[string]string{
		"cto": "GITLAB_TOKEN_CTO", "swe": "GITLAB_TOKEN_SWE",
	})
	if err == nil {
		t.Fatal("a failed record was reported as a successful run")
	}
	if live := f.liveTokens(); live != 0 {
		t.Fatalf("%d minted token(s) are still live after a rollback", live)
	}
	if sink.discards == 0 {
		t.Error("the sink was not asked to discard what it had recorded")
	}
	if len(sink.recorded()) != 0 {
		t.Errorf("the sink still holds %v", sink.recorded())
	}
	// THE ORIGINAL CAUSE SURVIVES. It is what an operator has to fix, and
	// a cleanup message that replaced it would hide the cause behind its
	// consequence.
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("the error lost the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("the error does not say the tokens were revoked: %v", err)
	}
}

// AND WHEN THE CLEANUP ITSELF FAILS, the report says so loudly: those
// credentials are live, nothing can use them, and only a human can remove
// them.
func TestAFailedRollbackNamesWhatIsStillLive(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.failTokenRevoke = true
	sink := newRecordingSink()
	sink.failOn = "GITLAB_TOKEN_SWE"

	_, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err == nil {
		t.Fatal("a failed run with a failed rollback was reported as success")
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Errorf("the error does not tell the operator to intervene: %v", err)
	}
}

// A HOOK IS MATCHED ON ITS URL and updated rather than duplicated, because
// the signing secret may have rotated — and an instance may carry hooks
// somebody else registered, which a run must not replace.
func TestAnExistingHookIsUpdatedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []gitlab.Hook{
		{ID: 9, URL: "https://someone-else.example.com/hook"},
		{ID: 10, URL: "https://crewlet.example.com/webhooks/gitlab"},
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 2 {
		t.Fatalf("hooks = %+v, want the existing pair untouched", f.hooks)
	}
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want one update", len(f.hookBodies))
	}
	// AND IT UPDATED OURS. An instance may carry hooks somebody else
	// registered, and a run that re-pointed the first one it found would
	// take down an unrelated integration — silently, since the count of
	// writes would look identical.
	if len(f.updatedHooks) != 1 || !strings.HasSuffix(f.updatedHooks[0], "/hooks/10") {
		t.Fatalf("updated %v, want only our own hook 10", f.updatedHooks)
	}
	if f.hookBodies[0]["signing_token"] != "whsec-abc" {
		t.Errorf("the update did not carry the signing secret: %v", f.hookBodies[0])
	}
}

// THE HOOK SUBSCRIBES TO WHAT THE PARSER ROUTES, and no more: a subscription
// to something nothing routes is delivery the engine answers 200 and drops,
// which looks from the instance's side like a healthy integration.
func TestTheHookSubscribesToExactlyWhatIsRouted(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body := f.hookBodies[0]
	for _, on := range []string{
		"issues_events", "merge_requests_events", "note_events", "pipeline_events",
	} {
		if body[on] != true {
			t.Errorf("%s is not subscribed", on)
		}
	}
	// OFF IS STATED, NOT OMITTED.
	//
	// This assertion used to demand that push be ABSENT from the body,
	// which is the bug it was written to prevent: GitLab defaults
	// push_events to TRUE, so a body that never mentions push subscribes
	// to it. Measured on a real instance — the hook came back
	// push_events: true from exactly this body.
	for _, off := range []string{
		"push_events", "tag_push_events", "job_events", "wiki_page_events",
		"deployment_events", "releases_events", "emoji_events",
		"confidential_issues_events", "confidential_note_events",
	} {
		value, present := body[off]
		if !present {
			t.Errorf("%s is omitted, which leaves it at whatever this "+
				"GitLab version defaults it to", off)
			continue
		}
		if value != false {
			t.Errorf("%s is subscribed, and nothing routes it", off)
		}
	}
	// TLS VERIFICATION STAYS ON. A provisioner that turned it off for a
	// self-signed development instance would leave it off in production,
	// where the hook carries a signing secret.
	if body["enable_ssl_verification"] != true {
		t.Error("TLS verification is off on a hook that carries a secret")
	}
}

// NO WEBHOOK BASE IS A NOTE, NOT A GUESS. A hook pointing at the wrong host
// is worse than no hook: the instance reports a healthy integration and the
// deliveries go somewhere nobody is looking.
func TestWithoutAPublicURLNoHookIsGuessed(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient(gitlab.ClientOptions{URL: srv.URL, Token: "admin"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	plan := &provision.Plan{}
	plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "T"})

	res, err := gitlab.Reconcile(context.Background(), gitlab.Options{
		Client: client, Config: enabledGitLab(), Plan: plan,
		Sink: newRecordingSink(),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Hooked != "" {
		t.Errorf("a hook was registered at %q with no public URL given", res.Hooked)
	}
	if len(res.Notes) == 0 || !strings.Contains(strings.Join(res.Notes, " "), "webhook") {
		t.Errorf("notes = %v, want the missing hook reported", res.Notes)
	}
}

func TestAReconcileNeedsAClientAndASink(t *testing.T) {
	t.Parallel()
	if _, err := gitlab.Reconcile(context.Background(), gitlab.Options{}); err == nil {
		t.Error("a reconcile with no client was accepted")
	}
	client, _ := gitlab.NewClient(gitlab.ClientOptions{URL: "https://x", Token: "t"})
	_, err := gitlab.Reconcile(context.Background(), gitlab.Options{Client: client})
	if !errors.Is(err, provision.ErrNoSink) {
		t.Errorf("err = %v, want ErrNoSink", err)
	}
}

// A CANCELLED RUN STILL REVOKES. Ctrl+C during provisioning is exactly when
// leaving credentials live is worst: the operator believes nothing happened.
// A rollback that inherited the cancelled context would do nothing at all.
func TestACancelledRunStillRevokesWhatItMinted(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient(gitlab.ClientOptions{URL: srv.URL, Token: "admin"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	plan := &provision.Plan{}
	plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})

	// The sink cancels the run as it is asked to record — the shape of an
	// interrupt arriving between minting and persisting.
	sink := &cancellingSink{cancel: cancel}
	_, err = gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: enabledGitLab(), Plan: plan, Sink: sink,
	})
	if err == nil {
		t.Fatal("a cancelled run was reported as successful")
	}
	if live := f.liveTokens(); live != 0 {
		t.Fatalf("%d token(s) are still live after a cancelled run", live)
	}
	if !sink.discarded {
		t.Error("the sink was not asked to discard after a cancelled run")
	}
}

// cancellingSink cancels the run's context from inside Record, which is the
// window a real interrupt lands in.
type cancellingSink struct {
	cancel    context.CancelFunc
	discarded bool
}

func (s *cancellingSink) Value(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (s *cancellingSink) Record(context.Context, string, string) error {
	s.cancel()
	return context.Canceled
}

func (s *cancellingSink) Discard(ctx context.Context) error {
	// A rollback that inherited the cancelled context would fail here
	// rather than clean up; asserting on ctx.Err() is what makes the
	// detachment visible rather than incidental.
	if ctx.Err() != nil {
		return fmt.Errorf("the rollback inherited a cancelled context")
	}
	s.discarded = true
	return nil
}

func (s *cancellingSink) Flush(context.Context) error { return nil }
func (s *cancellingSink) Describe() string            { return "a cancelling sink" }

// sortByUsername gives the fake a deterministic order, with the DECOY first
// — a caller taking element zero has to be wrong.
func sortByUsername(rows []map[string]any) {
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["username"].(string) < rows[j]["username"].(string)
	})
}

// A PREFIX MATCH IS NOT THE ACCOUNT. /users?username= filters rather than
// looks up on several GitLab versions, so a run that took the first row
// would mint a token for a retired account and record it as the live seat's
// — an authentication failure whose cause is invisible from either side.
func TestAPrefixMatchIsNotMistakenForTheAccount(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// A retired account whose name is a prefix of the one being sought,
	// and which sorts first.
	f.users["crewlet-swe-old"] = 55

	sink := newRecordingSink()
	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The run must have CREATED the real account rather than adopting the
	// decoy.
	if len(res.Created) != 1 {
		t.Fatalf("created %v, want the real account to have been created", res.Created)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens[55]) != 0 {
		t.Fatal("a token was minted on the retired account")
	}
	if _, exists := f.users["crewlet-swe"]; !exists {
		t.Fatal("the real account was never created")
	}
}

// A MINT THAT RETURNS NO VALUE IS A FAILURE, not a token. GitLab shows a
// personal access token's value once; a 200 carrying an empty one means the
// token exists, cannot be recovered, and would otherwise be recorded as the
// empty string — which resolves to an empty Bearer header and a 401 nobody
// can trace back here.
func TestAMintThatReturnsNoValueIsARollback(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.mintEmpty = true
	sink := newRecordingSink()

	_, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err == nil {
		t.Fatal("a token with no value was accepted")
	}
	if got := sink.recorded()["GITLAB_TOKEN_SWE"]; got != "" {
		t.Fatalf("an empty token was recorded as %q", got)
	}
	if !strings.Contains(err.Error(), "revoke") {
		t.Errorf("the error does not say the token has to be revoked: %v", err)
	}
}

// ---- what a re-run does and does not touch ------------------------------ //

// A PLAIN RE-RUN KEEPS A WORKING TOKEN. Rotating it would revoke what the
// running engine is authenticating with — an operator adding one seat would
// take the others down, from a command whose promise is that it is safe to
// re-run.
func TestARerunKeepsAWorkingToken(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.value("GITLAB_TOKEN_SWE")
	f.forget()

	res, err := reconcileAgainst(t, f, sink, seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 0 || len(res.Kept) != 1 {
		t.Fatalf("rotated %v, kept %v", res.Rotated, res.Kept)
	}
	if sink.value("GITLAB_TOKEN_SWE") != first {
		t.Error("the recorded credential changed under a running engine")
	}
	if f.revoked() != 0 {
		t.Errorf("a plain re-run revoked %d tokens", f.revoked())
	}
}

// -rotate IS THE OPERATOR ASKING, and it retires the previous token after
// recording the new one — never before, or a failed record leaves the seat
// with nothing.
func TestRotateMintsAfreshAndRetiresOnlyThisToolsToken(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.value("GITLAB_TOKEN_SWE")
	// An administrator's own token on the same account, which rotation
	// must not touch: nothing here knows what is using it.
	f.mu.Lock()
	for id := range f.tokens {
		f.tokens[id] = append(f.tokens[id], &gitlab.Token{
			ID: 9001, Name: "set up by an admin",
		})
	}
	f.mu.Unlock()
	f.forget()

	res, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) { o.Rotate = true })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v", res.Rotated)
	}
	if sink.value("GITLAB_TOKEN_SWE") == first {
		t.Error("-rotate left the credential alone")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	recorded := false
	for _, tokens := range f.tokens {
		for _, token := range tokens {
			if token.ID == 9001 && token.Revoked {
				t.Error("rotation revoked a token it did not mint")
			}
			if token.Value == sink.value("GITLAB_TOKEN_SWE") && !token.Revoked {
				recorded = true
			}
		}
	}
	// THE RECORDED VALUE MUST STILL BE LIVE. Retiring the previous
	// tokens re-lists them AFTER the mint, so the fresh one is in that
	// list — revoking it would record a credential that is already dead.
	if !recorded {
		t.Error("rotation revoked the token it had just recorded")
	}
}

// A VARIABLE NOBODY RECORDED IS MINTED INTO even though the account has a
// working token: GitLab will not show the value again, so minting is the
// only recovery.
func TestAnUnrecordedVariableIsMintedInto(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, newRecordingSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()
	fresh := newRecordingSink() // a second machine: nothing recorded here
	res, err := reconcileAgainst(t, f, fresh, seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
	if fresh.value("GITLAB_TOKEN_SWE") == "" {
		t.Error("nothing was recorded")
	}
}

// A REVOKED TOKEN IS MINTED OVER whatever the variable holds: a value whose
// credential is dead leaves the seat 401ing for ever.
func TestARevokedTokenIsReplacedEvenWithAValueOnRecord(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	for _, tokens := range f.tokens {
		for _, token := range tokens {
			token.Revoked = true
		}
	}
	f.mu.Unlock()
	f.forget()

	res, err := reconcileAgainst(t, f, sink, seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
}

// AN EXPIRED TOKEN IS NOT A LIVE ONE. GitLab serves the expiry as a bare
// date, which is not a timestamp and does not unmarshal as one.
func TestAnExpiredTokenIsReplaced(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	days := 30
	set := func(o *gitlab.Options) { o.ExpiryDays = &days }
	if _, err := reconcileWith(t, f, sink, seats, set); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if body := f.mintBodies[0]; body["expires_at"] != "2026-01-31" {
		t.Fatalf("expires_at = %v", body["expires_at"])
	}
	f.forget()

	// The instance's clock moves past the expiry too — otherwise the token
	// still authenticates and the run is right to keep it.
	f.mu.Lock()
	f.now = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	f.mu.Unlock()
	res, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.ExpiryDays = &days
		o.Now = func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("an expired token was kept: rotated = %v, kept = %v",
			res.Rotated, res.Kept)
	}
}

// NO EXPIRY IS SENT BY DEFAULT: nothing in Crewlet renews a credential on a
// schedule, so a lifetime nobody renews is an outage with a date on it.
func TestNoExpiryIsSentUnlessAskedFor(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.mintBodies) != 1 {
		t.Fatalf("minted %d times", len(f.mintBodies))
	}
	if _, set := f.mintBodies[0]["expires_at"]; set {
		t.Errorf("an unasked-for expiry was sent: %v", f.mintBodies[0]["expires_at"])
	}
}

// AN UNREADABLE SINK IS NOT AN EMPTY ONE. Reading it as empty would rotate
// every live credential in the company because a store blinked.
func TestAnUnreadableSinkStopsTheGitLabRun(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()
	sink.holdsErr = errors.New("the store is unreachable")
	if _, err := reconcileAgainst(t, f, sink, seats); err == nil {
		t.Fatal("an unreadable sink was read as holding nothing")
	}
	if f.revoked() != 0 {
		t.Errorf("%d tokens were revoked on an unreadable sink", f.revoked())
	}
}

// A ROLLBACK ON A PRE-EXISTING ACCOUNT REVOKES ONLY WHAT IT MINTED.
// Sweeping the account would take an administrator's own token with no way
// to tell that it had.
func TestARollbackOnAnExistingAccountSparesTheAdminsToken(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, newRecordingSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	for id := range f.tokens {
		f.tokens[id] = append(f.tokens[id], &gitlab.Token{
			ID: 9001, Name: "set up by an admin",
		})
	}
	f.mu.Unlock()
	f.forget()

	failing := newRecordingSink()
	failing.failOn = "GITLAB_TOKEN_SWE"
	if _, err := reconcileWith(t, f, failing, seats,
		func(o *gitlab.Options) { o.Rotate = true }); err == nil {
		t.Fatal("the run reported success with nothing recorded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tokens := range f.tokens {
		for _, token := range tokens {
			if token.ID == 9001 && token.Revoked {
				t.Fatal("the rollback revoked a token it did not mint")
			}
		}
	}
}

// RETIRED TOKENS ARE NOT RE-REVOKED. GitLab keeps a revoked row in the
// listing, and every rotation leaves another — so a run without that check
// issues one more pointless request than the run before it, for ever.
func TestRotationDoesNotReRevokeWhatItAlreadyRetired(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	rotate := func(o *gitlab.Options) { o.Rotate = true }
	for i := range 3 {
		if _, err := reconcileWith(t, f, sink, seats, rotate); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if i == 1 {
			f.forget()
		}
	}
	if n := f.revoked(); n != 1 {
		t.Errorf("the third run issued %d revocations, want 1 — the previous "+
			"rotation's row is already revoked", n)
	}
}

// -decommission DELETES THE ACCOUNTS WHOSE SEATS LEFT, and only those:
// scoped by the managed prefix AND by membership of this company's group,
// because either alone is too broad.
func TestDecommissionRemovesManagedAccountsWithNoSeat(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, newRecordingSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.users["crewlet-qa"] = 500 // a seat that used to exist
	f.users["ci-runner"] = 501  // somebody else's account in the group
	f.mu.Unlock()

	res, err := reconcileWith(t, f, newRecordingSink(), seats,
		func(o *gitlab.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Decommissioned) != 1 || res.Decommissioned[0] != "crewlet-qa" {
		t.Fatalf("decommissioned = %v", res.Decommissioned)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, still := f.users["ci-runner"]; !still {
		t.Error("an unmanaged account was deleted")
	}
	if _, still := f.users["crewlet-swe"]; !still {
		t.Error("a live seat's account was deleted")
	}
}

// A PERSON THE INSTANCE REFUSES TO DELETE is reported rather than aborting:
// that refusal is GitLab catching what the scan should not have proposed,
// so it is a signal about the prefix.
func TestDecommissionReportsAnAccountTheInstanceWillNotDelete(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.mu.Lock()
	f.users["crewlet-person"] = 600
	f.people["crewlet-person"] = true
	f.mu.Unlock()

	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Decommissioned) != 0 {
		t.Errorf("decommissioned %v", res.Decommissioned)
	}
	found := false
	for _, note := range res.Notes {
		if strings.Contains(note, "not catching people") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %q", res.Notes)
	}
}

// A COPY-PASTED VARIABLE IS CAUGHT AT THE INSTANCE. Minting over it would
// hand this seat a second identity while the other keeps authenticating as
// one account from two places, and nothing would report it.
func TestATokenBelongingToAnotherAccountStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.users["crewlet-qa"] = 700
	f.tokens[700] = []*gitlab.Token{{
		ID: 7001, Name: gitlab.TokenName("qa"), Value: "glpat-qa",
	}}
	f.mu.Unlock()
	sink.seed("GITLAB_TOKEN_SWE", "glpat-qa")

	if _, err := reconcileAgainst(t, f, sink, seats); err == nil {
		t.Fatal("a token belonging to another account was accepted")
	} else if !strings.Contains(err.Error(), "different account") {
		t.Errorf("error = %v", err)
	}
}

// "CANNOT TELL" LEAVES THE SEAT EXACTLY AS IT WAS.
func TestAnUnverifiableTokenIsLeftAloneWithANote(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := sink.value("GITLAB_TOKEN_SWE")
	f.forget()
	f.mu.Lock()
	f.identityFails = true
	f.mu.Unlock()

	res, err := reconcileAgainst(t, f, sink, seats)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rotated) != 0 || len(res.Kept) != 1 {
		t.Fatalf("rotated %v, kept %v", res.Rotated, res.Kept)
	}
	if sink.value("GITLAB_TOKEN_SWE") != before {
		t.Error("a token that could not be checked was replaced")
	}
	found := false
	for _, note := range res.Notes {
		if strings.Contains(note, "could not check") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %q", res.Notes)
	}
	if f.revoked() != 0 {
		t.Errorf("%d tokens were revoked on an unverifiable seat", f.revoked())
	}
}

// --- where the webhook lands ---------------------------------------------

// AN INSTANCE WITH NO GROUP HOOKS STILL GETS HOOKS.
//
// The API is Premium on gitlab.com and absent from Community Edition, and
// GitLab hides an unavailable endpoint as a 404 rather than a 402. So
// registering only at the group level failed the whole reconcile there —
// AFTER minting, so the rollback revoked every credential the run had just
// created.
//
// This is the only place that path is exercised: the unlicensed `gitlab-ee`
// image this repository's compose stack runs was measured to serve group
// hooks, so the local loop takes the group branch every time.
func TestOnFreeTheHookFallsBackToTheProjects(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	sink := newRecordingSink()

	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.projectHooks["nimbus/api"]) != 1 {
		t.Fatalf("project hooks = %+v, want one on the declared project", f.projectHooks)
	}
	if got := res.HookedOn; len(got) != 1 || got[0] != "nimbus/api" {
		t.Errorf("HookedOn = %v, want the project", got)
	}
	// AND THE TOKENS SURVIVED. The bug this covers was not "no webhook" —
	// it was a rollback that revoked every credential the run had minted.
	if sink.value("GITLAB_TOKEN_SWE") == "" {
		t.Error("the run rolled back and revoked what it had minted")
	}
	// SAID OUT LOUD, because the two are not interchangeable: a project
	// added to the group later is covered by a group hook and not by
	// these.
	if !strings.Contains(strings.Join(res.Notes, "\n"), "Premium") {
		t.Errorf("notes did not explain the fallback: %v", res.Notes)
	}
}

// ON PREMIUM IT STAYS AT THE GROUP, which is the level that covers a
// project added tomorrow.
func TestOnPremiumTheHookGoesOnTheGroup(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()

	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := res.HookedOn; len(got) != 1 || got[0] != "group" {
		t.Errorf("HookedOn = %v, want the group", got)
	}
	// NEVER BOTH. A group hook and a project hook subscribed to the same
	// events both fire for an in-project event.
	if len(f.projectHooks) != 0 {
		t.Errorf("project hooks were registered as well: %+v", f.projectHooks)
	}
}

// `group_webhook: false` GOES STRAIGHT TO THE PROJECTS, on an instance
// where the group endpoint would have worked.
func TestGroupWebhookFalseNeverTouchesTheGroup(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.GroupWebhook = config.GroupWebhookNever
		})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.hooks) != 0 {
		t.Errorf("a group hook was registered anyway: %+v", f.hooks)
	}
	if got := res.HookedOn; len(got) != 1 || got[0] != "nimbus/api" {
		t.Errorf("HookedOn = %v, want the project", got)
	}
}

// `group_webhook: true` REFUSES TO FALL BACK, and says why.
//
// The mode exists for an operator who needs the group-level guarantee —
// every project, including ones added later. Quietly giving them per-project
// hooks would be the opposite of what they asked for, and they would find
// out the day a new repository went unwatched.
func TestGroupWebhookTrueFailsRatherThanFallingBack(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.GroupWebhook = config.GroupWebhookRequire
		})
	if err == nil {
		t.Fatal("a required group hook was not available and the run succeeded")
	}
	if !strings.Contains(err.Error(), "Premium") {
		t.Errorf("the failure does not name the cause: %v", err)
	}
	if len(f.projectHooks) != 0 {
		t.Errorf("it fell back anyway: %+v", f.projectHooks)
	}
}

// A REAL REFUSAL IS NOT A TIER GATE. A 401 means the credential is wrong,
// and falling back on it would paper over a broken token with a set of
// project hooks the operator never asked for.
func TestABadCredentialDoesNotLookLikeAFreeInstance(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hookStatus = http.StatusUnauthorized
	_, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err == nil {
		t.Fatal("an unauthorized group-hooks call was treated as success")
	}
	if len(f.projectHooks) != 0 {
		t.Errorf("it fell back on a credential failure: %+v", f.projectHooks)
	}
}

// PER-PROJECT HOOKS WITH NO PROJECTS IS A REFUSAL, not a quiet no-op: a run
// that hooked nothing leaves the instance reporting a healthy integration
// that delivers to nobody.
func TestPerProjectHooksWithNoProjectsRefuses(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.GroupWebhook = config.GroupWebhookNever
			o.Config.Provisioning.Projects = nil
		})
	if err == nil {
		t.Fatal("no projects and no group hook, and the run reported success")
	}
	if !strings.Contains(err.Error(), "provisioning.projects") {
		t.Errorf("the failure does not name what to fix: %v", err)
	}
}

// AN EXISTING PROJECT HOOK IS RE-POINTED, not duplicated — the same rule
// the group level has, for the same reason: the signing secret may have
// rotated, and a hook still carrying the old one delivers events the engine
// then refuses.
func TestAnExistingProjectHookIsUpdated(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	f.projectHooks = map[string][]gitlab.Hook{
		"nimbus/api": {{ID: 4, URL: "https://crewlet.example.com/webhooks/gitlab"}},
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.projectHooks["nimbus/api"]) != 1 {
		t.Errorf("the hook was duplicated: %+v", f.projectHooks["nimbus/api"])
	}
	if len(f.updatedHooks) != 1 || !strings.HasSuffix(f.updatedHooks[0], "/hooks/4") {
		t.Errorf("updated = %v, want the existing hook re-pointed", f.updatedHooks)
	}
}

// A DECLARED PROJECT THIS INSTANCE DOES NOT HAVE IS DROPPED, not fatal.
//
// `provisioning.projects` names a company's real repositories, and one being
// renamed, moved or not created yet is an ordinary state of a config — not a
// reason to refuse to provision the other nine. Aborting on the first 404
// did exactly that, and it aborted MID-LOOP, after minting, so the rollback
// then revoked the credentials the run had already created. Measured against
// a real instance: the bootstrap seeds one project, the shipped example
// declares four, and the run died on the second.
func TestAMissingProjectIsSkippedRatherThanFatal(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.missingProjects = map[string]bool{"nimbus/gone": true}
	sink := newRecordingSink()

	res, err := reconcileWith(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.Projects = []string{"nimbus/api", "nimbus/gone"}
		})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// THE REST RECONCILED. The seat has its token and its membership of
	// the project that does exist.
	if sink.value("GITLAB_TOKEN_SWE") == "" {
		t.Error("the run rolled back and revoked what it had minted")
	}
	joined := false
	for key := range f.members {
		if strings.HasPrefix(key, "/projects/nimbus/api/members") {
			joined = true
		}
		if strings.HasPrefix(key, "/projects/nimbus/gone/") {
			t.Errorf("the seat was added to a project that does not exist: %s", key)
		}
	}
	if !joined {
		t.Errorf("the seat did not join the project that exists: %v", f.members)
	}
	// AND IT SAID WHAT IT SKIPPED, with the fix: an operator who is not
	// told cannot know why an agent never sees that repository.
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "nimbus/gone") || !strings.Contains(notes, "re-run") {
		t.Errorf("notes do not name the skipped project and the fix: %v", res.Notes)
	}
}

// THE CHECK RUNS BEFORE ANYTHING IS MUTATED, so a missing project cannot
// leave half a reconcile behind for the operator to reason about.
func TestProjectsAreCheckedBeforeAnySeatIsTouched(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.missingProjects = map[string]bool{"nimbus/gone": true}
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.Projects = []string{"nimbus/gone", "nimbus/api"}
		}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	probe, firstWrite := -1, -1
	for i, call := range f.calls {
		if probe < 0 && call == "GET /projects/nimbus/gone" {
			probe = i
		}
		if firstWrite < 0 && strings.HasPrefix(call, "POST ") {
			firstWrite = i
		}
	}
	if probe < 0 || firstWrite < 0 {
		t.Fatalf("expected both a probe and a write: %v", f.calls)
	}
	if probe > firstWrite {
		t.Errorf("the project was probed at %d, after the first write at %d", probe, firstWrite)
	}
}

// A PROJECT THAT REFUSES FOR ANOTHER REASON IS STILL AN ERROR. A 403 says
// the operator credential cannot see it, and dropping it silently would
// provision a company whose agents are missing from a repository nobody
// mentioned.
func TestAForbiddenProjectIsNotSilentlySkipped(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.projectStatus = http.StatusForbidden
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.Projects = []string{"nimbus/api"}
		})
	if err == nil {
		t.Fatal("a forbidden project was treated as merely absent")
	}
}

// --- the webhook signing secret -------------------------------------------

// A HOOK IS NEVER REGISTERED WITH AN EMPTY SIGNING TOKEN.
//
// GitLab's token is caller-supplied and write-only: the instance never
// returns it. A hook registered with an empty one is accepted, shows healthy
// in the settings page, and signs every delivery with nothing — which the
// engine then refuses. Measured against a real instance: the issue was
// created, the hook fired, and the only trace was one
// `webhook_signature_invalid` line in a log nobody was watching.
func TestAnUnsetSigningSecretIsMintedAndRecorded(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()

	res, err := reconcileWith(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.SigningSecret = ""
			o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
		})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	minted := sink.value("GITLAB_SIGNING_SECRET")
	if !strings.HasPrefix(minted, gitlab.SigningSecretPrefix) {
		t.Fatalf("recorded %q, want a %s secret", minted, gitlab.SigningSecretPrefix)
	}
	// THE HOOK CARRIES THE SAME VALUE. Recording one and stamping another
	// is the same outage with an extra step.
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.hookBodies[0]["signing_token"]; got != minted {
		t.Errorf("the hook was stamped with %q, not the recorded %q", got, minted)
	}
	// AND IT SAYS SO, with what to do about it: the value is useless to
	// the engine until it is in the engine's environment.
	if !strings.Contains(strings.Join(res.Notes, "\n"), "GITLAB_SIGNING_SECRET") {
		t.Errorf("the run did not say it minted one: %v", res.Notes)
	}
}

// THE KEY IS FULL STRENGTH. HMAC-SHA256 draws its security from the key, and
// a short one is invisible: every signature still verifies, so nothing fails
// until somebody brute-forces it.
func TestAMintedSecretCarriesAFullStrengthKey(t *testing.T) {
	t.Parallel()
	secret, err := gitlab.MintSigningSecret()
	if err != nil {
		t.Fatalf("MintSigningSecret: %v", err)
	}
	payload, found := strings.CutPrefix(secret, gitlab.SigningSecretPrefix)
	if !found {
		t.Fatalf("%q carries no %s prefix", secret, gitlab.SigningSecretPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the payload is not standard base64: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key is %d bytes, want 32 — the SHA-256 block-equivalent "+
			"strength the scheme rests on", len(key))
	}
}

// A LITERAL HAS NOWHERE TO RECORD ONE, so the run refuses rather than
// registering a hook that verifies nothing.
func TestAnUnsetLiteralSigningSecretIsRefused(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.SigningSecret = ""
			o.SigningSecretVar = ""
		})
	if err == nil {
		t.Fatal("a hook was registered with no signing secret")
	}
	if !strings.Contains(err.Error(), "signing_secret") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	if len(f.hooks) != 0 {
		t.Errorf("a hook was registered anyway: %+v", f.hooks)
	}
}

// A SECRET THAT RESOLVED IS USED AS IS. Minting over an operator's own
// value would invalidate the one every other deployment of this company
// holds — the same outage -rotate exists to make deliberate.
func TestAResolvedSigningSecretIsNotReminted(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	if _, err := reconcileWith(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.SigningSecret = "whsec_operators-own"
			o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
		}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := sink.value("GITLAB_SIGNING_SECRET"); got != "" {
		t.Errorf("it minted over a working secret and recorded %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.hookBodies[0]["signing_token"]; got != "whsec_operators-own" {
		t.Errorf("the hook carries %q, not the operator's value", got)
	}
}

// EVERY MINT IS DIFFERENT. A deterministic secret is not a secret, and the
// failure is invisible: the hook works.
func TestMintedSigningSecretsDiffer(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 8 {
		f := newAdminInstance()
		sink := newRecordingSink()
		if _, err := reconcileWith(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"},
			func(o *gitlab.Options) {
				o.SigningSecret = ""
				o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
			}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := sink.value("GITLAB_SIGNING_SECRET")
		if seen[got] {
			t.Fatalf("two runs minted the same secret: %q", got)
		}
		seen[got] = true
	}
}

// A GITLAB THAT CANNOT SIGN FAILS THE RUN, LOUDLY.
//
// `signing_token` arrived in GitLab 19.0 behind a feature flag and went
// generally available in 19.1. An older instance ACCEPTS the attribute,
// ignores it, and answers 200 — so the write succeeds, the hook exists, and
// GitLab's own settings page calls it healthy. It then delivers unsigned to
// an engine whose verification is mandatory, and every delivery is refused.
//
// Nothing else in the system can report that: the engine sees a stream of
// unauthenticated deliveries, which is indistinguishable from an attack, and
// the instance sees a hook it thinks is fine. The provisioner reading
// `signing_token_present` back is the only moment the two facts are in one
// place.
func TestAGitLabThatCannotSignIsRefused(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.signsNothing = true

	_, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err == nil {
		t.Fatal("the run succeeded against an instance that ignores signing " +
			"tokens, so the hook would deliver unsigned for ever")
	}
	// The error has to name the remedy: an operator reading "no signing
	// token" has no way to know it is a version floor rather than a
	// mistake they made.
	for _, want := range []string{"signing token", "19.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// AND THE WRITE ITSELF STILL HAPPENED. The failure is the confirmation, not
// the write — so a run against a too-old instance is refused rather than
// half-applied in some way an operator has to unpick.
func TestTheHookIsStillWrittenWhenTheConfirmationFails(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.signsNothing = true

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err == nil {
		t.Fatal("expected the run to be refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) == 0 {
		t.Fatal("no hook was written at all, so the failure was not the confirmation")
	}
	if got := f.hookBodies[0]["signing_token"]; got == nil || got == "" {
		t.Errorf("the hook was written without a signing token: %v", got)
	}
}

// THE OLD PLAINTEXT TOKEN IS CLEARED, NOT JUST STOPPED BEING SET.
//
// A hook an older Crewlet created holds the 32-byte signing key in GitLab's
// `token` attribute, which GitLab echoes back in cleartext on every
// delivery. An update that writes only `signing_token` leaves it there — and
// the hook now signs correctly, so nothing ever looks wrong again while a
// live key keeps going out in the clear.
//
// Sending the empty string is what removes it; omitting the field means
// "leave whatever is there", which is the state being cleaned up.
func TestTheLegacyPlaintextTokenIsCleared(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// A hook from before the fix: same URL, so the reconcile updates it.
	f.hooks = []gitlab.Hook{{ID: 4, URL: "https://crewlet.example.com/webhooks/gitlab"}}

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want one update", len(f.hookBodies))
	}
	body := f.hookBodies[0]
	token, present := body["token"]
	if !present {
		t.Fatal("the update omits `token` entirely, so GitLab keeps the old " +
			"plaintext value and goes on echoing it on every delivery")
	}
	if token != "" {
		t.Errorf("token = %q, want the empty string that clears it", token)
	}
	if body["signing_token"] == "" || body["signing_token"] == nil {
		t.Error("the update carries no signing token, so the hook cannot sign")
	}
}

// -rotate REPLACES THE SIGNING SECRET, not just the seat tokens.
//
// The key installed by any Crewlet before the signing_token fix went into
// GitLab's plaintext `token` attribute, so the instance echoed it back in
// cleartext on every delivery — into request logs, into any proxy in front
// of the engine, and into the stored delivery headers. Every one of those
// keys is compromised, and a provisioner that could not replace one would
// leave the operator editing environment variables by hand.
func TestRotateReplacesTheSigningSecret(t *testing.T) {
	t.Parallel()

	// Both runs point the config's signing_secret at a ${VAR} that resolves
	// to nothing, so the first MINTS one; the second sees a resolved value
	// and, with -rotate, must replace it rather than reuse it.
	run := func(rotate bool, resolved string) string {
		f, sink := newAdminInstance(), newRecordingSink()
		if _, err := reconcileWith(t, f, sink,
			map[string]string{"swe": "GITLAB_TOKEN_SWE"},
			func(o *gitlab.Options) {
				o.SigningSecret = resolved
				o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
				o.Rotate = rotate
			}); err != nil {
			t.Fatalf("Reconcile(rotate=%v): %v", rotate, err)
		}
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.values["GITLAB_SIGNING_SECRET"]
	}

	first := run(false, "")
	rotated := run(true, first)

	if first == "" || rotated == "" {
		t.Fatalf("no secret was written: %q then %q", first, rotated)
	}
	if first == rotated {
		t.Error("-rotate reused the existing signing secret, so a compromised " +
			"key cannot be replaced by the tool that installed it")
	}
	for _, got := range []string{first, rotated} {
		if !strings.HasPrefix(got, "whsec_") {
			t.Errorf("secret %q is not a whsec_ value", got)
		}
	}
}

// AND IT SAYS SO WHEN IT CANNOT. A literal signing_secret has nowhere to
// record a new value — but that must not fail a run whose actual subject is
// the seat tokens, so it is a note on a successful run rather than a refusal.
func TestRotateReportsASigningSecretItCannotReplace(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}, func(o *gitlab.Options) {
			o.SigningSecret = "whsec_a-literal-the-operator-manages"
			o.SigningSecretVar = "" // not a ${VAR}: nowhere to record one
			o.Rotate = true
		})
	if err != nil {
		t.Fatalf("the run was refused over a signing secret it was not asked "+
			"to rotate: %v", err)
	}
	var said bool
	for _, note := range res.Notes {
		if strings.Contains(note, "signing secret was left alone") {
			said = true
		}
	}
	if !said {
		t.Errorf("the run said nothing about the signing secret it could not "+
			"replace: %v", res.Notes)
	}
}
