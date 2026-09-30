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

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
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
	base := digest("/chart/units/eng?runtime=true", `{"name":"Eng","goal":"ship"}`)
	if len(base) != 16 {
		t.Errorf("the digest is %q, want sixteen hex digits", base)
	}
	if again := digest("/chart/units/eng?runtime=true",
		`{ "goal": "ship",  "name": "Eng" }`); again != base {
		t.Errorf("the same fields in another order digest to %q, want %q — a "+
			"client's retry would be a second operation", again, base)
	}
	for _, c := range []struct{ name, target, body string }{
		{"another object", "/chart/units/ops?runtime=true", `{"name":"Eng","goal":"ship"}`},
		{"another query", "/chart/units/eng", `{"name":"Eng","goal":"ship"}`},
		{"another body", "/chart/units/eng?runtime=true", `{"name":"Eng","goal":"stop"}`},
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
// back for a retry to send.
//
// Mutation: accept the caller's key unchecked, or mint a v4 uuid, and a case
// goes red.
func TestAKeyIsTheCallersInTheGrammarOrMintedHere(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ask := func(key string) (*httptest.ResponseRecorder, string, bool) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/work/items", nil)
		if key != "" {
			r.Header.Set(opkey.Header, key)
		}
		got, ok := opkey.Key(rec, r, now)
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

	if _, given, ok := ask(minted); !ok || given != minted {
		t.Errorf("a key in the grammar came back as %q (ok=%v), want it as sent",
			given, ok)
	}

	rec, _, ok = ask("not-an-operation-id")
	if ok {
		t.Fatal("a key outside the grammar was accepted")
	}
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
// `/chart`, `/iam`, the human write surface and the CLI each declared their
// own "Idempotency-Key", each commenting that it was "the same spelling" as
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
// reading prose is told what to do, and /chart, /iam and the human write
// surface each wrote it until their copies drifted. Both say to send the same
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
