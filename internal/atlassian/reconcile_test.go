package atlassian_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/integration"
)

// stubOrg is an organization answering the one route the company-level pass
// calls, and recording every route it was asked for.
type stubOrg struct {
	mu     sync.Mutex
	status int // the workspace listing's status; zero is 200
	calls  []string
}

func (o *stubOrg) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.calls = append(o.calls, r.Method+" "+r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, "/workspaces") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if o.status != 0 {
			w.WriteHeader(o.status)
			_, _ = w.Write([]byte(`{"message":"refused by the stand-in"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"ari:cloud:jira::site/cloud-1","attributes":` +
			`{"type":"JiraSoftware","hostUrl":"https://acme.atlassian.net"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reconcile(ctx context.Context, t *testing.T, o *stubOrg) (*atlassian.Result, error) {
	t.Helper()
	return atlassian.Reconcile(ctx, atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server(t).URL}),
		OrgID:  "org-1", Key: "fake-org-key",
	})
}

// THE COMPANY-LEVEL PASS DISCOVERS THE SITE AND TOUCHES NOTHING ELSE: every
// account, grant and token is per-seat access, so not one account route is
// called here.
func TestThePassDiscoversTheSiteAndTouchesNoAccount(t *testing.T) {
	t.Parallel()
	o := &stubOrg{}
	res, err := reconcile(context.Background(), t, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Site.CloudID != "cloud-1" || res.Site.HostURL != "https://acme.atlassian.net" {
		t.Fatalf("site = %+v", res.Site)
	}
	if len(o.calls) != 1 || !strings.HasSuffix(o.calls[0], "/workspaces") {
		t.Fatalf("calls = %v, want the workspace listing alone", o.calls)
	}
}

// A CANCELLED PASS RAISES, and does not blame the organization key: a node
// draining would otherwise write a false accusation about a working key into
// the fleet's status, which is the claim [integration.Refusal] exists to stop.
func TestACancelledPassRaisesRatherThanBlamingTheOrganizationCredential(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := reconcile(ctx, t, &stubOrg{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile over a cancelled context = (%+v, %v), want the cancellation raised", res, err)
	}
	if strings.Contains(err.Error(), "api_key") {
		t.Errorf("a cancelled pass accused the organization credential: %v", err)
	}
}

// A KEY ATLASSIAN REFUSES IS NAMED, and classified as a rejected credential
// so the card sends the operator to replace it.
func TestARefusedOrganizationKeyIsNamedAndRejected(t *testing.T) {
	t.Parallel()
	_, err := reconcile(context.Background(), t, &stubOrg{status: http.StatusUnauthorized})
	if err == nil || !strings.Contains(err.Error(), "integrations.atlassian.api_key") {
		t.Fatalf("err = %v, want the key named", err)
	}
	if !errors.Is(err, integration.ErrCredentialRejected) {
		t.Fatalf("err = %v, want it classified as a rejected credential", err)
	}
}

func TestAPassNeedsTheOrganizationAndItsKey(t *testing.T) {
	t.Parallel()
	client := atlassian.NewClient(atlassian.ClientOptions{BaseURL: "http://127.0.0.1:1"})
	for _, opts := range []atlassian.Options{
		{OrgID: "org-1", Key: "k"},
		{Client: client, Key: "k"},
		{Client: client, OrgID: "org-1"},
	} {
		if _, err := atlassian.Reconcile(context.Background(), opts); err == nil {
			t.Errorf("Reconcile(%+v) was accepted", opts)
		}
	}
}
