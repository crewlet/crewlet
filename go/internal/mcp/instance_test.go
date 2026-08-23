package mcp

import "testing"

func TestInstanceGrammar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		server, role string
		instance     string
	}{
		{"atlassian", "Engineer", "atlassian::Engineer"},
		{"slack", "Senior Dev", "slack::Senior_Dev"},
		{"jira", "PM", "jira::PM"},
		{"github", "Product Manager", "github::Product_Manager"},
	}
	for _, tc := range cases {
		got := InstanceName(tc.server, tc.role)
		if got != tc.instance {
			t.Errorf("InstanceName(%q, %q) = %q, want %q", tc.server, tc.role, got, tc.instance)
		}
		if back := ServerName(got); back != tc.server {
			t.Errorf("ServerName(%q) = %q, want %q", got, back, tc.server)
		}
	}
}

func TestServerNamePassesSharedServersThrough(t *testing.T) {
	t.Parallel()
	// A shared server has no per-role suffix, and stripping must be a no-op
	// for it rather than mangling the name it is indexed under.
	if got := ServerName("github"); got != "github" {
		t.Fatalf("ServerName(github) = %q", got)
	}
	if got := RoleOfInstance("github"); got != "" {
		t.Fatalf("RoleOfInstance(github) = %q, want empty", got)
	}
}

func TestRoleOfInstance(t *testing.T) {
	t.Parallel()
	if got := RoleOfInstance("slack::Senior_Dev"); got != "Senior_Dev" {
		t.Fatalf("RoleOfInstance = %q", got)
	}
	// A separator inside the ROLE half stays there: the split is on the
	// first, because the server name is what comes before it.
	if got := ServerName("a::b::c"); got != "a" {
		t.Fatalf("ServerName(a::b::c) = %q, want a", got)
	}
	if got := RoleOfInstance("a::b::c"); got != "b::c" {
		t.Fatalf("RoleOfInstance(a::b::c) = %q, want b::c", got)
	}
}

func TestTwoRolesGetDistinctInstances(t *testing.T) {
	t.Parallel()
	// The credentials in a per-role child ARE that seat's identity. Two roles
	// resolving to one instance name would mean one process, one set of
	// credentials, and either seat able to act as the other — invisibly,
	// because the tool call looks identical from the engine's side.
	a := InstanceName("atlassian", "Engineer")
	b := InstanceName("atlassian", "Product Manager")
	if a == b {
		t.Fatalf("two roles collapsed onto one instance name: %q", a)
	}
	if ServerName(a) != ServerName(b) {
		t.Fatal("both instances must still report the same bare server to the model")
	}
}
