package cloudflare_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/integration"
)

func newClient(t *testing.T, f *fakeCloudflare, token string) *cloudflare.Client {
	t.Helper()
	c, err := cloudflare.NewClient(cloudflare.ClientOptions{AccountID: testAccount, Token: token, HTTP: f.server(t)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// A CLIENT IS BUILT ONLY FOR AN ACCOUNT ID AND A TOKEN THAT CAN BE ONE, so a
// typo fails here rather than as a 4xx naming neither.
func TestNewClientRefusesWhatCannotAuthenticate(t *testing.T) {
	t.Parallel()
	for _, tc := range []cloudflare.ClientOptions{
		{AccountID: "acme", Token: provisioner},
		{AccountID: strings.ToUpper(testAccount), Token: provisioner},
		{AccountID: testAccount, Token: "  "},
	} {
		if _, err := cloudflare.NewClient(tc); err == nil {
			t.Errorf("NewClient(%q) built a client", tc.AccountID)
		}
	}
}

// A REFUSAL CARRIES CLOUDFLARE'S STATUS, CODES AND WORDS, and never the token
// it was sent with, so the loop classifies it and an operator reads why.
func TestARefusalIsTypedAndCarriesNoCredential(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	bad := newClient(t, f, "fake-wrong-token")
	_, err := bad.Verify(t.Context())
	if cloudflare.Status(err) != http.StatusUnauthorized {
		t.Fatalf("Status = %d, want 401: %v", cloudflare.Status(err), err)
	}
	api, ok := errors.AsType[*cloudflare.APIError](err)
	if !ok || !slices.Equal(api.Codes, []int{10000}) || api.Detail != "Authentication error" {
		t.Errorf("refusal = %+v, want code 10000 and Cloudflare's message", api)
	}
	if strings.Contains(err.Error(), "fake-wrong-token") {
		t.Errorf("the refusal %q repeats the token", err)
	}
	if !errors.Is(integration.Reject(err, cloudflare.Status(err)), integration.ErrCredentialRejected) {
		t.Error("a 401 is not classified as a refused credential")
	}
	if got := cloudflare.Status(errors.New("dial tcp: i/o timeout")); got != 0 {
		t.Errorf("a transport failure reported status %d", got)
	}
	f.onlyCloudflare(t)
}

// A 2XX WHOSE ENVELOPE SAYS FAILURE IS A REFUSAL, not an empty success.
func TestAnEnvelopeFailureInsideASuccessIsARefusal(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.refuse["zones"] = http.StatusOK
	_, err := newClient(t, f, provisioner).Zones(t.Context())
	if _, ok := errors.AsType[*cloudflare.APIError](err); !ok {
		t.Fatalf("Zones = %v, want an API error for success: false", err)
	}
}

// A SHORT RATE LIMIT IS WAITED OUT AS CLOUDFLARE ASKS, and the call answers as
// if it had not happened; a long one is handed back with Cloudflare's wait.
func TestARateLimitHonoursRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.limited["verify"], f.retryAfter = 2, "3"
	c := newClient(t, f, provisioner)
	var waited []time.Duration
	cloudflare.SetWait(c, func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	})
	status, err := c.Verify(t.Context())
	if err != nil || status.ID != provisionerID {
		t.Fatalf("Verify after two 429s = %+v, %v", status, err)
	}
	if !slices.Equal(waited, []time.Duration{3 * time.Second, 3 * time.Second}) {
		t.Errorf("waited %v, want Cloudflare's 3s twice", waited)
	}

	f.set(func(f *fakeCloudflare) { f.limited["verify"], f.retryAfter = 1, "120" })
	waited = nil
	_, err = c.Verify(t.Context())
	api, ok := errors.AsType[*cloudflare.APIError](err)
	if !ok || api.Status != http.StatusTooManyRequests || api.RetryAfter != 120*time.Second {
		t.Fatalf("a two-minute limit = %v, want the 429 handed back with its wait", err)
	}
	if len(waited) != 0 {
		t.Errorf("a wait past the cap was taken: %v", waited)
	}

	// AND NOT PAST THE CALLER'S DEADLINE.
	f.set(func(f *fakeCloudflare) { f.limited["verify"], f.retryAfter = 1, "10" })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.Verify(ctx); cloudflare.Status(err) != http.StatusTooManyRequests || len(waited) != 0 {
		t.Errorf("a wait past the deadline = %v, waited %v", err, waited)
	}
}

// A LISTING IS READ TO EXHAUSTION, page by page, and never past its end.
func TestAListingReadsEveryPage(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	for i := range 120 {
		f.zones = append(f.zones, cloudflare.Zone{ID: fmt.Sprintf("z-%03d", i), Name: fmt.Sprintf("site%d.example", i)})
	}
	zones, err := newClient(t, f, provisioner).Zones(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 123 {
		t.Errorf("read %d zones, want 123", len(zones))
	}
	var pages int
	for _, r := range f.requests {
		if strings.HasPrefix(r, "GET /zones") {
			pages++
		}
	}
	if pages != 3 || !slices.Equal(f.pageSize, []int{50, 50, 50}) {
		t.Errorf("read %d pages of %v, want three of 50", pages, f.pageSize)
	}
}

// THE TOKEN ROUTES ROUND-TRIP: a create returns its value once, an update
// replaces the spec, a roll replaces the value, and a delete of a token
// already gone is success.
func TestTheTokenRoutes(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	c := newClient(t, f, provisioner)
	ctx := t.Context()
	expires := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	policies := []cloudflare.Policy{{Effect: "allow", PermissionGroups: []cloudflare.GroupRef{{ID: groupID("Zone Read")}},
		Resources: map[string]any{"com.cloudflare.api.account.zone." + zoneExampleID: "*"}}}
	tok, value, err := c.CreateToken(ctx, cloudflare.TokenSpec{Name: "crewlet-acme-sre", Policies: policies, ExpiresOn: expires})
	if err != nil || value == "" || tok.ID == "" {
		t.Fatalf("CreateToken = %+v, %q, %v", tok, value, err)
	}
	got, found, err := c.Token(ctx, tok.ID)
	if err != nil || !found || !got.ExpiresOn.Equal(expires) || got.Status != cloudflare.StatusActive {
		t.Fatalf("Token = %+v, %v, %v", got, found, err)
	}
	if _, err := c.UpdateToken(ctx, tok.ID, cloudflare.TokenSpec{Name: tok.Name, Policies: policies,
		ExpiresOn: expires, Status: cloudflare.StatusDisabled}); err != nil {
		t.Fatal(err)
	}
	if held, _ := f.token(tok.ID); held.Status != cloudflare.StatusDisabled {
		t.Errorf("status after update = %q", held.Status)
	}
	rolled, err := c.RollToken(ctx, tok.ID)
	if err != nil || rolled == value {
		t.Fatalf("RollToken = %q, %v, want a new value", rolled, err)
	}
	if err := c.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteToken(ctx, tok.ID); err != nil {
		t.Errorf("deleting a token already gone = %v, want success", err)
	}
	if _, found, err := c.Token(ctx, tok.ID); found || err != nil {
		t.Errorf("a deleted token read as %v, %v", found, err)
	}
}

// A VALUE THAT NEVER ARRIVED IS AN ERROR, never an empty credential.
func TestARollWithNoValueIsAnError(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.tokens["tok-1"] = &fakeToken{ID: "tok-1", Name: "t", Status: cloudflare.StatusActive}
	f.emptyValue = true
	if _, err := newClient(t, f, provisioner).RollToken(t.Context(), "tok-1"); err == nil {
		t.Fatal("a roll answering no value succeeded")
	}
}
