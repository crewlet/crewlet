package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

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

	users        map[string]int   // username -> id
	tokens       map[int][]string // user id -> live token ids
	members      map[string]int   // "group:7" / "proj:a/b" -> access level
	hooks        []gitlab.Hook
	hookBodies   []map[string]any
	updatedHooks []string

	// failToken makes minting fail for this username, to reach the
	// rollback path.
	failToken string
	// failTokenRevoke makes revocation fail, to reach the "cleanup did
	// not finish" report.
	failTokenRevoke bool
	// mintEmpty answers a mint with a 200 carrying no token value, which
	// is a real GitLab response shape and the worst one.
	mintEmpty bool

	nextID    int
	nextToken int
	calls     []string
}

func newAdminInstance() *adminInstance {
	return &adminInstance{
		users: map[string]int{}, tokens: map[int][]string{},
		members: map[string]int{}, nextID: 100, nextToken: 1,
	}
}

func (f *adminInstance) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	f.calls = append(f.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")

	switch {
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
		f.nextToken++
		value := fmt.Sprintf("glpat-minted-%d", f.nextToken)
		// The token EXISTS either way — that is what makes the empty
		// response so bad — so it is recorded on the account regardless.
		f.tokens[atoi(id)] = append(f.tokens[atoi(id)], value)
		if f.mintEmpty {
			json.NewEncoder(w).Encode(map[string]any{"id": f.nextToken})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": value, "id": f.nextToken})

	case r.Method == http.MethodGet && path == "/personal_access_tokens":
		id := atoi(r.URL.Query().Get("user_id"))
		var out []map[string]any
		for i := range f.tokens[id] {
			out = append(out, map[string]any{"id": i + 1})
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/personal_access_tokens/"):
		if f.failTokenRevoke {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Revoking clears the account's tokens; which id is which does
		// not matter to what this asserts.
		for id := range f.tokens {
			f.tokens[id] = nil
		}

	case r.Method == http.MethodGet && path == "/groups/7/hooks":
		json.NewEncoder(w).Encode(f.hooks)

	case r.Method == http.MethodPost && path == "/groups/7/hooks":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		hook := gitlab.Hook{ID: len(f.hooks) + 1, URL: body["url"].(string)}
		f.hooks = append(f.hooks, hook)
		f.hookBodies = append(f.hookBodies, body)
		json.NewEncoder(w).Encode(hook)

	case r.Method == http.MethodPut && strings.Contains(path, "/hooks/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		f.updatedHooks = append(f.updatedHooks, path)

	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"404 Not Found"}`))
	}
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
		n += len(tokens)
	}
	return n
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
	mu       sync.Mutex
	values   map[string]string
	failOn   string
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

func (s *recordingSink) Flush(context.Context) error { return nil }
func (s *recordingSink) Describe() string            { return "a test sink" }

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
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: "admin-token",
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
	return gitlab.Reconcile(context.Background(), gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		WebhookBase: "https://crewlet.example.com", SigningSecret: "whsec-abc",
	})
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
	if f.hookBodies[0]["token"] != "whsec-abc" {
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
	if _, present := body["push_events"]; present {
		t.Error("push is subscribed, and nothing routes it")
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
