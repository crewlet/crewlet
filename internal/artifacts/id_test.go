package artifacts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// AN ID IS A FUNCTION OF THE OPERATION THAT CREATED IT, so a retry names the
// artifact its first attempt made and two operations name two artifacts.
func TestAnIDIsAFunctionOfItsCreatingOperation(t *testing.T) {
	t.Parallel()
	a, again, b := IDFor("op-1"), IDFor("op-1"), IDFor("op-2")
	if a != again {
		t.Fatalf("one operation derived two ids: %s and %s", a, again)
	}
	if a == b {
		t.Fatalf("two operations derived one id %s", a)
	}
	if uuid.UUID(a).Version() != 5 {
		t.Errorf("the id is a version %d uuid; a derived id is name-based (5)",
			uuid.UUID(a).Version())
	}
	// THE NAMESPACE IS PART OF THE FORMAT: this vector pins it, so a changed
	// namespace (which would give a retry straddling an upgrade a second
	// artifact) fails here first.
	if got, want := IDFor("op-1"), ID(uuid.NewSHA1(IDNamespace, []byte("op-1"))); got != want {
		t.Fatalf("IDFor(op-1) = %s, want %s", got, want)
	}
}

// ONE SPELLING PARSES. Every other form uuid.Parse is willing to read would be
// a second row key, subject or URL for one artifact.
func TestAnIDParsesOnlyInItsCanonicalForm(t *testing.T) {
	t.Parallel()
	id := IDFor("op-canonical")
	got, err := ParseID(id.String())
	if err != nil || got != id {
		t.Fatalf("ParseID(%q) = %s, %v; want %s", id, got, err, id)
	}
	refused := []string{
		"",
		strings.ToUpper(id.String()),
		"{" + id.String() + "}",
		"urn:uuid:" + id.String(),
		strings.ReplaceAll(id.String(), "-", ""),
		uuid.Nil.String(),
		"not-a-uuid-at-all-but-36-characters!",
	}
	for _, s := range refused {
		if _, err := ParseID(s); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) = %v, want ErrInvalidID", s, err)
		}
	}
}

// THE TEXT FORM ROUND-TRIPS THROUGH JSON, and the zero id travels as the
// absence it is rather than as sixteen zero bytes a decoder reads as an id.
func TestAnIDRoundTripsAsTextAndTheZeroIDAsEmpty(t *testing.T) {
	t.Parallel()
	type carrier struct {
		ID   ID `json:"id"`
		Zero ID `json:"zero"`
	}
	in := carrier{ID: IDFor("op-json")}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"` + in.ID.String() + `","zero":""}`
	if string(raw) != want {
		t.Fatalf("marshal = %s, want %s", raw, want)
	}
	var out carrier
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if err := json.Unmarshal([]byte(`{"id":"NOT"}`), &out); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("a malformed id decoded: %v", err)
	}
}
