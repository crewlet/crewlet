package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessOrg is a Datadog organization the access vendor is run against: its
// roles, its users with their roles and state, and each account's keys. It
// remembers what it was told, so a case asserts what a step DID.
type accessOrg struct {
	mu     sync.Mutex
	roles  map[string]string // id -> name
	users  map[string]*orgUser
	keys   map[string][]orgKey // user id -> keys
	writes []string
	// refuse answers a route ("METHOD /path") with a status and message.
	refuse map[string]refusal
	next   int
}

type orgUser struct {
	id, email, name, title string
	disabled, person       bool
	roles                  []string
}

type orgKey struct{ id, name, value string }

type refusal struct {
	status int
	said   string
}

const (
	roRole    = "role-ro"
	stdRole   = "role-std"
	adminRole = "role-admin"
	custRole  = "role-incident"
)

func newAccessOrg() *accessOrg {
	return &accessOrg{
		roles: map[string]string{
			roRole: datadog.RoleReadOnly, stdRole: datadog.RoleStandard,
			adminRole: datadog.RoleAdmin, custRole: "Incident Responders",
		},
		users: map[string]*orgUser{}, keys: map[string][]orgKey{},
		refuse: map[string]refusal{},
	}
}

// seat adds an account this engine made for handle, holding roles.
func (o *accessOrg) seat(handle string, roles ...string) *orgUser {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.addUser("crewlet-"+handle+"@agents.crewlet.invalid", false, roles)
}

func (o *accessOrg) addUser(email string, person bool, roles []string) *orgUser {
	o.next++
	user := &orgUser{id: "u-" + strconv.Itoa(o.next), email: email, person: person, roles: roles}
	o.users[user.id] = user
	return user
}

func (o *accessOrg) key(userID, name string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	id := "k-" + strconv.Itoa(o.next)
	o.keys[userID] = append(o.keys[userID], orgKey{id: id, name: name, value: "fake-" + id})
	return id
}

func (o *accessOrg) keyIDs(userID string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, key := range o.keys[userID] {
		out = append(out, key.id)
	}
	return out
}

func (o *accessOrg) user(id string) orgUser {
	o.mu.Lock()
	defer o.mu.Unlock()
	return *o.users[id]
}

func (o *accessOrg) wrote() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.writes)
}

func (o *accessOrg) userRow(u *orgUser) map[string]any {
	roles := make([]any, 0, len(u.roles))
	for _, id := range u.roles {
		roles = append(roles, map[string]any{"id": id, "type": "roles"})
	}
	return map[string]any{
		"id": u.id,
		"attributes": map[string]any{
			"email": u.email, "name": u.name, "title": u.title,
			"disabled": u.disabled, "service_account": !u.person,
		},
		"relationships": map[string]any{"roles": map[string]any{"data": roles}},
	}
}

func (o *accessOrg) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	route := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		o.writes = append(o.writes, route)
	}
	if refused, ok := o.refuse[route]; ok {
		w.WriteHeader(refused.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{refused.said}})
		return
	}
	write := func(body any) { _ = json.NewEncoder(w).Encode(body) }
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"errors": []string{"Not found"}})
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/api/v2/roles":
		rows := []any{}
		for id, name := range o.roles {
			rows = append(rows, map[string]any{"id": id, "attributes": map[string]any{"name": name}})
		}
		write(map[string]any{"data": rows})
	case len(parts) == 4 && parts[2] == "roles" && r.Method == http.MethodGet:
		name, ok := o.roles[parts[3]]
		if !ok {
			notFound()
			return
		}
		write(map[string]any{"data": map[string]any{"id": parts[3],
			"attributes": map[string]any{"name": name}}})
	case len(parts) == 5 && parts[2] == "roles" && parts[4] == "users":
		var body struct {
			Data struct{ ID string } `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		user, ok := o.users[body.Data.ID]
		if _, role := o.roles[parts[3]]; !ok || !role {
			notFound()
			return
		}
		if r.Method == http.MethodPost && !slices.Contains(user.roles, parts[3]) {
			user.roles = append(user.roles, parts[3])
		}
		if r.Method == http.MethodDelete {
			user.roles = slices.DeleteFunc(user.roles, func(id string) bool { return id == parts[3] })
		}
		write(map[string]any{"data": []any{}})
	case r.URL.Path == "/api/v2/users" && r.Method == http.MethodGet:
		rows := []any{}
		for _, user := range o.users {
			if strings.Contains(user.email, r.URL.Query().Get("filter")) {
				rows = append(rows, o.userRow(user))
			}
		}
		write(map[string]any{"data": rows})
	case len(parts) == 4 && parts[2] == "users":
		user, ok := o.users[parts[3]]
		if !ok {
			notFound()
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(map[string]any{"data": o.userRow(user)})
		case http.MethodDelete:
			user.disabled = true
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPatch:
			var body struct {
				Data struct {
					Attributes map[string]any `json:"attributes"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if title, ok := body.Data.Attributes["title"].(string); ok {
				user.title = title
			}
			if disabled, ok := body.Data.Attributes["disabled"].(bool); ok {
				user.disabled = disabled
			}
			write(map[string]any{"data": o.userRow(user)})
		}
	case r.URL.Path == "/api/v2/service_accounts" && r.Method == http.MethodPost:
		var body struct {
			Data struct {
				Attributes    struct{ Email, Name string } `json:"attributes"`
				Relationships struct {
					Roles struct {
						Data []struct{ ID string } `json:"data"`
					} `json:"roles"`
				} `json:"relationships"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var roles []string
		for _, role := range body.Data.Relationships.Roles.Data {
			roles = append(roles, role.ID)
		}
		user := o.addUser(body.Data.Attributes.Email, false, roles)
		user.name = body.Data.Attributes.Name
		write(map[string]any{"data": o.userRow(user)})
	case len(parts) == 5 && parts[2] == "service_accounts" && parts[4] == "application_keys":
		if r.Method == http.MethodGet {
			rows := []any{}
			for _, key := range o.keys[parts[3]] {
				rows = append(rows, map[string]any{"id": key.id,
					"attributes": map[string]any{"name": key.name}})
			}
			write(map[string]any{"data": rows})
			return
		}
		var body struct {
			Data struct {
				Attributes struct{ Name string } `json:"attributes"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		o.next++
		key := orgKey{id: "k-" + strconv.Itoa(o.next), name: body.Data.Attributes.Name,
			value: "fake-minted-" + strconv.Itoa(o.next)}
		o.keys[parts[3]] = append(o.keys[parts[3]], key)
		write(map[string]any{"data": map[string]any{"id": key.id,
			"attributes": map[string]any{"name": key.name, "key": key.value}}})
	case len(parts) == 6 && parts[2] == "service_accounts" && r.Method == http.MethodDelete:
		held := o.keys[parts[3]]
		o.keys[parts[3]] = slices.DeleteFunc(held, func(k orgKey) bool { return k.id == parts[5] })
		w.WriteHeader(http.StatusNoContent)
	default:
		notFound()
	}
}

// open is a session against the fake through the registered vendor, which is
// also the proof the package registered one.
func (o *accessOrg) open(t *testing.T) seataccess.Session {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(server.Close)
	vendor, ok := seataccess.Lookup(integration.KindDatadog)
	if !ok {
		t.Fatal("the datadog package registered no access vendor")
	}
	company := &config.Company{}
	company.Integrations.Datadog = &config.Datadog{
		Enabled: true,
		Provisioning: &config.DatadogProvisioning{
			Site: "datadoghq.com", APIKey: "${DD_API}", AppKey: "${DD_APP}",
		},
	}
	session, err := datadog.OpenAccess(context.Background(), vendor, seataccess.OpenInput{
		Config: company, Env: config.NewResolver(config.MapSource(map[string]string{
			"DD_API": "fake-api", "DD_APP": "fake-app",
		})),
	}, &http.Client{Transport: httpxtest.Rewrite(t, server)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return session
}

func sre() seataccess.SeatRef { return seataccess.SeatRef{Handle: "sre", Name: "SRE"} }

// step is the vendor's own step for op and verb, as the shared plan would
// hand it to Apply.
func step(t *testing.T, op seataccess.Op, verb seataccess.StepVerb) seataccess.Step {
	t.Helper()
	vendor, _ := seataccess.Lookup(integration.KindDatadog)
	for _, s := range vendor.Steps(sre(), seataccess.LaneDatadog, op,
		seataccess.Desired{}, seataccess.Observed{}) {
		if s.Verb == verb {
			return s
		}
	}
	t.Fatalf("no %s step for %s", verb, op)
	return seataccess.Step{}
}

func held(id string) *seataccess.Record {
	return &seataccess.Record{
		Kind: integration.KindDatadog, Handle: "sre",
		Account: &seataccess.Account{ID: id, State: seataccess.AccountActive},
	}
}

// THE CATALOG SEALS ONE CREDENTIAL and offers live roles with no scope, and
// the engine's binding table puts that credential on Datadog's MCP server
// under the header the server reads (D15).
func TestTheDatadogCatalog(t *testing.T) {
	t.Parallel()
	vendor, ok := seataccess.Lookup(integration.KindDatadog)
	if !ok {
		t.Fatal("no datadog vendor registered")
	}
	cat := vendor.Catalog()
	if !slices.Equal(cat.Credentials, []string{datadog.AppKeyField}) {
		t.Errorf("credentials = %q", cat.Credentials)
	}
	lane, ok := cat.Lane(seataccess.LaneDatadog)
	if !ok || !lane.Levels || !lane.LiveLevels || lane.Scoped || !lane.Rotatable {
		t.Errorf("lane = %+v", lane)
	}
	tool := seataccess.ToolFor(seataccess.LaneDatadog)
	if tool == nil || tool.Server != "datadog" || len(tool.Keys) != 1 ||
		tool.Keys[0].Field != datadog.AppKeyField || tool.Keys[0].Header != "DD_APPLICATION_KEY" {
		t.Fatalf("tool = %+v, want the key under DD_APPLICATION_KEY on the datadog server", tool)
	}
	// THE HEADER THE TOOL READS IS A RECOGNISED CREDENTIAL KEY, so the
	// document rules refuse it in a seat's mcp_env and the migration finds it.
	if !slices.Contains(datadog.CredentialKeys, tool.Keys[0].Header) {
		t.Errorf("CredentialKeys %q lacks the tool's header %q", datadog.CredentialKeys, tool.Keys[0].Header)
	}
	if got := seataccess.SinkName(integration.KindDatadog, datadog.AppKeyField, "sre"); got !=
		"DATADOG_APP_KEY_SRE" {
		t.Errorf("sink = %q", got)
	}
}

// EVERY OP IS DRAWN IN DATADOG'S STEPS, in the order Apply takes them, and
// Datadog has no scope to move.
func TestDatadogStepsPerOp(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindDatadog)
	for op, want := range map[seataccess.Op][]seataccess.StepVerb{
		seataccess.OpCreate:  {seataccess.VerbCreate, seataccess.VerbMint},
		seataccess.OpEnable:  {seataccess.VerbEnable, seataccess.VerbMint},
		seataccess.OpLevel:   {seataccess.VerbRerole},
		seataccess.OpMint:    {seataccess.VerbMint},
		seataccess.OpDisable: {seataccess.VerbDisable},
		seataccess.OpRevoke:  {seataccess.VerbRevoke},
		seataccess.OpRemove:  {seataccess.VerbRemove},
		seataccess.OpRotate:  {seataccess.VerbMint, seataccess.VerbRevoke},
		seataccess.OpScope:   nil,
	} {
		steps := vendor.Steps(sre(), seataccess.LaneDatadog, op, seataccess.Desired{}, seataccess.Observed{})
		var got []seataccess.StepVerb
		for i, s := range steps {
			got = append(got, s.Verb)
			if s.Op != op || s.Index != i || s.By != seataccess.ByCrewlet || s.Text == "" {
				t.Errorf("%s step %d = %+v", op, i, s)
			}
			if strings.Contains(s.Text, "—") || strings.Contains(s.Note, "—") {
				t.Errorf("%s step %d carries an em dash", op, i)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s verbs = %q, want %q", op, got, want)
		}
	}
	mint := step(t, seataccess.OpMint, seataccess.VerbMint)
	if !strings.Contains(mint.Text, "DATADOG_APP_KEY_SRE") {
		t.Errorf("the mint does not name the engine-owned variable: %q", mint.Text)
	}
}

// THE PICKER OFFERS EVERY ROLE, managed first in Datadog's order and custom
// after, with only Datadog Admin Role marked as administration (D7, D10).
func TestDatadogOptionsAreTheOrganizationsRoles(t *testing.T) {
	t.Parallel()
	session := newAccessOrg().open(t)
	opts, err := session.Options(context.Background(), seataccess.LaneDatadog)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	var ids []string
	for _, level := range opts.Levels {
		ids = append(ids, level.ID)
		if level.Administers != (level.ID == adminRole) {
			t.Errorf("%s administers = %v", level.Name, level.Administers)
		}
		if level.Custom != (level.ID == custRole) {
			t.Errorf("%s custom = %v", level.Name, level.Custom)
		}
	}
	if !slices.Equal(ids, []string{roRole, stdRole, adminRole, custRole}) {
		t.Errorf("levels = %q", ids)
	}
	if !opts.Live || len(opts.Scope) != 0 {
		t.Errorf("options = %+v, want live roles and no scope", opts)
	}
	if !seataccess.NeedsStepUp(opts, seataccess.Desired{Enabled: true, Level: roRole},
		seataccess.Desired{Enabled: true, Level: adminRole}) {
		t.Error("a raise to Datadog Admin Role does not ask for a step-up")
	}
}

// EVERY PASS READS EACH ACCOUNT BACK: a seat the ledger records by id, a seat
// adopted by the address this engine derives, a seat with no account, and a
// seat that left the document; its roles, its state and who turned it off,
// and whether it holds a key.
func TestDatadogObserveReadsEveryAccountBack(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	recorded := org.seat("sre", adminRole)
	org.key(recorded.id, "crewlet")
	adopted := org.seat("oncall", roRole, stdRole)
	byHand := org.seat("dba", roRole)
	byHand.disabled = true
	marked := org.seat("analyst", roRole)
	marked.disabled, marked.title = true, datadog.DisconnectedTitle
	departed := org.seat("old", roRole)
	departed.disabled = true
	org.addUser("crewlet-person@agents.crewlet.invalid", true, nil)

	obs, err := org.open(t).Observe(context.Background(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{{Handle: "sre"}, {Handle: "oncall"}, {Handle: "dba"},
			{Handle: "analyst"}, {Handle: "person"}, {Handle: "fresh"}},
		Records: map[string]seataccess.Record{
			"sre": *held(recorded.id),
			"old": {Handle: "old", Account: &seataccess.Account{ID: departed.id,
				State: seataccess.AccountDisabled}},
		},
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	lane := func(handle string) seataccess.Observed { return obs[handle][seataccess.LaneDatadog] }
	if got := lane("sre"); !got.Exists || !got.Enabled || got.Level != adminRole ||
		got.AccountID != recorded.id {
		t.Errorf("sre = %+v", got)
	}
	if got := lane("oncall"); got.AccountID != adopted.id || got.Level != roRole+" "+stdRole {
		t.Errorf("oncall = %+v, want adopted, holding both roles", got)
	}
	if got := lane("dba"); got.Enabled || got.DisabledBy != seataccess.DisabledByHand {
		t.Errorf("dba = %+v, want disabled by hand", got)
	}
	if got := lane("analyst"); got.DisabledBy != seataccess.DisabledByCrewlet {
		t.Errorf("analyst = %+v, want this engine's off by its marker", got)
	}
	if got := lane("old"); !got.Exists || got.DisabledBy != seataccess.DisabledByCrewlet {
		t.Errorf("old = %+v, want the departed seat observed, off by this engine", got)
	}
	if got := lane("person"); got.Exists {
		t.Errorf("a person's account was adopted: %+v", got)
	}
	if got := lane("fresh"); got.Exists {
		t.Errorf("fresh = %+v, want no account", got)
	}
}

// apply runs one step through a session.
func apply(t *testing.T, session seataccess.Session, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	t.Helper()
	if in.Seat.Handle == "" {
		in.Seat = sre()
	}
	return session.Apply(context.Background(), in)
}

// A CREATE MAKES THE ACCOUNT ALREADY HOLDING ITS ROLE, and the mint after it
// hands the sync a key whose Revoke takes it back: the rule that a key minted
// and not sealed is revoked (design 5.2) holds through the vendor's own call.
func TestDatadogCreateThenMint(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	session := org.open(t)
	want := seataccess.Desired{Enabled: true, Level: stdRole}

	created, err := apply(t, session, seataccess.ApplyInput{
		Step: step(t, seataccess.OpCreate, seataccess.VerbCreate), Want: want,
	})
	if err != nil || created.AccountID == "" ||
		created.Account != "crewlet-sre@agents.crewlet.invalid" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if got := org.user(created.AccountID).roles; !slices.Equal(got, []string{stdRole}) {
		t.Errorf("created holding %q, want the seat's role", got)
	}
	// THE SYNC RECORDS THE ACCOUNT AS SOON AS THE CREATE RETURNS, and the
	// mint reaches it through that record.
	minted, err := apply(t, session, seataccess.ApplyInput{
		Step: step(t, seataccess.OpCreate, seataccess.VerbMint), Want: want,
		Record: held(created.AccountID),
	})
	if err != nil || len(minted.Minted) != 1 || minted.Minted[0].Field != datadog.AppKeyField ||
		!strings.HasPrefix(minted.Minted[0].Value, "fake-minted-") {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	if len(org.keyIDs(created.AccountID)) != 1 {
		t.Fatal("no key on the account")
	}
	if err := minted.Minted[0].Revoke(context.Background()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if keys := org.keyIDs(created.AccountID); len(keys) != 0 {
		t.Errorf("the unsealed key is still live: %q", keys)
	}
}

// A ROLE DATADOG NO LONGER HAS IS A FINDING FOR A PERSON, never a fallback to
// another role (D7): nothing is created.
func TestDatadogCreateRefusesADeletedRole(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	_, err := apply(t, org.open(t), seataccess.ApplyInput{
		Step: step(t, seataccess.OpCreate, seataccess.VerbCreate),
		Want: seataccess.Desired{Enabled: true, Level: "role-deleted"},
	})
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || refusal.Actor != seataccess.ByYou ||
		!strings.Contains(refusal.Said, "role-deleted") || refusal.Remedy == "" ||
		!strings.HasSuffix(refusal.ActionURL, "/organization-settings/roles") {
		t.Fatalf("err = %#v, want a refusal naming the role, the remedy and where", err)
	}
	if writes := org.wrote(); len(writes) != 0 {
		t.Errorf("writes = %q, want none", writes)
	}
}

// A LEVEL CHANGE AND A DRIFT RESTORE ARE ONE STEP: the account ends holding
// exactly the seat's role, added before anything is taken away.
func TestDatadogLevelHoldsExactlyTheSeatsRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		held   []string
		want   string
		writes []string
	}{
		{"a lowering", []string{adminRole}, roRole, []string{
			"POST /api/v2/roles/" + roRole + "/users", "DELETE /api/v2/roles/" + adminRole + "/users",
		}},
		{"drift: a role added by hand", []string{roRole, adminRole}, roRole, []string{
			"DELETE /api/v2/roles/" + adminRole + "/users",
		}},
		{"a raise to a custom role", []string{roRole}, custRole, []string{
			"POST /api/v2/roles/" + custRole + "/users", "DELETE /api/v2/roles/" + roRole + "/users",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			org := newAccessOrg()
			account := org.seat("sre", tc.held...)
			_, err := apply(t, org.open(t), seataccess.ApplyInput{
				Step: step(t, seataccess.OpLevel, seataccess.VerbRerole),
				Want: seataccess.Desired{Enabled: true, Level: tc.want}, Record: held(account.id),
			})
			if err != nil {
				t.Fatalf("level: %v", err)
			}
			if got := org.user(account.id).roles; !slices.Equal(got, []string{tc.want}) {
				t.Errorf("roles = %q, want exactly %q", got, tc.want)
			}
			if got := org.wrote(); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %q, want %q", got, tc.writes)
			}
		})
	}
}

// OFF MARKS AND DISABLES THE ACCOUNT, which Datadog keeps (D1), and the revoke
// that follows deletes every key and hands the sync the field to clear. Back
// on enables it, clears the marker and mints a fresh key.
func TestDatadogOffAndBackOn(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	account := org.seat("sre", roRole)
	org.key(account.id, "crewlet")
	org.key(account.id, "added by hand")
	session := org.open(t)
	in := seataccess.ApplyInput{Want: seataccess.Desired{Level: roRole}, Record: held(account.id)}

	in.Step = step(t, seataccess.OpDisable, seataccess.VerbDisable)
	disabled, err := apply(t, session, in)
	if err != nil || !disabled.Disabled {
		t.Fatalf("disable = %+v, %v", disabled, err)
	}
	if got := org.user(account.id); !got.disabled || got.title != datadog.DisconnectedTitle {
		t.Errorf("account = %+v, want disabled and marked as this engine's off", got)
	}
	in.Step = step(t, seataccess.OpRevoke, seataccess.VerbRevoke)
	revoked, err := apply(t, session, in)
	if err != nil || !slices.Equal(revoked.Revoked, []string{datadog.AppKeyField}) {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if keys := org.keyIDs(account.id); len(keys) != 0 {
		t.Errorf("keys left live on an account turned off: %q", keys)
	}

	in.Want.Enabled = true
	in.Step = step(t, seataccess.OpEnable, seataccess.VerbEnable)
	if _, err := apply(t, session, in); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := org.user(account.id); got.disabled || got.title != "" {
		t.Errorf("account = %+v, want enabled with the marker cleared", got)
	}
	in.Step = step(t, seataccess.OpEnable, seataccess.VerbMint)
	minted, err := apply(t, session, in)
	if err != nil || len(minted.Minted) != 1 {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
}

// REMOVE DELETES NOTHING AT DATADOG, which keeps every user: an account
// already off is left as it is, and one somehow still live is turned off
// first so a remove can never leave a live account unmanaged.
func TestDatadogRemoveKeepsTheAccountDisabled(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	off := org.seat("sre", roRole)
	off.disabled, off.title = true, datadog.DisconnectedTitle
	session := org.open(t)
	remove := step(t, seataccess.OpRemove, seataccess.VerbRemove)
	if _, err := apply(t, session, seataccess.ApplyInput{Step: remove, Record: held(off.id)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if writes := org.wrote(); len(writes) != 0 {
		t.Errorf("writes = %q, want none for an account already off", writes)
	}
	live := org.seat("other", roRole)
	org.key(live.id, "crewlet")
	res, err := apply(t, session, seataccess.ApplyInput{Step: remove, Record: held(live.id)})
	if err != nil || !res.Disabled || len(org.keyIDs(live.id)) != 0 || !org.user(live.id).disabled {
		t.Errorf("remove of a live account = %+v, %v; want it turned off", res, err)
	}
}

// A ROTATION MINTS, THEN RETIRES EVERY OLDER KEY THIS ENGINE MINTED AND NEVER
// THE NEW ONE, sparing a key a person put on the account.
func TestDatadogRotate(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	account := org.seat("sre", roRole)
	old := org.key(account.id, "crewlet")
	hand := org.key(account.id, "ops")
	session := org.open(t)
	in := seataccess.ApplyInput{Record: held(account.id)}

	in.Step = step(t, seataccess.OpRotate, seataccess.VerbMint)
	minted, err := apply(t, session, in)
	if err != nil || len(minted.Minted) != 1 {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	in.Step = step(t, seataccess.OpRotate, seataccess.VerbRevoke)
	if _, err := apply(t, session, in); err != nil {
		t.Fatalf("retire: %v", err)
	}
	keys := org.keyIDs(account.id)
	if slices.Contains(keys, old) || !slices.Contains(keys, hand) || len(keys) != 2 {
		t.Errorf("keys = %q, want the new key and the person's, without %s", keys, old)
	}
}

// A RETIRE WITH NOTHING MINTED IN THIS SESSION DELETES NOTHING: it cannot tell
// which key is current, and guessing could delete the one the seat uses.
func TestDatadogRetireRefusesWithoutTheNewKey(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	account := org.seat("sre", roRole)
	org.key(account.id, "crewlet")
	org.key(account.id, "crewlet")
	_, err := apply(t, org.open(t), seataccess.ApplyInput{
		Step: step(t, seataccess.OpRotate, seataccess.VerbRevoke), Record: held(account.id),
	})
	if err == nil {
		t.Fatal("a retire with no minted key was taken")
	}
	if len(org.keyIDs(account.id)) != 2 {
		t.Error("a key was deleted on a guess")
	}
}

// A MINT FOR A SEAT WITH NOTHING SEALED REPLACES THIS ENGINE'S EARLIER KEYS,
// whose values nobody holds, and leaves a person's key alone.
func TestDatadogMintReplacesUnheldKeys(t *testing.T) {
	t.Parallel()
	org := newAccessOrg()
	account := org.seat("sre", roRole)
	stale := org.key(account.id, "crewlet")
	hand := org.key(account.id, "ops")
	_, err := apply(t, org.open(t), seataccess.ApplyInput{
		Step: step(t, seataccess.OpMint, seataccess.VerbMint), Record: held(account.id),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	keys := org.keyIDs(account.id)
	if slices.Contains(keys, stale) || !slices.Contains(keys, hand) || len(keys) != 2 {
		t.Errorf("keys = %q", keys)
	}
}

// A REFUSAL NAMES THE VENDOR'S REASON, THE REMEDY AND WHO ACTS; a rate limit
// or a server error is a fault the sync retries, never a refusal sent to a
// person; and an account gone at Datadog is a ledger record to close.
func TestDatadogRefusalsFaultsAndGone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		refusal bool
		remedy  string
	}{
		{"forbidden", http.StatusForbidden, true, "User Access Manage"},
		{"unauthorized", http.StatusUnauthorized, true, "Replace the organization's"},
		{"rate limited", http.StatusTooManyRequests, false, ""},
		{"server error", http.StatusInternalServerError, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			org := newAccessOrg()
			account := org.seat("sre", roRole)
			org.refuse["POST /api/v2/roles/"+stdRole+"/users"] = refusal{tc.status, "the vendor's reason"}
			_, err := apply(t, org.open(t), seataccess.ApplyInput{
				Step: step(t, seataccess.OpLevel, seataccess.VerbRerole),
				Want: seataccess.Desired{Enabled: true, Level: stdRole}, Record: held(account.id),
			})
			var refused *seataccess.Refusal
			if errors.As(err, &refused) != tc.refusal {
				t.Fatalf("err = %#v, want refusal=%v", err, tc.refusal)
			}
			if tc.refusal && (!strings.Contains(refused.Said, "the vendor's reason") ||
				!strings.Contains(refused.Remedy, tc.remedy) || refused.Actor != seataccess.ByYou ||
				!strings.HasSuffix(refused.ActionURL, "/organization-settings/users")) {
				t.Errorf("refusal = %+v", refused)
			}
			if got := org.user(account.id).roles; !slices.Equal(got, []string{roRole}) {
				t.Errorf("roles = %q, want the old role kept after a refused add", got)
			}
		})
	}
	org := newAccessOrg()
	gone, err := apply(t, org.open(t), seataccess.ApplyInput{
		Step: step(t, seataccess.OpLevel, seataccess.VerbRerole),
		Want: seataccess.Desired{Level: roRole}, Record: held("u-deleted"),
	})
	if err != nil || !gone.Gone {
		t.Errorf("an account deleted at Datadog = %+v, %v; want Gone", gone, err)
	}
}

// OPEN ANSWERS NOT CONFIGURED for a company that does not provision Datadog,
// so the loop forgets the lane rather than reporting a fault.
func TestDatadogOpenNotConfigured(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindDatadog)
	for name, company := range map[string]*config.Company{
		"no block":    {},
		"off":         {Integrations: config.Integrations{Datadog: &config.Datadog{}}},
		"no keys set": {Integrations: config.Integrations{Datadog: &config.Datadog{Enabled: true, Provisioning: &config.DatadogProvisioning{Site: "datadoghq.com"}}}},
	} {
		_, err := vendor.Open(context.Background(), seataccess.OpenInput{
			Config: company, Env: config.NewResolver(),
		})
		if !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: err = %v, want not configured", name, err)
		}
	}
}
