package config

import (
	"errors"
	"strings"
	"testing"
)

// A KEY NAMES ITS ROLE (ADR-0031), and there is no default in either
// direction: defaulting to admin is the posture the field exists to end — a
// teammate handed a key for their inbox read the company's secret values with
// it — and defaulting to member would quietly lock an operator's existing
// pipeline out of the configuration it writes. So a key that says nothing is
// refused at the field, and so is one that says something this build does not
// know, case included: `Admin` is not a role a person meant and the engine
// will not guess which one they did.
func TestAKeyWithoutARoleIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		key  string
		kind error
		says []string
	}{
		"no role": {
			key:  "{id: founder, token: a}",
			kind: ErrMissing,
			says: []string{"every key needs a role", `"member"`, `"admin"`},
		},
		"an empty role": {
			key:  `{id: founder, role: "", token: a}`,
			kind: ErrMissing,
			says: []string{"every key needs a role"},
		},
		"a role this build does not know": {
			key:  "{id: founder, role: owner, token: a}",
			kind: ErrShape,
			says: []string{`got "owner"`},
		},
		"a role spelt in another case": {
			key:  "{id: founder, role: Admin, token: a}",
			kind: ErrShape,
			says: []string{`got "Admin"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, "api:\n  auth:\n    tokens:\n      - "+tc.key+"\n",
				"api.auth.tokens[0].role")
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}

	// AND BOTH ROLES ARE ACCEPTED, each read back as itself — a check that
	// refused every role would pass every case above.
	b, err := ParseBootstrap([]byte("api:\n  auth:\n    tokens:\n"+
		"      - {id: ada, role: member, token: a}\n"+
		"      - {id: founder, role: admin, token: b}\n"), EnvOnly())
	if err != nil {
		t.Fatalf("a member key beside an admin key was refused: %v", err)
	}
	for id, want := range map[string]TokenRole{"ada": RoleMember, "founder": RoleAdmin, "nobody": ""} {
		if got := b.API.Auth.Role(id); got != want {
			t.Errorf("Role(%q) = %q, want %q", id, got, want)
		}
	}
}

// WHAT A CALLER WITH NO KEY REACHES is one of two postures, and an absent
// value is the default `public` — the company's name, mission and chart, so
// the page that asks for a key can say which company it signs you into. A
// value written out as empty, or a struct built by hand without one, is
// refused rather than read as either: guessing between them guesses about who
// may read the company.
func TestAnonymousReachIsOneOfTwoPostures(t *testing.T) {
	t.Parallel()

	b, err := ParseBootstrap(nil, EnvOnly())
	if err != nil {
		t.Fatalf("an empty bootstrap was refused: %v", err)
	}
	if b.API.Auth.Anonymous != AnonymousPublic {
		t.Errorf("an unset api.auth.anonymous = %q, want the default %q",
			b.API.Auth.Anonymous, AnonymousPublic)
	}

	for _, posture := range AnonymousAccesses {
		doc := "api:\n  auth:\n    anonymous: " + string(posture) + "\n" +
			"    tokens:\n      - {id: founder, role: admin, token: a}\n"
		b, err := ParseBootstrap([]byte(doc), EnvOnly())
		if err != nil {
			t.Errorf("anonymous: %s was refused: %v", posture, err)
			continue
		}
		if b.API.Auth.Anonymous != posture {
			t.Errorf("anonymous: %s read back as %q", posture, b.API.Auth.Anonymous)
		}
	}

	for name, value := range map[string]string{
		"written out as empty":      `""`,
		"a posture that is not one": "read",
		// The switch this field replaced. Refused as an unknown posture
		// rather than mapped onto one: no build ever shipped it.
		"a boolean": "true",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, "api:\n  auth:\n    anonymous: "+value+"\n",
				"api.auth.anonymous")
			if !errors.Is(err, ErrShape) {
				t.Fatalf("want %v, got %v", ErrShape, err)
			}
		})
	}

	// THE ZERO VALUE, built by hand rather than decoded: the default lives in
	// DefaultBootstrap, so a struct that skipped it has said nothing at all.
	hand := DefaultBootstrap()
	hand.API.Auth.Anonymous = ""
	if err := hand.Validate(); !errors.Is(err, ErrShape) || !strings.Contains(err.Error(), "api.auth.anonymous") {
		t.Errorf("a hand-built bootstrap with no anonymous posture: want %v at api.auth.anonymous, got %v", ErrShape, err)
	}
}

// NO KEYS AND NOTHING ANONYMOUS is the one pairing that leaves nothing past
// the sign-in page and no key to sign in with — refused where `crewlet
// validate` sees it, rather than met as a process that starts cleanly and
// answers its own dashboard with a page nobody can get past. Either half on
// its own is a real posture.
func TestNoKeysAndNoAnonymousReachIsRefused(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, "api:\n  auth:\n    anonymous: none\n", "api.auth.tokens")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want %v, got %v", ErrMissing, err)
	}
	for name, doc := range map[string]string{
		"no keys, anonymous public": "api:\n  auth:\n    anonymous: public\n",
		"a key, anonymous none": "api:\n  auth:\n    anonymous: none\n" +
			"    tokens:\n      - {id: ada, role: member, token: a}\n",
	} {
		if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// A WRITER IS AN ADMIN. The role is the boundary on who may change the
// company document at all; company_writers narrows which ADMIN keys may, so a
// managing system is not overwritten by hand (ADR-0030). A member key listed
// there would be the role's boundary crossed through the back door — refused
// at the entry rather than ignored, because an ignored entry is a writer the
// managing system's operator believes exists.
func TestAMemberKeyIsNeverACompanyWriter(t *testing.T) {
	t.Parallel()
	keys := "    tokens:\n" +
		"      - {id: ada, role: member, token: a}\n" +
		"      - {id: gitops, role: admin, token: b}\n"

	err := rejectsBootstrap(t, "api:\n  auth:\n    company_writers: [gitops, ada]\n"+keys,
		"api.auth.company_writers[1]")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want %v, got %v", ErrConflict, err)
	}
	if strings.Contains(err.Error(), "company_writers[0]") {
		t.Errorf("the admin writer beside it was refused too: %v", err)
	}
	for _, want := range []string{`"ada" is a member key`, "role: admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	// An admin key is a writer, and an id no key carries is not judged here:
	// it may be issued after this file is rolled out, so it is a warning
	// ([TestACompanyWriterThatNamesNoTokenIsWarnedAbout]), not a refusal.
	for _, writers := range []string{"[gitops]", "[gitops, not-yet-issued]"} {
		if _, err := ParseBootstrap([]byte("api:\n  auth:\n    company_writers: "+writers+"\n"+keys), EnvOnly()); err != nil {
			t.Errorf("company_writers: %s was refused: %v", writers, err)
		}
	}
}

// A KEY LIST WITH NO ADMIN IN IT is valid and leaves nobody running the engine
// through its API — which every node command meets as a missing key rather
// than as the decision that removed it, so `crewlet validate` says it first.
// Not said when there is no key at all (the anonymous posture is its own
// statement) nor with the guard off, where every caller is an admin.
func TestAKeyListWithNoAdminIsWarnedAbout(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		keys     []APIToken
		disabled bool
		warned   bool
	}{
		"members only":          {keys: []APIToken{{ID: "ada", Role: RoleMember, Token: "a"}}, warned: true},
		"a member and an admin": {keys: []APIToken{{ID: "ada", Role: RoleMember, Token: "a"}, {ID: "founder", Role: RoleAdmin, Token: "b"}}},
		"no keys":               {},
		"members only, guard off": {
			keys: []APIToken{{ID: "ada", Role: RoleMember, Token: "a"}}, disabled: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := DefaultBootstrap()
			b.API.Auth.Tokens = tc.keys
			b.API.Auth.Disabled = tc.disabled
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			var said string
			for _, w := range b.Warnings() {
				if w.Path == "api.auth.tokens" {
					said = w.Message
				}
			}
			if tc.warned != (said != "") {
				t.Fatalf("warned at api.auth.tokens = %q, want a warning: %v", said, tc.warned)
			}
			if tc.warned && !strings.Contains(said, "role: admin") {
				t.Errorf("the warning does not name the fix: %q", said)
			}
		})
	}
}
