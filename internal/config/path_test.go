package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// A PATH RENDERS AS THE OPERATOR WROTE IT. The rendered form is what every
// error message has always carried, so building paths from segments must not
// change a single character of it.
func TestAPathRendersAsTheAuthoredSpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path Path
		want string
	}{
		{nil, ""},
		{field("name"), "name"},
		{field("integrations.datadog.route_to"), "integrations.datadog.route_to"},
		{at(idx(field("units"), 0), "roles"), "units[0].roles"},
		{at(idx(at(idx(field("units"), 2), "children"), 1), "lead"), "units[2].children[1].lead"},
		{at(entry(field("providers.llm"), "claude-3.5"), "model"), "providers.llm.claude-3.5.model"},
		{idx(field("policies"), 3), "policies[3]"},
	} {
		if got := tc.path.String(); got != tc.want {
			t.Errorf("%#v renders %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A MAP KEY IS ONE SEGMENT, dots and all. The rendered string cannot say where
// a key holding a dot ends, which is the whole reason a path is kept as
// segments; a key split on its dots would send a consumer looking for fields
// that do not exist.
func TestAMapKeyHoldingADotStaysOneSegment(t *testing.T) {
	t.Parallel()
	cfg := DefaultCompany()
	cfg.Name = "Acme"
	cfg.Providers.LLM = map[string]LLMProvider{"claude-3.5": {Type: LLMAnthropic}}
	var found *Fault
	for _, problem := range flatten(cfg.Validate()) {
		if errors.As(problem, &found) && found.Path.String() == "providers.llm.claude-3.5.model" {
			break
		}
		found = nil
	}
	if found == nil {
		t.Fatalf("no fault at providers.llm.claude-3.5.model in %v", cfg.Validate())
	}
	want := Path{"providers", "llm", "claude-3.5", "model"}
	if !reflect.DeepEqual(found.Path, want) {
		t.Errorf("segments = %#v, want %#v", found.Path, want)
	}
}

// EXTENDING A PATH NEVER WRITES INTO ITS PARENT. Sibling fields are built from
// one parent, and a helper that appended into a shared backing array would let
// the second sibling overwrite the first one's last segment.
func TestSiblingPathsDoNotShareStorage(t *testing.T) {
	t.Parallel()
	parent := make(Path, 0, 8)
	parent = append(parent, "roles", 0)
	first := at(parent, "name")
	second := at(parent, "handle")
	if first.String() != "roles[0].name" || second.String() != "roles[0].handle" {
		t.Errorf("siblings rendered %q and %q", first, second)
	}
}

// A KEYRING FAULT POINTS AT THE ELEMENT'S MATERIAL. `keys` is a list, and the
// path used to name the key's id as if the list were a map, which is a place
// no operator can find in their file.
func TestAnUnreadableKeyIsReportedAtItsMaterial(t *testing.T) {
	t.Parallel()
	s := Secrets{ActiveKeyID: "k2", Keys: []SecretKey{
		{ID: "k1", Material: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="},
		{ID: "k2", Material: "not base64!"},
	}}
	_, err := s.Cipher()
	var f *Fault
	if !errors.As(err, &f) {
		t.Fatalf("Cipher() = %v, want a fault", err)
	}
	if want := (Path{"secrets", "keys", 1, "material"}); !reflect.DeepEqual(f.Path, want) {
		t.Errorf("path = %#v (%s), want %#v", f.Path, f.Path, want)
	}
}

// flatten returns the leaves of a joined error.
func flatten(err error) []error {
	if err == nil {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, inner := range joined.Unwrap() {
		out = append(out, flatten(inner)...)
	}
	return out
}

// A PATH SURVIVES THE WIRE. A Go client decoding a problem would otherwise
// receive its indexes as float64, which neither renders nor compares equal to
// the path the engine built.
func TestAPathRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	want := Path{"units", 2, "roles", 0, "mcp_env", "jira.cloud"}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Path
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) || got.String() != want.String() {
		t.Errorf("round trip = %#v (%s), want %#v", got, got, want)
	}
	var none Path
	if err := json.Unmarshal([]byte("null"), &none); err != nil || none != nil {
		t.Errorf("null decoded to %#v, %v; want a nil path", none, err)
	}
	if err := json.Unmarshal([]byte(`["roles", 1.5]`), &none); err == nil {
		t.Error("a fractional index was accepted")
	}
}
