package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/config"
)

// AN APP STORE CONNECT CONNECTION SAYS ITS TEAM'S APPS: the apps this node
// last read an agent's key list, by name with their bundle IDs and no serving
// state; open, and holding no role, since the company holds no key. A node
// that read nothing lists no app.
//
// Mutation: drop the answer's held reach, and the card lists no app.
func TestAnAppStoreConnectConnectionSaysItsApps(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.AppStoreConnect = &config.AppStoreConnect{Enabled: true,
		Connections: map[string]config.AppStoreConnectTeam{"main": {IssuerID: "57246542-96fe-1a63-e053-0824d011072a"}}}
	connection := func(held func() appstoreconnect.Reach) map[string]any {
		body := asMap(t, answer(t, queries.Sources{Company: companySource(t, cfg), AppStoreConnectReach: held},
			"integrations", nil))
		rows, _ := body["integrations"].([]any)
		for _, row := range rows {
			if entry, _ := row.(map[string]any); entry["key"] == "app_store_connect" {
				connections, _ := entry["connections"].([]any)
				if len(connections) != 1 {
					t.Fatalf("the row's connections are %v", entry["connections"])
				}
				c, _ := connections[0].(map[string]any)
				return c
			}
		}
		t.Fatal("no app_store_connect row")
		return nil
	}
	c := connection(func() appstoreconnect.Reach {
		return appstoreconnect.Reach{{Connection: "main", Read: true, Apps: []appstoreconnect.App{
			{ID: "111", Name: "Field Notes", BundleID: "com.example.notes"}}}}
	})
	roles, _ := c["roles_held"].([]any)
	accounts, _ := c["accounts"].([]any)
	if len(roles) != 0 || c["open"] != true || len(accounts) != 1 {
		t.Fatalf("the connection answers %v", c)
	}
	app, _ := accounts[0].(map[string]any)
	if _, stated := app["state"]; app["account"] != "111" || app["account_name"] != "Field Notes" ||
		app["detail"] != "com.example.notes" || stated {
		t.Fatalf("its app answers %v", app)
	}
	unread := connection(nil)
	if roles, _ := unread["roles_held"].([]any); len(roles) != 0 || unread["open"] != true {
		t.Fatalf("a connection this node never read answers %v", unread)
	}
	if accounts, _ := unread["accounts"].([]any); len(accounts) != 0 {
		t.Fatalf("a connection this node never read lists %v", accounts)
	}
}
