package iamapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeBootstrap answers a re-issue as the seam's owner would.
type fakeBootstrap struct {
	err   error
	calls int
}

func (b *fakeBootstrap) MintCode(context.Context) (iamapi.BootstrapFile, error) {
	b.calls++
	if b.err != nil {
		return iamapi.BootstrapFile{}, b.err
	}
	return iamapi.BootstrapFile{Path: "/var/lib/crewlet/bootstrap-code",
		Node: "node-b"}, nil
}

// A RE-ISSUE ASKS THE REDEMPTION'S OWN GATE, AND SAYS WHERE THE FILE IS.
//
// The route asked its own question — was there an active, credentialled
// administrator — and minted whenever the answer was no, so a company whose
// only person was suspended was handed a code the redemption refused for as
// long as it lived. It decides nothing now: the seam refuses a closed route
// with [iamdomain.ErrBootstrapClosed] and this answers 409. And the answer
// names the NODE, because on a fleet the file lands on whichever node served
// the request, and a path with no host is a file nobody can find.
//
// Mutation: put the administrator check back and a company whose rig holds an
// administrator is refused the mint the seam would have made; drop the node
// from the answer and the operator on a fleet has no host to look on.
func TestAReissueAsksTheRedemptionsGateAndNamesTheNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"minted", nil, http.StatusCreated, ""},
		{"closed", fmt.Errorf("%w: somebody is already enrolled",
			iamdomain.ErrBootstrapClosed), http.StatusConflict,
			"bootstrap_closed"},
		// A BROKER THAT DID NOT ACKNOWLEDGE is the framework's own
		// unavailable, which is what waiting clears; a plain fault is a
		// 500, and has its own case beside the outcome tests.
		{"failed", fmt.Errorf("%w: the broker did not acknowledge",
			statelog.ErrUnavailable), http.StatusServiceUnavailable, "unavailable"},
		// CODES KEPT ARRIVING as fast as the re-issue ended them: a lost
		// race, which the same request again wins — never `bad_params`,
		// which says it cannot succeed however often it is sent.
		{"raced", fmt.Errorf("%w: more codes kept landing",
			statelog.ErrConflict), http.StatusConflict, "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seam := &fakeBootstrap{err: tc.err}
			// THE RIG HOLDS AN ACTIVE, CREDENTIALLED ADMINISTRATOR,
			// which the old route read as "closed" whatever the gate
			// said — and minted past whenever it did not.
			r := newRig(t, func(o *iamapi.Options) { o.Bootstrap = seam })
			r.directory.creds = map[string][]iamdomain.CredentialRow{
				alice.String(): {{ID: "pw", PersonID: alice.String(),
					Method: iamdomain.MethodPassword, CreatedAt: at}},
			}
			req := httptest.NewRequest(http.MethodPost, "/iam/bootstrap-code", nil)
			req = req.WithContext(iam.WithPrincipal(req.Context(), administrator()))
			rec := httptest.NewRecorder()
			r.mux.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status,
					rec.Body.String())
			}
			if seam.calls != 1 {
				t.Errorf("the seam was asked %d times, want once — the "+
					"route decides nothing of its own", seam.calls)
			}
			switch tc.status {
			case http.StatusCreated:
				body := rec.Body.String()
				for _, want := range []string{`"node":"node-b"`,
					`"path":"/var/lib/crewlet/bootstrap-code"`} {
					if !strings.Contains(body, want) {
						t.Errorf("the answer %s does not carry %s", body, want)
					}
				}
			case http.StatusServiceUnavailable:
				if rec.Header().Get("Retry-After") == "" {
					t.Error("a 503 carried no Retry-After")
				}
			default:
				if !strings.Contains(rec.Body.String(), `"error":"`+tc.code+`"`) {
					t.Errorf("answered %s, want %s", rec.Body.String(), tc.code)
				}
			}
		})
	}
}
