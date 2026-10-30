package datadog_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
)

// writesOf is the calls a fake region saw that change something, in order.
func writesOf(reg *region) []string {
	var out []string
	for _, call := range reg.calls {
		if !strings.HasPrefix(call, http.MethodGet+" ") {
			out = append(out, call)
		}
	}
	return out
}

func okJSON(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }

// A ROLE CHANGE ADDS BEFORE IT REMOVES, so the account is never without a
// role and never wider than what it held plus what the document asks for. A
// converged account is not written to at all.
func TestSetRoleAddsBeforeItRemoves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		held    []string
		want    string
		writes  []string
		removed []string
	}{
		{
			name: "a lowering",
			held: []string{"adm"}, want: "ro",
			writes:  []string{"POST /api/v2/roles/ro/users", "DELETE /api/v2/roles/adm/users"},
			removed: []string{"adm"},
		},
		{
			name: "a second role added by hand",
			held: []string{"ro", "std"}, want: "ro",
			writes:  []string{"DELETE /api/v2/roles/std/users"},
			removed: []string{"std"},
		},
		{name: "converged", held: []string{"ro"}, want: "ro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newRegion(t)
			for _, id := range []string{"ro", "std", "adm"} {
				reg.handle["/api/v2/roles/"+id+"/users"] = okJSON
			}
			removed, err := reg.client(t).SetRole(context.Background(), pair,
				datadog.Account{ID: "u-1", RoleIDs: tc.held}, tc.want)
			if err != nil {
				t.Fatalf("set role: %v", err)
			}
			if got := writesOf(reg); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %q, want %q", got, tc.writes)
			}
			if !slices.Equal(removed, tc.removed) {
				t.Errorf("removed = %q, want %q", removed, tc.removed)
			}
		})
	}
}

// A REFUSED ADD TOUCHES NOTHING ELSE: the old role stays, so the seat keeps
// working at what it had while the refusal is reported.
func TestARefusedRoleAddRemovesNothing(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/roles/std/users"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}
	reg.handle["/api/v2/roles/ro/users"] = okJSON
	_, err := reg.client(t).SetRole(context.Background(), pair,
		datadog.Account{ID: "u-1", RoleIDs: []string{"ro"}}, "std")
	if datadog.Status(err) != http.StatusForbidden {
		t.Fatalf("err = %v, want the 403", err)
	}
	if got := writesOf(reg); len(got) != 1 {
		t.Errorf("writes = %q, want the refused add alone", got)
	}
}

// OFF REVOKES EVERY KEY, MARKS, THEN DISABLES, in that order: a live key on a
// disabled account works again the moment anybody re-enables it, and an
// unmarked disabled account can never be turned back on (D3). An account
// already disabled by hand is still marked, and not disabled twice.
func TestTurnOffRevokesMarksThenDisables(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		disabled bool
		writes   []string
	}{
		{name: "live", writes: []string{
			"DELETE /api/v2/service_accounts/u-1/application_keys/k-1",
			"DELETE /api/v2/service_accounts/u-1/application_keys/k-2",
			"PATCH /api/v2/users/u-1",
			"DELETE /api/v2/users/u-1",
		}},
		{name: "already disabled by hand", disabled: true, writes: []string{
			"DELETE /api/v2/service_accounts/u-1/application_keys/k-1",
			"DELETE /api/v2/service_accounts/u-1/application_keys/k-2",
			"PATCH /api/v2/users/u-1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newRegion(t)
			reg.handle["/api/v2/service_accounts/u-1/application_keys/k-1"] = okJSON
			reg.handle["/api/v2/service_accounts/u-1/application_keys/k-2"] = okJSON
			reg.handle["/api/v2/users/u-1"] = okJSON
			err := reg.client(t).TurnOff(context.Background(), pair, datadog.Account{
				ID: "u-1", Disabled: tc.disabled,
				Keys: []datadog.AppKey{{ID: "k-1", Name: "crewlet"}, {ID: "k-2", Name: "by hand"}},
			})
			if err != nil {
				t.Fatalf("turn off: %v", err)
			}
			if got := writesOf(reg); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %q, want %q", got, tc.writes)
			}
		})
	}
}

// A KEY THAT CANNOT BE REVOKED STOPS THE TURN-OFF BEFORE THE DISABLE, so the
// account stays visible with the credential nobody could withdraw rather than
// disabled and quietly holding it.
func TestTurnOffStopsAtAKeyItCannotRevoke(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v2/service_accounts/u-1/application_keys/k-1"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}
	err := reg.client(t).TurnOff(context.Background(), pair, datadog.Account{
		ID: "u-1", Keys: []datadog.AppKey{{ID: "k-1", Name: "crewlet"}},
	})
	if datadog.Status(err) != http.StatusForbidden {
		t.Fatalf("err = %v, want the refusal", err)
	}
	if got := writesOf(reg); len(got) != 1 {
		t.Errorf("writes = %q, want only the refused revoke", got)
	}
}
