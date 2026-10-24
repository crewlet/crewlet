package canon_test

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
)

// ONE VALUE, ONE ENCODING: whatever whitespace, key order or escaping a writer
// sent, the canonical bytes are the same, which is what lets every data node
// hold byte-identical rows.
func TestEveryEncodingOfOneValueIsCanonicallyTheSame(t *testing.T) {
	t.Parallel()
	want := `{"a":[1,2.50,{"k":"\u2028\n\""}],"b":true,"n":12345678901234567890123,"z":null}`
	for _, in := range []string{
		`{"z":null,"n":12345678901234567890123,"b":true,"a":[1,2.50,{"k":"\u2028\n\""}]}`,
		` { "b" : true , "a" : [ 1 , 2.50 , { "k" : "\u2028\u000a\u0022" } ] ,
		    "n" : 12345678901234567890123 , "z" : null } `,
		"{\"a\":[1,2.50,{\"k\":\"\u2028\\n\\\"\"}],\"b\":true,\"n\":12345678901234567890123,\"z\":null}",
	} {
		got, err := canon.Canonical([]byte(in), 0)
		if err != nil {
			t.Fatalf("Canonical(%s): %v", in, err)
		}
		if string(got) != want {
			t.Fatalf("Canonical(%s)\n got %s\nwant %s", in, got, want)
		}
	}
}

// A LARGE INTEGER KEEPS ITS DIGITS: a float on the way through would rewrite
// it, and two builds rounding differently would write different bytes for one
// record.
func TestANumberKeepsItsDecimalText(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"9007199254740993", "-0", "1e400", "0.1000", "123456789012345678901234567890"} {
		got, err := canon.Canonical([]byte(n), 0)
		if err != nil {
			t.Fatalf("Canonical(%s): %v", n, err)
		}
		if string(got) != n {
			t.Fatalf("Canonical(%s) = %s: a number's decimal text is carried as written", n, got)
		}
	}
}

// AN AMBIGUOUS OR MALFORMED VALUE IS REFUSED, never resolved: a duplicate key
// whose meaning depends on the parser, two values, a truncated one, and
// invalid UTF-8.
func TestAnAmbiguousValueIsRefused(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]error{
		`{"a":1,"a":2}`:         canon.ErrDuplicateKey,
		`{"a":{"b":1,"b":1}}`:   canon.ErrDuplicateKey,
		`1 2`:                   canon.ErrInvalid,
		`{"a":`:                 canon.ErrInvalid,
		"\"\xff\"":              canon.ErrInvalid,
		``:                      canon.ErrInvalid,
		`[[[[[[[[[[1]]]]]]]]]]`: canon.ErrTooDeep,
	} {
		_, err := canon.Parse([]byte(in), 4)
		if !errors.Is(err, want) {
			t.Fatalf("Parse(%q) = %v, want %v", in, err, want)
		}
	}
}

// THE ENCODER'S OWN DEPTH BOUND HOLDS whatever a caller asks for, so a hostile
// value costs a bounded stack.
func TestTheEncoderNeverWalksPastItsOwnDepth(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("[", canon.MaxDepth+1) + strings.Repeat("]", canon.MaxDepth+1)
	if _, err := canon.Parse([]byte(deep), 10_000); !errors.Is(err, canon.ErrTooDeep) {
		t.Fatalf("Parse past MaxDepth = %v, want ErrTooDeep", err)
	}
}

// THE STATE DIGEST IS THE SUM OF ITS TERMS, so the order terms are added in
// does not matter and removing a term undoes adding it: what lets a write move
// the digest by what it changed alone.
func TestTheDigestIsAnOrderFreeSumAndASubtractionUndoesAnAdd(t *testing.T) {
	t.Parallel()
	terms := make([]canon.Term, 64)
	for i := range terms {
		if _, err := rand.Read(terms[i][:]); err != nil {
			t.Fatal(err)
		}
	}
	// THE CARRY PATH, on purpose: an all-ones term added twice wraps.
	for i := range terms[0] {
		terms[0][i] = 0xff
	}
	var forward, backward canon.Digest
	for i := range terms {
		forward = forward.Add(terms[i])
		backward = backward.Add(terms[len(terms)-1-i])
	}
	if forward != backward {
		t.Fatal("the digest depends on the order its terms were added in")
	}
	// AGAINST AN INDEPENDENT REFERENCE: big-integer arithmetic modulo 2^256.
	mod := new(big.Int).Lsh(big.NewInt(1), 256)
	ref := new(big.Int)
	for _, term := range terms {
		ref.Add(ref, new(big.Int).SetBytes(term[:]))
		ref.Mod(ref, mod)
	}
	var want canon.Digest
	ref.FillBytes(want[:])
	if forward != want {
		t.Fatalf("the digest %s is not the sum of its terms modulo 2^256 (%s)", forward, want)
	}
	for _, term := range terms {
		forward = forward.Sub(term)
	}
	if !forward.IsZero() {
		t.Fatalf("subtracting every term left %s, want the empty digest", forward)
	}
}

// A DOCUMENT'S TERM NAMES EVERY PART OF IT: its collection, its id, its body
// and its version each move the term, so a digest that matches proves the
// documents match.
func TestADocumentTermMovesWithEveryPartOfTheDocument(t *testing.T) {
	t.Parallel()
	base := canon.DocumentTerm("tasks", "row-1", []byte(`{"a":1}`), 7)
	for name, other := range map[string]canon.Term{
		"collection": canon.DocumentTerm("taskz", "row-1", []byte(`{"a":1}`), 7),
		"id":         canon.DocumentTerm("tasks", "row-2", []byte(`{"a":1}`), 7),
		"body":       canon.DocumentTerm("tasks", "row-1", []byte(`{"a":2}`), 7),
		"version":    canon.DocumentTerm("tasks", "row-1", []byte(`{"a":1}`), 8),
		// A SPLIT OF THE SAME BYTES: the line is an object, not a
		// concatenation, so moving a character across the boundary
		// between the collection and the id is another document.
		"split": canon.DocumentTerm("task", "srow-1", []byte(`{"a":1}`), 7),
	} {
		if other == base {
			t.Errorf("changing the %s left the document's term unchanged", name)
		}
	}
}

// A STORED DIGEST READS BACK, and a column of the wrong length is refused.
func TestAStoredDigestReadsBackExactly(t *testing.T) {
	t.Parallel()
	d := canon.Digest{}.Add(canon.CollectionTerm("tasks", []byte(`{}`)))
	back, err := canon.DigestFrom(d.Bytes())
	if err != nil || back != d {
		t.Fatalf("DigestFrom(Bytes()) = %v, %v; want %s", back, err, d)
	}
	if empty, err := canon.DigestFrom(nil); err != nil || !empty.IsZero() {
		t.Fatalf("DigestFrom(nil) = %v, %v; want the empty digest", empty, err)
	}
	if _, err := canon.DigestFrom([]byte{1, 2, 3}); err == nil {
		t.Fatal("a three-byte digest was read back")
	}
}
