package gitlab_test

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// The suite's signing secrets are REAL ones: the whsec_ prefix over standard
// base64 of a 32-byte key, which is the only shape GitLab's API accepts. A
// fixture that merely looks the part is one a real instance rejects with a
// 400, and a fake lax enough to take it certifies the whole suite as valid
// while production refuses every hook — which is the class of bug this
// package spent an incident on.
const (
	testSigningSecret     = "whsec_Y3Jld2xldC10ZXN0LXNpZ25pbmcta2V5LTMyYnl0ZXM="
	operatorSigningSecret = "whsec_dGhlLW9wZXJhdG9ycy1vd24tMzItYnl0ZS1rZXkhISE="
)

// adminInstance is a GitLab that remembers what was done to it.
//
// A stand-in rather than a mock: the reconcile's whole job is a sequence of
// calls whose ORDER and IDEMPOTENCE matter, and asserting on expectations
// per-call would test the sequence this was written with rather than the
// property it has to hold.
type adminInstance struct {
	mu sync.Mutex

	users  map[string]int          // username -> id
	people map[string]bool         // usernames the instance treats as humans
	tokens map[int][]*gitlab.Token // user id -> its token rows

	// blocked is what a service-account delete actually LEAVES BEHIND on
	// the deployments this was measured against: the account is blocked
	// and removed from the group, and its access tokens survive.
	//
	// Seeded by [adminInstance.blocksRatherThanErases], because a fixture
	// that erases the tokens with the account cannot tell a teardown that
	// revoked first from one that did not — which is exactly how a live
	// disconnect came to leave `crewlet-sre-lead` blocked, out of the
	// group, and holding one working token.
	blocked map[string]bool
	blocks  bool

	// groupMembers is the group's ROSTER — user id -> access level — and
	// it is not the same thing as `users`.
	//
	// It used to be, near enough: the members route served every account
	// on the instance. That makes a fake that cannot fail the test that
	// matters, because a seat whose account exists and was never added to
	// the group reads back as already a member, so a pass comparing the
	// roster against its plan would skip exactly the write it owes.
	groupMembers map[int]int
	// projectMembers is the same per project path, and there was no route
	// for it at all.
	projectMembers map[string]map[int]int

	hooks      []hookRow
	hookBodies []map[string]any
	// signsNothing models a GitLab older than 19.1: it ACCEPTS the
	// signing_token attribute, ignores it, and answers 200 — so the write
	// succeeds and the hook still cannot sign. That is the only failure
	// mode confirmSigned exists for, and it is invisible without a fake
	// that reproduces it.
	signsNothing bool
	updatedHooks []string
	deletedHooks []string

	// noGroupHooks makes the GROUP hooks API answer 404, the way GitLab
	// hides an endpoint the instance's tier does not serve.
	noGroupHooks bool
	// noGroup makes the group PATH lookup answer 404 — a renamed group, a
	// typo, or one the credential cannot see, which GitLab never tells
	// apart.
	noGroup bool
	// plan is what GET /groups/:path reports as the subscription tier.
	// Empty is a self-managed instance, which sends no such field at all;
	// "free" is what gitlab.com answers for a group whose group webhooks
	// are accepted and never delivered.
	plan string
	// hookStatus answers the group-hooks route with this status instead,
	// for the refusals that are NOT a tier gate.
	hookStatus int
	// projectHooks is what the per-project route holds, keyed by project
	// path.
	projectHooks map[string][]hookRow
	// missingProjects answer the existence probe with a 404, the way a
	// renamed or not-yet-created repository does.
	missingProjects map[string]bool
	// projectStatus answers the existence probe with this status instead,
	// for the refusals that are NOT absence.
	projectStatus int

	// instanceOnly refuses the GROUP service-account create with a 403,
	// the way an instance-admin-only deployment does — and the way a
	// token that is not a group Owner does.
	instanceOnly bool
	// noInstanceRoute answers the instance service-account routes with a
	// 404, which is how GitLab.com answers: they are self-managed only.
	noInstanceRoute bool
	// instanceForbidden answers them with a 403, which is how a
	// non-administrator's token is refused.
	instanceForbidden bool
	// instanceOwned names the accounts created through the instance
	// route, so a delete down the wrong route can be caught.
	instanceOwned map[string]bool

	// instanceAdmin says this credential is an INSTANCE ADMIN token, which
	// is the only thing that may mint through /users/:id. A group Owner is
	// not one, and on gitlab.com nobody is. Set by the instance-mode tests,
	// where an admin token is what the mode requires; a group-mode run
	// leaves it false and the admin route is refused. See the mint handler.
	instanceAdmin bool
	// createRefusal makes account creation fail with this exact response,
	// which is how the vendor's own wording reaches the classifier: a name
	// still being released is told from a refusal that will never clear by
	// what GitLab says, and a fake that invented the words would be testing
	// this package against itself.
	createRefusal *refusal
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
	// unusable names accounts that cannot authenticate AT ALL: every
	// token they hold is refused, however new, which is how GitLab
	// answers for an account whose address was never confirmed
	// (`403 Your primary email address is not confirmed`). Keyed on the
	// user id, filled by the test once the account exists.
	unusable map[int]bool

	// createBodies are the service-account creation payloads, so a test
	// can assert what was SENT — an absent `email` key is the whole of
	// one fix and is invisible in anything the fake chose to remember.
	createBodies []map[string]any

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

// str reads a JSON string field, or "" where it is absent or not one.
func str(v any) string { sv, _ := v.(string); return sv }

// tokenRows is one account's tokens as the instance serves them.
//
// ONE BUILDER for the group listing and the admin one: they are the same rows
// down two routes, and the pair had already been written out twice.
func (f *adminInstance) tokenRows(userID int) []map[string]any {
	out := make([]map[string]any, 0, len(f.tokens[userID]))
	for _, t := range f.tokens[userID] {
		row := map[string]any{"id": t.ID, "name": t.Name, "revoked": t.Revoked}
		if !t.ExpiresAt.IsZero() {
			row["expires_at"] = t.ExpiresAt.Format(time.DateOnly)
		}
		out = append(out, row)
	}
	return out
}

// adminToken is the operator credential the fixture's client presents.
const adminToken = "admin-token"

func newAdminInstance() *adminInstance {
	return &adminInstance{
		users: map[string]int{}, tokens: map[int][]*gitlab.Token{},
		blocked:        map[string]bool{},
		people:         map[string]bool{},
		unusable:       map[int]bool{},
		instanceOwned:  map[string]bool{},
		groupMembers:   map[int]int{},
		projectMembers: map[string]map[int]int{},
		nextID:         100, nextToken: 1,
		now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// hookRow is one webhook as the instance HOLDS it: every attribute it was
// created or updated with, which is what GitLab serves back on a listing.
//
// The fixture used to keep an id and a url and nothing else, so every seeded
// hook read back as one that had never been configured. A reconcile
// comparing what it wants against what is there could not have told a
// converged hook from an empty one, and a fake like that certifies an
// unconditional re-write as correct.
type hookRow struct {
	id    int
	attrs map[string]any
	// signed is what `signing_token_present` reports: a token was sent AND
	// this instance is new enough to honour it. GitLab never returns the
	// token itself, so this is the only thing a listing says about it.
	signed bool
}

// render is the JSON GitLab serves for a hook.
//
// NEITHER SECRET COMES BACK. `signing_token` is write-only by design and
// `token` is not returned either, so a reconcile can never read which key a
// hook holds — which is the whole reason the pass has to be told separately
// whether it minted one this run.
func (h hookRow) render() map[string]any {
	out := make(map[string]any, len(h.attrs)+2)
	for name, value := range h.attrs {
		if name == "signing_token" || name == "token" {
			continue
		}
		out[name] = value
	}
	out["id"] = h.id
	out["signing_token_present"] = h.signed
	return out
}

// legacyHook is the hook an older Crewlet registered: the right URL, the
// signing key in GitLab's plaintext `token` attribute, and no signing token
// at all — which is the state a current pass has to repair.
func legacyHook(id int, target string) hookRow {
	return hookRow{id: id, attrs: map[string]any{
		"url": target, "token": testSigningSecret,
	}}
}

// foreignHook is somebody else's registration on the same instance, which a
// pass must never re-point.
func foreignHook(id int, target string) hookRow {
	return hookRow{id: id, attrs: map[string]any{"url": target}}
}

// namedHook is a hook carrying a name, which is how a current build of this
// engine — or another deployment of it — leaves one.
func namedHook(id int, name, target string) hookRow {
	return hookRow{id: id, signed: true, attrs: map[string]any{
		"url": target, "name": name, "enable_ssl_verification": true,
	}}
}

// dropHook removes one hook by id, the way a DELETE does.
func dropHook(rows []hookRow, id int) []hookRow {
	out := make([]hookRow, 0, len(rows))
	for _, row := range rows {
		if row.id != id {
			out = append(out, row)
		}
	}
	return out
}

// renderHooks is one listing.
func renderHooks(rows []hookRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.render())
	}
	return out
}

// held copies a request body into the attributes the instance keeps.
//
// A COPY, because the same decoded map is also appended to hookBodies as the
// record of what was SENT — and a later update merging into a shared map
// would rewrite that record, so a test asserting what the create carried
// would be reading the update instead.
func held(body map[string]any) map[string]any {
	out := make(map[string]any, len(body))
	for name, value := range body {
		out[name] = value
	}
	return out
}

// writeHook applies a create-or-update body the way GitLab does: the
// attributes named are set and everything else is left as it was.
func (f *adminInstance) writeHook(rows []hookRow, id int, body map[string]any) []hookRow {
	for i := range rows {
		if rows[i].id != id {
			continue
		}
		for name, value := range body {
			rows[i].attrs[name] = value
		}
		rows[i].signed = f.signs(body)
		return rows
	}
	return rows
}

// join seeds an account that is ALREADY IN THE GROUP.
//
// The instance's account list and the group's roster are different things —
// an account can exist and be a member of nothing — so a fixture that seeded
// only `users` was asking a scan to find something the group does not hold.
func (f *adminInstance) join(username string, id, level int) {
	f.users[username] = id
	f.groupMembers[id] = level
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
				if token.Value != presented || !live {
					continue
				}
				if f.unusable[id] {
					// THE ACCOUNT, NOT THE TOKEN. GitLab issues a token
					// on an unconfirmed account perfectly happily and
					// then refuses every request it makes, with a 403
					// naming the address rather than the credential.
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(
						`{"message":"403 Forbidden - Your primary email address is not confirmed"}`))
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"id": id})
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"401 Unauthorized"}`))

	case r.Method == http.MethodGet && path == "/groups/7/members":
		// THE ROSTER, not the account list. An account that exists on
		// this instance and was never added to the group is not here,
		// which is what makes a pass that skips a membership it owes
		// visible rather than invisible.
		out := make([]map[string]any, 0, len(f.groupMembers))
		for name, id := range f.users {
			level, member := f.groupMembers[id]
			if !member {
				continue
			}
			out = append(out, map[string]any{
				"id": id, "username": name, "access_level": level,
			})
		}
		sortByUsername(out)
		// PAGED, because a fake that served everything on page 1 makes a
		// caller that never asks for page 2 look correct.
		json.NewEncoder(w).Encode(pageOf(out, r.URL.Query()))

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/projects/") &&
		strings.HasSuffix(path, "/members"):
		project := strings.TrimSuffix(strings.TrimPrefix(path, "/projects/"), "/members")
		out := make([]map[string]any, 0, len(f.projectMembers[project]))
		for name, id := range f.users {
			level, member := f.projectMembers[project][id]
			if !member {
				continue
			}
			out = append(out, map[string]any{
				"id": id, "username": name, "access_level": level,
			})
		}
		sortByUsername(out)
		json.NewEncoder(w).Encode(pageOf(out, r.URL.Query()))

	// THE EDIT, which is the route an access-level change goes down.
	// GitLab 404s it for an account with no DIRECT membership, so a
	// caller that guessed instead of reading the roster first would be
	// refused on exactly the seat it had never added.
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/groups/7/members/"):
		id := atoi(strings.TrimPrefix(path, "/groups/7/members/"))
		if _, member := f.groupMembers[id]; !member {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404 Not found"}`))
			return
		}
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		f.groupMembers[id] = body["access_level"]

	case r.Method == http.MethodPut && strings.HasPrefix(path, "/projects/") &&
		strings.Contains(path, "/members/"):
		at := strings.LastIndex(path, "/members/")
		project := strings.TrimPrefix(path[:at], "/projects/")
		id := atoi(path[at+len("/members/"):])
		if _, member := f.projectMembers[project][id]; !member {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404 Not found"}`))
			return
		}
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		f.projectMembers[project][id] = body["access_level"]

	// THE ACCOUNT ITSELF, and not a token on it: the group's token routes
	// live under this same prefix, so a prefix match alone deleted the
	// whole account when a run asked to revoke one of its tokens.
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/groups/7/service_accounts/") &&
		!strings.Contains(path, "/personal_access_tokens"):
		id := atoi(strings.TrimPrefix(path, "/groups/7/service_accounts/"))
		for name, uid := range f.users {
			if uid != id {
				continue
			}
			if f.instanceOwned[name] {
				// THE GROUP DOES NOT OWN IT, so the group route answers
				// "no such account" — which a caller reads as "already
				// gone" for an account that is still live.
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"404 Not Found"}`))
				return
			}
			if f.people[name] {
				// GitLab refuses to delete an account that is not a
				// service account, which is the guard that makes this
				// operation human-safe.
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"message":"400 Bad request - Not a service account"}`))
				return
			}
			if f.blocks {
				// BLOCKED, NOT ERASED, and the tokens stay. See
				// [adminInstance.blocked].
				f.blocked[name] = true
				delete(f.groupMembers, id)
				break
			}
			delete(f.users, name)
			delete(f.tokens, id)
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && path == "/groups/nimbus":
		// A GROUP THAT DOES NOT RESOLVE, which GitLab answers 404 for in
		// three different situations — renamed, mistyped, or invisible to
		// the presenting credential — and never distinguishes.
		if f.noGroup {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "404 Group Not Found"})
			return
		}
		group := map[string]any{"id": 7, "full_path": "nimbus"}
		if f.plan != "" {
			group["plan"] = f.plan
		}
		json.NewEncoder(w).Encode(group)

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

	// A REFUSAL THE TEST ASKED FOR, on either creation route: it is the
	// vendor's own wording that tells a name still being released from a
	// refusal that will never clear, and a fake inventing the words would
	// be testing this package against itself.
	case r.Method == http.MethodPost && f.createRefusal != nil &&
		(path == "/groups/7/service_accounts" || path == "/service_accounts"):
		w.WriteHeader(f.createRefusal.status)
		w.Write([]byte(f.createRefusal.body))

	case r.Method == http.MethodPost && path == "/groups/7/service_accounts":
		if f.instanceOnly {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"403 Forbidden"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.createBodies = append(f.createBodies, body)
		f.nextID++
		f.users[str(body["username"])] = f.nextID
		json.NewEncoder(w).Encode(map[string]any{
			"id": f.nextID, "username": body["username"],
		})

	case r.Method == http.MethodPost && path == "/service_accounts":
		if f.noInstanceRoute {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404 Not Found"}`))
			return
		}
		if f.instanceForbidden {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"403 Forbidden"}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.createBodies = append(f.createBodies, body)
		f.nextID++
		f.users[str(body["username"])] = f.nextID
		f.instanceOwned[str(body["username"])] = true
		json.NewEncoder(w).Encode(map[string]any{
			"id": f.nextID, "username": body["username"],
		})

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/service_accounts/"):
		id := atoi(strings.TrimPrefix(path, "/service_accounts/"))
		for name, uid := range f.users {
			if uid != id {
				continue
			}
			if !f.instanceOwned[name] {
				// The instance route does not own a group's account.
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"404 Not Found"}`))
				return
			}
			delete(f.users, name)
			delete(f.instanceOwned, name)
			delete(f.tokens, id)
		}
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && path == "/groups/7/members":
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		if _, already := f.groupMembers[body["user_id"]]; already {
			// A REAL INSTANCE 409s a second add AND CHANGES NOTHING,
			// which is the half that made the level drift invisible: a
			// pass that only ever added could not move a membership it
			// already had.
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.groupMembers[body["user_id"]] = body["access_level"]

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/projects/") &&
		strings.HasSuffix(path, "/members"):
		project := strings.TrimSuffix(strings.TrimPrefix(path, "/projects/"), "/members")
		var body map[string]int
		json.NewDecoder(r.Body).Decode(&body)
		if _, already := f.projectMembers[project][body["user_id"]]; already {
			// THE SAME 409 THE GROUP ROUTE GIVES. This arm answered 200
			// and quietly overwrote the level, which let the pass look
			// idempotent on a project where a real instance would have
			// refused it.
			w.WriteHeader(http.StatusConflict)
			return
		}
		if f.projectMembers[project] == nil {
			f.projectMembers[project] = map[int]int{}
		}
		f.projectMembers[project][body["user_id"]] = body["access_level"]

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/personal_access_tokens"):
		// TWO ROUTES, AND GITLAB.COM ALLOWS ONE OF THEM.
		//
		// `/users/:id/personal_access_tokens` is INSTANCE ADMIN ONLY, and
		// on gitlab.com nobody is an instance admin: a group Owner who
		// created an account through the group route and minted through
		// this one got a 403 on every seat, for ever. So the admin route
		// is refused here unless the run says it holds an admin token,
		// which is what makes that mistake a red test rather than a live
		// company with no agent able to authenticate.
		id, viaGroup := mintTarget(path)
		if !viaGroup && !f.instanceAdmin {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"403 Forbidden"}`))
			return
		}
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

	// THE GROUP'S OWN LISTING, which is what a group Owner may read. The
	// admin listing below answers 401 for anyone else, and that 401 is
	// what a run hit on its SECOND pass, after minting had already
	// succeeded.
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/personal_access_tokens") &&
		strings.Contains(path, "/service_accounts/"):
		id, _ := mintTarget(path)
		// PAGED, for the reason the membership listing above says and
		// this one did not: a fake that served everything on page 1 makes
		// a caller that never asks for page 2 look correct. It did, and
		// the caller didn't, and the account that exposed it held 164
		// tokens of which this code could see the oldest 20.
		json.NewEncoder(w).Encode(pageOf(f.tokenRows(atoi(id)), r.URL.Query()))

	case r.Method == http.MethodGet && path == "/personal_access_tokens":
		if !f.instanceAdmin {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"401 Unauthorized"}`))
			return
		}
		id := atoi(r.URL.Query().Get("user_id"))
		json.NewEncoder(w).Encode(pageOf(f.tokenRows(id), r.URL.Query()))

	// AND THE GROUP'S OWN REVOKE, the third of the three routes a group
	// Owner may call. Rewritten onto the admin shape so one handler serves
	// both: what differs is the permission, which is checked above.
	case r.Method == http.MethodDelete && strings.Contains(path, "/service_accounts/") &&
		strings.Contains(path, "/personal_access_tokens/"):
		at := strings.LastIndex(path, "/personal_access_tokens/")
		path = "/personal_access_tokens/" + path[at+len("/personal_access_tokens/"):]
		fallthrough

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
			// PAGED, through the same helper every other listing in this
			// fake goes through. These two bypassed it, so a caller that
			// never asked for page two looked correct — which is exactly
			// how the token listing's own bug survived a suite.
			json.NewEncoder(w).Encode(pageOf(renderHooks(f.projectHooks[project]), r.URL.Query()))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		if badSigningToken(body) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "signing_token must be whsec_<base64> over 32 bytes"})
			return
		}
		hook := hookRow{
			id: len(f.projectHooks[project]) + 1, attrs: held(body), signed: f.signs(body),
		}
		if f.projectHooks == nil {
			f.projectHooks = map[string][]hookRow{}
		}
		f.projectHooks[project] = append(f.projectHooks[project], hook)
		json.NewEncoder(w).Encode(hook.render())

	case strings.HasPrefix(path, "/groups/7/hooks") && f.hookStatus != 0:
		w.WriteHeader(f.hookStatus)
		json.NewEncoder(w).Encode(map[string]any{"message": "refused"})

	case strings.HasPrefix(path, "/groups/7/hooks") && f.noGroupHooks:
		// GitLab HIDES a licensed endpoint rather than answering 402, so
		// Free says "not found" about a feature it has.
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"message": "404 Not Found"})

	case r.Method == http.MethodGet && path == "/groups/7/hooks":
		json.NewEncoder(w).Encode(pageOf(renderHooks(f.hooks), r.URL.Query()))

	case r.Method == http.MethodPost && path == "/groups/7/hooks":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		if badSigningToken(body) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "signing_token must be whsec_<base64> over 32 bytes"})
			return
		}
		hook := hookRow{id: len(f.hooks) + 1, attrs: held(body), signed: f.signs(body)}
		f.hooks = append(f.hooks, hook)
		json.NewEncoder(w).Encode(hook.render())

	case r.Method == http.MethodPut && strings.Contains(path, "/hooks/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.hookBodies = append(f.hookBodies, body)
		if badSigningToken(body) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "signing_token must be whsec_<base64> over 32 bytes"})
			return
		}
		f.updatedHooks = append(f.updatedHooks, path)
		// ADDRESSED BY ID, the way the request is. Matching on the url in
		// the BODY meant a fixture could not hold two hooks the pass
		// might address separately, and it made the fake agree with a
		// caller that re-pointed the wrong one.
		at := strings.LastIndex(path, "/hooks/")
		id := atoi(path[at+len("/hooks/"):])
		if strings.HasPrefix(path, "/groups/") {
			f.hooks = f.writeHook(f.hooks, id, body)
			return
		}
		project := strings.TrimPrefix(path[:at], "/projects/")
		f.projectHooks[project] = f.writeHook(f.projectHooks[project], id, body)

	case r.Method == http.MethodDelete && strings.Contains(path, "/hooks/"):
		f.deletedHooks = append(f.deletedHooks, path)
		at := strings.LastIndex(path, "/hooks/")
		id := atoi(path[at+len("/hooks/"):])
		if strings.HasPrefix(path, "/groups/") {
			f.hooks = dropHook(f.hooks, id)
			return
		}
		project := strings.TrimPrefix(path[:at], "/projects/")
		f.projectHooks[project] = dropHook(f.projectHooks[project], id)

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

// badSigningToken reports a value GitLab's API would refuse with a 400.
//
// The documented contract is "whsec_<base64> format encoding a 32-byte key",
// and a fake that took anything non-empty would certify every fixture in
// this suite as valid while a real instance rejected it — which is exactly
// the shape of bug this whole change exists to undo. An empty value is not
// checked here: it means "no signing token", which is a different thing and
// is what confirmSigned catches.
func badSigningToken(body map[string]any) bool {
	token, _ := body["signing_token"].(string)
	if token == "" {
		return false
	}
	payload, ok := strings.CutPrefix(token, "whsec_")
	if !ok {
		return true
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	return err != nil || len(raw) != 32
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
// pageOf serves one page of a listing, the way GitLab does: `per_page` is
// clamped at 100 and `page` is 1-based.
func pageOf(rows []map[string]any, q url.Values) []map[string]any {
	size := atoi(q.Get("per_page"))
	if size <= 0 || size > 100 {
		size = 20
	}
	page := atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * size
	if start >= len(rows) {
		return []map[string]any{}
	}
	return rows[start:min(start+size, len(rows))]
}

// called reports whether the instance saw this request.
func (f *adminInstance) called(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, want)
}

// hasUser reports whether an account still exists.
func (f *adminInstance) hasUser(username string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.users[username]
	return ok
}

// addPerson seeds a group member who is NOT a service account — a real
// human, the thing a decommission sweep must never propose.
func (f *adminInstance) addPerson(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.join(username, f.nextID, gitlabDeveloperLevel)
	f.people[username] = true
}

// mutations counts the requests this instance received that CHANGE it.
//
// # Counted by ROUTE, not by method
//
// [integrationtest]'s converged clause asks how many writes a third-party
// app took, and the tempting implementation is "everything that is not a
// GET". That is the wrong rule to write down even where it gives the right
// answer: some vendors model a listing as a POST, and a method counter would
// call that a write — which makes the clause impossible to satisfy and
// pushes the fix towards weakening the suite instead of fixing the pass.
//
// So the READ routes are named and everything else counts. GitLab serves
// every query on this surface as a GET with query parameters, so here the
// two rules pick out the same requests; the route is the one stated because
// it stays correct if that ever stops being true.
//
// UNRECOGNISED IS A MUTATION, deliberately. A route added to this fixture
// and forgotten here fails the converged clause loudly, where the opposite
// default would quietly stop counting a write the pass had begun making.
//
// A REFUSED write still counts. The pass POSTs a membership that already
// exists and GitLab answers 409; the instance received the request and had
// to answer it, and the clause is about traffic and standing authority
// rather than net effect.
func (f *adminInstance) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if mutatingRoute(call) {
			n++
		}
	}
	return n
}

// mutatingRoute classifies one recorded "METHOD /path".
func mutatingRoute(call string) bool {
	method, path, _ := strings.Cut(call, " ")
	if method != http.MethodGet {
		return true
	}
	switch {
	case path == "/user", // who does this credential authenticate as
		path == "/users",            // the account lookup
		path == "/groups/nimbus",    // the group, and the tier it is on
		path == "/service_accounts", // the instance's own account listing
		path == "/personal_access_tokens",
		strings.HasSuffix(path, "/personal_access_tokens"),
		strings.HasSuffix(path, "/members"),
		strings.HasSuffix(path, "/hooks"),
		strings.HasPrefix(path, "/projects/"): // the existence probe
		return false
	}
	return true
}

// The access levels GitLab takes as integers, mirrored here because a
// fixture asserting a level has to say which one it means.
const (
	gitlabDeveloperLevel  = 30
	gitlabMaintainerLevel = 40
)

func (f *adminInstance) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes, f.mintBodies, f.calls = 0, nil, nil
	f.updatedHooks, f.deletedHooks, f.hookBodies = nil, nil, nil
}

func (f *adminInstance) revoked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokes
}

// refusal is one response the fake is told to give instead of succeeding.
type refusal struct {
	status int
	body   string
}

// mintTarget reads the account a token is being minted for, and says which
// route asked: the group's, which a group Owner may call, or the instance's,
// which needs an admin.
func mintTarget(path string) (id string, viaGroup bool) {
	trimmed := strings.TrimSuffix(path, "/personal_access_tokens")
	if at := strings.LastIndex(trimmed, "/service_accounts/"); at >= 0 {
		return trimmed[at+len("/service_accounts/"):], true
	}
	return strings.TrimPrefix(trimmed, "/users/"), false
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
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

	mu     sync.Mutex
	values map[string]string
	failOn string
	// holdsErr makes the sink unreadable, which must never be read as
	// "nothing is held" — that would rotate every live credential.
	holdsErr error
	discards int
	// writes counts every successful Record, MONOTONICALLY — Discard does
	// not take it back.
	//
	// It is the sealed-store half of the conformance harness's write
	// counter. [integrationtest]'s package doc defines a write as anything
	// a person would have to undo and says a re-sealed credential is one,
	// and nothing about a Record reaches the instance's request log, so a
	// counter built only from HTTP routes reports zero for a pass that
	// rotates every seat's token on every run. Monotonic because the
	// harness samples it as a DELTA around one pass: a counter that a
	// rollback rewound would read as "no writes" for the pass that made
	// the most.
	writes int
	// written is what THIS run recorded, which is the only thing Discard
	// may take back — see Discard.
	written []string
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
	s.writes++
	s.written = append(s.written, name)
	return nil
}

// records is how many values this sink has been asked to seal, ever.
func (s *recordingSink) records() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// Discard implements the sink contract: it removes EVERYTHING THIS RUN
// RECORDED, and nothing else.
//
// It used to clear the whole map, which is a fake that cannot fail the test
// that matters. [provision.SecretStoreSink] — the sink the loop actually
// builds — tracks the names it wrote and unsets exactly those, so a rollback
// leaves a credential an EARLIER pass sealed exactly where it was. A fake
// that swept the lot agreed with a rollback that took a working company's
// tokens down with it, and disagreed with the real sink about the one case
// a rollback is for.
// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *recordingSink) Forget(_ context.Context, names ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgotten = append(s.forgotten, names...)
	return nil
}

func (s *recordingSink) Discard(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	for _, name := range s.written {
		delete(s.values, name)
	}
	s.written = nil
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
func (s *recordingSink) NextStep() string { return "a test next step" }

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
			Handle: handle, Role: strings.ToUpper(handle), TokenVar: tokenVar,
		})
	}
	opts := gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		WebhookBase: "https://crewlet.example.com", SigningSecret: testSigningSecret,
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	// NIL IS "NOTHING TO ADJUST", which most of these runs are: the
	// default options ARE the ordinary group-mode company, and a test that
	// had to pass an empty function to say so would make the exceptions
	// harder to spot rather than easier.
	if tune != nil {
		tune(&opts)
	}
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

// A RUN THAT CANNOT RECORD WHAT IT MINTED REVOKES IT. Between the third-party app
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

// A ROLLBACK DISCARDS WHAT THE RUN SEALED EVEN WHEN IT MINTED NO SEAT TOKEN.
//
// # The window, and why it never closed on its own
//
// The seat tokens are not the only thing a pass seals. When
// integrations.gitlab.signing_secret resolves to nothing, the pass MINTS a
// webhook signing secret and Records it — and only then writes the hook. So
// the steady-state pass, over a company whose seats all keep their tokens,
// can record a credential and mint no token at all.
//
// The rollback returned early on an empty minted map, so that pass left a
// FRESH secret in the fleet's sealed store while GitLab went on signing with
// whatever it had. Nothing recovered: at the next config apply the engine
// resolves the sealed value, so the pass sees a non-empty secret, takes
// SigningReuse and never mints or re-points the hook again. Every delivery
// fails verification from then on, off the back of one transient 500.
//
// Pinned on the SEALED STORE rather than on the count of Discard calls: an
// implementation that called Discard and then re-recorded would satisfy a
// call counter and leave the same broken instance behind.
func TestAFailedHookRollsBackASecretItHadNoTokensToRevoke(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}

	// A converged company: the seat's token is live and recorded, so the
	// pass below keeps it and mints nothing.
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("the run that converges the seat: %v", err)
	}
	f.forget()
	token := sink.value("GITLAB_TOKEN_SWE")

	// A SINK PER PASS, which is what the loop builds: internal/engine's
	// reconcile calls SetupSink once per pass, so Discard's "everything
	// this run recorded" really is this run's. The seat's credential is
	// SEEDED rather than recorded — that is what an earlier pass left
	// behind, and it is what the rollback below must not touch.
	next := newRecordingSink()
	next.seed("GITLAB_TOKEN_SWE", token)

	// And now a run with no resolved signing secret — so it mints one —
	// against a group whose hooks API answers 500. A 500 is a FAULT rather
	// than a tier gate, so the pass raises after the secret is sealed.
	f.hookStatus = http.StatusInternalServerError

	res, err := reconcileWith(t, f, next, seats, func(o *gitlab.Options) {
		o.SigningSecret = ""
		o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
	})
	if err == nil {
		t.Fatalf("a refused hooks API was reported as a successful run: %+v", res)
	}
	if next.discards == 0 {
		t.Error("the sink was never asked to take back what the run sealed")
	}
	if got := next.value("GITLAB_SIGNING_SECRET"); got != "" {
		t.Errorf("the rolled-back run left a signing secret sealed (%q); the "+
			"hook carries a different one and no later pass will ever mint "+
			"again, because this value now resolves", got)
	}
	// AND NOTHING THIS RUN DID NOT RECORD WAS TAKEN. Discard is scoped to
	// the run — the seat's credential was sealed by an earlier pass and is
	// what every agent is currently authenticating with, so a rollback that
	// swept the whole store would take a working company down.
	if next.value("GITLAB_TOKEN_SWE") != token {
		t.Error("the rollback discarded a credential this run did not record")
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

// A HOOK IS UPDATED RATHER THAN DUPLICATED, because the signing secret may
// have rotated — and an instance may carry hooks somebody else registered,
// which a run must not replace. The delivery path is what separates the two:
// see [gitlab.ours], where the name does the selecting and the path is the
// guard.
func TestAnExistingHookIsUpdatedRatherThanDuplicated(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		foreignHook(9, "https://someone-else.example.com/hook"),
		legacyHook(10, "https://crewlet.example.com/webhooks/gitlab"),
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
	if f.hookBodies[0]["signing_token"] != testSigningSecret {
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

// A NODE WITH NO KEYRING READS AND REPORTS RATHER THAN FAULTING.
//
// A nil sink is still refused above — that is the CLI's case, where a run
// that minted live credentials and printed none of them is the worst outcome
// available. What the reconcile loop hands such a node is
// provision.ReadOnly, and the difference matters: ErrNoSink from the entry
// point is a FAULT, which integration.Observe reports as "the last pass could
// not read this integration" and retries on the waiting backoff for ever. An
// ordinary deployment that keeps its ${VAR}s in the environment has no
// secrets.keys at all, so that was every tick, permanently, on a node doing
// exactly what it was configured to do.
func TestASinklessPassReportsRatherThanFailing(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	res, err := reconcileAgainst(t, f, provision.ReadOnly(), map[string]string{
		"swe": "GITLAB_TOKEN_SWE",
	})
	if err != nil {
		t.Fatalf("a node with no keyring failed the whole pass: %v", err)
	}
	if res == nil {
		t.Fatal("a sinkless pass returned no result to report from")
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing {
		t.Fatalf("findings = %+v, want the missing keyring reported", findings)
	}
	if findings[0].Subject != "secrets.keys" {
		t.Errorf("the finding names %q rather than the setting to add",
			findings[0].Subject)
	}
	// AND NOTHING WAS CREATED AT THE INSTANCE. This pass makes an ACCOUNT
	// before it mints a token and the rollback can only revoke the token,
	// so reaching the first Record on a sink that cannot record leaves an
	// identity nobody asked for and nothing recorded.
	if len(res.Created) != 0 {
		t.Errorf("a node that cannot seal a credential created %v", res.Created)
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

// Forget implements [provision.TokenSink].
func (s *cancellingSink) Forget(context.Context, ...string) error { return nil }

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
func (s *cancellingSink) NextStep() string            { return "a test next step" }

// sortByUsername gives the fake a deterministic order, with the DECOY first
// — a caller taking element zero has to be wrong.
func sortByUsername(rows []map[string]any) {
	slices.SortFunc(rows, func(a, b map[string]any) int {
		return cmp.Compare(a["username"].(string), b["username"].(string))
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
	f.join("crewlet-qa", 500, gitlabDeveloperLevel) // a seat that used to exist
	f.join("ci-runner", 501, gitlabDeveloperLevel)  // somebody else's account
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

// A COMPANY WITH NO PUBLIC BASE URL IS DEGRADED, NOT READY.
//
// integrations.public_base_url is optional, the reconcile loop feeds it into
// every pass verbatim, and with nothing in it this pass registers no webhook
// at all: no merge request, pipeline, issue or comment ever reaches a seat.
// The pass said so in a NOTE — which only whoever runs the CLI ever sees —
// and returned no findings, so integration.Classify reported READY to the
// dashboard where a running company is actually watched.
//
// The finding that should have carried it could not: it was keyed on
// `Hooked != "" && len(HookedOn) == 0`, and every route through ensureHooks
// either returns a non-empty list or an error, so the branch was unreachable
// and this state reached nothing at all.
//
// Classify is asserted rather than just the finding, because READY is the
// answer that was wrong and the finding is only how it gets fixed.
func TestNoPublicBaseURLIsReportedRatherThanNoted(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	res, err := reconcileWith(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) { o.WebhookBase = "" })
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want the delivery path reported as blocked", findings)
	}
	if findings[0].Subject != "integrations.public_base_url" {
		t.Errorf("the finding names %q rather than the setting to fill in",
			findings[0].Subject)
	}
	if report := integration.Classify(findings); report.Phase == integration.PhaseReady {
		t.Errorf("a GitLab that delivers nothing classifies as %+v", report)
	}
	// AND THE SEATS WERE STILL PROVISIONED. Ingress is independent of
	// identity: refusing to provision because there is nowhere to deliver
	// would make one missing setting take the whole integration down.
	if len(res.Created) != 1 || sink.value("GITLAB_TOKEN_SWE") == "" {
		t.Errorf("the seat half did not run: created %v, sink %v",
			res.Created, sink.recorded())
	}
}

// A SUCCESSFUL DECOMMISSION IS NOT A FINDING.
//
// It reported one per deleted account, as FindingIdentityMissing — whose
// verdict is PhaseProvisioning / ActorEngine, rendered as "creating agent
// identities". So an operator who removed a seat and ran -decommission got a
// run that did exactly what they asked for, and a status row claiming the
// engine was mid-way through creating an identity for a handle the company
// document no longer contains. Nothing ever cleared it: no later pass
// provisions a seat that is not in the plan.
//
// The branch's own comment gave it away — "it is in the plan, so the company
// expects it to act on GitLab" — which is the one thing Result.Decommissioned
// is guaranteed not to be. Deleting a departed seat's account is the
// successful outcome of a destructive flag, exactly as Result.Kept is the
// successful outcome of a re-run, and neither is something a person has to
// look at.
func TestADecommissionedAccountIsNotReportedAsWorkOutstanding(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, newRecordingSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.join("crewlet-qa", 500, gitlabDeveloperLevel) // a seat that used to exist
	f.mu.Unlock()

	res, err := reconcileWith(t, f, newRecordingSink(), seats,
		func(o *gitlab.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Decommissioned) != 1 {
		t.Fatalf("decommissioned = %v, want the departed seat's account gone",
			res.Decommissioned)
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Errorf("a run that did what -decommission asked reported %+v", findings)
	}
}

// A PERSON THE INSTANCE REFUSES TO DELETE is reported rather than aborting:
// that refusal is GitLab catching what the scan should not have proposed,
// so it is a signal about the prefix.
func TestDecommissionReportsAnAccountTheInstanceWillNotDelete(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.mu.Lock()
	f.join("crewlet-person", 600, gitlabDeveloperLevel)
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
			o.Config.Provisioning.GroupWebhook = config.ContainerWebhookNever
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
			o.Config.Provisioning.GroupWebhook = config.ContainerWebhookRequire
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
			o.Config.Provisioning.GroupWebhook = config.ContainerWebhookNever
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
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {legacyHook(4, "https://crewlet.example.com/webhooks/gitlab")},
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
	if len(f.projectMembers["nimbus/api"]) != 1 {
		t.Errorf("the seat did not join the project that exists: %v", f.projectMembers)
	}
	if len(f.projectMembers["nimbus/gone"]) != 0 {
		t.Errorf("the seat was added to a project that does not exist: %v",
			f.projectMembers["nimbus/gone"])
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
			o.SigningSecret = operatorSigningSecret
			o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
		}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := sink.value("GITLAB_SIGNING_SECRET"); got != "" {
		t.Errorf("it minted over a working secret and recorded %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.hookBodies[0]["signing_token"]; got != operatorSigningSecret {
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
	f.hooks = []hookRow{legacyHook(4, "https://crewlet.example.com/webhooks/gitlab")}

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
			o.SigningSecret = operatorSigningSecret
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

// --- instance mode -------------------------------------------------------- //

// AN INSTANCE SERVICE ACCOUNT IS CREATED ON THE INSTANCE ROUTE, and is still
// made a member of the company's group — that is what gives a seat access to
// the repositories, and it is what keeps a decommission scan scoped.
func TestInstanceModeCreatesOnTheInstanceRoute(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// The group route is refused, so a run that fell back to it fails
	// rather than passing by accident. And the credential IS an admin
	// token, which is what instance mode requires and what makes the
	// /users/:id mint permitted at all.
	f.instanceOnly, f.instanceAdmin = true, true
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { o.Mode = gitlab.ModeInstance })
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !f.called("POST /service_accounts") {
		t.Errorf("the instance route was never used: %v", f.calls)
	}
	if !f.called("POST /groups/7/members") {
		t.Errorf("the account was not made a group member: %v", f.calls)
	}
}

// THE DEFAULT IS THE GROUP ROUTE, which is the only shape GitLab.com has.
func TestTheDefaultModeCreatesUnderTheGroup(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noInstanceRoute = true
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(*gitlab.Options) {}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !f.called("POST /groups/7/service_accounts") {
		t.Errorf("the group route was never used: %v", f.calls)
	}
	if f.called("POST /service_accounts") {
		t.Errorf("the instance route was used by default: %v", f.calls)
	}
}

// GITLAB.COM DOES NOT SERVE THE ROUTE AT ALL, and a bare 404 tells an
// operator nothing about which of their two flags was wrong.
func TestInstanceModeOnAnInstanceThatHasNoSuchRouteSaysSo(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noInstanceRoute = true
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { o.Mode = gitlab.ModeInstance })
	if err == nil {
		t.Fatal("the run succeeded on an instance without the route")
	}
	if !strings.Contains(err.Error(), "self-managed only") {
		t.Errorf("error = %v", err)
	}
}

// A 403 MEANS TWO DIFFERENT THINGS depending on the mode, so the message has
// to name the credential this mode actually needs.
func TestEachModesRefusalNamesTheCredentialItNeeds(t *testing.T) {
	t.Parallel()
	t.Run("instance", func(t *testing.T) {
		t.Parallel()
		f := newAdminInstance()
		f.instanceForbidden = true
		_, err := reconcileWith(t, f, newRecordingSink(),
			map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
			func(o *gitlab.Options) { o.Mode = gitlab.ModeInstance })
		if err == nil || !strings.Contains(err.Error(), "INSTANCE ADMINISTRATOR") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("group", func(t *testing.T) {
		t.Parallel()
		f := newAdminInstance()
		f.instanceOnly = true
		_, err := reconcileWith(t, f, newRecordingSink(),
			map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
			func(*gitlab.Options) {})
		if err == nil || !strings.Contains(err.Error(), "Owner of provisioning.group") {
			t.Errorf("error = %v", err)
		}
	})
}

// AN ACCOUNT THAT ALREADY EXISTS IS FOUND WHATEVER OWNS IT, which is what
// makes switching modes safe: the lookup is /users?username=, and an
// operator moving a company between modes must not collide with its own
// usernames.
func TestSwitchingModesFindsTheAccountsTheCompanyAlreadyHas(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// Instance mode holds an admin token, which is what permits the
	// /users/:id mint at all; a group Owner is refused there.
	f.instanceAdmin = true
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(*gitlab.Options) {}); err != nil {
		t.Fatalf("group run: %v", err)
	}
	f.instanceForbidden = true // a second create would fail
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { o.Mode = gitlab.ModeInstance })
	if err != nil {
		t.Fatalf("instance run: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("the second run created %v", res.Created)
	}
}

// THE DELETE ROUTE IS NOT INTERCHANGEABLE. The group route answers "no such
// account" for one the instance owns, which a caller reads as "already gone"
// — a decommission that silently kept every credential it was asked to
// destroy.
func TestInstanceModeDecommissionsDownTheInstanceRoute(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// Instance mode holds an admin token, which is what permits the
	// /users/:id mint at all; a group Owner is refused there.
	f.instanceAdmin = true
	instance := func(o *gitlab.Options) { o.Mode = gitlab.ModeInstance }
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}", "qa": "${GITLAB_TOKEN_QA}"},
		instance); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { instance(o); o.Decommission = true })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Decommissioned) != 1 || res.Decommissioned[0] != "crewlet-qa" {
		t.Fatalf("decommissioned = %v", res.Decommissioned)
	}
	if f.hasUser("crewlet-qa") {
		t.Error("the account is still live")
	}
}

// AN UNKNOWN MODE IS REFUSED BEFORE ANYTHING IS TOUCHED: it decides which
// endpoint every account is created on, and finding out from a 404 half way
// through leaves an operator working out which seats landed.
func TestAnUnknownModeIsRefused(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { o.Mode = gitlab.Mode("cluster") })
	if err == nil {
		t.Fatal("an unknown mode ran")
	}
	if !strings.Contains(err.Error(), "group, instance") {
		t.Errorf("the error does not name the modes: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("the instance was touched: %v", f.calls)
	}
}

// A GROUP LISTING IS PAGED TO EXHAUSTION. It asked for one page and took
// what came back — so on a group with more members than a page, every
// managed account past the first was invisible to a decommission and stayed
// live for ever, with the run reporting success.
func TestADecommissionSeesPastTheFirstPageOfMembers(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// 130 members, and the 128 people SORT BEFORE the managed accounts —
	// so both of those land on page two, which is the only arrangement
	// that can tell a paged walk from a single-page one.
	for i := range 128 {
		f.addPerson(fmt.Sprintf("aaa-person-%03d", i))
	}
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}", "qa": "${GITLAB_TOKEN_QA}"},
		func(*gitlab.Options) {}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"},
		func(o *gitlab.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Decommissioned) != 1 || res.Decommissioned[0] != "crewlet-qa" {
		t.Fatalf("decommissioned = %v", res.Decommissioned)
	}
}

// A GROUP OWNER MINTS THROUGH THE GROUP, because that is the only route it
// may use.
//
// `POST /users/:id/personal_access_tokens` is INSTANCE ADMIN ONLY, and on
// gitlab.com nobody is an instance admin. So a run that created a service
// account through the group route, which a group Owner may do, and then
// minted through the admin one got a 403 on every seat, for ever: an account
// existed with no token, the seat authenticated as nobody, and the card said
// the sync would take care of it.
func TestAGroupOwnerUsesTheGroupRouteForEveryTokenOperation(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// NOT AN ADMIN, which is every gitlab.com credential: the fake refuses
	// the /users/:id route, exactly as the real instance does.
	res, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"}, nil)
	if err != nil {
		t.Fatalf("a group Owner could not provision a seat: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("result = %+v", res)
	}

	// THE GROUP'S OWN ROUTES, and never the instance's. All three of them:
	// the mint 403s outright, the list 401s on the NEXT pass after minting
	// had already succeeded, and the revoke fails inside a rollback where
	// the message says a live credential was left behind.
	for _, call := range f.calls {
		if !strings.Contains(call, "/personal_access_tokens") {
			continue
		}
		if !strings.Contains(call, "/service_accounts/") {
			t.Errorf("a token operation went through %q, which a group Owner "+
				"may not call: on gitlab.com nobody is an instance admin", call)
		}
	}

	// AND A SECOND PASS IS THE ONE THAT LISTS. The first mints; the next
	// reads the tokens back to decide whether to keep what it finds, and
	// that read is the admin listing unless it goes through the group.
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"}, nil); err != nil {
		t.Fatalf("a second pass could not read the tokens it had minted: %v", err)
	}
}

// A NAME GITLAB HAS NOT RELEASED YET IS A DELETION STILL RUNNING.
//
// GitLab removes a user asynchronously: the account is gone from every
// listing the moment the delete is accepted, and its username and email stay
// reserved until a background job finishes. So a disconnect followed by a
// reconnect inside that window looks the account up, honestly does not find
// it, creates one, and is refused with "has already been taken".
//
// Reported as an ordinary failure it read as "the last pass could not read
// this integration", which sends an operator looking for an outage over a
// state that clears itself on the next tick. Named, the caller can say what
// is actually happening.
func TestANameStillBeingReleasedIsNotAFailureToRead(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.createRefusal = &refusal{
		status: http.StatusBadRequest,
		body:   `{"message":"400 Bad request - Email has already been taken and Username has already been taken"}`,
	}

	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"}, nil)
	if err == nil {
		t.Fatal("a refused creation was reported as a clean run")
	}
	if !errors.Is(err, gitlab.ErrNameReserved) {
		t.Fatalf("error = %v, and nothing marks it as a deletion still running", err)
	}
}

// AND EVERY OTHER 400 IS STILL A REFUSAL. A creation GitLab rejected for a
// reason of its own is not something a later tick fixes, and dressing one as
// work in progress would leave a card reporting progress for ever.
func TestAnOrdinaryRefusalIsNotDressedAsProgress(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.createRefusal = &refusal{
		status: http.StatusBadRequest,
		body:   `{"message":"400 Bad request - Name is too long"}`,
	}

	_, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "${GITLAB_TOKEN_SWE}"}, nil)
	if err == nil {
		t.Fatal("a refused creation was reported as a clean run")
	}
	if errors.Is(err, gitlab.ErrNameReserved) {
		t.Fatalf("an unrelated refusal reads as a deletion still running: %v", err)
	}
}

// A FREE GROUP TAKES THE REGISTRATION AND NEVER DELIVERS, which no error can
// tell you.
//
// The fallback beside this one turns on the create call FAILING, and on
// gitlab.com's free tier it does not fail: POST /groups/:id/hooks answers
// 201, the hook is listed in the group's settings, and its own event log
// stays empty for ever. Measured on a live free group, where the pass
// reported ready and not one delivery had ever arrived. So the tier is read
// rather than inferred from a refusal.
func TestAFreeTierGroupHooksTheProjectsWithoutTrying(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.plan = "free"
	sink := newRecordingSink()

	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.projectHooks["nimbus/api"]) != 1 {
		t.Fatalf("project hooks = %+v, want one on the declared project", f.projectHooks)
	}
	// AND NO GROUP HOOK AT ALL. One registered here is one an operator
	// finds in the settings, believes covers the group, and never receives
	// a delivery from.
	if len(f.hooks) != 0 {
		t.Errorf("a group hook was registered on a tier that never delivers: %+v", f.hooks)
	}
	if got := res.HookedOn; len(got) != 1 || got[0] != "nimbus/api" {
		t.Errorf("HookedOn = %v, want the project", got)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "free tier") {
		t.Errorf("notes did not say why the group was skipped: %v", res.Notes)
	}
}

// SILENCE IS NOT "FREE". A self-managed instance answers with no plan at all,
// and reading that as free would send every self-managed deployment down a
// fallback it does not need, replacing one hook that covers the group with
// one per declared project.
func TestAnInstanceThatNamesNoPlanKeepsTheGroupHook(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.plan = ""
	sink := newRecordingSink()

	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(f.hooks) != 1 {
		t.Fatalf("group hooks = %+v, want the one this instance serves", f.hooks)
	}
	if got := res.HookedOn; len(got) != 1 || got[0] != "group" {
		t.Errorf("HookedOn = %v, want the group", got)
	}
}

// --- reading before writing ----------------------------------------------- //

// A CONVERGED PASS SENDS NO MEMBERSHIP WRITE AT ALL.
//
// It used to send one POST per seat to the group and one per seat per project
// on every pass, for ever, and GitLab answered every one of them with a 409
// the client read as success. That is the shape [integration.Reconciler]'s
// contract forbids: the loop runs this every few minutes for the life of the
// deployment, so a write here is a third-party-app mutation on a timer.
func TestASecondPassSendsNoMembershipItAlreadyRead(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE", "cto": "GITLAB_TOKEN_CTO"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, unwanted := range []string{
		"POST /groups/7/members", "POST /projects/nimbus/api/members",
	} {
		if f.called(unwanted) {
			t.Errorf("a converged pass sent %s", unwanted)
		}
	}
	// AND IT READ INSTEAD. The point is not that the write vanished but
	// that the pass now knows what the instance holds.
	for _, wanted := range []string{
		"GET /groups/7/members", "GET /projects/nimbus/api/members",
	} {
		if !f.called(wanted) {
			t.Errorf("the pass never read %s, so it cannot know what to write", wanted)
		}
	}
}

// A CHANGED ACCESS LEVEL REACHES A MEMBERSHIP THAT ALREADY EXISTS.
//
// This is the bug the swallowed 409 was hiding. Adding was the only thing the
// pass could do and GitLab answers a second add with a conflict and changes
// nothing, so editing provisioning.access_level did NOTHING for a seat that
// already had a membership: the company document said maintainer, the
// instance kept developer, and every pass reported converged for the life of
// the deployment.
func TestAChangedAccessLevelReachesAnExistingMembership(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	id := f.users["crewlet-swe"]
	if got := f.groupMembers[id]; got != gitlabDeveloperLevel {
		f.mu.Unlock()
		t.Fatalf("the first run joined at %d, want the default developer level", got)
	}
	f.mu.Unlock()

	if _, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.Config.Provisioning.AccessLevel = config.GitLabMaintainer
	}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.groupMembers[id]; got != gitlabMaintainerLevel {
		t.Errorf("the group membership is at %d after the config said maintainer; "+
			"the instance keeps the old level and every pass reports converged", got)
	}
	if got := f.projectMembers["nimbus/api"][id]; got != gitlabMaintainerLevel {
		t.Errorf("the project membership is at %d, want maintainer", got)
	}
}

// AND A PER-HANDLE OVERRIDE DOES TOO, which is the same drift reached from
// the other config field.
func TestAPerHandleAccessOverrideReachesAnExistingMembership(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE", "cto": "GITLAB_TOKEN_CTO"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.Config.Provisioning.AccessLevels = map[string]config.GitLabAccessLevel{
			"cto": config.GitLabMaintainer,
		}
	}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.groupMembers[f.users["crewlet-cto"]]; got != gitlabMaintainerLevel {
		t.Errorf("the overridden seat is at %d, want maintainer", got)
	}
	// AND ONLY THAT SEAT. An override that moved everybody would be the
	// opposite failure and just as silent.
	if got := f.groupMembers[f.users["crewlet-swe"]]; got != gitlabDeveloperLevel {
		t.Errorf("a seat with no override moved to %d", got)
	}
}

// A SEAT THE ROSTER DOES NOT CARRY IS STILL ADDED. Reading first must not
// turn into reading instead: an account somebody removed from the group by
// hand is exactly what a reconcile is for.
func TestAMembershipTheGroupLostIsPutBack(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	id := f.users["crewlet-swe"]
	delete(f.groupMembers, id)
	delete(f.projectMembers["nimbus/api"], id)
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, member := f.groupMembers[id]; !member {
		t.Error("the seat was left out of the group it had been removed from")
	}
	if _, member := f.projectMembers["nimbus/api"][id]; !member {
		t.Error("the seat was left out of the project it had been removed from")
	}
}

// A PROJECT'S MEMBERSHIP IS WALKED TO EXHAUSTION.
//
// The group listing already had this correction; the project one arrived with
// it. A single-page read makes every member past the first hundred invisible,
// and invisible here means the pass re-adds a seat that is already there — on
// every pass, for ever, which is the clause it must not break.
func TestAProjectMembershipPastTheFirstPageIsSeen(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// 128 members whose usernames SORT BEFORE the managed account, so the
	// seat lands on page two — the only arrangement that tells a paged
	// walk from a single-page one.
	f.mu.Lock()
	for i := range 128 {
		f.nextID++
		f.projectMembers["nimbus/api"][f.nextID] = gitlabDeveloperLevel
		f.users[fmt.Sprintf("aaa-person-%03d", i)] = f.nextID
	}
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if f.called("POST /projects/nimbus/api/members") {
		t.Error("the seat was re-added to a project it is already in, so the " +
			"membership read stopped at the first page")
	}
}

// --- the hook, written only on a difference -------------------------------- //

// A CONVERGED HOOK IS LEFT ENTIRELY ALONE.
//
// It used to be re-written unconditionally, on the reasoning that the signing
// secret may have rotated. It may — and the run knows whether it did, so that
// reasoning justifies a write on the pass that rotates and nothing on the
// hundreds that do not. What the unconditional PUT cost was the steady state:
// the identical body, all nineteen event flags of it, every few minutes for
// the life of the deployment.
func TestAConvergedGroupHookIsNotRewritten(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()

	res, err := reconcileAgainst(t, f, sink, seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 0 {
		t.Errorf("a converged pass wrote the hook again: %v", f.hookBodies)
	}
	// AND ONE LISTING, not two. confirmSigned re-read the hook after every
	// write to prove the instance honoured the signing token; with nothing
	// written there is nothing to confirm, and the listing the pass already
	// took is what said the token is there.
	reads := 0
	for _, call := range f.calls {
		if call == "GET /groups/7/hooks" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the hook was listed %d times on a pass that wrote nothing", reads)
	}
	// AND IT STILL REPORTS THE HOOK. Skipping the write must not turn into
	// reporting no webhook, which Findings reads as a blocked ingress.
	if got := res.HookedOn; len(got) != 1 || got[0] != "group" {
		t.Errorf("HookedOn = %v, want the group it is still registered on", got)
	}
}

// THE SAME SKIP ON THE PER-PROJECT PATH, where the cost was multiplied by the
// project count.
func TestAConvergedProjectHookIsNotRewritten(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.plan = "free"
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.Config.Provisioning.Projects = []string{"nimbus/api", "nimbus/web"}
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.forget()

	res, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.Config.Provisioning.Projects = []string{"nimbus/api", "nimbus/web"}
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 0 {
		t.Errorf("a converged pass rewrote %d project hook(s)", len(f.hookBodies))
	}
	if len(res.HookedOn) != 2 {
		t.Errorf("HookedOn = %v, want both projects it is still registered on",
			res.HookedOn)
	}
}

// A SECRET THIS RUN MINTED IS WRITTEN TO THE HOOK EVEN THOUGH THE HOOK SAYS
// IT HAS ONE.
//
// GitLab never returns a signing token, so `signing_token_present` says a
// hook holds SOME key and nothing says which. A run that rotated the key
// therefore cannot read its way to the answer — it has to carry its own
// decision to the hook, or the instance goes on signing with the value the
// engine has just replaced and every delivery is refused.
func TestARotatedSigningSecretIsWrittenToAHookThatLooksConverged(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.SigningSecret = ""
		o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	minted := sink.value("GITLAB_SIGNING_SECRET")
	f.forget()

	if _, err := reconcileWith(t, f, sink, seats, func(o *gitlab.Options) {
		o.SigningSecret = minted
		o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
		o.Rotate = true
	}); err != nil {
		t.Fatalf("rotating run: %v", err)
	}
	rotated := sink.value("GITLAB_SIGNING_SECRET")
	if rotated == minted {
		t.Fatal("-rotate did not replace the signing secret")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes on a run that replaced the key, want one",
			len(f.hookBodies))
	}
	if got := f.hookBodies[0]["signing_token"]; got != rotated {
		t.Errorf("the hook carries %q, not the key this run recorded", got)
	}
}

// A HOOK THAT LOST A ROUTED EVENT IS PUT BACK.
//
// The whole point of comparing rather than re-writing is that the comparison
// has to be able to say no. Somebody unticking `issues_events` in the
// settings page is the ordinary way a hook drifts, and a pass that only
// checked the signing token would call that converged and leave the engine
// deaf to every issue in the group.
func TestAHookThatLostARoutedEventIsPutBack(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.hooks[0].attrs["issues_events"] = false
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want the one that re-subscribes it", len(f.hookBodies))
	}
	if f.hooks[0].attrs["issues_events"] != true {
		t.Error("the hook is still unsubscribed from issues")
	}
}

// AND ONE SUBSCRIBED TO SOMETHING NOTHING ROUTES IS PUT BACK TOO. A hook
// somebody ticked `push_events` on delivers every push in the group to an
// engine that answers 200 and drops it, which looks healthy from both ends.
func TestAHookSubscribedToWhatNothingRoutesIsPutBack(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.hooks[0].attrs["push_events"] = true
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want the one that unsubscribes it", len(f.hookBodies))
	}
	if f.hooks[0].attrs["push_events"] != false {
		t.Error("the hook is still subscribed to every push in the group")
	}
}

// AND ONE WITH TLS VERIFICATION TURNED OFF IS PUT BACK. It carries a signing
// secret, so a delivery that skips certificate checking hands the payload —
// and the header proving it is ours — to whoever answered.
func TestAHookWithTLSVerificationOffIsPutBack(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	f.mu.Lock()
	f.hooks[0].attrs["enable_ssl_verification"] = false
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want the one that turns verification back on",
			len(f.hookBodies))
	}
	if f.hooks[0].attrs["enable_ssl_verification"] != true {
		t.Error("the hook still delivers a signed payload without checking the " +
			"certificate")
	}
}

// A HOOK THAT REPORTS NO SIGNING TOKEN IS ALWAYS WRITTEN, whatever else about
// it matches. That is the state a GitLab older than 19.1 leaves behind and
// the state an older Crewlet's hook is in, and it is the difference between
// an integration that works and one that has been silently unauthenticated
// since it was created.
func TestAHookWithNoSigningTokenIsAlwaysWritten(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	seats := map[string]string{"swe": "GITLAB_TOKEN_SWE"}
	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Everything else exactly as the pass left it — only the signing
	// token is gone, the way an instance that never honoured it reports.
	f.mu.Lock()
	f.hooks[0].signed = false
	f.mu.Unlock()
	f.forget()

	if _, err := reconcileAgainst(t, f, sink, seats); err != nil {
		t.Fatalf("second run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want the one that sets the signing token",
			len(f.hookBodies))
	}
}

// --- a cancelled pass is a fault ------------------------------------------ //

// A CANCELLED PASS RAISES RATHER THAN REPORTING A CONVERGED INSTANCE, EVEN
// WITH NOTHING TO DO.
//
// An empty plan is reachable in production: [gitlab.PlanFor] skips every seat
// whose mcp_env.gitlab token is a literal rather than a ${VAR}, so a company
// managing its GitLab credentials by hand plans nothing. That return handed
// back a Result with no findings without touching the context or making one
// request — and to the loop an error and an empty findings list are opposite
// claims. A node shutting down would record GitLab as ready on its way out,
// and the next node to hold the duty trusts that for a full settled interval.
func TestACancelledPassWithNothingToDoIsStillAFault(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: enabledGitLab(), Plan: &provision.Plan{},
		Sink: newRecordingSink(),
	})
	if err == nil {
		t.Fatalf("a cancelled pass answered %+v, which the loop reads as a "+
			"converged integration and trusts for a full settled interval", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the fault does not name the cancellation: %v", err)
	}
}

// AND SO DOES ONE WITH SEATS, which is the same claim reached the other way:
// the guard is at the top of the pass rather than beside the one early return
// that happened to have the hole.
func TestACancelledPassWithSeatsMakesNoRequest(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	plan := &provision.Plan{}
	plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: enabledGitLab(), Plan: plan,
		Sink: newRecordingSink(),
	}); err == nil {
		t.Fatal("a cancelled pass with work outstanding reported success")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("a cancelled pass still talked to the instance: %v", f.calls)
	}
}

// AN ACCOUNT THAT CANNOT AUTHENTICATE IS REPORTED, NOT MINTED AT — AND THE
// TOKENS ALREADY MINTED FOR IT ARE SWEPT.
//
// This is the loop, and it needs three things to be true at once, which is
// why it is one case rather than three.
//
// A pass mints because the seat's STORED token was refused, and reads that
// refusal as "the credential is stale" — which is right, except when it is
// the account GitLab is refusing rather than the credential. Then the new
// token is refused for the same reason, the next pass reads it as stale
// again, and the run mints for ever. Measured against gitlab.com: 144 live
// `api`-scoped tokens on one service account, valid for a year, none of which
// had ever worked, plus twenty more from a slower build of the same loop.
//
// Nothing retired them, because [gitlab.Client.Tokens] read one unpaged page
// — the oldest twenty, every one already revoked — so `retirePrevious`
// revoked nothing and reported success.
//
// So: mint at most once, prove it, and if a token this pass created seconds
// ago is itself refused, sweep every token this tool owns on that account and
// say what is wrong. A seat is left with NO credential, deliberately: the
// alternative is the pile above.
func TestAnAccountThatCannotAuthenticateIsSweptRatherThanMintedAt(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()

	// PASS ONE creates the account and mints its first token. Nothing is
	// wrong yet — this is the healthy connect.
	if _, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	userID := f.userID(t, "crewlet-swe")
	sealed := sink.values["T_SWE"]
	if sealed == "" {
		t.Fatal("precondition: the first pass sealed no token")
	}

	// AND THEN THE ACCOUNT GOES BAD — or, in the field, was never good:
	// GitLab refuses everything it presents, however new.
	f.setUnusable(userID)

	// Three passes, which is what a reconcile loop does in half a minute.
	for pass := 2; pass <= 4; pass++ {
		res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if !slices.Contains(res.Unusable, "swe") {
			t.Fatalf("pass %d reported Unusable = %v, want swe", pass, res.Unusable)
		}
		if len(res.Rotated) != 0 {
			t.Errorf("pass %d rotated %v: a token that was refused on sight "+
				"must not be reported as this seat's credential", pass, res.Rotated)
		}
	}

	// THE INVARIANT THE INCIDENT BROKE: nothing accumulates. Every token
	// this tool minted on the account is revoked, including the one the
	// last pass made a moment ago.
	if live := f.liveTokensOf(userID); live != 0 {
		t.Errorf("the account holds %d live token(s) after three refused "+
			"passes, want 0 — this is the pile that reached 144", live)
	}

	// AND NO REFUSED TOKEN IS EVER SEALED. The variable still holds what
	// the healthy pass put there — which is stale, and says so through the
	// finding below rather than by being overwritten with something that
	// works no better. Recording a refused token would both hand the engine
	// a credential that cannot authenticate and, because sealing
	// re-activates the revision, wake the pass that seals the next one.
	if got := sink.values["T_SWE"]; got != sealed {
		t.Errorf("T_SWE moved from %q to %q: a token GitLab refused on sight "+
			"was sealed as this seat's credential", sealed, got)
	}

	// THE OPERATOR IS TOLD, as a seat's identity failing rather than as a
	// surface fault: no retry fixes this and the engine is not working on it.
	res, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"})
	if err != nil {
		t.Fatalf("final pass: %v", err)
	}
	var kinds []integration.FindingKind
	for _, finding := range res.Findings() {
		kinds = append(kinds, finding.Kind)
	}
	if !slices.Contains(kinds, integration.FindingIdentityFailed) {
		t.Fatalf("findings = %v, want an identity_failed naming the seat", kinds)
	}
	phase, actor := integration.FindingIdentityFailed.Verdict()
	if !actor.WaitsOnAPerson() {
		t.Errorf("identity_failed is %s/%s, which does not ask anybody to act "+
			"— nothing in this engine can fix an account GitLab refuses",
			phase, actor)
	}
}

// A SERVICE ACCOUNT IS CREATED WITH NO ADDRESS, and the absent key is the
// whole of it.
//
// GitLab's service-account routes take `email` as optional and generate one
// under the instance's own noreply domain when it is omitted. A CUSTOM address
// "requires confirmation before the account is active" — and every address
// this tool could derive is undeliverable by construction, so confirmation
// never arrives and the account is permanently inactive. See the case above
// for what that cost.
func TestAServiceAccountIsCreatedWithNoAddress(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name  string
		route string
		tune  func(*gitlab.Options)
	}{
		{"group", "POST /groups/7/service_accounts", func(*gitlab.Options) {}},
		{"instance", "POST /service_accounts", func(o *gitlab.Options) {
			o.Mode = gitlab.ModeInstance
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			f := newAdminInstance()
			f.instanceAdmin = mode.name == "instance"
			if _, err := reconcileWith(t, f, newRecordingSink(),
				map[string]string{"swe": "T_SWE"}, mode.tune); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if !f.called(mode.route) {
				t.Fatalf("no account was created down %s", mode.route)
			}
			for _, body := range f.creates() {
				if _, sent := body["email"]; sent {
					t.Errorf("the create sent email=%v; GitLab must name the "+
						"address, because one this tool derives needs a "+
						"confirmation that can never arrive", body["email"])
				}
				if body["username"] != "crewlet-swe" {
					t.Errorf("the create sent username=%v", body["username"])
				}
			}
		})
	}
}

// THE TOKEN LIST IS PAGED TO EXHAUSTION, because a destructive decision is
// made from it.
//
// It read one default page — TWENTY rows — and both callers treat what comes
// back as the whole truth: `retirePrevious` revokes this tool's earlier
// tokens, and the decommission sweep empties an account it is deleting. A
// truncated read is not a slow report there; it is a live `api`-scoped
// credential left behind by a run that said it cleaned up.
func TestTheTokenListIsPagedToExhaustion(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()

	if _, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	userID := f.userID(t, "crewlet-swe")

	// A PILE LIKE THE ONE THE FIELD FOUND, well past one page and past
	// two, so a loop that stopped after the first is caught and so is one
	// that stopped after any fixed number.
	f.pileTokens(userID, gitlab.TokenName("swe"), 150)
	if live := f.liveTokensOf(userID); live != 151 {
		t.Fatalf("precondition: %d live tokens, want 151", live)
	}

	// A ROTATION, which is the gesture that retires what came before.
	if _, err := reconcileWith(t, f, sink, map[string]string{"swe": "T_SWE"},
		func(o *gitlab.Options) { o.Rotate = true }); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// EXACTLY THE NEW ONE SURVIVES. Unpaged, 131 of these stayed live and
	// the run reported a successful rotation.
	if live := f.liveTokensOf(userID); live != 1 {
		t.Errorf("%d live tokens after a rotation, want 1: every token this "+
			"tool minted earlier should have been revoked", live)
	}
}

// AN ADMINISTRATOR'S OWN TOKEN SURVIVES THE SWEEP, which is what keeps the
// two cases above from being a licence to empty an account.
//
// The name is the only thing separating a token this tool minted from one a
// person created by hand, and revoking the second breaks whatever is using it
// — silently, since nothing here knows what that is.
func TestTheSweepLeavesTokensThisToolDidNotMint(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	if _, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	userID := f.userID(t, "crewlet-swe")
	f.pileTokens(userID, "deploy-key-do-not-touch", 3)
	f.setUnusable(userID)

	if _, err := reconcileAgainst(t, f, sink, map[string]string{"swe": "T_SWE"}); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	// The three are all that is left: this tool's own are gone, and the
	// sweep for an unusable account is the most destructive path there is.
	if live := f.liveTokensOf(userID); live != 3 {
		t.Errorf("%d live tokens, want the 3 this tool never minted", live)
	}
}

// userID is an account's id, failed loudly rather than returned as zero:
// every caller below uses it to address tokens, and zero addresses nobody.
func (f *adminInstance) userID(t *testing.T, username string) int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.users[username]
	if !ok {
		t.Fatalf("no account %q was created", username)
	}
	return id
}

// setUnusable makes every token this account holds be refused, however new,
// which is how GitLab answers for an account whose address was never
// confirmed.
func (f *adminInstance) setUnusable(userID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unusable[userID] = true
}

// liveTokensOf counts what ONE account could still authenticate with,
// where [adminInstance.liveTokens] counts the whole instance's.
func (f *adminInstance) liveTokensOf(userID int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	live := 0
	for _, token := range f.tokens[userID] {
		if !token.Revoked && (token.ExpiresAt.IsZero() || token.ExpiresAt.After(f.now)) {
			live++
		}
	}
	return live
}

// pileTokens seeds n live tokens under one name, which is the state a run
// that minted without retiring leaves behind.
func (f *adminInstance) pileTokens(userID int, name string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.nextToken++
		f.tokens[userID] = append(f.tokens[userID], &gitlab.Token{
			ID: f.nextToken, Name: name,
			Value: fmt.Sprintf("glpat-piled-%d", f.nextToken),
		})
	}
}

// creates are the service-account creation payloads this instance received.
func (f *adminInstance) creates() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.createBodies)
}

// A HOOK THIS ENGINE LEFT AT AN ADDRESS IT NO LONGER USES IS REMOVED.
//
// The reported failure, measured on a real deployment behind a tunnel: the
// public base moved twice, the pass matched hooks on the URL so it could not
// see either of the two it had already registered, and it created a third.
// All three stayed enabled, all three stayed signed, and two of them
// delivered to addresses that no longer answered — with nothing anywhere to
// say they existed.
func TestTheHooksLeftAtAMovedAddressAreRemoved(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		namedHook(1, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab"),
		namedHook(2, "crewlet", "https://older-tunnel.example.com/webhooks/gitlab"),
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 1 {
		t.Fatalf("hooks = %+v, want exactly one", f.hooks)
	}
	if got := f.hooks[0].attrs["url"]; got != "https://crewlet.example.com/webhooks/gitlab" {
		t.Errorf("the surviving hook points at %v, want the address in force", got)
	}
	if len(f.deletedHooks) != 1 {
		t.Errorf("deleted %v, want the one hook the first was not re-pointed onto",
			f.deletedHooks)
	}
}

// THE SAME AT THE PROJECT LEVEL, which is the half a group-only fix would
// have left behind: a company on a tier with no group hooks carries one
// registration per project, so a moved base orphans one per project rather
// than one in total.
func TestTheProjectHooksLeftAtAMovedAddressAreRemoved(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {
			namedHook(1, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab"),
			namedHook(2, "crewlet", "https://older-tunnel.example.com/webhooks/gitlab"),
		},
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.projectHooks["nimbus/api"]
	if len(held) != 1 {
		t.Fatalf("hooks on nimbus/api = %+v, want exactly one", held)
	}
	if got := held[0].attrs["url"]; got != "https://crewlet.example.com/webhooks/gitlab" {
		t.Errorf("the surviving hook points at %v, want the address in force", got)
	}
}

// ANOTHER DEPLOYMENT'S HOOK IS NOT TOUCHED, which is the whole reason the
// name is a config field rather than a constant. Staging and production of
// one company share this document and watch one instance; with one name each
// pass would repoint the other's hook and only the last to run would receive
// anything.
func TestAHookNamedByAnotherDeploymentIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		namedHook(1, "crewlet-staging", "https://staging.example.com/webhooks/gitlab"),
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deletedHooks) != 0 {
		t.Errorf("deleted %v, want another deployment's hook left alone", f.deletedHooks)
	}
	if len(f.hooks) != 2 {
		t.Fatalf("hooks = %+v, want the staging hook plus this deployment's own", f.hooks)
	}
}

// AND NEITHER IS A HOOK THAT MERELY SHARES THE NAME. The delivery path is
// what says a hook is this engine's at all: matching on the name alone would
// adopt — and then re-point — an unrelated integration that happened to be
// called crewlet.
func TestAHookSharingTheNameButNotThePathIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		namedHook(1, "crewlet", "https://someone-else.example.com/ci-trigger"),
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updatedHooks) != 0 || len(f.deletedHooks) != 0 {
		t.Errorf("updated %v and deleted %v, want somebody else's hook untouched",
			f.updatedHooks, f.deletedHooks)
	}
}

// EVERY HOOK CARRIES THE NAME, because the name is what the next pass finds
// it by. A create that omitted it would leave a registration this engine
// could identify only by the address it was about to move away from.
func TestEveryHookIsRegisteredUnderTheConfiguredName(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 1 {
		t.Fatalf("%d hook writes, want one create", len(f.hookBodies))
	}
	if got := f.hookBodies[0]["name"]; got != gitlab.DefaultWebhookName {
		t.Errorf("the hook was registered as %v, want %q", got, gitlab.DefaultWebhookName)
	}
}

// A GROUP HOOK IS SWEPT UP WHEN THE COMPANY MOVES TO PROJECT HOOKS.
//
// `group_webhook` is a live config field and the `auto` answer depends on the
// group's PLAN, so the level this engine writes at MOVES. Nothing looked at
// the level it had stopped writing, so the group hook went on delivering
// every event the project hooks already deliver, for ever, with no pass that
// had any reason to mention it.
func TestTheGroupHookGoesWhenTheCompanyMovesToProjectHooks(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		namedHook(1, "crewlet", "https://crewlet.example.com/webhooks/gitlab"),
	}
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) {
			o.Config.Provisioning.GroupWebhook = config.ContainerWebhookNever
		}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 0 {
		t.Errorf("group hooks = %+v, want the one this engine left removed", f.hooks)
	}
	if len(f.projectHooks["nimbus/api"]) != 1 {
		t.Errorf("project hooks = %+v, want one", f.projectHooks["nimbus/api"])
	}
}

// A NAMELESS HOOK IS ADOPTED RATHER THAN STRANDED.
//
// GitLab has taken a name on a hook since 17.1 and this engine never sent
// one, so every hook it has ever registered is nameless — the orphans this
// change exists to sweep up included. A name match that refused to touch them
// would leave exactly those behind for ever.
func TestANamelessHookThisEngineLeftIsAdopted(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		legacyHook(9, "https://old-tunnel.example.com/webhooks/gitlab"),
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 1 {
		t.Fatalf("hooks = %+v, want the nameless one re-pointed rather than a second", f.hooks)
	}
	if len(f.updatedHooks) != 1 || !strings.HasSuffix(f.updatedHooks[0], "/hooks/9") {
		t.Fatalf("updated %v, want hook 9 adopted", f.updatedHooks)
	}
	if got := f.hooks[0].attrs["name"]; got != gitlab.DefaultWebhookName {
		t.Errorf("the adopted hook is named %v, want %q — it has to be findable "+
			"by the next pass", got, gitlab.DefaultWebhookName)
	}
}

// THE NAME IS ONE VALUE AND TWO PACKAGES READ IT.
//
// config is the leaf every vendor package depends on, so it cannot import
// this one and restates the default instead. Two spellings of it would make
// a company that set no name have its hooks registered under one and swept
// under the other — which is every hook orphaned on the first pass.
func TestConfigAndGitLabAgreeAboutTheDefaultWebhookName(t *testing.T) {
	t.Parallel()
	if got := (&config.GitLab{}).WebhookNameOrDefault(); got != gitlab.DefaultWebhookName {
		t.Errorf("config says %q, gitlab says %q", got, gitlab.DefaultWebhookName)
	}
}

// A MOVED PUBLIC BASE RE-POINTS THE HOOK, and leaves exactly one.
//
// The end-to-end shape of the reported failure, run against the hook this
// engine's own pass registered rather than a fixture's idea of one: the first
// pass creates it, the base moves, and the second pass has to find the hook
// it made — which nothing about the address can tell it any more.
func TestAMovedPublicBaseRepointsTheHookRatherThanAddingOne(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) { o.WebhookBase = "https://first.example.com" }); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	f.forget()
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) { o.WebhookBase = "https://second.example.com" }); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 1 {
		t.Fatalf("hooks = %+v, want the first one re-pointed rather than a second", f.hooks)
	}
	if got := f.hooks[0].attrs["url"]; got != "https://second.example.com/webhooks/gitlab" {
		t.Errorf("the hook points at %v, want the address now in force — a "+
			"converged check that cannot see the address leaves it where it was", got)
	}
	if len(f.updatedHooks) != 1 {
		t.Errorf("updated %v, want the existing hook written once", f.updatedHooks)
	}
}

// AND THE PROJECT HOOKS GO WHEN THE COMPANY MOVES UP TO A GROUP HOOK.
//
// The other direction of the same defect, and this one was written into the
// documentation as permanent: "the reconcile does not remove the old
// per-project hooks — you would get double delivery until you delete them.
// Deleting a redundant project hook is a manual step." A group hook fires for
// every event in every project of the group, so each of those hooks delivers
// a second copy of everything, and this engine registered both.
func TestTheProjectHooksGoWhenTheCompanyMovesUpToAGroupHook(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {namedHook(1, "crewlet", "https://crewlet.example.com/webhooks/gitlab")},
	}
	res, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Hooked != "https://crewlet.example.com/webhooks/gitlab" {
		t.Fatalf("Hooked = %q, want the group hook registered", res.Hooked)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 1 {
		t.Fatalf("group hooks = %+v, want one", f.hooks)
	}
	if len(f.projectHooks["nimbus/api"]) != 0 {
		t.Errorf("project hooks = %+v, want the redundant one removed — a group "+
			"hook already delivers every event it does",
			f.projectHooks["nimbus/api"])
	}
}

// AND ONLY THIS ENGINE'S. A project carries hooks other integrations
// registered, and the sweep that removes a redundant registration must not
// take one of those with it.
func TestTheProjectSweepLeavesSomebodyElsesHookAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {foreignHook(1, "https://someone-else.example.com/hook")},
	}
	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.projectHooks["nimbus/api"]) != 1 {
		t.Errorf("project hooks = %+v, want somebody else's left alone",
			f.projectHooks["nimbus/api"])
	}
}

// THE HOOK LISTINGS ARE PAGED, AND THIS BRANCH MADE THEM DECIDE DELETIONS.
//
// Unpaged they returned GitLab's default first page of twenty. Every hook
// choice is made out of that listing — which hook is this deployment's, which
// extras to delete, converged-or-create — so on a project already carrying
// twenty hooks from CI, chat and scanners, this engine's own sat past the
// boundary, was invisible, and every pass registered another one. Exactly the
// duplicate-hook failure the name matching exists to stop, reached by the same
// road the token listing's own bug took.
func TestTheProjectHookListingIsPagedToExhaustion(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	// MORE THAN ONE PAGE OF STRANGERS' HOOKS, then this deployment's own
	// at the end — which is where a create puts it, and where a caller
	// that reads one page cannot see it. The count has to exceed a FULL
	// page (userPageSize, 100) rather than GitLab's unasked-for default of
	// 20: a fixture of 26 is served whole to a caller asking for 100, so
	// it cannot tell a walk that exhausts the listing from one that stops.
	rows := make([]hookRow, 0, hookPageProbe+1)
	for i := 1; i <= hookPageProbe; i++ {
		rows = append(rows, foreignHook(i, fmt.Sprintf("https://ci-%d.example.com/hook", i)))
	}
	rows = append(rows, namedHook(hookPageProbe+1, "crewlet",
		"https://crewlet.example.com/webhooks/gitlab"))
	f.projectHooks = map[string][]hookRow{"nimbus/api": rows}

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, want := len(f.projectHooks["nimbus/api"]), hookPageProbe+1; got != want {
		t.Errorf("%d hooks on nimbus/api, want the %d it started with: a "+
			"listing that stops short cannot see this engine's own hook "+
			"and registers a second", got, want)
	}
}

// AND THE GROUP LISTING TOO, on the same terms.
func TestTheGroupHookListingIsPagedToExhaustion(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	for i := 1; i <= hookPageProbe; i++ {
		f.hooks = append(f.hooks,
			foreignHook(i, fmt.Sprintf("https://ci-%d.example.com/hook", i)))
	}
	f.hooks = append(f.hooks, namedHook(hookPageProbe+1, "crewlet",
		"https://crewlet.example.com/webhooks/gitlab"))

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, want := len(f.hooks), hookPageProbe+1; got != want {
		t.Errorf("%d group hooks, want the %d it started with", got, want)
	}
}

// hookPageProbe is how many strangers' hooks a paging case seeds before this
// deployment's own, and it is deliberately larger than one full page of the
// size the client asks for. A smaller fixture is served whole by the instance
// and proves nothing about whether the caller asked for page two.
const hookPageProbe = 120

// A SECRET ROTATED OUTSIDE A RECONCILE RUN REACHES THE HOOK.
//
// GitLab never returns a hook's signing token and answers exactly one question
// about it — whether one is set — so "does this hook hold the key the fleet
// currently holds" was unanswerable. A secret changed any other way (`crewlet
// secrets set`, the signing-secret field on the setup form, a peer's apply)
// therefore never reached the hook: the pass found it converged, wrote
// nothing, and GitLab went on signing with the previous key while the engine
// verified with the new one and refused every delivery — on a surface
// reporting ready, permanently, because every later pass reached the same
// conclusion.
func TestASecretRotatedOutsideARunIsWrittenToTheHook(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()

	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}, nil); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	f.forget()

	// THE VALUE CHANGES UNDERNEATH, with nothing else about the world
	// different: same address, same name, same events, and GitLab still
	// reporting a signing token present.
	const rotated = "whsec_Y3Jld2xldC1yb3RhdGVkLXNpZ25pbmcta2V5LTMyYnk="
	if _, err := reconcileWith(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"},
		func(o *gitlab.Options) { o.SigningSecret = rotated }); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updatedHooks) != 1 {
		t.Fatalf("updated %v, want the hook re-keyed once", f.updatedHooks)
	}
	if len(f.hookBodies) != 1 || f.hookBodies[0]["signing_token"] != rotated {
		t.Errorf("the write carried %v, want the rotated key", f.hookBodies)
	}
}

// AND A PASS OVER AN UNCHANGED SECRET STILL WRITES NOTHING, which is the
// clause the whole settled cadence is anchored on.
func TestAnUnchangedSecretLeavesTheHookAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	f.forget()

	if _, err := reconcileAgainst(t, f, newRecordingSink(),
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 0 {
		t.Errorf("a converged pass wrote %v", f.hookBodies)
	}
}

// AND THE DIGEST IS NEVER THE KEY. It is published in a field GitLab shows to
// anyone who can read the hook's settings, so it has to be one-way and short:
// the secret is 32 random bytes, so 48 bits is far more than enough to notice
// a change and useless for recovering anything.
func TestTheHookDigestIsNotTheKey(t *testing.T) {
	t.Parallel()
	digest := gitlab.HookDigest(testSigningSecret)
	if digest == "" {
		t.Fatal("a real secret digested to nothing")
	}
	if strings.Contains(digest, testSigningSecret) ||
		strings.Contains(testSigningSecret, strings.TrimPrefix(digest, "crewlet:")) {
		t.Fatalf("the digest %q carries the key", digest)
	}
	if gitlab.HookDigest(testSigningSecret+"x") == digest {
		t.Error("two different keys digest the same, so a rotation is invisible")
	}
	// AN ABSENT KEY DIGESTS TO NOTHING rather than to the hash of the empty
	// string: a hook whose key this run does not know must not read as
	// carrying it.
	if got := gitlab.HookDigest("  "); got != "" {
		t.Errorf("an empty secret digested to %q", got)
	}
}

// A SECOND PASS DOES NOT MINT OVER A SIGNING SECRET THIS DEPLOYMENT SEALED.
//
// `opts.SigningSecret` comes from the resolver, and the resolver answers from
// a SNAPSHOT taken at apply time — so the variable a previous pass minted
// into resolves to nothing until something rebuilds it. Every pass in that
// window used to mint a SECOND key, seal it over the first and re-point the
// hook at it, and the engine's own /webhooks/gitlab route verifies with the
// snapshot: each rotation moved the instance further from the value the
// running process holds, on the reconcile loop's timer. The seat tokens in
// this same file have asked the sink first since they hit it; the signing
// secret never did.
func TestASecondPassDoesNotMintOverASigningSecretThisDeploymentSealed(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	sink := newRecordingSink()
	// THE RESOLVER NEVER GAINS THE VALUE, which is the state a node is in
	// for as long as the snapshot lags the seal.
	blind := func(o *gitlab.Options) {
		o.SigningSecret = ""
		o.SigningSecretVar = "GITLAB_SIGNING_SECRET"
	}

	if _, err := reconcileWith(t, f, sink,
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}, blind); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	minted := sink.value("GITLAB_SIGNING_SECRET")
	if minted == "" {
		t.Fatal("the premise is wrong: the first pass sealed no signing secret")
	}
	f.forget()

	res, err := reconcileWith(t, f, sink,
		map[string]string{"swe": "GITLAB_TOKEN_SWE"}, blind)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := sink.value("GITLAB_SIGNING_SECRET"); got != minted {
		t.Error("the signing secret was rotated by a pass nobody asked to " +
			"rotate anything, so the instance now signs with a key this " +
			"deployment does not hold and every delivery is refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hookBodies) != 0 {
		t.Errorf("the second pass rewrote the hook with %v", f.hookBodies)
	}
	for _, note := range res.Notes {
		if strings.Contains(note, "signing secret") {
			t.Errorf("a pass that minted nothing reported %q", note)
		}
	}
}
