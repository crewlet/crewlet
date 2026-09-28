package secrets

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// THE WALK OVER AN ENCODED DOCUMENT.
//
// What it is for is finding every credential in a half the caller cannot
// decode, so each case is a way a credential could be missed — or a value
// that is not one could be taken for one.

type walkStep struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files,omitempty" secret:"true"`
	Note  string            `json:"note,omitempty"`
}

func (s walkStep) IdentityKey() string { return s.Name }

type walkApp struct {
	Token string `json:"token,omitempty" secret:"true"`
	User  string `json:"user,omitempty"`
}

type walkDoc struct {
	Env    map[string]map[string]string `json:"env,omitempty" secret:"true"`
	App    walkApp                      `json:"app,omitzero"`
	Steps  []walkStep                   `json:"steps,omitempty"`
	Keys   []string                     `json:"keys,omitempty" secret:"true"`
	Plain  string                       `json:"plain,omitempty"`
	Hidden string                       `json:"-"`
}

// seen is every credential a walk was handed, by its dotted path.
func walkAll(t *testing.T, raw string, answer func(Path, string) string) (
	map[string]string, string) {

	t.Helper()
	seen := map[string]string{}
	out, err := Walk(reflect.TypeOf(walkDoc{}), json.RawMessage(raw),
		func(p Path, v string) (string, error) {
			seen[p.String()] = v
			if answer == nil {
				return v, nil
			}
			return answer(p, v), nil
		})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return seen, string(out)
}

func TestAWalkFindsEveryCredentialAndNothingElse(t *testing.T) {
	t.Parallel()
	seen, _ := walkAll(t, `{
		"env": {"tracker": {"TOKEN": "tok", "HOST": "${HOST}"}},
		"app": {"token": "xoxb", "user": "bot"},
		"steps": [{"name": "npm", "files": {"/root/.npmrc": "rc"}, "note": "n"}],
		"keys": ["k1", "k2"],
		"plain": "not a secret",
		"future": {"token": "a field this build does not have"}
	}`, nil)
	want := map[string]string{
		"env.tracker.TOKEN":            "tok",
		"env.tracker.HOST":             "${HOST}",
		"app.token":                    "xoxb",
		"steps.npm.files./root/.npmrc": "rc",
		"keys[0]":                      "k1",
		"keys[1]":                      "k2",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("the walk was handed %v, want %v — a credential it misses "+
			"reaches the log in the clear, and a value it takes for one is "+
			"rewritten under somebody's feet", seen, want)
	}
}

// A KEY IS MATCHED AS encoding/json MATCHES IT, because that is how the
// document is read back: a credential under another case of a field's name
// decodes onto that field, and a walk that matched exactly left it in the
// clear for the decode to pick up.
func TestAWalkMatchesAKeyTheWayADecodeDoes(t *testing.T) {
	t.Parallel()
	seen, _ := walkAll(t, `{"ENV": {"tracker": {"TOKEN": "tok"}}, "App": {"TOKEN": "x"}}`, nil)
	for _, path := range []string{"env.tracker.TOKEN", "app.token"} {
		if _, found := seen[path]; !found {
			t.Errorf("%s was not walked under another case of its name: %v", path, seen)
		}
	}
	var decoded walkDoc
	if err := json.Unmarshal([]byte(`{"ENV": {"tracker": {"TOKEN": "tok"}}}`), &decoded); err != nil ||
		decoded.Env["tracker"]["TOKEN"] != "tok" {
		t.Fatalf("the premise does not hold: encoding/json read %+v (%v)", decoded, err)
	}
}

// A MEMBER IS NAMED BY WHO IT IS, and by position only where it cannot be.
func TestAListMemberIsNamedByItsIdentity(t *testing.T) {
	t.Parallel()
	var paths []Path
	_, err := Walk(reflect.TypeOf(walkDoc{}), json.RawMessage(`{"steps": [
		{"name": "npm", "files": {"a": "1"}},
		{"name": "dup", "files": {"a": "2"}},
		{"name": "dup", "files": {"a": "3"}},
		{"files": {"a": "4"}}
	]}`), func(p Path, v string) (string, error) {
		paths = append(paths, p)
		return v, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range paths {
		got = append(got, p.String())
		if p.Positional() != !strings.HasPrefix(p.String(), "steps.npm") {
			t.Errorf("%s reports Positional() = %v", p, p.Positional())
		}
	}
	slices.Sort(got)
	want := []string{"steps.npm.files.a", "steps[1].files.a", "steps[2].files.a",
		"steps[3].files.a"}
	if !slices.Equal(got, want) {
		t.Errorf("the members were named %v, want %v — a shared or missing "+
			"identity is no identity", got, want)
	}
}

// A HALF NOTHING CHANGED IS HANDED BACK AS IT ARRIVED, byte for byte; one that
// changed keeps everything the answers did not touch — a field this build
// does not know included.
func TestAWalkRewritesOnlyWhatItsAnswersChanged(t *testing.T) {
	t.Parallel()
	raw := `{"plain":"x",  "env":{"s":{"K":"v"}}, "future":{"n":1.50}}`
	_, same := walkAll(t, raw, nil)
	if same != raw {
		t.Errorf("an untouched half came back as %s, want the bytes it arrived as", same)
	}
	_, changed := walkAll(t, raw, func(Path, string) string { return "${REF}" })
	var got map[string]any
	if err := json.Unmarshal([]byte(changed), &got); err != nil {
		t.Fatalf("the rewritten half does not decode: %v", err)
	}
	if !strings.Contains(changed, `"future":{"n":1.50}`) ||
		!strings.Contains(changed, `"K":"${REF}"`) || !strings.Contains(changed, `"plain":"x"`) {
		t.Errorf("the rewritten half is %s — a field this build does not know, "+
			"or a number's spelling, was lost on the way through", changed)
	}
}

// AN UNREADABLE DOCUMENT IS REFUSED, never walked in part.
func TestAWalkRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"env":`, `{} {}`} {
		if _, err := Walk(reflect.TypeOf(walkDoc{}), json.RawMessage(raw),
			func(_ Path, v string) (string, error) { return v, nil }); err == nil {
			t.Errorf("Walk(%s) was accepted", raw)
		}
	}
}

// A TYPE THAT DECODES ITSELF HIDES WHAT IT HOLDS, and Walkable says so.
type selfDecoding struct {
	Token string `json:"token" secret:"true"`
}

func (s *selfDecoding) UnmarshalJSON([]byte) error { return nil }

func TestWalkableNamesATaggedFieldBeneathASelfDecodingType(t *testing.T) {
	t.Parallel()
	if err := Walkable(reflect.TypeOf(walkDoc{})); err != nil {
		t.Errorf("a plain document is reported unwalkable: %v", err)
	}
	type hiding struct {
		Block selfDecoding `json:"block"`
	}
	if err := Walkable(reflect.TypeOf(hiding{})); err == nil {
		t.Error("a tagged field beneath a self-decoding type was reported walkable")
	}
}

// ONLY A WHOLE REFERENCE IS SHOWN.
func TestMaskShowsOnlyAWholeReference(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"":                "",
		"${TOKEN}":        "${TOKEN}",
		"literal":         redact.FieldMask,
		"Bearer ${TOKEN}": redact.FieldMask,
		"${line#host=}":   redact.FieldMask,
		"${A}${B}":        redact.FieldMask,
		redact.FieldMask:  redact.FieldMask,
	} {
		if got := Mask(value); got != want {
			t.Errorf("Mask(%q) = %q, want %q", value, got, want)
		}
	}
}
