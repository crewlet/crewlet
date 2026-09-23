package authapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MACHINE TOKEN MANAGES NO PROOF.
//
// A personal access token acts as its owner — a person, stepped up by
// construction — so the two checks every proof-management route already had
// (a person, and a fresh step-up) both pass for it. Without a refusal of its
// own, whoever holds a pipeline's environment could enrol their own second
// factor on the owner's account or regenerate the recovery codes and read them
// back. The request goes through the REAL guard so the credential shape is the
// guard's own answer. Mutation: drop [machineToken] from steppedUp and the
// recovery codes are regenerated for the token's holder.
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
		Grants:    []iam.Grant{iam.GrantStateRead},
		Owner: credential.TokenOwner{
			Found: true, ID: owner.String(), Kind: iam.KindPerson,
			Stage: iam.StageActive, Login: "jane.doe",
			Grants: []iam.Grant{iam.GrantStateRead},
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
	buildWith(t, b, nil, func(o *authapi.Options) { o.Writer = writer }).Routes(mux)
	guarded := auth.New(&b).WithTokens(arm).Middleware(mux)

	for _, path := range []string{"/auth/totp/recovery", "/auth/totp", "/auth/step-up"} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"password":"a-password-long-enough","code":"123456"}`))
		req.Header.Set("Authorization", "Bearer "+presented.Value())
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a machine token answered %d, want 403: %s", path,
				rec.Code, rec.Body.String())
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

func (d oneTokenRow) MachineToken(context.Context, string) (credential.TokenRow, error) {
	return d.row, nil
}

// credentialCounter counts the credential writes it is asked for.
type credentialCounter struct {
	stubWriter
	mu sync.Mutex
	n  int
}

func (c *credentialCounter) SetCredentials(context.Context, iamdomain.CredentialSet) (
	statelog.Position, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return statelog.Position{}, nil
}

func (c *credentialCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var _ session.Chart = (*seatChart)(nil)
