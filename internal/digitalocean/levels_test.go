package digitalocean

import (
	"bufio"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// pickerScopes is the committed snapshot of DigitalOcean's Custom Scopes
// picker.
func pickerScopes(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("testdata/picker-scopes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		if line := strings.TrimSpace(scan.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// EVERY SCOPE THE PICKER OFFERS IS CLASSIFIED EXACTLY ONCE: granted by a
// level, refused, or accepted as unneeded. A scope DigitalOcean adds fails
// here until somebody decides which it is, and the sheet's untick lists, drawn
// from these, stay complete.
func TestEveryPickerScopeIsClassifiedOnce(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	place := func(list string, scopes []string) {
		for _, s := range scopes {
			if prior, dup := seen[s]; dup {
				t.Errorf("%s is in both %s and %s", s, prior, list)
			}
			seen[s] = list
		}
	}
	for _, l := range levels {
		place(l.id+" adds", l.adds)
	}
	place("refused", refusedScopes)
	place("unneeded", unneededScopes)
	picker := pickerScopes(t)
	if len(picker) != 162 {
		t.Errorf("the snapshot holds %d scopes, want the 162 read on 2026-11-03", len(picker))
	}
	for _, s := range picker {
		if _, ok := seen[s]; !ok {
			t.Errorf("the picker offers %s, which no list classifies", s)
		}
	}
	for s, list := range seen {
		if !slices.Contains(picker, s) {
			t.Errorf("%s names %s, which the picker does not offer", list, s)
		}
	}
}

// THE LEVELS NEST AND MATCH THE DOCUMENT'S: each holds the one before it, in
// the order config lists them, and only Administer administers.
func TestTheLevelsNestInTheDocumentsOrder(t *testing.T) {
	t.Parallel()
	if got := Levels(); !slices.Equal(got, config.DigitalOceanLevels()) {
		t.Fatalf("Levels() = %v, want %v", got, config.DigitalOceanLevels())
	}
	var before []string
	for _, id := range Levels() {
		grants := Grants(id)
		for _, s := range before {
			if !slices.Contains(grants, s) {
				t.Errorf("%s does not hold %s, which the level before it grants", id, s)
			}
		}
		if LevelName(id) == id || LevelNote(id) == "" {
			t.Errorf("%s has no name or note of its own", id)
		}
		before = grants
	}
	if !Administers(config.DigitalOceanLevelAdminister) || Administers(config.DigitalOceanLevelOperate) ||
		Administers(config.DigitalOceanLevelRead) {
		t.Error("only Administer administers")
	}
	if Grants("full_access") != nil {
		t.Error("a level the engine does not offer grants something")
	}
	if !slices.Contains(Grants(config.DigitalOceanLevelRead), "account:read") {
		t.Error("Read lacks account:read, which every check starts with")
	}
}

// EVERY CHECK PROVES A SCOPE OF ITS OWN CLASS, and every write check is one
// that cannot have an effect: a malformed or empty body, or an id that cannot
// exist. No delete is filtered by a query, which would delete by tag or name.
func TestEveryCheckProvesItsClassWithNoEffect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		set   []check
		class []string
	}{
		{"read", readChecks, readScopes},
		{"separator", separatorChecks, operateScopes},
		{"forbidden", forbiddenChecks, refusedScopes},
		{"administer", administerChecks, administerScopes},
	} {
		for _, ch := range tc.set {
			if !slices.Contains(tc.class, ch.scope) {
				t.Errorf("%s check %s proves a scope outside its class", tc.name, ch.scope)
			}
			if tc.name != "read" && ch.can == "" {
				t.Errorf("%s check %s says nothing a refusal can name", tc.name, ch.scope)
			}
			path := ch.path()
			if ch.method == http.MethodGet {
				continue
			}
			if strings.Contains(path, "?") {
				t.Errorf("%s %s filters a write by query", ch.method, path)
			}
			absent := uuidPattern.MatchString(path) || invalidPattern.MatchString(path) ||
				strings.HasSuffix(path, "/0") || strings.Contains(path, "/0/")
			if ch.method == http.MethodDelete && !absent {
				t.Errorf("DELETE %s targets an id that could exist", path)
			}
			if ch.method != http.MethodDelete && ch.body != "{" && ch.body != "{}" {
				t.Errorf("%s %s sends a body DigitalOcean could execute: %q", ch.method, path, ch.body)
			}
		}
	}
	// THE SPACES KEY SCOPES ARE PROVEN BY A READ ALONE: DigitalOcean audits a
	// refused POST /v2/spaces/keys as a key added.
	for _, ch := range forbiddenChecks {
		if strings.HasPrefix(ch.path(), "/v2/spaces/keys") && ch.method != http.MethodGet {
			t.Errorf("%s %s is sent, which DigitalOcean audits", ch.method, ch.path())
		}
	}
	// TWO CHECKS FOR ONE SCOPE would say nothing more, and one is missing
	// where a provable read has none.
	for _, s := range readScopes {
		provable := s != "account:read" && s != "spaces:read"
		has := slices.ContainsFunc(readChecks, func(c check) bool { return c.scope == s })
		if provable != has {
			t.Errorf("read %s: checked %v, want %v", s, has, provable)
		}
	}
}

// A CHECK'S ANSWER IS READ BY ITS STATUS: 403 is the scope missing, a refusal
// of the request is the scope held, and a write that succeeds is an error.
func TestACheckIsReadByItsStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		method string
		status int
		held   bool
		fault  bool
	}{
		{http.MethodGet, 200, true, false},
		{http.MethodGet, 404, true, false},
		{http.MethodGet, 403, false, false},
		{http.MethodPost, 400, true, false},
		{http.MethodPost, 422, true, false},
		{http.MethodDelete, 404, true, false},
		{http.MethodPut, 403, false, false},
		{http.MethodDelete, 204, false, true},
		{http.MethodPost, 201, false, true},
		{http.MethodGet, 401, false, true},
		{http.MethodGet, 412, false, true},
		{http.MethodGet, 500, false, true},
	} {
		held, err := heldBy(tc.method, tc.status)
		if held != tc.held || (err != nil) != tc.fault {
			t.Errorf("%s %d = %v, %v; want held %v, fault %v", tc.method, tc.status, held, err, tc.held, tc.fault)
		}
	}
}

// THE PROVEN SCOPES ARE A LEVEL ONLY WHEN THEY ARE ALL OF ITS CHECKS AND NONE
// OF THE NEXT LEVEL'S.
func TestProvenScopesAreALevelOnlyWhole(t *testing.T) {
	t.Parallel()
	held := func(sets ...[]check) Held {
		h := Held{}
		for _, set := range sets {
			for _, ch := range set {
				h[ch.scope] = true
			}
		}
		return h
	}
	partial := held(readChecks)
	partial[separatorChecks[0].scope] = true
	missingRead := held(readChecks, separatorChecks)
	missingRead[readChecks[0].scope] = false
	adminPart := held(readChecks, separatorChecks)
	adminPart[administerChecks[0].scope] = true
	for _, tc := range []struct {
		name string
		h    Held
		want string
	}{
		{"reads", held(readChecks), config.DigitalOceanLevelRead},
		{"reads and writes", held(readChecks, separatorChecks), config.DigitalOceanLevelOperate},
		{"everything", held(readChecks, separatorChecks, administerChecks), config.DigitalOceanLevelAdminister},
		{"one write", partial, ""},
		{"a read missing", missingRead, ""},
		{"part of Administer", adminPart, ""},
		{"Administer without writes", held(readChecks, administerChecks), ""},
	} {
		if got := tc.h.Level(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A TOKEN'S NAME SAYS WHICH AGENT OF WHICH COMPANY AT WHICH LEVEL.
func TestATokenNameNamesTheAgentAndLevel(t *testing.T) {
	t.Parallel()
	if got, want := TokenName("Acme Works!", "sre", "operate"), "crewlet-acme-works-sre-operate"; got != want {
		t.Errorf("TokenName = %q, want %q", got, want)
	}
	if got, want := TokenName("", "sre", "read"), "crewlet-sre-read"; got != want {
		t.Errorf("TokenName with no company = %q, want %q", got, want)
	}
}
