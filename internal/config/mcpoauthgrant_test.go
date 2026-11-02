package config

import (
	"errors"
	"strings"
	"testing"
)

// A SERVICE IDENTITY SIGNS IN AS THE COMPANY'S OWN CLIENT, WITH ITS ID AND
// SECRET AND NOTHING ELSE: client_credentials needs both, refuses a
// registration (nobody signs in to register one), and leaves a person's
// sign-in, the default whether written or not, as it was. Each refusal is at
// the field that has the problem.
func TestAServiceIdentityNeedsItsClientsIDAndSecretAndNoRegistration(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, oauthDoc("    oauth:\n      grant: client_credentials\n      client_id: crewlet-svc\n"+
		"      client_secret: ${LINEAR_CLIENT_SECRET}\n      scopes: [read]\n"))
	o := cfg.MCPServers[0].OAuth
	if !o.ServiceIdentity() || o.GrantType() != GrantClientCredentials || o.ClientID != "crewlet-svc" {
		t.Fatalf("parsed %+v", o)
	}
	for _, lines := range []string{
		"    oauth:\n      client: cimd\n",
		"    oauth:\n      grant: authorization_code\n      client: cimd\n",
	} {
		o := mustCompany(t, oauthDoc(lines)).MCPServers[0].OAuth
		if o.ServiceIdentity() || o.GrantType() != GrantAuthorizationCode {
			t.Errorf("%q: read as %q, want a person's sign-in", lines, o.GrantType())
		}
	}
	for _, tc := range []struct {
		name, lines, path string
		want              error
	}{
		{"no client id", "    oauth:\n      grant: client_credentials\n      client_secret: ${S}\n",
			"mcp_servers[0].oauth.client_id", ErrMissing},
		{"no client secret", "    oauth:\n      grant: client_credentials\n      client_id: svc\n",
			"mcp_servers[0].oauth.client_secret", ErrMissing},
		{"a registration beside it", "    oauth:\n      grant: client_credentials\n      client: own\n" +
			"      client_id: svc\n      client_secret: ${S}\n", "mcp_servers[0].oauth.client", ErrConflict},
		{"an unknown grant", "    oauth:\n      grant: device_code\n      client: cimd\n",
			"mcp_servers[0].oauth.grant", ErrUnknownValue},
		{"a person's sign-in with no registration", "    oauth:\n      grant: authorization_code\n",
			"mcp_servers[0].oauth.client", ErrMissing},
		{"a scope with a space", "    oauth:\n      grant: client_credentials\n      client_id: svc\n" +
			"      client_secret: ${S}\n      scopes: [\"read write\"]\n",
			"mcp_servers[0].oauth.scopes[0]", ErrUnknownValue},
	} {
		err := rejects(t, oauthDoc(tc.lines), tc.path)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: refused with %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A SERVICE IDENTITY IS THE COMPANY'S TOO: refused on a stdio server, a
// per-agent server and beside an Authorization header, by the rules a person's
// sign-in is held to, and never handed to a box, which cannot renew a token.
func TestAServiceIdentityIsHeldToTheRulesOfEverySignIn(t *testing.T) {
	t.Parallel()
	const block = "    oauth:\n      grant: client_credentials\n      client_id: svc\n      client_secret: ${S}\n"
	err := rejects(t, oauthDoc("    shared: false\n"+block), "mcp_servers[0].oauth")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "mcp_env") {
		t.Errorf("a per-agent server with a service identity: %v", err)
	}
	err = rejects(t, oauthDoc("    headers:\n      Authorization: Bearer ${T}\n"+block),
		"mcp_servers[0].headers.Authorization")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("an Authorization header beside a service identity: %v", err)
	}
	err = rejects(t, oauthDoc(block)+`
roles:
  - name: Engineer
    handle: eng
    sandbox:
      mcp:
        servers: [linear]
`, "sandbox.mcp.servers[0]")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a box listing a service identity's server: %v", err)
	}
}

// THE CLIENT'S SECRET IS A VALUE THE PAGE STORES, AT ITS OWN KEY AND ONLY WHERE
// THERE IS ONE: a service identity's and the company's own client's secret is
// slotted at oauth.client_secret, kept at the variable it reads already or
// pointed at one of its own, listed among the server's values by name, and
// never a seat's; a registration's public client and a server with no oauth
// block have none; and no header beside an oauth block takes the key's name.
func TestAClientsSecretIsStoredAtItsOwnKeyAndOnlyWhereThereIsOne(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, oauthDoc("    oauth:\n      grant: client_credentials\n      client_id: svc\n"+
		"      client_secret: ${LINEAR_CLIENT_SECRET}\n"))
	slot, err := cfg.MCPValueSlot("linear", "", MCPValueClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	if slot.As != MCPValueOAuth || slot.Variable != "LINEAR_CLIENT_SECRET" || slot.Repoint ||
		slot.Path.String() != "mcp_servers[0].oauth.client_secret" {
		t.Fatalf("slot = %+v", slot)
	}
	keys, _ := cfg.MCPValueKeys("linear", "")
	if len(keys) != 1 || keys[0].Key != MCPValueClientSecret || keys[0].As != MCPValueOAuth ||
		keys[0].Variable != "LINEAR_CLIENT_SECRET" {
		t.Fatalf("the server's values are %+v", keys)
	}

	literal := mustCompany(t, oauthDoc("    oauth:\n      client: own\n      client_id: own-id\n"+
		"      client_secret: typed-in-the-file\n"))
	slot, err = literal.MCPValueSlot("linear", "", MCPValueClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !slot.Repoint || slot.Variable != MCPValueVariable("linear", MCPValueClientSecret, "") {
		t.Fatalf("a literal secret's slot = %+v", slot)
	}
	if err := literal.PointMCPValue(slot); err != nil {
		t.Fatal(err)
	}
	if got := literal.MCPServers[0].OAuth.ClientSecret; got != "${"+slot.Variable+"}" {
		t.Fatalf("the pointer wrote %q", got)
	}
	if _, err := ParseCompany([]byte(oauthDoc("    oauth:\n      client: own\n      client_id: own-id\n" +
		"      client_secret: ${" + slot.Variable + "}\n"))); err != nil {
		t.Fatalf("the pointer's variable is not one a document can read: %v", err)
	}

	if _, err := cfg.MCPValueSlot("linear", "eng", MCPValueClientSecret); !errors.Is(err, ErrMCPValueSeatOnShared) {
		t.Errorf("a seat's client secret: %v", err)
	}
	for _, lines := range []string{"    oauth:\n      client: cimd\n", ""} {
		c := mustCompany(t, oauthDoc(lines))
		if _, err := c.MCPValueSlot("linear", "", MCPValueClientSecret); !errors.Is(err, ErrMCPValueNoSecret) {
			t.Errorf("%q: a client secret slot answered %v", lines, err)
		}
		if keys, _ := c.MCPValueKeys("linear", ""); len(keys) != 0 {
			t.Errorf("%q: values %+v", lines, keys)
		}
	}
	err = rejects(t, oauthDoc("    headers:\n      OAuth.Client_Secret: x\n    oauth:\n      client: cimd\n"),
		"mcp_servers[0].headers.OAuth.Client_Secret")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a header named as the client secret's key: %v", err)
	}
}
