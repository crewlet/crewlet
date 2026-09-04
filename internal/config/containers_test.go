package config

import (
	"errors"
	"strings"
	"testing"
)

// THE GRAMMAR IS THE NARROWEST BACKEND'S, and that is what makes the org
// chart portable: a native company that wrote a key Jira cannot hold would
// have to rename its whole hierarchy on the day it switched, with every item
// key it ever minted pointing at the old name.
func TestAContainerKeyIsPortableAcrossBackends(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"EN", "ENG", "PROD", "TS2", "ABCDEFGHIJ", "X1234"} {
		if !ValidContainerKey(key) {
			t.Errorf("%q is refused and every backend accepts it", key)
		}
	}
	for _, key := range []string{
		"",
		"E",            // Jira's minimum is two characters
		"ABCDEFGHIJK",  // one over Jira's ten
		"eng",          // lower case: two spellings of one container
		"ENG-PLATFORM", // a hyphen; Confluence refuses it too
		"1ENG",         // must start with a letter
		"ENG PLATFORM", // a space
		"ÜBER",         // non-ASCII
	} {
		if ValidContainerKey(key) {
			t.Errorf("%q is accepted and at least one backend refuses it", key)
		}
	}
}

// The refusal names the path an operator can search their file for, and the
// rule — "invalid value" with no location is the error this package exists
// to avoid producing.
func TestABadContainerKeyIsRefusedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		path string
	}{
		{"a unit project", "name: Acme\nunits:\n  - name: Eng\n    project: eng\n", "units[0].project"},
		{"a unit space", "name: Acme\nunits:\n  - name: Eng\n    space: eng-wiki\n", "units[0].space"},
		{"a nested unit", "name: Acme\nunits:\n  - name: Eng\n    children:\n      - name: Core\n        project: c\n",
			"units[0].children[0].project"},
		{"a seat's own project", "name: Acme\nroles:\n  - name: Solo\n    project: solo\n", "roles[0].project"},
		{"a seat inside a unit", "name: Acme\nunits:\n  - name: Eng\n    roles:\n      - name: Dev\n        space: x\n",
			"units[0].roles[0].space"},
		{"a read scope entry", "name: Acme\nknowledge:\n  scope: [HANDBOOK, bad-key]\n", "knowledge.scope[1]"},
		{"the skills container", "name: Acme\nknowledge:\n  skills_container: skills-and-more\n", "knowledge.skills_container"},
		{"the root space", "name: Acme\nknowledge:\n  root_space: my home\n", "knowledge.root_space"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, tc.path)
			if !errors.Is(err, ErrUnknownValue) {
				t.Fatalf("want ErrUnknownValue, got %v", err)
			}
			if !strings.Contains(err.Error(), "backend switch") {
				t.Errorf("the message must say why the grammar is this tight; got:\n%v", err)
			}
		})
	}
}

// A UNIT WRITING INTO A RESERVED CONTAINER IS SILENTLY UNREADABLE: the skills
// container is excluded from knowledge search and from routing, so a team
// filing its pages there would publish into a hole. The refusal names which
// of the two settings to move.
func TestAReservedContainerIsRefusedAsAUnitsOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, doc, want string }{
		{"the skills container", "name: Acme\nunits:\n  - name: Eng\n    space: TS\n", "tool-skill pages"},
		{"the root space", "name: Acme\nunits:\n  - name: Eng\n    space: HOME\n", "root Onboarding page"},
		{"a moved skills container", "name: Acme\nknowledge:\n  skills_container: SK\nunits:\n  - name: Eng\n    space: SK\n", "tool-skill pages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.doc, "units[0].space")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("want ErrConflict, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the message must say what reserves the key; got:\n%v", err)
			}
		})
	}

	// A PROJECT IS A DIFFERENT NAMESPACE. Reserving a knowledge container
	// says nothing about the tracker, and refusing TS as a project key
	// would deny a company a project it can legitimately hold.
	if _, err := ParseCompany([]byte("name: Acme\nunits:\n  - name: Eng\n    project: TS\n")); err != nil {
		t.Errorf("a tracker project named TS was refused: %v", err)
	}

	// TURNING TOOL SKILLS OFF UNRESERVES THE KEY, which is the whole reason
	// the setting is three-valued.
	if _, err := ParseCompany([]byte(
		"name: Acme\nknowledge:\n  skills_container: \"\"\nunits:\n  - name: Eng\n    space: TS\n")); err != nil {
		t.Errorf("TS stayed reserved after tool skills were turned off: %v", err)
	}
}

// TWO UNITS MAY SHARE A KEY. The shipped example does it deliberately, and
// what actually goes wrong is narrower — units resolving to different leads —
// which org.LeadsBy reports with the candidates it chose between.
func TestTwoUnitsMayShareAContainerKey(t *testing.T) {
	t.Parallel()
	mustCompany(t, `
name: Acme
roles:
  - name: PM
    handle: pm
units:
  - name: Product
    lead: PM
    project: PROD
    space: PROD
  - name: Content
    lead: PM
    project: PROD
    space: PROD
`)
}
