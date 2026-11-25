package notice_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/notice"
)

const (
	callbackNow    = "https://crewlet.example.com/webhooks/mcp-oauth"
	callbackBefore = "https://old-tunnel.example.com/webhooks/mcp-oauth"
)

// client is a registered client of vendor labelled label, last signed in
// through redirect.
func client(vendor, label, redirect string) notice.RegisteredClient {
	return notice.RegisteredClient{Vendor: vendor, ID: label + "-id", Label: label,
		Uses: []notice.ClientUse{{Name: label, Title: label}}, Redirect: redirect,
		At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
}

// ONE NOTICE NAMES EVERY CLIENT SIGNED IN ELSEWHERE, Google's before the MCP
// servers', in words a person acts on and with the URI to add: a client signed
// in through the callback now, or whose grant recorded no redirect, is not
// named, and none is raised with nothing stale or no public address.
//
// Mutation: keep a client whose redirect is "" and a grant made before the
// redirect was recorded raises the notice.
func TestOneNoticeNamesEveryClientSignedInElsewhere(t *testing.T) {
	t.Parallel()
	clients := []notice.RegisteredClient{
		client(notice.VendorMCP, "search", callbackBefore),
		client(notice.VendorGoogle, "123456789012-abcdefgh…", callbackBefore),
		client(notice.VendorMCP, "docs", callbackNow),
		client(notice.VendorGoogle, "210987654321-unknown", ""),
	}
	got := notice.FromRedirects(callbackNow, clients)
	if len(got) != 1 {
		t.Fatalf("the notices are %+v, want one", got)
	}
	n := got[0]
	if n.Class != notice.ClassOAuthRedirectMoved || n.Subject != (advisor.Subject{Kind: advisor.SubjectCompany}) {
		t.Errorf("the notice is %s about %+v", n.Class, n.Subject)
	}
	if n.Title != "Add the new redirect URI to 2 OAuth clients" {
		t.Errorf("the title is %q", n.Title)
	}
	for _, said := range []string{callbackNow, "Authorized redirect URIs", "Google Cloud, Credentials",
		"the vendor's app settings", "Sign-ins already made keep working",
		"the next sign-in through each client fails at the vendor until it lists the new URI",
		"clears for each client once it signs in through the new address"} {
		if !strings.Contains(n.Person, said) {
			t.Errorf("the words %q do not say %q", n.Person, said)
		}
	}
	labels := []string{}
	for _, c := range n.Redirect.Clients {
		labels = append(labels, c.Label)
	}
	if n.Redirect.URI != callbackNow || !slices.Equal(labels, []string{"123456789012-abcdefgh…", "search"}) {
		t.Errorf("the notice names %s and %q, want %s and the Google client before search",
			n.Redirect.URI, labels, callbackNow)
	}

	if got := notice.FromRedirects(callbackNow, clients[2:]); len(got) != 0 {
		t.Errorf("with no client stale the notices are %+v, want none", got)
	}
	if got := notice.FromRedirects("", clients); len(got) != 0 {
		t.Errorf("with no callback the notices are %+v, want none", got)
	}
	one := notice.FromRedirects(callbackNow, clients[:1])
	if len(one) != 1 || one[0].Title != "Add the new redirect URI to 1 OAuth client" {
		t.Errorf("one stale client's notice is %+v", one)
	}
}

// THE DASHBOARD KNOWS THE REDIRECT NOTICE BY THE ENGINE'S CLASS AND EVERY
// VENDOR A CLIENT IS REGISTERED AT, so its row and its links are never dropped.
func TestTheDashboardKnowsTheRedirectNoticeAndItsVendors(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	class, err := clientsource.Scalar(tree, "OAUTH_REDIRECT_MOVED")
	if err != nil {
		t.Fatal(err)
	}
	if class != notice.ClassOAuthRedirectMoved {
		t.Errorf("the dashboard's class is %q, the engine's %q", class, notice.ClassOAuthRedirectMoved)
	}
	body, err := clientsource.Literal(tree, "OAUTH_CLIENT_VENDORS")
	if err != nil {
		t.Fatal(err)
	}
	if got := clientsource.Strings(body); !slices.Equal(got, notice.Vendors) {
		t.Errorf("the dashboard's vendors are %q, the engine's %q", got, notice.Vendors)
	}
}
