package secretsapi_test

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
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
// the way the guard attaches one.
func surface(t *testing.T, cipher secrets.Cipher, keyID string) (http.Handler, coord.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	svc := secretsapi.New(secretsapi.Options{
		Fleet: fleet, Cipher: cipher, ActiveKeyID: keyID,
		Now: func() time.Time { return clock },
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "ops")))
	}), fleet
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
	mux := http.NewServeMux()
	secretsapi.New(secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1", "k2"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}).Routes(mux)
	old := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "ops")))
	})
	call(t, old, http.MethodPut, "/secrets/A", "one")

	rotated := http.NewServeMux()
	secretsapi.New(secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k2", "k1"), ActiveKeyID: "k2",
		Now: func() time.Time { return clock },
	}).Routes(rotated)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rotated.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "ops")))
	})

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

// A PROCESS THAT CANNOT REACH THE FLEET SERVES NO ROUTES AT ALL.
//
// 404 is the honest answer for a surface this process does not implement; an
// endpoint that exists and answers 503 to everything reads as broken.
func TestWithoutAFleetTheRoutesAreAbsentRatherThanBroken(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	secretsapi.New(secretsapi.Options{Cipher: cipherFor(t, "k1")}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secrets", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /secrets = %d, want 404 on a process with no fleet store", rec.Code)
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
