package opkey_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SAME REQUEST IS THE SAME DIGEST, AND ANY OTHER REQUEST IS ANOTHER.
//
// The digest is what a write's published id is stepped by, so a key sent with
// another request is another operation rather than the first one answered back
// from the ledger with nothing of the second written. Each of the three parts
// has to move it — the path names the object, the query and the body what is
// asked of it — and the body is compared as the value it DECODED to, so a
// client re-sending the same fields in another order or spacing is still the
// same request.
//
// Mutation: drop any part from the hash, or hash the raw bytes rather than
// the decoded value, and a row goes red.
func TestTheSameRequestIsTheSameDigest(t *testing.T) {
	t.Parallel()
	digest := func(target, body string) string {
		t.Helper()
		var asks any
		if body != "" {
			if err := json.Unmarshal([]byte(body), &asks); err != nil {
				t.Fatal(err)
			}
		}
		got, err := opkey.Digest(httptest.NewRequest(http.MethodPatch, target, nil), asks)
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		return got
	}
	base := digest("/iam/people/eng?view=full", `{"name":"Eng","goal":"ship"}`)
	if len(base) != 16 {
		t.Errorf("the digest is %q, want sixteen hex digits", base)
	}
	if again := digest("/iam/people/eng?view=full",
		`{ "goal": "ship",  "name": "Eng" }`); again != base {
		t.Errorf("the same fields in another order digest to %q, want %q — a "+
			"client's retry would be a second operation", again, base)
	}
	for _, c := range []struct{ name, target, body string }{
		{"another object", "/iam/people/ops?view=full", `{"name":"Eng","goal":"ship"}`},
		{"another query", "/iam/people/eng", `{"name":"Eng","goal":"ship"}`},
		{"another body", "/iam/people/eng?view=full", `{"name":"Eng","goal":"stop"}`},
	} {
		if got := digest(c.target, c.body); got == base {
			t.Errorf("%s digests the same as the first request, so a key sent "+
				"with it is answered as the first one's operation", c.name)
		}
	}
}

// A KEY IS THE CALLER'S, HELD TO THE ENGINE'S GRAMMAR, OR ONE MINTED HERE.
//
// A key outside the grammar carries no instant, reads as minted at the epoch
// and is answered `unknown` for ever once the ledger has swept anything, so it
// is refused naming the header — `400 op_id_invalid`, the one code every
// surface refuses an operation id with — rather than published. No key at all
// is a fresh one that the grammar itself accepts, since the answer hands it
// back for a retry to send — and sent back, it is the same key.
//
// Mutation: accept the caller's key unchecked, mint a v4 uuid, or scope a key
// the answer already scoped a second time, and a case goes red.
func TestAKeyIsTheCallersInTheGrammarOrMintedHere(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	jane := person("jane.doe")
	ask := func(key string) (*httptest.ResponseRecorder, string, bool) {
		rec := httptest.NewRecorder()
		got, ok := opkey.Key(rec, request(jane, key), now)
		return rec, got, ok
	}

	rec, minted, ok := ask("")
	if !ok || rec.Body.Len() != 0 {
		t.Fatalf("no key answered ok=%v and wrote %q, want a fresh key", ok, rec.Body)
	}
	if err := statelog.CheckCallerOpID(minted); err != nil {
		t.Fatalf("the minted key %q is not one the grammar accepts back: %v",
			minted, err)
	}
	if at, _ := statelog.OpMintedAt(minted); !at.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("the minted key carries %v, want the instant it was minted at %v",
			at, now)
	}

	// THE RETRY: the key an answer handed back, sent again, is the same
	// operation — scoped a second time it would be a second write.
	if _, given, ok := ask(minted); !ok || given != minted {
		t.Errorf("the key an answer handed back came back as %q (ok=%v), want it "+
			"as sent", given, ok)
	}

	rec, _, ok = ask("not-an-operation-id")
	if ok {
		t.Fatal("a key outside the grammar was accepted")
	}
	assertRefusedNamingTheHeader(t, rec)
}

// A KEY NAMES THE OPERATIONS OF WHOEVER SENT IT, AND NOBODY ELSE'S.
//
// The ledger answers an operation it already holds before the write is
// decided, so an unscoped key was a handle on somebody else's write: sent with
// another request it was answered from the ledger as theirs, and sent FIRST it
// made their real write collapse into nothing. Scoped by the principal's ID, a
// copied key — the caller's own spelling or the scoped one an answer handed
// back — names an operation of the copier's, never the owner's; and the scope
// is the ID, so a rename of the same person keeps their retry their retry.
//
// Mutation: drop the scope, scope by the login, or accept another principal's
// scoped key as it is, and a case goes red.
func TestAKeyIsScopedByThePrincipalThatSentIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	jane, mallory := person("jane.doe"), person("mallory.x")
	key := statelog.NewOpID(now, "")
	scope := func(p iam.Principal, sent string) string {
		t.Helper()
		got, ok := opkey.Key(httptest.NewRecorder(), request(p, sent), now)
		if !ok {
			t.Fatalf("the key %q was refused", sent)
		}
		return got
	}

	janes := scope(jane, key)
	if janes == key {
		t.Fatal("a caller's key came back unscoped, so it names the same " +
			"operation whoever sends it")
	}
	if at, _ := statelog.OpMintedAt(janes); !at.Equal(now.Truncate(time.Millisecond)) {
		t.Errorf("the scoped key carries %v, want the instant of the key it was "+
			"scoped from — the ledger vouches for a retry by it", at)
	}
	if again := scope(jane, key); again != janes {
		t.Errorf("one person's key scoped twice is %q and %q, so their retry is a "+
			"second write", janes, again)
	}
	if mine := scope(mallory, key); mine == janes {
		t.Error("two principals sending one key share its operation")
	}
	if stolen := scope(mallory, janes); stolen == janes {
		t.Error("another principal sending a key an answer handed back to jane " +
			"named jane's operation")
	}
	renamed := jane
	renamed.Login = "jane.smith"
	if got := scope(renamed, key); got != janes {
		t.Errorf("a rename moved jane's operation from %q to %q — the scope must "+
			"be the ID, never the login", janes, got)
	}
}

// A ROUTE THAT REQUIRES A KEY REFUSES A REQUEST WITHOUT ONE, NAMING THE HEADER.
//
// The act route's caller retries by itself, so a key minted for it here would
// be handed back in exactly the answer a retry exists because it never
// arrived. Absent is refused like malformed: `400 op_id_invalid` naming the
// header. A key it does carry is scoped exactly as [opkey.Key] scopes it.
//
// Mutation: mint one where none was sent, and the first case goes red.
func TestARequiredKeyIsRefusedWhenAbsent(t *testing.T) {
	t.Parallel()
	jane := person("jane.doe")
	rec := httptest.NewRecorder()
	if _, ok := opkey.Require(rec, request(jane, "")); ok {
		t.Fatal("a request with no key was given one")
	}
	assertRefusedNamingTheHeader(t, rec)

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	key := statelog.NewOpID(now, "")
	required, ok := opkey.Require(httptest.NewRecorder(), request(jane, key))
	if !ok {
		t.Fatal("a key in the grammar was refused")
	}
	keyed, _ := opkey.Key(httptest.NewRecorder(), request(jane, key), now)
	if required != keyed {
		t.Errorf("Require scoped the key to %q and Key to %q — one key, two "+
			"operations", required, keyed)
	}
}

// person is a resolved principal by its login, with an ID of its own.
func person(login string) iam.Principal {
	return iam.Principal{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(login)), Login: login,
		Kind: iam.KindPerson, Stage: iam.StageActive,
	}
}

// request is a write by p, sending key in the header where it is not empty.
func request(p iam.Principal, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/work/items", nil)
	if key != "" {
		r.Header.Set(opkey.Header, key)
	}
	return r.WithContext(iam.WithPrincipal(r.Context(), p))
}

// assertRefusedNamingTheHeader holds a refusal to `400 op_id_invalid` naming
// the header.
func assertRefusedNamingTheHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v", err)
	}
	if rec.Code != http.StatusBadRequest ||
		body["error"] != string(httpjson.CodeOpIDInvalid) || body["field"] != opkey.Header {
		t.Errorf("refused %d %v, want 400 op_id_invalid naming %s", rec.Code, body,
			opkey.Header)
	}
}

// THE HEADER IS DECLARED HERE AND NOWHERE ELSE.
//
// `/iam`, the human write surface and the CLI each declared their own
// "Idempotency-Key", each commenting that it was "the same spelling" as
// the others, which nothing compared. A retry is one script whichever surface
// it wrote to, so a fifth copy is where a spelling drifts — the walk refuses
// the literal in any non-test file of the module outside this package.
//
// Mutation: declare the header again in any surface and this goes red.
func TestTheHeaderIsDeclaredOnce(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	own := filepath.Join(root, "internal", "api", "opkey")
	walked := 0
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "static", "dist", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Dir(path) == own {
			return nil
		}
		walked++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), `"Idempotency-Key"`) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s spells the operation-key header itself; use opkey.Header", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	// THE CONTROL: a walk that read a handful of files certifies nothing.
	if walked < 500 {
		t.Fatalf("read %d source files, which is not this module", walked)
	}
}

// AN UNKNOWN IS RETRIED UNDER THE SAME KEY, AND AN UNVOUCHED ONE ELSEWHERE.
//
// The sentence beside an unknown outcome's 503 is the one place a client
// reading prose is told what to do, and /iam and the human write surface each
// wrote it until their copies drifted. Both say to send the same
// operation back under the header; only the unvouched one sends it to another
// node, since asked here again it answers the same way until the change
// arrives. Mutation: swap the two sentences and both rows go red.
func TestAnUnknownSaysToRetryUnderTheSameKey(t *testing.T) {
	t.Parallel()
	for _, unvouched := range []bool{false, true} {
		said := opkey.UnknownDetail(unvouched)
		if !strings.Contains(said, opkey.Header) || !strings.Contains(said, "SAME") {
			t.Errorf("unvouched=%v: %q does not say to send the same operation "+
				"back as the %s", unvouched, said, opkey.Header)
		}
		if elsewhere := strings.Contains(said, "another node"); elsewhere != unvouched {
			t.Errorf("unvouched=%v: %q names another node: %v, want %v",
				unvouched, said, elsewhere, unvouched)
		}
	}
}
