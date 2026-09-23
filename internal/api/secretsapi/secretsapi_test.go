package secretsapi_test

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

var clock = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func cipherFor(t *testing.T, ids ...string) secrets.Cipher {
	t.Helper()
	k := secrets.Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for _, id := range ids {
		sum := sha256.Sum256([]byte(id))
		k.Keys[id] = sum[:]
	}
	c, err := secrets.NewCipher(k)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// surface builds the routes over a memory fleet, with an operator attached
// the way the guard attaches one — carrying every grant, so a case about what
// the store does is not a case about authority.
func surface(t *testing.T, cipher secrets.Cipher, keyID string) (http.Handler, coord.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	return mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipher, ActiveKeyID: keyID,
		Now: func() time.Time { return clock },
	}, iam.AllGrants...), fleet
}

// mounted builds the surface and serves it as the operator `ops` carrying
// exactly these grants.
func mounted(t *testing.T, opts secretsapi.Options, grants ...iam.Grant) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := newService(t, opts).Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{
			ID:    uuid.NewSHA1(auth.TokenNamespace, []byte("ops")),
			Login: auth.TokenLogin("ops"), Kind: iam.KindMachine,
			Stage: iam.StageActive, Grants: grants,
		})))
	})
}

// newService builds the surface, failing the test on a wiring mistake.
func newService(t *testing.T, opts secretsapi.Options) *secretsapi.Service {
	t.Helper()
	svc, err := secretsapi.New(opts)
	if err != nil {
		t.Fatalf("secretsapi.New: %v", err)
	}
	return svc
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, reader))
	return rec.Code, rec.Body.String()
}

// A VALUE ROUND TRIPS AS RAW BYTES.
//
// A credential is arbitrary text — a PEM key has newlines, a token can hold
// anything — so the body IS the value. Any encoding step between the operator
// and the bytes the vendor compares is a 401 nobody can explain.
func TestAValueRoundTripsVerbatim(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	const value = "-----BEGIN KEY-----\nline two\n\ttabbed \n"

	if code, body := call(t, h, http.MethodPut, "/secrets/PEM", value); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	code, body := call(t, h, http.MethodGet, "/secrets/PEM?reveal=true", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	var out struct{ Value string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Value != value {
		t.Errorf("value = %q, want it byte for byte", out.Value)
	}
}

// A READ WITHOUT ?reveal=true NEVER CARRIES THE VALUE.
//
// This is the route a dashboard or a crawl reaches, and the overwhelmingly
// common question is "is it set". A value served here would put a credential
// into a browser history and a proxy log for a request nobody meant to make.
func TestAPlainReadCarriesNoValue(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	call(t, h, http.MethodPut, "/secrets/TOKEN", "glpat-not-real")

	code, body := call(t, h, http.MethodGet, "/secrets/TOKEN", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	if strings.Contains(body, "glpat") {
		t.Fatalf("a plain read served the credential: %s", body)
	}
	// AND NO VALUE FIELD AT ALL — see the listing's test for why an empty
	// one is not good enough.
	if strings.Contains(body, `"value"`) {
		t.Fatalf("a plain read has a value field at all: %s", body)
	}
	for _, want := range []string{`"key_id":"k1"`, `"updated_by":"ops"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the answer omits %s: %s", want, body)
		}
	}
}

// A REVEALED VALUE IS NEVER CACHEABLE. Without no-store it can sit in a
// shared proxy's cache — a credential leak with no log line anywhere and no
// way to find it afterwards.
func TestARevealedValueRefusesToBeCached(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	call(t, h, http.MethodPut, "/secrets/TOKEN", "v")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secrets/TOKEN?reveal=true", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A LISTING CARRIES NO ENVELOPE. It is what an operator prints, and the one
// thing that must never reach a terminal is the ciphertext.
func TestAListingCarriesNoEnvelope(t *testing.T) {
	t.Parallel()
	h, fleet := surface(t, cipherFor(t, "k1"), "k1")
	call(t, h, http.MethodPut, "/secrets/A", "first")
	call(t, h, http.MethodPut, "/secrets/B", "second")

	rec, _, err := fleet.Secret(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	code, body := call(t, h, http.MethodGet, "/secrets", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	if strings.Contains(body, rec.Value) {
		t.Fatalf("the listing carried the envelope: %s", body)
	}
	// THE KEY IS ABSENT, not merely empty. A `"value":""` in the shape
	// would be one field assignment away from carrying the real one, and
	// nothing downstream would notice until it did.
	if strings.Contains(body, `"value"`) {
		t.Fatalf("the listing has a value field at all: %s", body)
	}
	if strings.Index(body, `"A"`) > strings.Index(body, `"B"`) {
		t.Errorf("the listing is not name-ordered: %s", body)
	}
}

// UNSET IS IDEMPOTENT AND SAYS WHICH HAPPENED. A 404 for a name already gone
// would make a cleanup script fail on its second run; "it was not there" is
// the outcome the caller wanted, not an error.
func TestUnsetIsIdempotentAndReportsWhatItDid(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	call(t, h, http.MethodPut, "/secrets/A", "v")

	if code, body := call(t, h, http.MethodDelete, "/secrets/A", ""); code != http.StatusOK ||
		!strings.Contains(body, `"removed":true`) {
		t.Fatalf("first delete = %d %s", code, body)
	}
	if code, body := call(t, h, http.MethodDelete, "/secrets/A", ""); code != http.StatusOK ||
		!strings.Contains(body, `"removed":false`) {
		t.Fatalf("second delete = %d %s", code, body)
	}
}

// AN ABSENT NAME IS 404 WITH THE not_found BODY, which is what lets a client
// tell "no such secret" from "this node serves no /secrets at all" — and the
// provisioning sink acts oppositely on the two.
func TestAnAbsentNameIsAnswerableAsNotFound(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	for _, path := range []string{"/secrets/GONE", "/secrets/GONE?reveal=true"} {
		code, body := call(t, h, http.MethodGet, path, "")
		if code != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
			t.Errorf("%s = %d %s, want 404 not_found", path, code, body)
		}
	}
}

// A NODE WITH NO KEYRING REFUSES RATHER THAN STORING PLAINTEXT, and says what
// to run. A store that could hold unencrypted secrets is a footgun with no
// upside.
func TestWithoutAKeyringEveryWriteIsRefusedWithTheRemedy(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, nil, "")
	code, body := call(t, h, http.MethodPut, "/secrets/A", "v")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("PUT = %d %s, want 503", code, body)
	}
	if !strings.Contains(body, "keygen") {
		t.Errorf("the refusal does not say how to get a keyring: %s", body)
	}
}

// A VALUE OVER THE LIMIT IS REFUSED, not truncated. A silently shortened
// credential fails at the vendor with a 401 that names neither.
func TestAnOversizedValueIsRefused(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	code, _ := call(t, h, http.MethodPut, "/secrets/BIG",
		strings.Repeat("x", secretsapi.MaxValueBytes+1))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT = %d, want 413", code)
	}
}

// A REKEY RE-SEALS THE STALE ROWS AND NAMES THEM.
func TestRekeyMovesTheStaleRowsAndNamesThem(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	old := mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1", "k2"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...)
	call(t, old, http.MethodPut, "/secrets/A", "one")

	h := mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k2", "k1"), ActiveKeyID: "k2",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...)

	code, body := call(t, h, http.MethodPost, "/secrets/rekey", "")
	if code != http.StatusOK || !strings.Contains(body, `"moved":["A"]`) {
		t.Fatalf("rekey = %d %s", code, body)
	}
	if code, body = call(t, h, http.MethodGet, "/secrets/A", ""); !strings.Contains(body, `"key_id":"k2"`) {
		t.Fatalf("after the rekey A reads %d %s, want key-2", code, body)
	}
}

// A REKEY ONTO A KEY THIS NODE DOES NOT SEAL WITH IS REFUSED.
//
// A CLI whose Tier A names a different active key is rekeying onto a key the
// fleet will not use, and a silent success reports a completed rotation the
// operator is about to retire the old key on the strength of.
func TestRekeyRefusesAKeyIDThisNodeDoesNotUse(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	code, body := call(t, h, http.MethodPost, "/secrets/rekey?key_id=k9", "")
	if code != http.StatusConflict {
		t.Fatalf("rekey = %d %s, want 409", code, body)
	}
	for _, want := range []string{`"key_id":"k1"`, `"your_key_id":"k9"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal omits %s: %s", want, body)
		}
	}
}

// A SECRET NAMED "rekey" IS STILL ADDRESSABLE. The literal route and the
// wildcard share a path shape, and a name a company legitimately uses must
// not become unreachable because of it.
func TestASecretNamedRekeyIsStillReachable(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	if code, body := call(t, h, http.MethodPut, "/secrets/rekey", "v"); code != http.StatusOK {
		t.Fatalf("PUT /secrets/rekey = %d %s", code, body)
	}
	code, body := call(t, h, http.MethodGet, "/secrets/rekey?reveal=true", "")
	if code != http.StatusOK || !strings.Contains(body, `"value":"v"`) {
		t.Fatalf("GET = %d %s", code, body)
	}
}

// A FLEET IS REQUIRED, and a missing one is refused by name.
//
// Every node opens the fleet's store, so a nil is a wiring mistake, and an
// unregistered /secrets answering 404 would look like a deliberate answer.
func TestWithoutAFleetTheSurfaceIsRefused(t *testing.T) {
	t.Parallel()
	svc, err := secretsapi.New(secretsapi.Options{Cipher: cipherFor(t, "k1")})
	if err == nil {
		t.Fatalf("no fleet built a secrets surface: %v", svc)
	}
	if !strings.Contains(err.Error(), "Options.Fleet") {
		t.Errorf("the refusal does not name Options.Fleet: %v", err)
	}
}

// THE AUTHENTICATED OPERATOR IS THE AUTHOR, not anything the caller supplies.
// Provenance a client chooses answers nothing months later.
func TestTheAuthenticatedOperatorIsRecordedAsTheAuthor(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	call(t, h, http.MethodPut, "/secrets/A?source=gitlab-provision", "v")

	_, body := call(t, h, http.MethodGet, "/secrets/A", "")
	for _, want := range []string{`"updated_by":"ops"`, `"source":"gitlab-provision"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the row omits %s: %s", want, body)
		}
	}
}

// A NAME NO ${VAR} CAN REACH IS REFUSED, and that is not pedantry about
// spelling. The store is keyed by environment-variable name because that is
// what a reference in the company document resolves through, so a row stored
// under "gitlab-token" would be sealed, listed, reported as written, and read
// by nothing at all. The operator's only evidence would be a provider failing
// to authenticate hours later.
func TestAWriteRefusesANameNoReferenceCanEverName(t *testing.T) {
	h, fleet := surface(t, cipherFor(t, "k1"), "k1")
	for _, name := range []string{"gitlab-token", "my%20token", "9LIVES", "TOKEN.SUB"} {
		code, body := call(t, h, http.MethodPut, "/secrets/"+name, "value")
		if code != http.StatusBadRequest {
			t.Errorf("PUT /secrets/%s = %d, want 400: %s", name, code, body)
		}
		if !strings.Contains(body, "invalid_name") {
			t.Errorf("PUT /secrets/%s said %q, want an invalid_name refusal", name, body)
		}
	}
	// AND NOTHING WAS SEALED. A refusal that had already written the row
	// would be worse than accepting it.
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused write left %d row(s) behind", len(rows))
	}
}

// The refusal comes BEFORE the body is read, so an oversized value under a
// bad name is refused for the name rather than for its size: the name is the
// thing the caller has to change, and a 413 would point at the wrong half.
func TestABadNameIsRefusedBeforeTheValueIsRead(t *testing.T) {
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	code, body := call(t, h, http.MethodPut, "/secrets/bad-name",
		strings.Repeat("x", secretsapi.MaxValueBytes+1))
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_name") {
		t.Fatalf("PUT with a bad name and an oversized body = %d %s, want 400 invalid_name",
			code, body)
	}
}

// READING AND REMOVING TAKE THE NAME AS GIVEN. Only the write checks the
// grammar: a row that predates the check must stay removable, and a delete
// that refused the name would strand it forever.
func TestRemovingANameTheWritePathWouldRefuseStillWorks(t *testing.T) {
	h, fleet := surface(t, cipherFor(t, "k1"), "k1")
	// Written past the API, the way a build without the guard wrote it.
	if err := fleet.PutSecret(t.Context(), coord.SecretRecord{
		Name: "gitlab-token", Value: "enc:v1:k1:not-openable", KeyID: "k1",
		UpdatedAt: clock, UpdatedBy: "ops", Source: "cli",
	}); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	code, body := call(t, h, http.MethodDelete, "/secrets/gitlab-token", "")
	if code != http.StatusOK || !strings.Contains(body, `"removed":true`) {
		t.Fatalf("DELETE = %d %s, want 200 with removed:true", code, body)
	}
}

// EVERY ROUTE TAKES ITS GRANT, and the value takes one of its own.
//
// These routes decided nothing: the guard in front of them answers only
// whether somebody resolved, which meant "the operator" while an operator
// token was the only credential this engine had. A person signed in holding
// `state:read` alone could then list, reveal, overwrite and delete the
// company's credentials. So each route is stated against a caller holding
// exactly the grant it needs, and against one holding everything BUT that
// grant — the most authority a caller can have and still be refused, which is
// what makes the refusal about the rule. A refusal names the grant.
func TestEveryRouteTakesTheGrantItsVerbNames(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		method, path string
		body         string
		// needs is every grant the request takes, all of which the
		// admitted caller carries; each is then withheld in turn.
		needs []iam.Grant
	}{
		{"the listing", http.MethodGet, "/secrets", "",
			[]iam.Grant{iam.GrantConfigRead}},
		{"one row's metadata", http.MethodGet, "/secrets/A", "",
			[]iam.Grant{iam.GrantConfigRead}},
		// THE VALUE TAKES BOTH: the route's own grant, because the
		// metadata is the listing's, and the one the value needs.
		{"revealing the value", http.MethodGet, "/secrets/A?reveal=true", "",
			[]iam.Grant{iam.GrantConfigRead, iam.GrantSecretRead}},
		{"a write", http.MethodPut, "/secrets/B", "two",
			[]iam.Grant{iam.GrantSecretWrite}},
		{"a delete", http.MethodDelete, "/secrets/A", "",
			[]iam.Grant{iam.GrantSecretWrite}},
		{"a rekey", http.MethodPost, "/secrets/rekey", "",
			[]iam.Grant{iam.GrantSecretWrite}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			build := func(grants ...iam.Grant) http.Handler {
				fleet := coordmem.NewFleet()
				opts := secretsapi.Options{Fleet: fleet, Cipher: cipherFor(t, "k1"),
					ActiveKeyID: "k1", Now: func() time.Time { return clock }}
				seed := mounted(t, opts, iam.AllGrants...)
				if code, body := call(t, seed, http.MethodPut, "/secrets/A", "one"); code != http.StatusOK {
					t.Fatalf("seed = %d %s", code, body)
				}
				return mounted(t, opts, grants...)
			}
			if code, body := call(t, build(c.needs...), c.method, c.path, c.body); code >= 400 {
				t.Errorf("holding %v: %d %s, want it admitted", c.needs, code, body)
			}
			for _, withheld := range c.needs {
				rest := slices.DeleteFunc(slices.Clone(iam.AllGrants),
					func(g iam.Grant) bool { return g == withheld })
				code, body := call(t, build(rest...), c.method, c.path, c.body)
				if code != http.StatusForbidden {
					t.Errorf("holding everything but %s: %d %s, want 403",
						withheld, code, body)
					continue
				}
				var refusal map[string]any
				if err := json.Unmarshal([]byte(body), &refusal); err != nil {
					t.Fatalf("decode %s: %v", body, err)
				}
				grants, _ := refusal[authz.DetailGrants].([]any)
				if len(grants) != 1 || grants[0] != string(withheld) {
					t.Errorf("refused naming %v, want %s", refusal[authz.DetailGrants],
						withheld)
				}
			}
		})
	}
}

// AND A REFUSED REVEAL NEVER OPENS THE VALUE: the grant is asked before the
// store is read, so a caller without it cannot learn even whether the value
// would have decrypted — the answer is the same 403 for a name that exists and
// one that does not.
func TestARefusedRevealIsTheSameForANameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	h := mounted(t, secretsapi.Options{Fleet: coordmem.NewFleet(),
		Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1"}, iam.GrantConfigRead)
	present, _ := call(t, h, http.MethodGet, "/secrets/NOWHERE?reveal=true", "")
	if present != http.StatusForbidden {
		t.Errorf("revealing an absent name without secrets:read = %d, want 403: "+
			"a 404 would tell a caller without the grant which names exist",
			present)
	}
}
