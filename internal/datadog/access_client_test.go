package datadog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
)

// THE PICKER READS THE WHOLE CATALOGUE (D7). A custom role an admin made is as
// much an option as the three Datadog ships, so the read carries no filter and
// walks every page; the answer comes back in one order so the picker and the
// preview render the same list.
func TestTheRoleCatalogueIsReadWholeAndInOrder(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	var filters []string
	reg.handle["/api/v2/roles"] = func(w http.ResponseWriter, r *http.Request) {
		if _, set := r.URL.Query()["filter"]; set {
			filters = append(filters, r.URL.Query().Get("filter"))
		}
		if r.URL.Query().Get("page[number]") == "0" {
			rows := make([]string, 0, 100)
			for i := range 100 {
				rows = append(rows, fmt.Sprintf(
					`{"id":"c-%03d","attributes":{"name":"Custom %03d"}}`, i, i))
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(rows, ",") + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"ro","attributes":{"name":"Datadog Read Only Role"}},
			{"id":"adm","attributes":{"name":"Datadog Admin Role"}}]}`))
	}

	roles, err := reg.client(t).Roles(context.Background(), pair)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(filters) != 0 {
		t.Errorf("the catalogue read sent a filter: %q", filters)
	}
	if len(roles) != 102 {
		t.Fatalf("read %d roles, want every one of 102 across both pages", len(roles))
	}
	if !slices.IsSortedFunc(roles, func(a, b datadog.Role) int {
		return strings.Compare(a.Name, b.Name)
	}) {
		t.Error("the catalogue is not in name order")
	}
}

// A ROLE DELETED AT DATADOG IS AN ANSWER, AND A FAILED READ IS NOT. The first
// becomes a finding on the seat (D7); the second is a blip the next pass asks
// again about. Collapsing them would report a live role as deleted whenever
// Datadog rate-limited the pass.
func TestARoleReadTellsAbsentFromFailed(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/roles/std"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"std","attributes":{"name":"Datadog Standard Role"}}}`))
	}
	reg.handle["/api/v2/roles/limited"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":["rate limited"]}`))
	}
	c := reg.client(t)

	role, found, err := c.Role(context.Background(), pair, "std")
	if err != nil || !found || role.Name != datadog.RoleStandard {
		t.Fatalf("present role: %+v, %v, %v", role, found, err)
	}
	if _, found, err = c.Role(context.Background(), pair, "deleted"); err != nil || found {
		t.Fatalf("deleted role: found=%v err=%v, want an absent answer", found, err)
	}
	if _, found, err = c.Role(context.Background(), pair, "limited"); err == nil || found {
		t.Fatalf("a refused read answered found=%v err=%v, want the error", found, err)
	}
	if datadog.Status(err) != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the refusal's own", datadog.Status(err))
	}
	if _, _, err := c.Role(context.Background(), pair, " "); err == nil {
		t.Error("an empty role id was sent to Datadog")
	}
}

// A ROLE CHANGE IS DATADOG'S OWN RELATION WRITE, in both directions, carrying
// the user as the relation's data. Removing an account already out of the
// role is success, so a restore retried after a partial failure converges.
func TestRoleMembershipWritesTheRelation(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	type write struct{ method, body string }
	var writes []write
	reg.handle["/api/v2/roles/std/users"] = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writes = append(writes, write{r.Method, string(body)})
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	reg.handle["/api/v2/roles/gone/users"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":["not found"]}`))
	}
	reg.handle["/api/v2/roles/locked/users"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["user_access_manage required"]}`))
	}
	c := reg.client(t)
	ctx := context.Background()

	if err := c.AddRoleMember(ctx, pair, "std", "u-1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := c.RemoveRoleMember(ctx, pair, "std", "u-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(writes) != 2 || writes[0].method != http.MethodPost ||
		writes[1].method != http.MethodDelete {
		t.Fatalf("writes = %+v, want a POST then a DELETE on the relation", writes)
	}
	for _, w := range writes {
		var got struct {
			Data struct{ ID, Type string } `json:"data"`
		}
		if err := json.Unmarshal([]byte(w.body), &got); err != nil {
			t.Fatalf("decode %s body %q: %v", w.method, w.body, err)
		}
		if got.Data.ID != "u-1" || got.Data.Type != "users" {
			t.Errorf("%s carried %+v, want the user as the relation's data", w.method, got.Data)
		}
	}
	if err := c.RemoveRoleMember(ctx, pair, "gone", "u-1"); err != nil {
		t.Errorf("removing from a relation Datadog no longer has: %v, want success", err)
	}
	err := c.AddRoleMember(ctx, pair, "locked", "u-1")
	if datadog.Status(err) != http.StatusForbidden ||
		!strings.Contains(err.Error(), "user_access_manage") {
		t.Errorf("a refused add = %v, want the 403 and Datadog's reason", err)
	}
	if err := c.AddRoleMember(ctx, pair, "", "u-1"); err == nil {
		t.Error("an add with no role was sent")
	}
}

// AN ACCOUNT'S ROLES ARE READ FROM DATADOG ON EVERY PASS, which is what drift
// is measured against. Both reads carry them: the listing a pass already makes
// and the single read the ledger makes for a seat that left the document.
func TestAnAccountsRolesAreReadBack(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	roles := `"relationships":{"roles":{"data":[
		{"id":"std","type":"roles"},{"id":"adm","type":"roles"},
		{"id":"std","type":"roles"},{"id":"x","type":"permissions"}]}}`
	reg.handle["/api/v2/users/u-1"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"u-1","attributes":{
			"email":"crewlet-sre@agents.crewlet.invalid","disabled":true,
			"title":"crewlet:disconnected"},` + roles + `}}`))
	}
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u-1","attributes":{
			"email":"crewlet-sre@agents.crewlet.invalid","service_account":true},` +
			roles + `}]}`))
	}
	c := reg.client(t)
	want := []string{"adm", "std"}

	user, found, err := c.User(context.Background(), pair, "u-1")
	if err != nil || !found {
		t.Fatalf("user: found=%v err=%v", found, err)
	}
	if !slices.Equal(user.RoleIDs, want) || !user.Disabled ||
		user.Title != datadog.DisconnectedTitle {
		t.Errorf("user = %+v, want roles %v, disabled and marked", user, want)
	}
	listed, err := c.ListServiceAccounts(context.Background(), pair, "agents.crewlet.invalid")
	if err != nil || len(listed) != 1 {
		t.Fatalf("listing: %v, %v", listed, err)
	}
	if !slices.Equal(listed[0].RoleIDs, want) {
		t.Errorf("listed roles = %v, want %v", listed[0].RoleIDs, want)
	}
	if _, found, err := c.User(context.Background(), pair, "deleted"); err != nil || found {
		t.Errorf("a deleted account: found=%v err=%v, want an absent answer", found, err)
	}
}

// THE CLASS TABLE. Only Datadog Admin Role is the administration class (D10),
// and only the three Datadog ships are managed; a custom role is neither.
func TestRoleClasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		managed, admin bool
	}{
		{datadog.RoleReadOnly, true, false},
		{datadog.RoleStandard, true, false},
		{" " + datadog.RoleAdmin + " ", true, true},
		{"Incident Responders", false, false},
		{"", false, false},
	} {
		if got := datadog.ManagedRole(tc.name); got != tc.managed {
			t.Errorf("ManagedRole(%q) = %v, want %v", tc.name, got, tc.managed)
		}
		if got := datadog.AdministrationRole(tc.name); got != tc.admin {
			t.Errorf("AdministrationRole(%q) = %v, want %v", tc.name, got, tc.admin)
		}
	}
}
