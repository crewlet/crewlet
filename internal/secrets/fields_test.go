package secrets

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

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

// A CONTENT FIELD IS A CREDENTIAL.
//
// A setup step's file and its env are both credentials, and they are read
// differently where they are used — a `${…}` in an env value is the engine's,
// one in a file's body belongs to whatever reads the file. Either tag is a
// credential to every surface that masks one; a surface that masked only the
// setting kind would serve a registry's auth file.
//
// The control is the untagged field, which is no credential.
func TestAContentFieldIsACredential(t *testing.T) {
	t.Parallel()
	type box struct {
		Files map[string]string `json:"files,omitempty" secret:"content"`
		Env   map[string]string `json:"env,omitempty" secret:"true"`
		Note  string            `json:"note,omitempty"`
	}
	for name, want := range map[string]bool{"Files": true, "Env": true, "Note": false} {
		f, _ := reflect.TypeOf(box{}).FieldByName(name)
		if got := Field(f); got != want {
			t.Errorf("%s reads as a credential %v, want %v", name, got, want)
		}
	}
}

// CONTENT IS A POINTER ONLY WHEN IT IS EXACTLY ONE REFERENCE, and then it reads
// as what the reference names, byte for byte and expanded no further.
func TestContentIsReadWholeOrAsItIs(t *testing.T) {
	t.Parallel()
	held := map[string]string{
		"SEALED": "registry=https://r.example.com\n//r.example.com/:_authToken=${NPM_TOKEN}\n",
		"EMPTY":  "",
	}
	lookup := func(name string) (string, bool) {
		v, ok := held[name]
		return v, ok
	}
	for _, c := range []struct {
		value, want, unresolved string
	}{
		{"${SEALED}", held["SEALED"], ""},
		{"  ${SEALED}\n", held["SEALED"], ""},
		{"${EMPTY}", "", ""},
		{"${GONE}", "", "GONE"},
		{"#!/bin/sh\necho ${HOME}\n", "#!/bin/sh\necho ${HOME}\n", ""},
		{"${SEALED}${SEALED}", "${SEALED}${SEALED}", ""},
		{"plain", "plain", ""},
		{"", "", ""},
	} {
		got, unresolved := ReadContent(c.value, lookup)
		if got != c.want || unresolved != c.unresolved {
			t.Errorf("ReadContent(%q) = (%q, %q), want (%q, %q)",
				c.value, got, unresolved, c.want, c.unresolved)
		}
	}
}
