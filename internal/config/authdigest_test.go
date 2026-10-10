package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// What a node's peers compare to learn whether its API admits the keys theirs
// does: a fingerprint of api.auth under the keyring every node shares.

// fleetKey is one keyring key every node of these tests holds — a fixed
// material, so two Bootstraps built from it are two nodes of one fleet.
var fleetKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

// servingNode is a node that serves the API — the ingress role on a bound
// port — with the given keys, under the fleet's keyring.
func servingNode(tokens ...config.APIToken) *config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Port = 8080
	b.Node.Roles = []string{"ingress", "seats", "workers", "data"}
	b.API.Auth.Tokens = tokens
	b.Secrets = config.Secrets{ActiveKeyID: "k1", Keys: []config.SecretKey{{ID: "k1", Material: fleetKey}}}
	return &b
}

func apiKey(id string, role config.TokenRole, value string) config.APIToken {
	return config.APIToken{ID: id, Role: role, Token: value}
}

// agreement is how one node's table stands against another's, as their peers
// would compare them.
func agreement(t *testing.T, a, b *config.Bootstrap) secrets.Agreement {
	t.Helper()
	da, err := a.AuthDigest("a")
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}
	db, err := b.AuthDigest("b")
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	return da.Against(db)
}

// TWO NODES WITH ONE TABLE AGREE, AND EVERY WAY A TABLE CAN REACH MORE ON ONE
// NODE IS A DIFFERENCE: a key's role, its VALUE under the same id — a member's
// key pasted under the id an admin's carries elsewhere — what a caller with no
// key reaches, a guard switched off, and which admin keys may change a managed
// document.
func TestTheDigestCoversEverythingThatDecidesWhatAKeyReaches(t *testing.T) {
	t.Parallel()
	base := func() *config.Bootstrap {
		return servingNode(
			apiKey("founder", config.RoleAdmin, "admin-secret-value"),
			apiKey("ada", config.RoleMember, "member-secret-value"),
		)
	}
	if got := agreement(t, base(), base()); got != secrets.AgreementSame {
		t.Fatalf("two nodes with one table = %s, want same", got)
	}
	for name, change := range map[string]func(b *config.Bootstrap){
		"a role differing": func(b *config.Bootstrap) {
			b.API.Auth.Tokens[1].Role = config.RoleAdmin
		},
		"a value differing under the same id": func(b *config.Bootstrap) {
			b.API.Auth.Tokens[0].Token = "member-secret-value"
		},
		"a key one node does not hold": func(b *config.Bootstrap) {
			b.API.Auth.Tokens = b.API.Auth.Tokens[:1]
		},
		"the anonymous posture differing": func(b *config.Bootstrap) {
			b.API.Auth.Anonymous = config.AnonymousNone
		},
		"the guard switched off": func(b *config.Bootstrap) {
			b.API.Auth.Disabled = true
		},
		"the document's writers differing": func(b *config.Bootstrap) {
			b.API.Auth.CompanyWriters = []string{"founder"}
		},
	} {
		changed := base()
		change(changed)
		if got := agreement(t, base(), changed); got != secrets.AgreementDifferent {
			t.Errorf("%s: = %s, want different", name, got)
		}
	}
}

// ONE TABLE IN ANOTHER ORDER IS ONE TABLE, and what decides nothing a key
// reaches is not part of it: the order of a list means nothing to the guard,
// and the browser origins allowed to read an answer give no caller more.
func TestTheDigestIsOfTheTableNotItsSpelling(t *testing.T) {
	t.Parallel()
	a := servingNode(apiKey("founder", config.RoleAdmin, "one"), apiKey("ada", config.RoleMember, "two"))
	a.API.Auth.CompanyWriters = []string{"founder", "ops"}
	b := servingNode(apiKey("ada", config.RoleMember, "two"), apiKey("founder", config.RoleAdmin, "one"))
	b.API.Auth.CompanyWriters = []string{"ops", "founder"}
	b.API.Auth.AllowedOrigins = []string{"https://example.com"}
	if got := agreement(t, a, b); got != secrets.AgreementSame {
		t.Errorf("one table in two orders = %s, want same", got)
	}
}

// ONLY A NODE THAT SERVES THE API PUBLISHES A TABLE, and one with no keyring
// publishes that it serves and nothing to compare.
//
// A worker whose Tier A carries no keys is not a divergence: nothing reaches the
// company through it. And a node serving the API with no keyring has nothing to
// key a fingerprint with, so it says so rather than publishing an unkeyed hash of
// credentials on a record every peer can read.
func TestOnlyANodeServingTheAPIPublishesItsTable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(b *config.Bootstrap)
		serves bool
	}{
		"the ingress role on a bound port": {func(*config.Bootstrap) {}, true},
		"no ingress role": {func(b *config.Bootstrap) {
			b.Node.Roles = []string{"seats", "workers", "data"}
		}, false},
		"no port": {func(b *config.Bootstrap) { b.API.Port = 0 }, false},
	} {
		b := servingNode(apiKey("founder", config.RoleAdmin, "v"))
		tc.change(b)
		got, err := b.AuthDigest("n")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Serves != tc.serves || got.Serves != b.ServesAPI() {
			t.Errorf("%s: serves = %v (ServesAPI %v), want %v", name, got.Serves, b.ServesAPI(), tc.serves)
		}
		if !got.Serves && len(got.Keys) != 0 {
			t.Errorf("%s: a node serving no API published a table %v", name, got.Keys)
		}
	}

	unkeyed := servingNode(apiKey("founder", config.RoleAdmin, "v"))
	unkeyed.Secrets = config.Secrets{}
	got, err := unkeyed.AuthDigest("n")
	if err != nil {
		t.Fatalf("no keyring: %v", err)
	}
	if !got.Serves || len(got.Keys) != 0 {
		t.Errorf("no keyring published %+v, want serving with nothing to compare", got)
	}
	if ag := got.Against(got); ag != secrets.AgreementIncomparable {
		t.Errorf("a node with no keyring against itself = %s, want incomparable", ag)
	}

	broken := servingNode(apiKey("founder", config.RoleAdmin, "v"))
	broken.Secrets.Keys[0].Material = "not base64!"
	if _, err := broken.AuthDigest("n"); err == nil {
		t.Error("a keyring that will not decode fingerprinted anyway")
	}
}
