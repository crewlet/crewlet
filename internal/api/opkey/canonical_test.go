package opkey_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/opkey"
)

// AN OPERATION'S DIGEST IS THE CANONICAL ENCODING, CUT TO SIXTEEN DIGITS.
//
// Digest and RequestDigest bind to one encoding (ADR-0008), so Digest must be
// exactly the hash of Canonical: a Digest that drifted onto an encoding of its
// own would leave the two bindings agreeing only by accident. Mutation: hash
// the body before the query in Digest and this fails.
func TestAnOperationsDigestIsTheCanonicalEncoding(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/worklet/items?b=2&a=1", nil)
	asks := map[string]any{"title": "Fix the flaky test", "priority": "high"}
	got, err := opkey.Digest(r, asks)
	if err != nil {
		t.Fatal(err)
	}
	canon, err := opkey.Canonical(r.URL.Path, r.URL.Query(), asks)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	if want := hex.EncodeToString(sum[:])[:16]; got != want {
		t.Errorf("Digest answered %s and the canonical encoding hashes to %s", got, want)
	}
}

// A REQUEST DIGEST IS THE REQUEST, AND NOTHING ABOUT HOW IT WAS SPELLED.
//
// An approval is bound to this value (ADR-0051), so the same request must
// digest the same however its body is ordered or spaced, and any change to
// the method, the object, the query or one argument must move it. Mutation:
// leave the method out of the hash and the POST and PUT digests agree.
func TestARequestDigestBindsEveryPartOfTheRequest(t *testing.T) {
	t.Parallel()
	digest := func(method, target, body string) string {
		t.Helper()
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		d, err := opkey.RequestDigestOf(method, u, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
			t.Fatalf("digest %q is not 64 lowercase hex digits", d)
		}
		return d
	}
	base := digest("POST", "/operator/act/create_work_item?x=1&y=2",
		`{"args":{"title":"A","priority":"high"}}`)
	same := digest("POST", "/operator/act/create_work_item?y=2&x=1",
		"{ \"args\" : { \"priority\":\"high\", \"title\":\"A\" } }")
	if base != same {
		t.Errorf("the same request spelled another way digests %s, want %s", same, base)
	}
	for what, other := range map[string]string{
		"another method": digest("PUT", "/operator/act/create_work_item?x=1&y=2",
			`{"args":{"title":"A","priority":"high"}}`),
		"another path": digest("POST", "/operator/act/update_work_item?x=1&y=2",
			`{"args":{"title":"A","priority":"high"}}`),
		"another query": digest("POST", "/operator/act/create_work_item?x=1&y=3",
			`{"args":{"title":"A","priority":"high"}}`),
		"another argument": digest("POST", "/operator/act/create_work_item?x=1&y=2",
			`{"args":{"title":"B","priority":"high"}}`),
		"no body": digest("POST", "/operator/act/create_work_item?x=1&y=2", ""),
	} {
		if other == base {
			t.Errorf("%s digests the same as the approved request", what)
		}
	}
}

// A BODY THAT IS NOT ONE JSON VALUE HAS NO DIGEST, rather than the digest of
// whatever part of it decoded.
func TestARequestDigestRefusesABodyThatIsNotJSON(t *testing.T) {
	t.Parallel()
	u, _ := url.Parse("/operator/act/create_work_item")
	if d, err := opkey.RequestDigestOf("POST", u, []byte(`{"args":`)); err == nil {
		t.Errorf("a truncated body digests to %s", d)
	}
}
