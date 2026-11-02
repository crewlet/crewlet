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
	"github.com/crewlet/crewlet/internal/iam/session"
)

// THE DASHBOARD READS EXACTLY THE SIGN-IN POLICY THE ENGINE SENDS: both
// interfaces member for member against their Go structs, and the second
// factor's union value for value against [iam.SecondFactors], in both
// directions.
func TestTheDashboardReadsExactlyTheSignInPolicyTheEngineSends(t *testing.T) {
	t.Parallel()
	heldBothWays(t, "SignInPolicyAnswer", reflect.TypeFor[queries.SignInPolicyAnswer](), 2)
	heldBothWays(t, "SignInPolicy", reflect.TypeFor[queries.SignInPolicy](), 5)

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

// THE SIGN-IN POLICY IS THIS NODE'S TIER A, AS THE ENGINE ENFORCES IT, AND AN
// ADMINISTRATOR'S.
//
// Every value is read through the accessor the sign-in path enforces it by, so
// a file that writes nothing answers the defaults rather than zeros, and one
// that writes a value answers that value. The answer names the node, since a
// fleet's nodes may disagree. A reader without admin is refused: the posture
// is no secret, but it is the deployment's, read beside the people and tokens
// it governs.
//
// Mutation: answer the raw settings rather than the accessors, and the
// defaults case reads zeros; register the question for everybody, and the
// reader is answered.
func TestTheSignInPolicyIsThisNodesTierA(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(*config.APIAuth)
		want  queries.SignInPolicy
	}{
		{"nothing written", func(*config.APIAuth) {}, queries.SignInPolicy{
			SecondFactor: iam.SecondFactorRequired, MinPasswordLength: iam.MinPasswordChars,
			SessionAbsoluteSeconds: int64(config.DefaultSessionAbsolute / time.Second),
			SessionIdleSeconds:     int64(session.Idle / time.Second),
			StepUpSeconds:          int64(config.DefaultSessionStepUp / time.Second),
		}},
		{"every setting written", func(a *config.APIAuth) {
			a.TOTP = iam.SecondFactorOptional
			a.MinPasswordLength = 16
			a.Session = config.APISession{AbsoluteRaw: "48h", StepUpRaw: "15m"}
		}, queries.SignInPolicy{
			SecondFactor: iam.SecondFactorOptional, MinPasswordLength: 16,
			SessionAbsoluteSeconds: 48 * 3600,
			SessionIdleSeconds:     int64(session.Idle / time.Second),
			StepUpSeconds:          15 * 60,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			authorize(&b,
				config.APIToken{ID: "ops", Token: orgOpsToken},
				config.APIToken{ID: "reader", Token: orgReaderToken, Grants: []iam.Grant{iam.GrantWorklet}},
			)
			tc.write(&b.API.Auth)
			a := newApp(t, api.Options{Bootstrap: &b,
				Sources: queries.Sources{NodeID: "node-a"}})

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
			if got.Policy != tc.want {
				t.Errorf("the policy is %+v, want %+v", got.Policy, tc.want)
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
