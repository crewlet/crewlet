package secrets

import (
	"bytes"
	"encoding/base64"
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
	// THE KEY SET IS THE STRUCTURAL CLAIM, and it is exact: one key, the
	// envelope's, so not one field name of the document survives as a
	// field. Everything else is one opaque token.
	var envelope map[string]string
	if err := json.Unmarshal(sealed, &envelope); err != nil {
		t.Fatalf("the sealed document is not an envelope: %v", err)
	}
	if len(envelope) != 1 {
		t.Fatalf("the sealed document has %d keys: %s", len(envelope), sealed)
	}
	token, present := envelope[EnvelopeKey]
	if !present {
		t.Fatalf("the sealed document is keyed on something else: %s", sealed)
	}

	// AND NOTHING IS SCANNED FOR A SHORT NEEDLE, in either encoding.
	//
	// This used to search `string(sealed)` for "Acme", "CEO", "ceo" and
	// "roles", which is a coin toss rather than a check: base64 draws from
	// a 64-symbol alphabet that INCLUDES those characters, so a short ASCII
	// needle turns up in random ciphertext by luck — about one run in seven
	// hundred over those four. It did, under `-race` in this tree:
	// `…AcCEOGNYhF1…`, a "CEO" the cipher put there, reported as a leak of
	// the document's. Decoding first makes the alphabet 256 symbols wide
	// and the odds one in a hundred thousand, which is the same defect
	// wearing a longer fuse — and a test that fails at random teaches a
	// reader to re-run it, which is the habit that hides a real failure.
	//
	// So the two claims left are the two that are exact: the key set above,
	// which IS "the structure is hidden" — no field name survives as a
	// field — and the whole plaintext below, 54 bytes of it, which cannot
	// collide with anything.
	payload := token[strings.LastIndex(token, ":")+1:]
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the envelope payload is not base64: %v", err)
	}
	if bytes.Contains(raw, document) {
		t.Errorf("the sealed document carries its own plaintext: %x", raw)
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

// A PLAINTEXT DOCUMENT IS REFUSED BY A READER HOLDING A KEYRING.
//
// The seal is what authenticates a document a peer published: sealed under
// the fleet's keyring, a forged body does not open, while a plaintext one
// opens as whatever its author wrote — and anything that reaches the
// coordination store can author one. It used to come back verbatim whatever
// the reader held, so a node with a keyring applied a forged plaintext
// document as readily as its own. The refusal names the command that seals
// the one legitimate plaintext revision left, an older build's.
//
// Mutation: hand a plaintext payload back whatever the cipher, and this fails.
func TestAPlaintextDocumentIsRefusedByAReaderHoldingAKeyring(t *testing.T) {
	t.Parallel()
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(cipher, document)
	if !errors.Is(err, ErrUnsealedWithKey) {
		t.Fatalf("Open(keyring, plaintext) = %v, want ErrUnsealedWithKey", err)
	}
	if opened != nil {
		t.Errorf("a refused open still produced %s", opened)
	}
	if !strings.Contains(err.Error(), "crewlet config seal") {
		t.Errorf("the refusal does not name the command that seals it: %v", err)
	}

	// THE CONTROL: the same document sealed under the ring opens, so the
	// refusal above is about the missing seal and nothing else.
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err = Open(cipher, sealed); err != nil || string(opened) != string(document) {
		t.Fatalf("Open(keyring, sealed) = %s, %v, want the document", opened, err)
	}
}

// WITHOUT A KEYRING A PLAINTEXT DOCUMENT STILL READS, for the caller holding
// none — which is no node, and is what a unit that seals nothing runs on.
func TestAReaderWithNoKeyringReadsPlaintext(t *testing.T) {
	t.Parallel()
	opened, err := Open(nil, document)
	if err != nil {
		t.Fatalf("Open(nil, plaintext): %v", err)
	}
	if string(opened) != string(document) {
		t.Fatalf("opened %s, want the document unchanged", opened)
	}
}

// THE MIGRATION READS BOTH, and only with the keyring it will seal under.
//
// `crewlet config seal` rewrites an older build's plaintext revision sealed,
// and `crewlet config scrub` must reach a plaintext revision's personal data as
// surely as a sealed one's — both write back sealed, so both must read what
// they replace. Without a keyring there is nothing to seal under, so the read
// is refused before it happens.
func TestTheMigrationOpensEitherAndOnlyWithAKeyring(t *testing.T) {
	t.Parallel()
	cipher, err := NewCipher(testRing(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{"plaintext": document, "sealed": sealed} {
		opened, err := OpenToReseal(cipher, payload)
		if err != nil || string(opened) != string(document) {
			t.Errorf("OpenToReseal(%s) = %s, %v, want the document", name, opened, err)
		}
	}
	if _, err := OpenToReseal(nil, document); !errors.Is(err, ErrNoKeyring) {
		t.Errorf("OpenToReseal with no keyring = %v, want ErrNoKeyring", err)
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
