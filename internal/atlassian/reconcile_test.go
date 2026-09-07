package atlassian_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/provision"
)

// org is a stub of the parts of Atlassian one pass touches.
type org struct {
	mu sync.Mutex
	// tokens is how many API tokens the account holds. A recreated account
	// holds none, which is the state this suite is about.
	tokens int
	// listFails makes the token read unanswerable, which is a different
	// fact from "there are none".
	listFails bool
	minted    int
}

func (o *org) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/workspaces"):
			_, _ = w.Write([]byte(`{"data":[{"id":"ari:cloud:jira::site/cloud-1","attributes":` +
				`{"type":"JiraSoftware","hostUrl":"https://acme.atlassian.net"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/service-accounts") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]string{{
					"id":          "acct-1",
					"displayName": atlassian.AccountName("SRE Lead", "sre-lead"),
					"email":       "acct@example.invalid",
				}},
			})
		case strings.Contains(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodGet:
			if o.listFails {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"unwell"}`))
				return
			}
			out := make([]map[string]string, o.tokens)
			for i := range out {
				out[i] = map[string]string{"id": "t"}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.Contains(r.URL.Path, "/manage/api-tokens") && r.Method == http.MethodPost:
			o.minted++
			_, _ = w.Write([]byte(`{"token":"ATSTT-fresh"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sink is a store that already holds a credential for the seat.
type sink struct {
	held  string
	wrote map[string]string
}

func (s *sink) Record(_ context.Context, name, value string) error {
	if s.wrote == nil {
		s.wrote = map[string]string{}
	}
	s.wrote[name] = value
	return nil
}
func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	if name == "SEAT_TOKEN" && s.held != "" {
		return s.held, true, nil
	}
	if v, ok := s.wrote[name]; ok {
		return v, true, nil
	}
	return "", false, nil
}
func (s *sink) Discard(context.Context) error { return nil }
func (s *sink) Flush(context.Context) error   { return nil }
func (s *sink) Describe() string              { return "test" }
func (s *sink) NextStep() string              { return "" }

func run(t *testing.T, o *org, s *sink) *atlassian.Result {
	t.Helper()
	plan := &provision.Plan{}
	plan.Add(provision.Seat{
		Handle: "sre-lead", Role: "SRE Lead",
		TokenVar: "SEAT_TOKEN", EmailVar: "SEAT_EMAIL",
	})
	res, err := atlassian.Reconcile(context.Background(), atlassian.Options{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server(t).URL}),
		OrgID:  "org-1", Key: "key", Plan: plan, Sink: s,
		Now: func() time.Time { return time.Unix(1, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// A HELD CREDENTIAL IS NOT NECESSARILY A WORKING ONE.
//
// Disconnecting with "remove accounts" deletes them at Atlassian and leaves
// the minted token in the sealed store, because the store is the company's
// and a teardown that emptied it would take values an operator put there by
// hand. A later reconnect creates a NEW account, finds a credential already
// held, mints nothing, and every call the seat makes is refused with a 401
// naming nothing. The only cure was deleting the secret by hand.
//
// The account having NO tokens is the test: no account this engine has
// finished with is in that state, and every freshly created one is.
func TestATokenForAnAccountThatNoLongerExistsIsMintedOver(t *testing.T) {
	t.Parallel()
	o := &org{tokens: 0}
	s := &sink{held: "ATSTT-for-the-deleted-account"}

	res := run(t, o, s)

	if o.minted != 1 {
		t.Fatalf("minted %d times, want one fresh token (seats %+v)", o.minted, res.Seats)
	}
	if s.wrote["SEAT_TOKEN"] != "ATSTT-fresh" {
		t.Errorf("the store still holds %q", s.wrote["SEAT_TOKEN"])
	}
	if len(res.Seats) != 1 || !res.Seats[0].TokenMinted {
		t.Errorf("the pass did not report a mint: %+v", res.Seats)
	}
}

// AND A CREDENTIAL THE ACCOUNT STILL HAS IS LEFT ALONE. A mint is not
// idempotent: Atlassian issues a new token every time and shows it once, so
// minting on every pass would rotate the credential every running seat is
// authenticating with, on the loop's timer.
func TestAWorkingCredentialIsNotRotatedOnEveryPass(t *testing.T) {
	t.Parallel()
	o := &org{tokens: 1}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.minted != 0 {
		t.Errorf("minted %d times over a credential the account still holds", o.minted)
	}
}

// AND "CANNOT TELL" LEAVES IT ALONE TOO.
//
// A failed read is not evidence that a credential is dead. Minting on it
// would rotate a working token every time Atlassian was briefly unreachable,
// on a timer, which is the failure the three-valued rule exists to prevent
// everywhere else in this engine.
func TestAnUnreadableAccountDoesNotCostTheSeatItsCredential(t *testing.T) {
	t.Parallel()
	o := &org{listFails: true}
	s := &sink{held: "ATSTT-live"}

	run(t, o, s)

	if o.minted != 0 {
		t.Errorf("minted %d times on an answer the vendor never gave", o.minted)
	}
}
