package expo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// The fake's fixed account. Obviously fake values throughout.
const testAccountName = "example-org"

// fakeActor is who one token acts as: a robot with a role per account, or a
// person.
type fakeActor struct {
	id, kind, name string
	roles          map[string]expo.Role
}

// fakeToken is one access token: its value, its owner and the prefix Expo
// shows for it.
type fakeToken struct {
	id, value, owner, prefix, created string
	revoked                           bool
}

// fakeRefusal is how the fake refuses one operation: an HTTP status, or a
// GraphQL error code on a 200.
type fakeRefusal struct {
	status    int
	code      string
	errorType string
	transient bool
	message   string
}

// fakeExpo is Expo's GraphQL API as the seats' own tokens see it: each token
// answers as its actor, and a revoked or unknown token answers 401.
type fakeExpo struct {
	mu sync.Mutex

	actors map[string]*fakeActor
	tokens map[string]*fakeToken // by value
	seq    int

	// refuse answers an operation with a refusal, by operation name.
	refuse map[string]fakeRefusal
	// limited answers an operation 429 this many times first.
	limited map[string]int

	ops []string
	rw  *httpxtest.Rewriter

	// projects is the account's projects as the atlas's listing answers
	// them, pageCap the most the fake serves a page, and queries the text
	// of each operation it was sent.
	projects []map[string]any
	pageCap  int
	queries  map[string]string
}

func newFakeExpo() *fakeExpo {
	return &fakeExpo{actors: map[string]*fakeActor{}, tokens: map[string]*fakeToken{},
		refuse: map[string]fakeRefusal{}, limited: map[string]int{}}
}

// server starts the fake and returns a client transport that reaches it in
// place of api.expo.dev.
func (f *fakeExpo) server(t testing.TB) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.rw = httpxtest.Rewrite(t, srv)
	return &http.Client{Transport: f.rw}
}

// onlyExpo fails a test whose client addressed anything but Expo's API host.
func (f *fakeExpo) onlyExpo(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if host != "https://api.expo.dev" {
			t.Errorf("the client addressed %s", host)
		}
	}
}

// robot puts a robot on the account at role and answers its first token.
func (f *fakeExpo) robot(name string, role expo.Role) (*fakeActor, *fakeToken) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	a := &fakeActor{id: "robot-" + strconv.Itoa(f.seq), kind: "Robot", name: name,
		roles: map[string]expo.Role{testAccountName: role}}
	f.actors[a.id] = a
	return a, f.mintLocked(a.id)
}

// person puts a person on the account at role and answers their token.
func (f *fakeExpo) person(name string, role expo.Role) *fakeToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	a := &fakeActor{id: "user-" + strconv.Itoa(f.seq), kind: "User", name: name,
		roles: map[string]expo.Role{testAccountName: role}}
	f.actors[a.id] = a
	return f.mintLocked(a.id)
}

// mint creates another token for an actor, as a person does on its row.
func (f *fakeExpo) mint(actorID string) *fakeToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintLocked(actorID)
}

func (f *fakeExpo) mintLocked(actorID string) *fakeToken {
	f.seq++
	n := strconv.Itoa(f.seq)
	// The prefix Expo shows is the value's own beginning.
	t := &fakeToken{id: "token-" + n, value: "fake" + n + "-expo-seat-token", owner: actorID, prefix: "fake" + n + "-",
		created: fmt.Sprintf("2026-11-03T00:%02d:00Z", f.seq%60)}
	f.tokens[t.value] = t
	return t
}

// revoke revokes a token, as a person does at Expo.
func (f *fakeExpo) revoke(t *fakeToken) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.revoked = true
}

// deleteActor deletes a robot and every token it holds, as a person does.
func (f *fakeExpo) deleteActor(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.actors, id)
	for v, t := range f.tokens {
		if t.owner == id {
			delete(f.tokens, v)
		}
	}
}

// setRole changes an actor's role on the account, as Edit robot does.
func (f *fakeExpo) setRole(id string, role expo.Role) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actors[id].roles[testAccountName] = role
}

// writes is every operation the fake answered that is not one of the two
// reads a seat's token makes or the atlas's listing: anything a person would have to undo at Expo.
func (f *fakeExpo) writes() []string {
	var out []string
	for _, op := range f.operations() {
		if op != "CrewletViewer" && op != "CrewletRole" && op != "CrewletAtlasProjects" {
			out = append(out, op)
		}
	}
	return out
}

// operations is every operation the fake answered, in order.
func (f *fakeExpo) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

type gqlBody struct {
	OperationName string          `json:"operationName"`
	Query         string          `json:"query"`
	Variables     json.RawMessage `json:"variables"`
}

func (f *fakeExpo) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var body gqlBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.gqlError(w, fakeRefusal{code: "GRAPHQL_PARSE_FAILED", message: "bad request"})
		return
	}
	op := body.OperationName
	f.ops = append(f.ops, op)
	if f.queries == nil {
		f.queries = map[string]string{}
	}
	f.queries[op] = body.Query
	tok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if tok == nil || tok.revoked || f.actors[tok.owner] == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Invalid access token"}]}`))
		return
	}
	if !strings.Contains(body.Query, op) {
		f.gqlError(w, fakeRefusal{code: "GRAPHQL_VALIDATION_FAILED", message: "operation name mismatch"})
		return
	}
	if n := f.limited[op]; n > 0 {
		f.limited[op] = n - 1
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`Too Many Requests`))
		return
	}
	if ref, ok := f.refuse[op]; ok {
		if ref.status != 0 {
			w.WriteHeader(ref.status)
			_, _ = w.Write([]byte(ref.message))
			return
		}
		f.gqlError(w, ref)
		return
	}
	actor := f.actors[tok.owner]
	var vars map[string]any
	_ = json.Unmarshal(body.Variables, &vars)
	switch op {
	case "CrewletViewer":
		me := map[string]any{"__typename": actor.kind, "id": actor.id, "displayName": actor.name}
		if actor.kind == "Robot" {
			tokens := []any{}
			for _, t := range f.tokens {
				if t.owner != actor.id {
					continue
				}
				var revoked any
				if t.revoked {
					revoked = "2026-11-03T00:00:00Z"
				}
				tokens = append(tokens, map[string]any{"id": t.id, "visibleTokenPrefix": t.prefix,
					"createdAt": t.created, "revokedAt": revoked})
			}
			me["accessTokens"] = tokens
		}
		f.data(w, map[string]any{"meActor": me})
	case "CrewletRole":
		name, _ := vars["name"].(string)
		role, ok := actor.roles[name]
		if !ok {
			f.gqlError(w, fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not a member of " + name})
			return
		}
		f.data(w, map[string]any{"account": map[string]any{"byName": map[string]any{
			"viewerUserPermission": map[string]any{"role": role}}}})
	case "CrewletAtlasProjects":
		name, _ := vars["name"].(string)
		if _, ok := actor.roles[name]; !ok {
			f.gqlError(w, fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not a member of " + name})
			return
		}
		first, _ := vars["first"].(float64)
		after, _ := vars["after"].(string)
		from, _ := strconv.Atoi(after)
		per := int(first)
		if f.pageCap > 0 {
			per = min(per, f.pageCap)
		}
		to := min(from+per, len(f.projects))
		edges := []any{}
		for _, p := range f.projects[min(from, to):to] {
			edges = append(edges, map[string]any{"cursor": "c", "node": p})
		}
		f.data(w, map[string]any{"account": map[string]any{"byName": map[string]any{
			"id": "account-1", "name": name, "owners": []any{map[string]any{"email": "owner@example.com"}},
			"appsPaginated": map[string]any{"edges": edges,
				"pageInfo": map[string]any{"hasNextPage": to < len(f.projects), "endCursor": strconv.Itoa(to)}}}}})
	default:
		f.gqlError(w, fakeRefusal{code: "GRAPHQL_VALIDATION_FAILED", message: "unknown operation " + op})
	}
}

func (f *fakeExpo) data(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakeExpo) gqlError(w http.ResponseWriter, ref fakeRefusal) {
	w.Header().Set("Content-Type", "application/json")
	ext := map[string]any{"errorCode": ref.code, "requestId": "req-1"}
	if ref.errorType != "" {
		ext["errorType"] = ref.errorType
	}
	if ref.transient {
		ext["isTransient"] = true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":   nil,
		"errors": []any{map[string]any{"message": ref.message, "extensions": ext}},
	})
}
