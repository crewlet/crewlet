package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notice"
)

// The engine's callback before and after its address moved.
const (
	movedFrom = "https://old-tunnel.example.com/webhooks/mcp-oauth"
	movedTo   = "https://crewlet.example.com/webhooks/mcp-oauth"
)

// A GRANT KEEPS THE REDIRECT ITS SIGN-IN CAME BACK THROUGH, in either family:
// the only proof of what a client a person registered lists.
//
// Mutation: leave Redirect off the record CompleteSignIn writes, and both
// grants read "".
func TestAGrantKeepsTheRedirectItsSignInCameBackThrough(t *testing.T) {
	t.Parallel()
	v := newOAuthVendor(t)
	a, b := oauthNodes(t, v, "dcr")
	signIn(t, v, a, b)
	if got := mustGrant(t, a).Redirect; got != movedTo {
		t.Errorf("the MCP grant's redirect = %q, want %q", got, movedTo)
	}

	g := newFakeGoogle(t)
	ca, cb, _ := consentNodes(t, g)
	const client = "123456789012-redirectkept.apps.googleusercontent.com"
	kind := consentProduct(t, "redirect_kept", client, nil)
	consentSignIn(t, g, ca, cb, kind, client)
	grant, found, err := ca.IntegrationGrant(t.Context(), kind)
	if err != nil || !found || grant.Redirect != movedTo {
		t.Errorf("the consent grant's redirect = %q (found %t, %v), want %q", grant.Redirect, found, err, movedTo)
	}
}

// THE REDIRECT NOTICE NAMES EXACTLY THE CLIENTS A PERSON REGISTERED WHOSE LAST
// SIGN-IN CAME BACK ELSEWHERE: a Google client once however many products share
// it, the product last signed in first, an MCP server's own client; never a dcr, cimd or service identity's, a
// grant recorded without a redirect, a server turned off, or a grant made as
// another client than the one configured now. A client clears once it signs in
// through the new address, a shared one through any product, and the notice
// once none is left.
//
// Mutation: compare a shared client's first grant instead of its newest, or
// drop the client ID check on an MCP grant, and the list is wrong.
func TestTheRedirectNoticeNamesEveryRegisteredClientSignedInElsewhere(t *testing.T) {
	t.Parallel()
	const (
		shared = "123456789012-redirectshared.apps.googleusercontent.com"
		alone  = "210987654321-redirectalone.apps.googleusercontent.com"
		before = "345678901234-redirectbefore.apps.googleusercontent.com"
	)
	sharedA := consentProduct(t, "redirect_shared_a", shared, nil)
	sharedB := consentProduct(t, "redirect_shared_b", shared, nil)
	aloneKind := consentProduct(t, "redirect_alone", alone, nil)
	unrecorded := consentProduct(t, "redirect_unrecorded", before, nil)
	company := companyFor(t, `
name: Acme
mcp_servers:
  - name: own-stale
    transport: http
    url: https://own-stale.example.com/mcp
    oauth: {client: own, client_id: own-stale-client}
  - name: own-current
    transport: http
    url: https://own-current.example.com/mcp
    oauth: {client: own, client_id: own-current-client}
  - name: own-unrecorded
    transport: http
    url: https://own-unrecorded.example.com/mcp
    oauth: {client: own, client_id: own-unrecorded-client}
  - name: own-off
    transport: http
    url: https://own-off.example.com/mcp
    enabled: false
    oauth: {client: own, client_id: own-off-client}
  - name: own-replaced
    transport: http
    url: https://own-replaced.example.com/mcp
    oauth: {client: own, client_id: own-replaced-client}
  - name: by-dcr
    transport: http
    url: https://by-dcr.example.com/mcp
    oauth: {client: dcr}
  - name: by-cimd
    transport: http
    url: https://by-cimd.example.com/mcp
    oauth: {client: cimd}
  - name: service
    transport: http
    url: https://service.example.com/mcp
    oauth: {grant: client_credentials, client_id: service-client, client_secret: "${SERVICE_SECRET}"}
`)
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, webhookBase: "https://crewlet.example.com"}
	e.epoch.current.Store(company)
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	put := func(fam coord.GrantFamily, name, client, id, redirect string, when time.Time) {
		t.Helper()
		g := coord.OAuthGrant{Name: name, Grant: uuid.New(), Client: client, ClientID: id,
			Issuer: "https://issuer.example.com", Redirect: redirect, By: "jane.doe", ByKind: iam.ActorHuman, At: when}
		if fam.Resourced {
			g.Resource = "https://" + name + ".example.com/mcp"
		}
		if held, found, err := fleet.OAuthGrant(t.Context(), fam, name); err != nil {
			t.Fatal(err)
		} else if found {
			g.Version = held.Version
			if _, wrote, err := fleet.UpdateOAuthGrant(t.Context(), fam, g); err != nil || !wrote {
				t.Fatalf("update %s: %t, %v", name, wrote, err)
			}
			return
		}
		if _, created, err := fleet.CreateOAuthGrant(t.Context(), fam, g); err != nil || !created {
			t.Fatalf("create %s: %t, %v", name, created, err)
		}
	}
	google, mcp := coord.IntegrationGrantFamily, coord.MCPGrantFamily
	put(google, string(sharedB), "own", shared, movedFrom, at)
	put(google, string(aloneKind), "own", alone, movedTo, at)
	put(google, string(unrecorded), "own", before, "", at)
	put(mcp, "own-stale", "own", "own-stale-client", movedFrom, at)
	put(mcp, "own-current", "own", "own-current-client", movedTo, at)
	put(mcp, "own-unrecorded", "own", "own-unrecorded-client", "", at)
	put(mcp, "own-off", "own", "own-off-client", movedFrom, at)
	put(mcp, "own-replaced", "own", "an-earlier-client", movedFrom, at)
	put(mcp, "by-dcr", "dcr", "registered-client", movedFrom, at)
	put(mcp, "by-cimd", "cimd", "https://old-tunnel.example.com/webhooks/mcp-oauth/client-metadata.json",
		movedFrom, at)
	put(mcp, "service", "own", "service-client", movedFrom, at)

	read := func() []notice.Notice {
		t.Helper()
		clients, err := e.RegisteredClients(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return notice.FromRedirects(e.CallbackURL(), clients)
	}
	got := read()
	if len(got) != 1 || got[0].Redirect == nil || got[0].Redirect.URI != movedTo {
		t.Fatalf("the notices are %+v, want one naming %s", got, movedTo)
	}
	want := []notice.RegisteredClient{
		{Vendor: notice.VendorGoogle, ID: shared, Label: "123456789012-redirect…",
			Uses: []notice.ClientUse{{Name: string(sharedB), Title: "Consent " + string(sharedB)},
				{Name: string(sharedA), Title: "Consent " + string(sharedA)}},
			Redirect: movedFrom, At: at},
		{Vendor: notice.VendorMCP, ID: "own-stale-client", Label: "own-stale",
			Uses: []notice.ClientUse{{Name: "own-stale", Title: "own-stale"}}, Redirect: movedFrom, At: at},
	}
	if !slices.EqualFunc(got[0].Redirect.Clients, want, equalClient) {
		t.Errorf("the stale clients are\n%+v\nwant\n%+v", got[0].Redirect.Clients, want)
	}
	if got[0].Title != "Add the new redirect URI to 2 OAuth clients" {
		t.Errorf("the title is %q", got[0].Title)
	}

	// THE SHARED CLIENT SIGNED IN THROUGH THE NEW ADDRESS by its other product
	// lists it now, whatever the first product's older grant says.
	put(google, string(sharedA), "own", shared, movedTo, at.Add(time.Hour))
	if got := read(); len(got) != 1 || len(got[0].Redirect.Clients) != 1 ||
		got[0].Redirect.Clients[0].Label != "own-stale" || got[0].Title != "Add the new redirect URI to 1 OAuth client" {
		t.Fatalf("after the shared client signed in again the notices are %+v, want own-stale alone", got)
	}
	put(mcp, "own-stale", "own", "own-stale-client", movedTo, at.Add(time.Hour))
	if got := read(); len(got) != 0 {
		t.Errorf("with every client signed in through %s the notices are %+v, want none", movedTo, got)
	}
}

func equalClient(a, b notice.RegisteredClient) bool {
	return a.Vendor == b.Vendor && a.ID == b.ID && a.Label == b.Label && slices.Equal(a.Uses, b.Uses) &&
		a.Redirect == b.Redirect && a.At.Equal(b.At)
}
