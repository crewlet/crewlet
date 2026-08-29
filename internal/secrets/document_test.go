package secrets

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var document = []byte(`{"name":"Acme","roles":[{"name":"CEO","handle":"ceo"}]}`)

func TestASealedDocumentRoundTrips(t *testing.T) {
	t.Parallel()
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	opened, err := Open(cipher, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != string(document) {
		t.Fatalf("opened %s, want %s", opened, document)
	}
}

func TestSealingHidesTheSTRUCTURE(t *testing.T) {
	t.Parallel()
	// The whole document is one blob, not a per-field seal. An org chart,
	// the role names, which integrations a company runs and how many seats
	// it has are all STRUCTURE — and structure is what a config document
	// mostly is, so a field-by-field seal publishes nearly everything worth
	// knowing about a deployment.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Acme", "CEO", "ceo", "roles"} {
		if strings.Contains(string(sealed), leak) {
			t.Errorf("the sealed document still names %q: %s", leak, sealed)
		}
	}
}

func TestSealingIsIdempotent(t *testing.T) {
	t.Parallel()
	// Re-sealing would nest one envelope inside another, and the outer one
	// opens to something no config parser has ever seen — which surfaces as
	// a parse error naming a field nobody wrote.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	once, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Seal(cipher, once)
	if err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	opened, err := Open(cipher, twice)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != string(document) {
		t.Fatalf("opened %s, want the original document", opened)
	}
}

func TestAPlaintextStoreKeepsReading(t *testing.T) {
	t.Parallel()
	// A deployment with no keyring in Tier A stores plaintext — the
	// documented opt-out. Open must hand that back verbatim, or configuring
	// a keyring later would be the moment every existing revision became
	// unreadable.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Cipher{nil, cipher} {
		opened, err := Open(c, document)
		if err != nil {
			t.Fatalf("open plaintext: %v", err)
		}
		if string(opened) != string(document) {
			t.Fatalf("opened %s, want the document unchanged", opened)
		}
	}
}

func TestASealedDocumentWithNoKeyIsAnErrorNotAnEmptyCompany(t *testing.T) {
	t.Parallel()
	// Returning nothing would boot the node onto an empty company, which
	// reads on every surface as an operator who has configured nothing —
	// and the actual fault is a deployment that lost its root of trust.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(nil, sealed)
	if !errors.Is(err, ErrSealedWithoutKey) {
		t.Fatalf("err = %v, want ErrSealedWithoutKey", err)
	}
	if opened != nil {
		t.Errorf("a refused open still produced %s", opened)
	}
}

func TestADocumentSealedByAnotherKeyIsRefused(t *testing.T) {
	t.Parallel()
	mine, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(theirs, document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(mine, sealed); err == nil {
		t.Fatal("a document sealed under different key material opened")
	}
}

func TestSealedIsStructuralNotASearch(t *testing.T) {
	t.Parallel()
	// A plaintext document that happens to mention the envelope key must
	// not be mistaken for one — the check is the SHAPE, a single-field
	// object holding a well-formed envelope.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"a real envelope", string(sealed), true},
		{"the key mentioned in a value", `{"summary":"__encrypted__"}`, false},
		{"the key beside another field", `{"__encrypted__":"enc:v1:a:Yg==","n":1}`, false},
		{"the key holding a non-envelope", `{"__encrypted__":"just a string"}`, false},
		{"the key holding an object", `{"__encrypted__":{"a":1}}`, false},
		{"a plain document", string(document), false},
		{"not json at all", `nonsense`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Sealed([]byte(tc.raw)); got != tc.want {
				t.Errorf("Sealed(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestOpeningARealEnvelopeYieldsUsableJSON(t *testing.T) {
	t.Parallel()
	// The point of the round trip: what comes out is parseable as the
	// document that went in, not merely equal as bytes.
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(cipher, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var company struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(opened, &company); err != nil {
		t.Fatalf("the opened document is not JSON: %v", err)
	}
	if company.Name != "Acme" {
		t.Errorf("name = %q", company.Name)
	}
}
