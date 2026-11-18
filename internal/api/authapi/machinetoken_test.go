package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MACHINE TOKEN MANAGES NO PROOF.
//
// A token acts as its owner, stepped up by construction, so the person and
// step-up checks both pass for it: whoever holds a pipeline's environment could
// enrol a second factor or read regenerated recovery codes. The proof routes and
// the password change are refused by the AUTHORITY TABLE (`token_refused`, the
// proof verb needing a person present), the step-up by its own check. Both
// answer `token_refused`, never `step_up_required`: no proof would lift it. The
// token carries ADMIN (ADR-0033), so no permission stands in; the REAL guard runs.
// Mutation: spare the proof verb's self arm in internal/authz, drop the
// step-up's check, or answer `step_up_required` again, and a row goes red.
func TestAMachineTokenManagesNoProof(t *testing.T) {
	t.Parallel()
	owner := uuid.Must(uuid.NewV7())
	secret, err := credential.NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	presented := credential.Token{
		ID: uuid.Must(uuid.NewV7()).String(), Position: 3, Secret: secret,
	}
	row := credential.TokenRow{
		Applied: 10, Found: true, IsToken: true,
		Verifier:  credential.TokenVerifier(presented.ID, secret),
		ExpiresAt: clock.Add(time.Hour),
		Grants:    []iam.Grant{iam.GrantAdmin},
		Owner: credential.TokenOwner{
			Found: true, ID: owner.String(), Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: "jane.doe",
			Grants: []iam.Grant{iam.GrantAdmin},
		},
	}
	b := bootstrapFor(t)
	arm, err := auth.NewTokens(auth.TokensDeps{
		Directory: oneTokenRow{row}, Chart: &seatChart{},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	writer := &credentialCounter{}
	mux := http.NewServeMux()
	buildWith(t, b, func(o *authapi.Options) { o.Writer = writer }).Routes(mux)
	guarded := auth.New(&b).WithTokens(arm).Middleware(mux)

	for path, reason := range map[string]authz.Reason{
		"/auth/totp/recovery": authz.ReasonTokenRefused,
		"/auth/totp":          authz.ReasonTokenRefused,
		// AND THE PASSWORD ITSELF, decided by the same row: a token that
		// could change its owner's password would take the account.
		"/auth/password": authz.ReasonTokenRefused,
		"/auth/step-up":  authz.ReasonTokenRefused,
	} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"password":"a-password-long-enough","code":"123456"}`))
		req.Header.Set("Authorization", "Bearer "+presented.Value())
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body[authz.DetailReason] != string(reason) ||
			body["error"] != string(httpjson.CodeTokenRefused) {
			t.Errorf("%s with a machine token answered %d %v, want 403 "+
				"token_refused naming %s", path, rec.Code, body, reason)
		}
	}
	if n := writer.count(); n != 0 {
		t.Errorf("the writer was asked to change the owner's credentials %d "+
			"times by a request carrying their token", n)
	}

	// THE CONTROL: the same owner, resolved WITHOUT a token and freshly
	// stepped up, regenerates — so the refusal above is the token's.
	req := httptest.NewRequest(http.MethodPost, "/auth/totp/recovery", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID: owner, Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, ReauthAt: clock.Add(time.Minute),
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || writer.count() != 1 {
		t.Errorf("the owner signed in and stepped up answered %d with %d "+
			"writes, want 200 and one", rec.Code, writer.count())
	}
}

// oneTokenRow is an identity directory holding one token.
type oneTokenRow struct{ row credential.TokenRow }

func (d oneTokenRow) MachineToken(context.Context, credential.Token) (credential.TokenRow, error) {
	return d.row, nil
}

// credentialCounter counts the credential writes it is asked for.
type credentialCounter struct {
	stubWriter
	mu sync.Mutex
	n  int
}

func (c *credentialCounter) SetCredentials(context.Context, iamdomain.CredentialSet) (
	statelog.Result, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return applied(statelog.Position{}), nil
}

func (c *credentialCounter) SetPassword(context.Context, iamdomain.PasswordSet) (
	statelog.Result, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return applied(statelog.Position{}), nil
}

func (c *credentialCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var _ session.Chart = (*seatChart)(nil)
