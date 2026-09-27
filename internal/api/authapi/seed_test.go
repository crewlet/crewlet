package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SECOND FACTOR'S SEED IS SEALED BEFORE IT ENTERS ANY RECORD, AND STILL
// SIGNS ITS HOLDER IN.
//
// The seed is the one credential this estate keeps as a secret, and it went
// into the credential set in the clear — so into the person's document, their
// credential row and the trail entry, on every node, in every snapshot and
// every backup, and readable off the cluster port by whoever could read a
// stream. The enrolment now seals it under the person's key, bound to the
// credential it is enrolled as, before the write is formed: nothing the writer
// is handed carries it, and the credential opens back to it as that credential
// only. And sealing costs nothing at the door: the next sign-in checks a code
// against it as before.
//
// Mutation: store in.Secret rather than the sealed value, and the set carries
// the seed.
func TestASecondFactorsSeedIsSealedBeforeItEntersAnyRecord(t *testing.T) {
	t.Parallel()
	r := newSignInRig(t)
	secret, err := credential.NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	step := credential.TOTPStep(clock)
	code, err := credential.TOTPCode(secret, step)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"secret": secret, "code": code})
	if rec := r.asPerson(http.MethodPost, "/auth/totp", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("the enrolment answered %d (%s)", rec.Code, rec.Body)
	}

	r.estate.mu.Lock()
	held := r.estate.person.Credentials
	written, _ := json.Marshal(held)
	var app iamdomain.Credential
	for _, c := range held {
		if c.Method == iamdomain.MethodTOTP {
			app = c
		}
	}
	r.estate.mu.Unlock()
	if strings.Contains(string(written), secret) {
		t.Fatalf("the credential set the writer was handed carries the seed "+
			"in the clear: %s", written)
	}
	if app.ID == "" || app.ID == "app" {
		t.Fatalf("no new second factor was stored: %+v", held)
	}
	opened, err := stubSealer{}.OpenCredential(t.Context(), r.estate.person.ID,
		app.ID, iamdomain.FieldTOTP, app.Verifier)
	if err != nil || opened != secret {
		t.Fatalf("the stored seed opens as (%q, %v), want the seed enrolled", opened, err)
	}

	// THE ENROLMENT'S OWN CODE IS SPENT, so the sign-in presents the next.
	next, err := credential.TOTPCode(secret, step+1)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.login(t, "jane.doe", password, next); got != http.StatusOK {
		t.Errorf("a code from the sealed seed answered %d, want the sign-in", got)
	}
}

// A SEED THAT DOES NOT OPEN IS NEVER A MATCH — AND AN OUTAGE IS NOT A WRONG
// CODE.
//
// A seed that is not this credential's — pasted from another credential, or
// enrolled in the clear before seeds were sealed — is refused exactly as a
// wrong code is: the one generic refusal, and no session. A key store this node
// cannot reach is the unknown arm: 503 with a Retry-After, because answering
// it as a wrong code would send somebody to re-type a code that was right.
//
// The CONTROL is the rig's own seed, sealed as its credential, which signs in.
// Mutation: fall back to the stored value when it does not open and the
// in-the-clear row signs in; answer an unreachable store as a refusal and its
// row is 401.
func TestASeedThatDoesNotOpenIsNeverAMatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		seed   func(t *testing.T, person string) string
		sealer authapi.Sealer
		status int
		code   httpjson.Code
	}{
		{"the credential's own sealed seed (the control)",
			func(t *testing.T, person string) string {
				return sealedSeed(t, person, "app", totpSeed)
			}, stubSealer{}, http.StatusOK, ""},
		{"a seed sealed for another credential",
			func(t *testing.T, person string) string {
				return sealedSeed(t, person, "a-replaced-credential", totpSeed)
			}, stubSealer{}, http.StatusUnauthorized, httpjson.CodeSignInRefused},
		{"a seed written in the clear",
			func(*testing.T, string) string { return totpSeed },
			stubSealer{}, http.StatusUnauthorized, httpjson.CodeSignInRefused},
		{"a key store this node cannot reach",
			func(t *testing.T, person string) string {
				return sealedSeed(t, person, "app", totpSeed)
			}, unreachableKeys{}, http.StatusServiceUnavailable, httpjson.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newSignInRigWith(t, func(o *authapi.Options) { o.Sealer = tc.sealer })
			r.estate.mu.Lock()
			for i, c := range r.estate.person.Credentials {
				if c.Method == iamdomain.MethodTOTP {
					r.estate.person.Credentials[i].Verifier = tc.seed(t, r.estate.person.ID)
				}
			}
			r.estate.mu.Unlock()
			rec := r.signIn(t, "jane.doe", password, appCode(t, clock))
			if rec.Code != tc.status {
				t.Fatalf("answered %d (%s), want %d", rec.Code, rec.Body, tc.status)
			}
			if tc.status == http.StatusOK {
				return
			}
			var answer map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &answer)
			if answer["error"] != string(tc.code) {
				t.Errorf("answered %v, want %s", answer["error"], tc.code)
			}
			if tc.status == http.StatusServiceUnavailable &&
				rec.Header().Get("Retry-After") == "" {
				t.Error("an outage answered 503 with no Retry-After")
			}
			r.estate.mu.Lock()
			opened := len(r.estate.starts)
			r.estate.mu.Unlock()
			if opened != 0 {
				t.Errorf("a session was opened on a seed that did not open")
			}
		})
	}
}

// unreachableKeys is a sealer whose key store cannot be reached: every open of
// a credential secret fails with an error that is neither refusal.
type unreachableKeys struct{ stubSealer }

func (unreachableKeys) OpenCredential(context.Context, string, string,
	iamdomain.Field, string) (string, error) {

	return "", errors.New("the coordination store is unreachable")
}
