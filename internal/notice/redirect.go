package notice

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/words"
)

// ClassOAuthRedirectMoved is the engine's sign-in callback moved away from
// the redirect URI an OAuth client a person registered was last signed in
// through; its subject is the company, and [Notice.Redirect] names the clients.
const ClassOAuthRedirectMoved = "oauth_redirect_moved"

// The vendors a [RegisteredClient] is registered at.
const (
	// VendorGoogle is a Google consent product's client, in Google Cloud.
	VendorGoogle = "google"
	// VendorMCP is an MCP server's own client (`oauth.client: own`), at its
	// vendor.
	VendorMCP = "mcp"
)

// Vendors is every vendor a client is registered at.
var Vendors = []string{VendorGoogle, VendorMCP}

// RegisteredClient is an OAuth client a person registered at its vendor,
// whose list of redirect URIs only a person changes, as its last grant left it.
type RegisteredClient struct {
	// Vendor is where it is registered: [VendorGoogle] or [VendorMCP].
	Vendor string `json:"vendor"`
	// ID is its client ID, public in every kind, and Label what a person
	// finds it by: a Google client's ID shortened, an MCP server's name.
	ID    string `json:"id"`
	Label string `json:"label"`
	// Uses is what signs in as it: each Google product sharing it, or the
	// MCP server.
	Uses []ClientUse `json:"uses"`
	// Redirect is the redirect URI its last grant came back through, "" for
	// a grant recorded before it was kept, and At when.
	Redirect string    `json:"redirect"`
	At       time.Time `json:"at"`
}

// ClientUse is one thing that signs in as a client: an integration kind or an
// MCP server's name, and what a person calls it.
type ClientUse struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// RedirectMoved is an [ClassOAuthRedirectMoved] notice's new redirect URI and
// every client still registered without it.
type RedirectMoved struct {
	URI     string             `json:"uri"`
	Clients []RegisteredClient `json:"clients"`
}

// FromRedirects is one notice naming every client whose last grant came back
// through a redirect other than callback, the engine's sign-in callback now;
// none where callback is "" (no sign-in can come back) or no client is stale.
func FromRedirects(callback string, clients []RegisteredClient) []Notice {
	if callback == "" {
		return nil
	}
	stale := slices.DeleteFunc(slices.Clone(clients), func(c RegisteredClient) bool {
		return c.Redirect == "" || c.Redirect == callback
	})
	if len(stale) == 0 {
		return nil
	}
	slices.SortFunc(stale, func(a, b RegisteredClient) int {
		return cmp.Or(cmp.Compare(a.Vendor, b.Vendor), cmp.Compare(a.Label, b.Label), cmp.Compare(a.ID, b.ID))
	})
	return []Notice{{Class: ClassOAuthRedirectMoved, Subject: advisor.Subject{Kind: advisor.SubjectCompany},
		Title: fmt.Sprintf("Add the new redirect URI to %s", words.Count(len(stale), "OAuth client", "OAuth clients")),
		Person: fmt.Sprintf("This engine's address changed, so every OAuth sign-in now comes back to %s. "+
			"Add it to each client's Authorized redirect URIs: in Google Cloud, Credentials for a Google "+
			"client, or in the vendor's app settings for an MCP server. Sign-ins already made keep working, "+
			"but the next sign-in through each client fails at the vendor until it lists the new URI. The "+
			"alarm clears for each client once it signs in through the new address.", callback),
		Redirect: &RedirectMoved{URI: callback, Clients: stale}}}
}
