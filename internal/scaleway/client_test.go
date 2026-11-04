package scaleway_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/scaleway"
)

func newClient(t *testing.T, f *fakeScaleway) *scaleway.Client {
	t.Helper()
	c, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A REFUSAL CARRIES WHAT SCALEWAY SAID, its type and its status, and never the
// key it was refused for; a 401 or 403 is the credential's.
func TestARefusalSaysWhatScalewaySaid(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	f.refuse["apps"] = http.StatusForbidden
	_, err := newClient(t, f).Applications(t.Context())
	api, ok := errors.AsType[*scaleway.APIError](err)
	if !ok || api.Status != http.StatusForbidden || api.Type != "permissions_denied" ||
		api.Detail != "insufficient permissions" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "permissions_denied: insufficient permissions") || strings.Contains(err.Error(), provSecret) {
		t.Errorf("the error reads %q", err)
	}
	if !errors.Is(integration.Reject(err, scaleway.Status(err)), integration.ErrCredentialRejected) {
		t.Error("a 403 is not read as the credential's")
	}
	f.onlyScaleway(t)
}

// A SHORT RATE LIMIT IS WAITED OUT AS SCALEWAY ASKS, up to three times; a
// fourth is the pass's to back off from.
func TestARateLimitIsWaitedOutThenReturned(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	c := newClient(t, f)
	var waits []time.Duration
	scaleway.SetWait(c, func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil })
	f.edit(func(f *fakeScaleway) { f.limited["apps"] = 2 })
	if _, err := c.Applications(t.Context()); err != nil || len(waits) != 2 {
		t.Fatalf("two 429s: %v after %d waits, want an answer after 2", err, len(waits))
	}
	f.edit(func(f *fakeScaleway) { f.limited["apps"] = 4 })
	waits = nil
	_, err := c.Applications(t.Context())
	if scaleway.Status(err) != http.StatusTooManyRequests || len(waits) != 3 {
		t.Errorf("four 429s: %v after %d waits, want the fourth returned after 3", err, len(waits))
	}
}

// A LISTING READS EVERY PAGE, a hundred rows at a time, to the total Scaleway
// reports: a short read would read as applications that do not exist.
func TestAListingReadsEveryPage(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	for i := range 250 {
		id := fmt.Sprintf("dddddddd-0000-4000-8000-%012d", i)
		f.apps[id] = &fakeApp{ID: id, Name: fmt.Sprintf("app-%03d", i), Org: testOrg}
	}
	apps, err := newClient(t, f).Applications(t.Context())
	if err != nil || len(apps) != 251 {
		t.Fatalf("read %d applications (%v), want 251", len(apps), err)
	}
	pages := 0
	for _, r := range f.requests {
		if r == "GET /iam/v1alpha1/applications" {
			pages++
		}
	}
	if pages != 3 {
		t.Errorf("read %d pages, want 3", pages)
	}
}

// A CLIENT THAT WOULD FAIL EVERY CALL IS NEVER BUILT: an organization that is
// not a UUID, or no secret.
func TestAClientThatWouldFailEveryCallIsRefused(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]scaleway.ClientOptions{
		"an organization that is not one": {OrganizationID: "acme", SecretKey: provSecret},
		"no secret":                       {OrganizationID: testOrg, SecretKey: "  "},
	} {
		if _, err := scaleway.NewClient(opts); err == nil {
			t.Errorf("%s: built", name)
		}
	}
	c, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: " " + strings.ToUpper(testOrg) + " ", SecretKey: provSecret})
	if err != nil || c.OrganizationID() != testOrg {
		t.Errorf("an organization in capitals and spaces: %v, %q, want it as Scaleway matches it", err, c.OrganizationID())
	}
}
