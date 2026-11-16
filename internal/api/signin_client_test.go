package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DASHBOARD READS EXACTLY THE SIGN-IN POLICY THE ENGINE SENDS: each
// interface member for member against its Go struct, and the second factor's
// union value for value against [iam.SecondFactors], in both directions.
func TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends(t *testing.T) {
	t.Parallel()
	heldBothWays(t, "SignInPolicyAnswer", reflect.TypeFor[queries.SignInPolicyAnswer](), 5)
	heldBothWays(t, "SignInPolicy", reflect.TypeFor[queries.SignInPolicy](), 5)
	heldBothWays(t, "SignInLimits", reflect.TypeFor[queries.SignInLimits](), 5)

	client, err := clientsource.Union(clientsource.Tree(t), "SecondFactorRule")
	if err != nil {
		t.Fatal(err)
	}
	engine := backendNames(iam.SecondFactors)
	slices.Sort(engine)
	slices.Sort(client)
	if !slices.Equal(engine, client) {
		t.Errorf("the dashboard's SecondFactorRule union is %v and the engine sends %v",
			client, engine)
	}
}

// THE SIGN-IN POLICY IS THE COMPANY'S, WITHIN THIS NODE'S LIMITS, AND AN
// ADMINISTRATOR'S (ADR-0152). Mutation: answer the policy unheld, or read the
// ranges off config.RangeOf, and the strict case admits a month.
func TestTheSignInPolicyIsTheCompanysWithinThisNodesLimits(t *testing.T) {
	t.Parallel()
	month := 30 * 24 * time.Hour
	company := &config.Company{Name: "Acme", SignIn: config.SignIn{
		RequireSecondFactor: new(false), SessionLifetimeSeconds: new(month.Seconds()),
	}}
	for _, tc := range []struct {
		name     string
		limits   config.SignInLimits
		enforced queries.SignInPolicy
		most     float64
	}{
		{"a node limiting nothing", config.SignInLimits{SecondFactor: iam.SecondFactorOptional},
			queries.SignInPolicy{
				SecondFactor: iam.SecondFactorOptional, MinPasswordLength: iam.MinPasswordChars,
				SessionAbsoluteSeconds: int64(month.Seconds()),
				SessionIdleSeconds:     int64(config.DefaultIdleTimeout.Seconds()),
				StepUpSeconds:          int64(config.DefaultStepUp.Seconds()),
			}, month.Seconds()},
		{"a strict node", config.SignInLimits{MaxSessionLifetimeRaw: "168h"},
			queries.SignInPolicy{
				SecondFactor: iam.SecondFactorRequired, MinPasswordLength: iam.MinPasswordChars,
				SessionAbsoluteSeconds: 7 * 24 * 3600,
				SessionIdleSeconds:     int64(config.DefaultIdleTimeout.Seconds()),
				StepUpSeconds:          int64(config.DefaultStepUp.Seconds()),
			}, 7 * 24 * 3600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			authorize(&b,
				config.APIToken{ID: "ops", Token: orgOpsToken},
				config.APIToken{ID: "reader", Token: orgReaderToken, Grants: []iam.Grant{iam.GrantWorklet}},
			)
			b.API.Auth.Limits = tc.limits
			a := newApp(t, api.Options{Bootstrap: &b, Sources: queries.Sources{
				NodeID: "node-a", Company: companySource(t, company)}})

			status, body := querySignIn(t, a, orgReaderToken)
			if status != http.StatusForbidden {
				t.Errorf("a reader without admin was answered %d, want 403: %s", status, body)
			}
			status, body = querySignIn(t, a, orgOpsToken)
			if status != http.StatusOK {
				t.Fatalf("an admin was answered %d: %s", status, body)
			}
			var got queries.SignInPolicyAnswer
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Node != "node-a" {
				t.Errorf("the answer names node %q, want node-a", got.Node)
			}
			if got.Policy != tc.enforced {
				t.Errorf("the node enforces %+v, want %+v", got.Policy, tc.enforced)
			}
			var lifetime *queries.LimitRow
			for i := range got.Settings {
				if got.Settings[i].Key == config.SignInSessionLifetimeKey {
					lifetime = &got.Settings[i]
				}
			}
			if lifetime == nil || lifetime.Value != month.Seconds() || lifetime.Range == nil ||
				lifetime.Range.Max == nil || *lifetime.Range.Max != tc.most {
				t.Errorf("the lifetime row is %+v, want the company's month with a range "+
					"up to %v", lifetime, tc.most)
			}
			if got.SecondFactorSetting.Value != false || !got.SecondFactorSetting.Set {
				t.Errorf("the second factor row is %+v, want the company's false, set",
					got.SecondFactorSetting)
			}
		})
	}
}

// querySignIn is GET /query/sign_in_policy as the holder of token reads it.
func querySignIn(t *testing.T, a *api.App, token string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/query/sign_in_policy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}
