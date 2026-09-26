package secretsapi_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
)

var clock = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// logs is every record this package's tests wrote.
//
// Installed as the PROCESS's sink, from TestMain, because that is where
// [logging.Configure] lets a test put one: the package logger resolves the
// process root per record. A case finds its own records by a value no other
// case logs, which is what lets parallel cases share one sink.
var logs tap

// tap is a concurrency-safe sink the JSON handler writes one record per line
// into.
type tap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

// records is every record named event, decoded, beside its raw line.
func (t *tap) records(tb testing.TB, event string) (decoded []map[string]any, raw []string) {
	tb.Helper()
	t.mu.Lock()
	all := bytes.Clone(t.buf.Bytes())
	t.mu.Unlock()
	lines := bufio.NewScanner(bytes.NewReader(all))
	lines.Buffer(nil, len(all)+1)
	for lines.Scan() {
		var record map[string]any
		if err := json.Unmarshal(lines.Bytes(), &record); err != nil {
			tb.Fatalf("a log line is not a JSON record: %v\n%s", err, lines.Bytes())
		}
		if record["msg"] == event {
			decoded = append(decoded, record)
			raw = append(raw, lines.Text())
		}
	}
	if err := lines.Err(); err != nil {
		tb.Fatalf("reading the captured log: %v", err)
	}
	return decoded, raw
}

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelDebug, logging.FormatJSON, &logs)
	os.Exit(m.Run())
}

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
	return surfaceAs(t, "ops", cipher, keyID)
}

// surfaceAs is [surface] with the operator the guard authenticated named.
func surfaceAs(t *testing.T, operator string, cipher secrets.Cipher, keyID string) (http.Handler, coord.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	svc := newService(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipher, ActiveKeyID: keyID,
		Now: func() time.Time { return clock },
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), operator)))
	}), fleet
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
	mux := http.NewServeMux()
	newService(t, secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1", "k2"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	}).Routes(mux)
	old := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "ops")))
	})
	call(t, old, http.MethodPut, "/secrets/A", "one")

	rotated := http.NewServeMux()
	newService(t, secretsapi.Options{
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

// EVERY VALUE COMES BACK IN ONE ANSWER, AND ONLY WITH THE FLAG.
//
// A command resolving the company document off the node reads the fleet's
// whole store at once; the listing an operator or the dashboard reads must
// still carry no value at all. The revealing answer is a map from name to
// value, the exact store, and a proxy may not keep it.
//
// Mutation: answer the listing's shape under the flag, or drop a value from
// the map, and the revealing half fails; serve values without the flag and
// the plain half does.
func TestEveryValueComesBackInOneAnswerOnlyWithTheFlag(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	want := map[string]string{"GL_TOKEN": "glpat-one", "SIGNING": "whsec_two\nline"}
	for name, value := range want {
		if code, body := call(t, h, http.MethodPut, "/secrets/"+name, value); code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", name, code, body)
		}
	}

	code, plain := call(t, h, http.MethodGet, "/secrets", "")
	if code != http.StatusOK || strings.Contains(plain, "glpat") ||
		strings.Contains(plain, `"values"`) {
		t.Fatalf("the plain listing answered %d %s, want names and no values", code, plain)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secrets?reveal=true", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /secrets?reveal=true = %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(body.Values) != len(want) {
		t.Errorf("revealed %d values, want %d: %v", len(body.Values), len(want), body.Values)
	}
	for name, value := range want {
		if body.Values[name] != value {
			t.Errorf("%s revealed as %q, want %q", name, body.Values[name], value)
		}
	}
}

// A NODE WITH NO KEYRING REVEALS NOTHING, and says what to install: it holds
// no key to open a row with, and an empty map would read as a fleet that
// holds no secret.
func TestWithoutAKeyringNothingIsRevealedInBulk(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, nil, "")
	code, body := call(t, h, http.MethodGet, "/secrets?reveal=true", "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "no_keyring") {
		t.Fatalf("GET /secrets?reveal=true with no keyring = %d %s, want 503 no_keyring",
			code, body)
	}
}

// A BULK REVEAL LEAVES ONE LINE NAMING EVERY NAME IT RETURNED AND WHO ASKED.
//
// A read-back that leaves no trace is indistinguishable from an exfiltration,
// and a read of the whole store is the largest one there is. One line per
// read, naming every value's name and the operator the guard authenticated —
// and never a value, because logging one would be the leak.
//
// Mutation: drop the line, or its names or its operator, or log the values,
// and this fails.
func TestABulkRevealIsLoggedByNameAndOperator(t *testing.T) {
	t.Parallel()
	const operator = "bulk-reveal-reader"
	h, _ := surfaceAs(t, operator, cipherFor(t, "k1"), "k1")
	values := map[string]string{
		"AUDITED_ONE": "glpat-audited-one", "AUDITED_TWO": "whsec_audited_two",
	}
	for name, value := range values {
		if code, body := call(t, h, http.MethodPut, "/secrets/"+name, value); code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", name, code, body)
		}
	}
	if code, body := call(t, h, http.MethodGet, "/secrets?reveal=true", ""); code != http.StatusOK {
		t.Fatalf("GET /secrets?reveal=true = %d %s", code, body)
	}

	records, raw := logs.records(t, "secrets_revealed")
	var mine []map[string]any
	var lines []string
	for i, record := range records {
		if record["operator"] == operator {
			mine = append(mine, record)
			lines = append(lines, raw[i])
		}
	}
	if len(mine) != 1 {
		t.Fatalf("the bulk reveal logged %d lines for its operator, want one: %v", len(mine), mine)
	}
	if mine[0]["level"] != "WARN" {
		t.Errorf("the line is at %v, want WARN beside the break-glass read", mine[0]["level"])
	}
	var names []string
	listed, _ := mine[0]["names"].([]any)
	for _, name := range listed {
		text, _ := name.(string)
		names = append(names, text)
	}
	if want := []string{"AUDITED_ONE", "AUDITED_TWO"}; !slices.Equal(names, want) {
		t.Errorf("the line names %v, want every name revealed, sorted: %v", mine[0]["names"], want)
	}
	for _, value := range values {
		if strings.Contains(lines[0], value) {
			t.Errorf("the audit line carries a revealed value: %s", lines[0])
		}
	}
}
