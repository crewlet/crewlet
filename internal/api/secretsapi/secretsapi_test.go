package secretsapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
	// FRESH IN BOTH STEP-UP WINDOWS, as the guard stamps every Tier A
	// token, so a case here is decided on its grants alone.
	return mountedAs(t, opts, iam.Principal{
		ID:    uuid.NewSHA1(auth.TokenNamespace, []byte("ops")),
		Login: iam.TokenLogin("ops"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: grants,
		ReauthAt: time.Now().Add(time.Hour),
	})
}

// mountedAs builds the surface and serves it as caller.
func mountedAs(t *testing.T, opts secretsapi.Options, caller iam.Principal) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := newService(t, opts).Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), caller)))
	})
}

// newService builds the surface, failing the test on a wiring mistake.
//
// A CASE THAT NAMES NO COMPANY writes into one that reads none of its
// secrets, so it is about the store alone and re-activates nothing.
func newService(t *testing.T, opts secretsapi.Options) *secretsapi.Service {
	t.Helper()
	if opts.Config == nil {
		opts.Config = &company{}
	}
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
	for _, want := range []string{`"key_id":"k1"`, `"updated_by":"token:ops"`} {
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

// WITHOUT A KEYRING THERE IS NO SURFACE, and the refusal names the field.
//
// Built with no keyring, this surface used to answer every route 503
// no_keyring and tell the caller to run `crewlet secrets keygen` — a posture
// for a node with no keyring, which cannot exist: validation refuses a Tier A
// file without secrets.keys and the engine refuses to start. So a nil cipher,
// or a keyring with no active key to rekey onto, is the wiring mistake a nil
// fleet is, and it fails the boot by name.
func TestWithoutAKeyringTheSurfaceIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		opts  secretsapi.Options
		field string
	}{
		{"no keyring", secretsapi.Options{Fleet: coordmem.NewFleet(),
			ActiveKeyID: "k1"}, "Options.Cipher"},
		{"no active key", secretsapi.Options{Fleet: coordmem.NewFleet(),
			Cipher: cipherFor(t, "k1")}, "Options.ActiveKeyID"},
		{"no config surface", secretsapi.Options{Fleet: coordmem.NewFleet(),
			Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1"}, "Options.Config"},
	} {
		svc, err := secretsapi.New(c.opts)
		if err == nil {
			t.Errorf("%s: built a secrets surface: %v", c.name, svc)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: the refusal does not name %s: %v", c.name, c.field, err)
		}
	}
}

// EVERY REFUSAL HERE CARRIES ITS SENTENCE, AND NONE OF THEM SAYS WHEN.
//
// This surface built its own `{"error": code}` bodies, so the credential
// screen rendered a code and no line a person could act on. And none of these
// clears by waiting — a name is refused for its shape and a rekey for naming
// another key — so a Retry-After on one would teach a client to repeat it.
func TestARefusalCarriesItsSentenceAndNoRetryAfter(t *testing.T) {
	t.Parallel()
	keyed, _ := surface(t, cipherFor(t, "k1"), "k1")
	for _, c := range []struct {
		name         string
		h            http.Handler
		method, path string
		status       int
		code         httpjson.Code
	}{
		{"a name no reference can name", keyed, http.MethodPut, "/secrets/not-a-name",
			http.StatusBadRequest, httpjson.CodeInvalidName},
		{"a rekey onto a key this node does not seal with", keyed, http.MethodPost,
			"/secrets/rekey?key_id=k9", http.StatusConflict, httpjson.CodeKeyIDMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			c.h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader("v")))
			if rec.Code != c.status {
				t.Fatalf("answered %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v: %s", err, rec.Body)
			}
			if body["error"] != string(c.code) {
				t.Errorf("error = %v, want %s", body["error"], c.code)
			}
			if body["message"] != c.code.Message() {
				t.Errorf("message = %v, want the code's own sentence", body["message"])
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Errorf("Retry-After = %q over a refusal no wait clears", got)
			}
		})
	}
}

// A VALUE OVER THE LIMIT IS REFUSED, not truncated. A silently shortened
// credential fails at the vendor with a 401 that names neither.
func TestAnOversizedValueIsRefused(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	code, _ := call(t, h, http.MethodPut, "/secrets/BIG",
		strings.Repeat("x", secrets.MaxValueBytes+1))
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

// THE ENGINE'S OWN KEYS ARE UNREACHABLE HERE, whatever the caller holds.
//
// The directory's blind-index key lives in the same bucket as the operator's
// credentials. When it carried an operator name, this surface listed it and a
// DELETE orphaned every address in the directory. Every route that takes a name
// now refuses one BY NAME — including a caller carrying every grant — the
// listing counts them without naming any, and a rekey moves and counts them.
func TestTheEnginesOwnKeysAreUnreachableHere(t *testing.T) {
	t.Parallel()
	const key = "iam/blind-index-key"
	fleet := coordmem.NewFleet()
	if created, err := fleetsecrets.New(fleet, cipherFor(t, "k1", "k2")).Estate().Create(
		t.Context(), key, "the-engine-key", secrets.Author{Name: "node-a",
			Kind: string(iam.ActorSystem)}, secrets.SourceIAM, clock); err != nil || !created {
		t.Fatalf("seed the engine key: (%v, %v)", created, err)
	}
	h := mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1", "k2"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...)
	call(t, h, http.MethodPut, "/secrets/GITLAB_TOKEN", "glpat")

	path := "/secrets/" + url.PathEscape(key)
	for _, tc := range []struct{ name, method, path, body string }{
		{"a metadata read", http.MethodGet, path, ""},
		{"a reveal", http.MethodGet, path + "?reveal=true", ""},
		{"an overwrite", http.MethodPut, path, "overwritten"},
		{"a delete", http.MethodDelete, path, ""},
	} {
		code, body := call(t, h, tc.method, tc.path, tc.body)
		if code != http.StatusForbidden || !strings.Contains(body, `"reserved_name"`) {
			t.Errorf("%s of an engine key answered %d %s, want 403 reserved_name",
				tc.name, code, body)
		}
		if strings.Contains(body, "the-engine-key") {
			t.Errorf("%s of an engine key carried its value: %s", tc.name, body)
		}
	}
	if got, err := fleetsecrets.New(fleet, cipherFor(t, "k1", "k2")).Estate().Get(
		t.Context(), key); err != nil || got != "the-engine-key" {
		t.Fatalf("after every refused gesture the engine key reads %q (%v)", got, err)
	}

	code, body := call(t, h, http.MethodGet, "/secrets", "")
	if code != http.StatusOK || strings.Contains(body, key) ||
		!strings.Contains(body, `"engine_keys":{"total":1,"by_key":{"k1":1}}`) {
		t.Errorf("the listing = %d %s, want the operator row named and the "+
			"engine key counted", code, body)
	}

	rotated := mounted(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k2", "k1"), ActiveKeyID: "k2",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...)
	code, body = call(t, rotated, http.MethodPost, "/secrets/rekey", "")
	if code != http.StatusOK || !strings.Contains(body, `"engine_keys_moved":1`) ||
		strings.Contains(body, key) {
		t.Errorf("rekey = %d %s, want the engine key moved and counted", code, body)
	}
}

// fakeIdentity is the identity estate's half of a rotation, as the rekey and
// the listing ask it.
type fakeIdentity struct {
	count    iamdomain.SealedCount
	report   iamdomain.ResealReport
	err      error
	resealed int
}

func (f *fakeIdentity) SealedKeys(context.Context) (iamdomain.SealedCount, error) {
	return f.count, nil
}

func (f *fakeIdentity) Reseal(context.Context) (iamdomain.ResealReport, error) {
	f.resealed++
	return f.report, f.err
}

// THE LISTING COUNTS THE ASSISTANT'S SEALED VALUES BY KEY, which a rotation
// waits out rather than moves: an operator retires the old key on "nothing is
// under it", and a listing that left the conversations out read nothing under
// it while live conversations still were. A node given no assistant estate
// says so with a null. Mutation: drop the count from the listing and the
// first case fails.
func TestAListingCountsTheAssistantsSealedValues(t *testing.T) {
	t.Parallel()
	opts := secretsapi.Options{Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"),
		ActiveKeyID: "k1", Now: func() time.Time { return clock },
		Assistant: fakeAssistant{assistantdomain.SealedCount{
			Values: secrets.KeyCounts{"k0": 4, "k1": 2}}}}
	code, body := call(t, mounted(t, opts, iam.AllGrants...), http.MethodGet, "/secrets", "")
	if code != http.StatusOK ||
		!strings.Contains(body, `"assistant_values":{"values":{"k0":4,"k1":2}}`) {
		t.Errorf("the listing = %d %s, want the assistant's values counted by key", code, body)
	}
	opts.Assistant = nil
	code, body = call(t, mounted(t, opts, iam.AllGrants...), http.MethodGet, "/secrets", "")
	if code != http.StatusOK || !strings.Contains(body, `"assistant_values":null`) {
		t.Errorf("a node with no assistant estate listed %d %s, want it said", code, body)
	}
}

// fakeAssistant is an assistant estate holding count.
type fakeAssistant struct{ count assistantdomain.SealedCount }

func (f fakeAssistant) SealedKeys(context.Context) (assistantdomain.SealedCount, error) {
	return f.count, nil
}

// THE LISTING COUNTS THE SEATS' EXECUTOR SESSIONS BY KEY, for the assistant's
// reason: each session is an object sealed under the keyring that leaves only
// with its row, so a key dropped under a live one leaves that session
// unreadable and its next turn starting fresh, and a listing that left them
// out read nothing under the old key while they still were. A node given no
// executor estate says so with a null. Mutation: drop the count from the
// listing and the first case fails.
func TestAListingCountsTheExecutorSessionsSealedValues(t *testing.T) {
	t.Parallel()
	opts := secretsapi.Options{Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"),
		ActiveKeyID: "k1", Now: func() time.Time { return clock },
		Executor: fakeExecutor{executordomain.SealedCount{
			Values: secrets.KeyCounts{"k0": 3, "k1": 5}}}}
	code, body := call(t, mounted(t, opts, iam.AllGrants...), http.MethodGet, "/secrets", "")
	if code != http.StatusOK ||
		!strings.Contains(body, `"executor_values":{"values":{"k0":3,"k1":5}}`) {
		t.Errorf("the listing = %d %s, want the executor sessions counted by key", code, body)
	}
	opts.Executor = nil
	code, body = call(t, mounted(t, opts, iam.AllGrants...), http.MethodGet, "/secrets", "")
	if code != http.StatusOK || !strings.Contains(body, `"executor_values":null`) {
		t.Errorf("a node with no executor estate listed %d %s, want it said", code, body)
	}
}

// fakeExecutor is an executor estate holding count.
type fakeExecutor struct{ count executordomain.SealedCount }

func (f fakeExecutor) SealedKeys(context.Context) (executordomain.SealedCount, error) {
	return f.count, nil
}

// A REKEY IS THE WHOLE ROTATION, THE IDENTITY ESTATE INCLUDED.
//
// The keyring that seals the store also seals every person's name, address and
// second factor, in rows only a record can change — so a rekey that moved the
// store alone reported a finished rotation while every person's values were
// still under the key the operator was about to drop. The rekey now moves both
// and counts both, the listing counts both, and a node that runs no identity
// estate SAYS so (`null`) rather than reading as an estate with nothing to
// move. An outcome nobody could confirm is `rekey_incomplete`, because the old
// key is dropped on the strength of this answer.
//
// Mutation: drop the call to Reseal and the first case fails.
func TestARekeyMovesTheIdentityEstateToo(t *testing.T) {
	t.Parallel()
	opts := func(identity secretsapi.Identity) secretsapi.Options {
		return secretsapi.Options{
			Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"),
			ActiveKeyID: "k1", Identity: identity,
			Now: func() time.Time { return clock },
		}
	}

	moved := &fakeIdentity{
		count: iamdomain.SealedCount{People: map[string]int{"k0": 3},
			Invitations: map[string]int{"k0": 1}},
		report: iamdomain.ResealReport{People: []string{"p1", "p2"}, Values: 5},
	}
	h := mounted(t, opts(moved), iam.AllGrants...)
	code, body := call(t, h, http.MethodGet, "/secrets", "")
	if code != http.StatusOK || !strings.Contains(body,
		`"identity_values":{"people":{"k0":3},"invitations":{"k0":1}}`) {
		t.Errorf("the listing = %d %s, want the identity estate's values counted "+
			"by key", code, body)
	}
	code, body = call(t, h, http.MethodPost, "/secrets/rekey", "")
	if code != http.StatusOK || moved.resealed != 1 ||
		!strings.Contains(body, `"identity":{"people":2,"values":5}`) ||
		strings.Contains(body, "p1") {
		t.Errorf("rekey = %d %s (re-sealed %d times), want the identity estate "+
			"moved once and counted, naming nobody", code, body, moved.resealed)
	}

	absent := mounted(t, opts(nil), iam.AllGrants...)
	if code, body := call(t, absent, http.MethodGet, "/secrets", ""); code !=
		http.StatusOK || !strings.Contains(body, `"identity_values":null`) {
		t.Errorf("a node with no identity estate listed %d %s, want it said", code, body)
	}
	if code, body := call(t, absent, http.MethodPost, "/secrets/rekey", ""); code !=
		http.StatusOK || !strings.Contains(body, `"identity":null`) {
		t.Errorf("a node with no identity estate rekeyed %d %s, want it said", code, body)
	}

	for name, identity := range map[string]*fakeIdentity{
		"an outcome nobody could confirm": {report: iamdomain.ResealReport{
			People: []string{"p1"}, Values: 2, Unknown: []string{"p2"}}},
		"a person whose values would not open": {
			report: iamdomain.ResealReport{People: []string{"p1"}, Values: 2},
			err:    errors.New("open p2's name: secret could not be decrypted")},
	} {
		h := mounted(t, opts(identity), iam.AllGrants...)
		code, body := call(t, h, http.MethodPost, "/secrets/rekey", "")
		if code != http.StatusInternalServerError ||
			!strings.Contains(body, `"rekey_incomplete"`) ||
			!strings.Contains(body, `"identity":{"people":1,"values":2}`) {
			t.Errorf("%s: rekey = %d %s, want rekey_incomplete carrying what "+
				"did move", name, code, body)
		}
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

// THE AUTHENTICATED OPERATOR IS THE AUTHOR, AND THE ROUTE IS THE SOURCE — not
// anything the caller supplies. Provenance a client chooses answers nothing
// months later: `?source=` once stored any word it was sent, so a row read as
// having arrived by whatever path its writer claimed. Mutation: stamp any
// other word on this route and both the answer and the row fail.
func TestTheAuthenticatedOperatorIsRecordedAsTheAuthor(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	code, written := call(t, h, http.MethodPut, "/secrets/A", "v")
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, written)
	}
	_, row := call(t, h, http.MethodGet, "/secrets/A", "")
	for name, body := range map[string]string{"the write's answer": written,
		"the row": row} {
		for _, want := range []string{`"updated_by":"token:ops"`,
			`"updated_by_kind":"operator"`, `"operator_id":"token:ops"`,
			`"source":"api"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s omits %s: %s", name, want, body)
			}
		}
	}
}

// A WRITE THAT NAMES ITS OWN PROVENANCE IS REFUSED, never quietly stamped
// `api`: a client still sending `?source=` would be told its write landed
// while the row said otherwise, and one that judged its own rows by that word
// would read each as overwritten and write it again, for ever. So any
// parameter is `400 invalid_query`, named, before the body is read — an
// oversized value is refused for its query, not its size — and nothing is
// sealed. Mutation: drop the check and every case here stores a row.
func TestAWriteThatNamesItsProvenanceIsRefused(t *testing.T) {
	t.Parallel()
	h, fleet := surface(t, cipherFor(t, "k1"), "k1")
	for _, tc := range []struct {
		query, body string
		names       []string
	}{
		{"source=operator", "v", []string{`"source"`}},
		{"source=api", "v", []string{`"source"`}},
		{"by=ops&source=cli", "v", []string{`"by"`, `"source"`}},
		{"source=operator", strings.Repeat("x", secrets.MaxValueBytes+1),
			[]string{`"source"`}},
	} {
		code, raw := call(t, h, http.MethodPut, "/secrets/A?"+tc.query, tc.body)
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("?%s: the refusal is not JSON: %s", tc.query, raw)
		}
		if code != http.StatusBadRequest || body["error"] != string(httpjson.CodeInvalidQuery) {
			t.Errorf("?%s = %d %v, want 400 invalid_query", tc.query, code, body["error"])
		}
		detail, _ := body["detail"].(string)
		for _, name := range tc.names {
			if !strings.Contains(detail, name) {
				t.Errorf("?%s: the detail %q does not name %s", tc.query, detail, name)
			}
		}
		hint, _ := body["hint"].(string)
		for _, want := range []string{"updated_by", "operator_id", "api"} {
			if !strings.Contains(hint, want) {
				t.Errorf("?%s: the hint %q does not say %s", tc.query, hint, want)
			}
		}
	}
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatalf("SecretValues: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused write left %d row(s) behind", len(rows))
	}
}

// A PERSON'S TOKEN RECORDS THE PERSON AS THE AUTHOR, AND THE TOKEN BESIDE THEM.
//
// The row had room for one name and took the credential: `pat:<id>`, whose own
// row — the one thing saying whose it was — the identity sweep deletes a week
// after it lapses. So a credential in the store named, a month on, nobody. The
// write's answer says the same thing the row does, because it is the only
// confirmation a client that could not choose the author gets. Mutation:
// record the credential as the author and every assertion here fails.
func TestAPersonsTokenIsRecordedBesideThePerson(t *testing.T) {
	t.Parallel()
	const pat = "pat:0192f00d-0000-7000-8000-00000000000a"
	h := mountedAs(t, secretsapi.Options{
		Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}, iam.Principal{
		ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, Via: pat,
		ReauthAt: time.Now().Add(time.Hour),
	})
	code, written := call(t, h, http.MethodPut, "/secrets/A", "v")
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, written)
	}
	_, row := call(t, h, http.MethodGet, "/secrets/A", "")
	for name, body := range map[string]string{"the write's answer": written,
		"the row": row} {
		for _, want := range []string{`"updated_by":"jane.doe"`,
			`"updated_by_kind":"operator"`, `"operator_id":"` + pat + `"`,
			`"source":"api"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s omits %s: %s", name, want, body)
			}
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
		strings.Repeat("x", secrets.MaxValueBytes+1))
	if code != http.StatusBadRequest || !strings.Contains(body, "invalid_name") {
		t.Fatalf("PUT with a bad name and an oversized body = %d %s, want 400 invalid_name",
			code, body)
	}
}

// A NAME OUTSIDE THE GRAMMAR NAMES NO SECRET. Only the write checks it, so a
// read or a delete of one is answered as the absent name it is — not_found
// and removed:false — rather than as a malformed request.
func TestANameTheWritePathWouldRefuseNamesNoSecret(t *testing.T) {
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	if code, body := call(t, h, http.MethodGet, "/secrets/gitlab-token", ""); code != http.StatusNotFound {
		t.Errorf("GET = %d %s, want 404", code, body)
	}
	code, body := call(t, h, http.MethodDelete, "/secrets/gitlab-token", "")
	if code != http.StatusOK || !strings.Contains(body, `"removed":false`) {
		t.Fatalf("DELETE = %d %s, want 200 with removed:false", code, body)
	}
}

// EVERY ROUTE IS ADMIN'S, the value included.
//
// These routes decided nothing: the guard in front of them answers only
// whether somebody resolved, which meant "the operator" while an operator
// token was the only credential this engine had. A person signed in holding a
// read-only permission alone could then list, reveal, overwrite and delete the
// company's credentials. So each route is stated against a caller holding
// admin, and against one holding EVERY APP — the most a caller can carry
// without admin, and still refused, which is what makes the refusal about the
// rule (ADR-0033: a listing of the company's credentials is not part of
// anybody's overview, and no app opens it). A refusal names admin and nothing
// else — the minimal grant that would admit.
func TestEveryRouteTakesTheGrantItsVerbNames(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		method, path string
		body         string
	}{
		{"the listing", http.MethodGet, "/secrets", ""},
		{"one row's metadata", http.MethodGet, "/secrets/A", ""},
		// THE VALUE IS ITS OWN VERB, `secrets.reveal`, asked on top of the
		// route's once the flag is read: admin as the listing is, so a
		// caller holding every app is refused it naming admin too.
		{"revealing the value", http.MethodGet, "/secrets/A?reveal=true", ""},
		// AND SEVERAL AT ONCE, which is the same verb mounted on its
		// own route: a caller holding every app is refused it naming
		// admin, before the body is read.
		{"revealing several values", http.MethodPost, "/secrets/reveal",
			`{"names":["A"]}`},
		{"a write", http.MethodPut, "/secrets/B", "two"},
		{"a delete", http.MethodDelete, "/secrets/A", ""},
		{"a rekey", http.MethodPost, "/secrets/rekey", ""},
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
			if code, body := call(t, build(iam.GrantAdmin), c.method, c.path, c.body); code >= 400 {
				t.Errorf("holding admin: %d %s, want it admitted", code, body)
			}
			code, body := call(t, build(iam.Apps...), c.method, c.path, c.body)
			if code != http.StatusForbidden {
				t.Fatalf("holding every app and not admin: %d %s, want 403",
					code, body)
			}
			var refusal map[string]any
			if err := json.Unmarshal([]byte(body), &refusal); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			grants, _ := refusal[authz.DetailGrants].([]any)
			if len(grants) != 1 || grants[0] != string(iam.GrantAdmin) {
				t.Errorf("refused naming %v, want [admin]", refusal[authz.DetailGrants])
			}
		})
	}
}

// AND A REFUSED REVEAL NEVER OPENS THE VALUE: `secrets.reveal` is asked before
// the store is read, so a caller the listing admits and the reveal does not
// cannot learn even whether the value would have decrypted — the answer is the
// same 403 for a name that exists and one that does not.
//
// Admin opens both verbs, so what tells them apart is the reveal row's own
// lock: a person present and a recent proof. An admin acting through a machine
// token, and an admin whose sign-in is a day old, may each list and are each
// refused the value — before the name is looked up.
func TestARefusedRevealIsTheSameForANameThatDoesNotExist(t *testing.T) {
	t.Parallel()
	opts := secretsapi.Options{Fleet: coordmem.NewFleet(),
		Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock }}
	if code, body := call(t, mounted(t, opts, iam.AllGrants...),
		http.MethodPut, "/secrets/A", "one"); code != http.StatusOK {
		t.Fatalf("seed = %d %s", code, body)
	}
	for name, caller := range map[string]iam.Principal{
		"an admin acting through a machine token": {
			ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
			Stage: iam.StageActive, Grants: iam.AllGrants,
			Via:      "pat:0192f00d-0000-7000-8000-00000000000b",
			ReauthAt: time.Now().Add(time.Hour),
		},
		"an admin whose proof is stale": {
			ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
			Stage: iam.StageActive, Grants: iam.AllGrants,
			ReauthAt: time.Now().Add(-time.Hour),
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := mountedAs(t, opts, caller)
			if code, body := call(t, h, http.MethodGet, "/secrets/A", ""); code != http.StatusOK {
				t.Fatalf("the metadata = %d %s, want 200: the case is about "+
					"the reveal's own row, not the listing's", code, body)
			}
			present, presentBody := call(t, h, http.MethodGet, "/secrets/A?reveal=true", "")
			absent, absentBody := call(t, h, http.MethodGet, "/secrets/NOWHERE?reveal=true", "")
			if present != http.StatusForbidden || absent != http.StatusForbidden {
				t.Errorf("revealing a name that exists = %d, one that does not "+
					"= %d, want both 403: a 404 would tell a caller the reveal "+
					"refuses which names exist", present, absent)
			}
			// THE SAME BYTES, which is what says the value was never read:
			// a refusal that had opened it could not match one for a name
			// with nothing under it.
			if presentBody != absentBody {
				t.Errorf("the two refusals differ:\n%s\n%s", presentBody, absentBody)
			}
			// AND THE SAME FOR SEVERAL AT ONCE, decided on the route
			// before the body is read.
			many, manyBody := call(t, h, http.MethodPost, "/secrets/reveal", `{"names":["A"]}`)
			none, noneBody := call(t, h, http.MethodPost, "/secrets/reveal", `{"names":["NOWHERE"]}`)
			if many != http.StatusForbidden || none != http.StatusForbidden ||
				manyBody != noneBody {
				t.Errorf("revealing several values answered %d and %d, want the "+
					"one 403 for both:\n%s\n%s", many, none, manyBody, noneBody)
			}
		})
	}
}

// EVERY REFUSAL ON THIS SURFACE IS THE ENGINE'S ENVELOPE.
//
// The surface wrote its own `{"error": ...}` maps: a code the rest of the API
// has no sentence for, so the dashboard — which renders the envelope's
// `message` verbatim — showed nothing, and a code the vocabulary does not
// declare is one nothing holds against its spelling. Each refusal is walked
// here, and each must carry its code AND the vocabulary's sentence for it.
func TestEveryRefusalHereIsTheEnginesEnvelope(t *testing.T) {
	t.Parallel()
	sealed := mounted(t, secretsapi.Options{
		Fleet: coordmem.NewFleet(), Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}, iam.AllGrants...)
	for _, tc := range []struct {
		name         string
		h            http.Handler
		method, path string
		body         string
		status       int
		code         httpjson.Code
	}{
		{"a reserved name", sealed, http.MethodGet,
			"/secrets/" + url.PathEscape("iam/blind-index-key"), "",
			http.StatusForbidden, httpjson.CodeReservedName},
		{"a name no reference can hold", sealed, http.MethodPut,
			"/secrets/gitlab-token", "v", http.StatusBadRequest,
			httpjson.CodeInvalidName},
		{"an absent name", sealed, http.MethodGet, "/secrets/NOT_HERE", "",
			http.StatusNotFound, httpjson.CodeNotFound},
		{"a rekey onto another key", sealed, http.MethodPost,
			"/secrets/rekey?key_id=k9", "", http.StatusConflict,
			httpjson.CodeKeyIDMismatch},
		{"a write that names its provenance", sealed, http.MethodPut,
			"/secrets/A?source=operator", "v", http.StatusBadRequest,
			httpjson.CodeInvalidQuery},
	} {
		code, raw := call(t, tc.h, tc.method, tc.path, tc.body)
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Errorf("%s: the refusal is not JSON: %s", tc.name, raw)
			continue
		}
		if code != tc.status || body["error"] != string(tc.code) {
			t.Errorf("%s answered %d %v, want %d %s", tc.name, code,
				body["error"], tc.status, tc.code)
		}
		if body["message"] != tc.code.Message() || tc.code.Message() == "" {
			t.Errorf("%s carries message %q, want the vocabulary's sentence "+
				"for %s", tc.name, body["message"], tc.code)
		}
	}
}
