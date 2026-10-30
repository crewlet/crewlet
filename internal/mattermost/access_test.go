package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
)

// THE FOUR OPTIONS ARE MATTERMOST'S OWN, and a role list outside them is
// refused rather than written: the document stores what the vendor's API
// takes, so a list the picker never offers is a typo or a widening, never an
// access somebody chose.
func TestCanonicalRolesAcceptsExactlyTheFourOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, []string{"system_user"}},
		{[]string{"system_user"}, []string{"system_user"}},
		{[]string{" System_Post_All_Public ", "system_user", "system_user"},
			[]string{"system_user", "system_post_all_public"}},
		{[]string{"system_post_all", "system_user"}, []string{"system_user", "system_post_all"}},
		{[]string{"system_user", "system_admin"}, []string{"system_admin", "system_user"}},
	} {
		got, err := mattermost.CanonicalRoles(tc.in)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("CanonicalRoles(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, refused := range [][]string{
		{"system_admin"},
		{"system_post_all"},
		{"system_user", "system_post_all", "system_post_all_public"},
		{"system_user", "system_manager"},
		{"team_admin"},
	} {
		if _, err := mattermost.CanonicalRoles(refused); !errors.Is(err, mattermost.ErrUnknownRoles) {
			t.Errorf("CanonicalRoles(%q) = %v, want ErrUnknownRoles", refused, err)
		}
	}
}

// THE ORDER A RAISE IS DERIVED FROM, and the one administration class (D10).
func TestRoleOptionsCarryMattermostsOrderAndClass(t *testing.T) {
	t.Parallel()
	options := mattermost.RoleOptions()
	labels := make([]string, 0, len(options))
	for i, option := range options {
		labels = append(labels, option.Label)
		if rank := mattermost.RolesRank(option.Roles); rank != i {
			t.Errorf("%s ranks %d, want %d", option.Label, rank, i)
		}
		if mattermost.AdministrationRoles(option.Roles) != option.Administration {
			t.Errorf("%s administration class disagrees with its option", option.Label)
		}
		if got := mattermost.RolesLabel(option.Roles); got != option.Label {
			t.Errorf("label of %q = %q, want %q", option.Roles, got, option.Label)
		}
	}
	want := []string{"Member", "Member with post:channels", "Member with post:all", "System Admin"}
	if !slices.Equal(labels, want) {
		t.Errorf("options = %q, want %q", labels, want)
	}
	// A CALLER CANNOT EDIT THE TABLE through what it was handed.
	options[0].Roles[0] = "system_admin"
	if mattermost.RoleOptions()[0].Roles[0] != "system_user" {
		t.Error("the option table was mutated through RoleOptions")
	}
	if mattermost.RolesRank([]string{"system_user", "system_manager"}) != -1 {
		t.Error("an unoffered list was given a rank")
	}
	if got := mattermost.RolesLabel([]string{"system_user", "system_manager"}); got !=
		"system_manager system_user" {
		t.Errorf("an unoffered list is labelled %q, want it as Mattermost spells it", got)
	}
}

// DRIFT IS A SET COMPARISON: Mattermost serves the roles as one string in
// whatever order it stored them.
func TestRolesOfReadsTheServedString(t *testing.T) {
	t.Parallel()
	got := mattermost.RolesOf(mattermost.User{Roles: "system_post_all  system_user "})
	if !mattermost.SameRoles(got, []string{"system_user", "system_post_all"}) {
		t.Errorf("RolesOf = %q", got)
	}
	if mattermost.SameRoles(got, []string{"system_user"}) {
		t.Error("a bot holding post:all compared equal to Member")
	}
}

// A ROLE UPDATE WRITES THE WHOLE LIST, which is the server's own shape: it
// stores exactly the string it is sent. An unoffered list never leaves the
// process.
func TestUpdateRolesWritesTheWholeCanonicalList(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	var body string
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/roles") {
			raw, _ := io.ReadAll(r.Body)
			body = string(raw)
		}
		return false
	})
	c := client(t, s)
	if err := c.UpdateRoles(context.Background(), "u-1",
		[]string{"system_post_all", "system_user"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	var got struct {
		Roles string `json:"roles"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.Roles != "system_user system_post_all" {
		t.Errorf("roles sent = %q, want the canonical list", got.Roles)
	}
	if calls := s.seen(); len(calls) != 1 || calls[0] != "PUT /api/v4/users/u-1/roles" {
		t.Errorf("calls = %q", calls)
	}
	if err := c.UpdateRoles(context.Background(), "u-1", []string{"system_admin"}); err == nil {
		t.Error("an unoffered role list was accepted")
	}
	if len(s.seen()) != 1 {
		t.Error("an unoffered role list reached the server")
	}
}

// LEAVING A CHANNEL IS IDEMPOTENT, and a refusal keeps its status so the step
// can say why.
func TestRemoveChannelMember(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.Contains(r.URL.Path, "/channels/gone/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not a member"}`))
			return true
		case strings.Contains(r.URL.Path, "/channels/locked/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You do not have the appropriate permissions."}`))
			return true
		}
		return false
	})
	c := client(t, s)
	ctx := context.Background()
	if err := c.RemoveChannelMember(ctx, "ch-1", "u-1"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if calls := s.seen(); calls[0] != "DELETE /api/v4/channels/ch-1/members/u-1" {
		t.Errorf("calls = %q", calls)
	}
	if err := c.RemoveChannelMember(ctx, "gone", "u-1"); err != nil {
		t.Errorf("leaving a channel already left: %v, want success", err)
	}
	err := c.RemoveChannelMember(ctx, "locked", "u-1")
	if mattermost.Status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "permissions") {
		t.Errorf("a refused leave = %v, want the 403 and the server's reason", err)
	}
}

// AN ACCOUNT THAT IS GONE IS AN ANSWER, which the ledger closes; a read that
// failed is not.
func TestUserByIDTellsAbsentFromFailed(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/users/u-1":
			_, _ = w.Write([]byte(`{"id":"u-1","username":"agent-sre",` +
				`"roles":"system_user system_admin","delete_at":0}`))
		case "/api/v4/users/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"bad"}`))
		}
		return true
	})
	c := client(t, s)
	user, found, err := c.UserByID(context.Background(), "u-1")
	if err != nil || !found || !mattermost.AdministrationRoles(mattermost.RolesOf(user)) {
		t.Fatalf("user = %+v, %v, %v", user, found, err)
	}
	if _, found, err := c.UserByID(context.Background(), "gone"); err != nil || found {
		t.Errorf("gone: found=%v err=%v", found, err)
	}
	if _, found, err := c.UserByID(context.Background(), "broken"); err == nil || found {
		t.Errorf("broken: found=%v err=%v, want the error", found, err)
	}
}
