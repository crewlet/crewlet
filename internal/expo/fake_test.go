package expo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// The fake's fixed identities. Obviously fake values throughout.
const (
	testAccountID   = "acct-0001"
	testAccountName = "example-org"
	provisioner     = "fake-expo-provisioner-token"
	provisionerID   = "robot-provisioner"
)

// fakeRobot is one robot user on the fake account.
type fakeRobot struct {
	id, name string
	perms    []expo.Permission
	deleting bool
	created  time.Time
}

// fakeToken is one access token the fake holds.
type fakeToken struct {
	id, owner, note, value string
	revoked                bool
	created                time.Time
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

// fakeExpo is one Expo account behind the GraphQL API: its robots, their
// tokens and the provisioner's own permissions, with GraphQL's errors.
type fakeExpo struct {
	mu sync.Mutex

	viewerKind  string
	viewerPerms []expo.Permission
	appCount    int
	robots      map[string]*fakeRobot
	tokens      map[string]*fakeToken
	seq         int
	pageSize    int

	// refuse answers an operation with a refusal, by operation name.
	refuse map[string]fakeRefusal
	// limited answers an operation 429 this many times first.
	limited map[string]int
	// emptyValue answers a token create with no value.
	emptyValue bool
	// noViewer answers the viewer query with a null actor.
	noViewer bool

	ops []string
	rw  *httpxtest.Rewriter
}

func newFakeExpo() *fakeExpo {
	return &fakeExpo{
		viewerKind:  "Robot",
		viewerPerms: []expo.Permission{expo.PermissionAdmin, expo.PermissionView},
		appCount:    3,
		robots:      map[string]*fakeRobot{},
		tokens:      map[string]*fakeToken{},
		refuse:      map[string]fakeRefusal{},
		limited:     map[string]int{},
		pageSize:    2,
	}
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

// onlyExpo fails a test whose client addressed anything but Expo's API host,
// which is where the provisioner token may go.
func (f *fakeExpo) onlyExpo(t testing.TB) {
	t.Helper()
	for _, host := range f.rw.Hosts() {
		if host != "https://api.expo.dev" {
			t.Errorf("the client addressed %s", host)
		}
	}
}

// addRobot puts a robot on the account, as if created earlier.
func (f *fakeExpo) addRobot(name string, perms ...expo.Permission) *fakeRobot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	r := &fakeRobot{id: "robot-" + strconv.Itoa(f.seq), name: name, perms: perms,
		created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.robots[r.id] = r
	return r
}

// addToken puts a token on an actor, as if minted earlier.
func (f *fakeExpo) addToken(owner, note string) *fakeToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintLocked(owner, note)
}

func (f *fakeExpo) mintLocked(owner, note string) *fakeToken {
	f.seq++
	n := strconv.Itoa(f.seq)
	t := &fakeToken{id: "token-" + n, owner: owner, note: note, value: "fake-expo-seat-token-" + n,
		created: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	f.tokens[t.id] = t
	return t
}

// robot is a copy of the robot with id, and whether the fake has it.
func (f *fakeExpo) robot(id string) (fakeRobot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.robots[id]
	if !ok {
		return fakeRobot{}, false
	}
	out := *r
	out.perms = slices.Clone(r.perms)
	return out, true
}

// liveTokens are the unrevoked tokens on owner, by id, sorted.
func (f *fakeExpo) liveTokens(owner string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.tokens {
		if t.owner == owner && !t.revoked {
			out = append(out, t.id)
		}
	}
	slices.Sort(out)
	return out
}

// value is a token's value, for a test asserting what was sealed.
func (f *fakeExpo) value(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tokens[id]; ok {
		return t.value
	}
	return ""
}

// operations is every operation the fake answered, in order.
func (f *fakeExpo) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

// writes is every mutation the fake answered, in order.
func (f *fakeExpo) writes() []string {
	var out []string
	for _, op := range f.operations() {
		switch op {
		case "CrewletViewer", "CrewletAccount", "CrewletViewerPermissions", "CrewletAppCount", "CrewletRobots":
		default:
			out = append(out, op)
		}
	}
	return out
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
	if r.Header.Get("Authorization") != "Bearer "+provisioner {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Invalid access token"}]}`))
		return
	}
	var body gqlBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.gqlError(w, fakeRefusal{code: "GRAPHQL_PARSE_FAILED", message: "bad request"})
		return
	}
	op := body.OperationName
	f.ops = append(f.ops, op)
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
	var vars map[string]any
	_ = json.Unmarshal(body.Variables, &vars)
	str := func(k string) string { s, _ := vars[k].(string); return s }
	switch op {
	case "CrewletViewer":
		if f.noViewer {
			f.data(w, map[string]any{"meActor": nil})
			return
		}
		f.data(w, map[string]any{"meActor": map[string]any{
			"__typename": f.viewerKind, "id": provisionerID, "displayName": "provisioner"}})
	case "CrewletAccount", "CrewletViewerPermissions", "CrewletAppCount", "CrewletRobots":
		if !strings.EqualFold(str("name"), testAccountName) {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", errorType: "USER",
				message: "Account " + str("name") + " not found"})
			return
		}
		f.accountQuery(w, op, vars)
	case "CrewletCreateRobot":
		if str("accountID") != testAccountID {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "account not found"})
			return
		}
		f.seq++
		data, _ := vars["robotData"].(map[string]any)
		name, _ := data["name"].(string)
		robot := &fakeRobot{id: "robot-" + strconv.Itoa(f.seq), name: name,
			perms: permsOf(vars["permissions"]), created: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
		f.robots[robot.id] = robot
		f.data(w, map[string]any{"robot": map[string]any{"createRobotForAccount": map[string]any{
			"id": robot.id, "displayName": robot.name, "created": robot.created}}})
	case "CrewletRenameRobot":
		robot, ok := f.robots[str("id")]
		if !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "robot not found"})
			return
		}
		data, _ := vars["robotData"].(map[string]any)
		robot.name, _ = data["name"].(string)
		f.data(w, map[string]any{"robot": map[string]any{"updateRobot": map[string]any{"id": robot.id}}})
	case "CrewletDeleteRobot":
		robot, ok := f.robots[str("id")]
		if !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "robot not found"})
			return
		}
		robot.deleting = true
		f.data(w, map[string]any{"robot": map[string]any{"scheduleRobotDeletion": map[string]any{
			"state": "QUEUED", "willRetry": false, "errorMessage": nil}}})
	case "CrewletCreateToken":
		data, _ := vars["data"].(map[string]any)
		actor, _ := data["actorID"].(string)
		note, _ := data["note"].(string)
		if _, ok := f.robots[actor]; !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "actor not found"})
			return
		}
		tok := f.mintLocked(actor, note)
		value := tok.value
		if f.emptyValue {
			value = ""
		}
		f.data(w, map[string]any{"accessToken": map[string]any{"createAccessToken": map[string]any{
			"token": value, "accessToken": map[string]any{"id": tok.id, "note": tok.note, "createdAt": tok.created}}}})
	case "CrewletRevokeToken":
		tok, ok := f.tokens[str("id")]
		if !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "access token not found"})
			return
		}
		tok.revoked = true
		f.data(w, map[string]any{"accessToken": map[string]any{"setAccessTokenRevoked": map[string]any{"id": tok.id}}})
	case "CrewletDeleteToken":
		if _, ok := f.tokens[str("id")]; !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "access token not found"})
			return
		}
		delete(f.tokens, str("id"))
		f.data(w, map[string]any{"accessToken": map[string]any{"deleteAccessToken": map[string]any{"id": str("id")}}})
	case "CrewletGrant", "CrewletRevoke":
		robot, ok := f.robots[str("actorID")]
		if str("accountID") != testAccountID || !ok {
			f.gqlError(w, fakeRefusal{code: "ENTITY_NOT_FOUND", message: "not found"})
			return
		}
		change := permsOf(vars["permissions"])
		if op == "CrewletGrant" {
			for _, p := range change {
				if !slices.Contains(robot.perms, p) {
					robot.perms = append(robot.perms, p)
				}
			}
		} else {
			robot.perms = slices.DeleteFunc(robot.perms, func(p expo.Permission) bool {
				return slices.Contains(change, p)
			})
		}
		field := "grantActorPermissions"
		if op == "CrewletRevoke" {
			field = "revokeActorPermissions"
		}
		f.data(w, map[string]any{"account": map[string]any{field: map[string]any{"id": testAccountID}}})
	default:
		f.gqlError(w, fakeRefusal{code: "GRAPHQL_VALIDATION_FAILED", message: "unknown operation " + op})
	}
}

// accountQuery answers the four reads under account.byName.
func (f *fakeExpo) accountQuery(w http.ResponseWriter, op string, vars map[string]any) {
	byName := map[string]any{}
	switch op {
	case "CrewletAccount":
		byName = map[string]any{"id": testAccountID, "name": testAccountName}
	case "CrewletViewerPermissions":
		byName["viewerUserPermission2"] = map[string]any{
			"permissions": f.viewerPerms, "effectivePermissions": f.viewerPerms}
	case "CrewletAppCount":
		byName["appCount"] = f.appCount
	case "CrewletRobots":
		ids := make([]string, 0, len(f.robots))
		for id := range f.robots {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		start := 0
		if after, _ := vars["after"].(string); after != "" {
			start, _ = strconv.Atoi(after)
		}
		first := f.pageSize
		if n, ok := vars["first"].(float64); ok && int(n) < first {
			first = int(n)
		}
		end := min(start+first, len(ids))
		edges := []any{}
		for _, id := range ids[start:end] {
			r := f.robots[id]
			var deleting any
			if r.deleting {
				deleting = "2026-11-02T00:00:00Z"
			}
			tokens := []any{}
			for _, t := range f.tokens {
				if t.owner != id {
					continue
				}
				var revoked any
				if t.revoked {
					revoked = "2026-11-02T00:00:00Z"
				}
				tokens = append(tokens, map[string]any{"id": t.id, "note": t.note,
					"revokedAt": revoked, "createdAt": t.created})
			}
			edges = append(edges, map[string]any{"node": map[string]any{
				"permissions": r.perms,
				"actor": map[string]any{"__typename": "Robot", "id": r.id, "displayName": r.name,
					"created": r.created, "lastDeletionAttemptTime": deleting, "accessTokens": tokens},
			}})
		}
		byName["membersPaginated"] = map[string]any{"edges": edges, "pageInfo": map[string]any{
			"hasNextPage": end < len(ids), "endCursor": strconv.Itoa(end)}}
	}
	f.data(w, map[string]any{"account": map[string]any{"byName": byName}})
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

func permsOf(v any) []expo.Permission {
	list, _ := v.([]any)
	out := make([]expo.Permission, 0, len(list))
	for _, p := range list {
		if s, ok := p.(string); ok {
			out = append(out, expo.Permission(s))
		}
	}
	return out
}
