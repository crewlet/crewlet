package gitlet

import (
	"reflect"
	"testing"
)

// THE SETTINGS COMPARISON READS EVERY FIELD BUT THE ACTIVATION: under the
// activation fence it alone decides whether a declared change reaches the log,
// so a skipped field is a change every activation drops silently. The cases are
// keyed by field name and held to RepoSettings' fields by reflection, so a new
// field fails here until it is given a variant, and with it a clause.
func TestSettingsEqualReadsEveryFieldButTheActivation(t *testing.T) {
	t.Parallel()
	base := RepoSettings{
		ManagedBy: ManagedConfig, Kind: RepoProject, Owner: Owner{Kind: OwnerUnit, Ref: "ENG"},
		DefaultBranch: "main", Description: "one", Visibility: VisibilityCompany, Search: SearchIndexed,
		Protections: []Protection{{PushRule: PushRule{Pattern: "refs/heads/main", DirectPush: DirectPersons}}},
		PathOwners:  []PathOwner{{Owner: Owner{Kind: OwnerUnit, Ref: "ENG"}, Paths: []string{"web/**"}}},
		MirrorFrom:  &MirrorSource{URL: "https://forge.example.com/a/web.git", Credential: "${A}"},
		MirrorTo: &MirrorTarget{URL: "https://forge.example.com/b/web.git", Credential: "${B}", As: "dev",
			Refs: []string{"refs/heads/main"}},
		ConfigEpoch: 1,
	}
	variants := map[string]func(*RepoSettings){
		"ManagedBy":     func(s *RepoSettings) { s.ManagedBy = "other" },
		"Kind":          func(s *RepoSettings) { s.Kind = RepoScratch },
		"Owner":         func(s *RepoSettings) { s.Owner.Ref = "OPS" },
		"DefaultBranch": func(s *RepoSettings) { s.DefaultBranch = "trunk" },
		"Description":   func(s *RepoSettings) { s.Description = "two" },
		"Archived":      func(s *RepoSettings) { s.Archived = true },
		"Visibility":    func(s *RepoSettings) { s.Visibility = VisibilityUnit },
		"Search":        func(s *RepoSettings) { s.Search = SearchGrep },
		"Protections": func(s *RepoSettings) {
			s.Protections = []Protection{{PushRule: PushRule{Pattern: "refs/heads/main", DirectPush: DirectNone}}}
		},
		// A GLOB ADDED TO THE ONE OWNER, which comparing globs by count or by
		// first element would miss.
		"PathOwners": func(s *RepoSettings) {
			s.PathOwners = []PathOwner{{Owner: Owner{Kind: OwnerUnit, Ref: "ENG"}, Paths: []string{"docs/**",
				"web/**"}}}
		},
		// EACH MIRROR BY WHAT IT SAYS, never by its pointer: a credential
		// moved to another variable, and a ref added to a push mirror's.
		"MirrorFrom": func(s *RepoSettings) {
			s.MirrorFrom = &MirrorSource{URL: s.MirrorFrom.URL, Credential: "${C}"}
		},
		"MirrorTo": func(s *RepoSettings) {
			m := *s.MirrorTo
			m.Refs = []string{"refs/heads/main", "refs/tags/*"}
			s.MirrorTo = &m
		},
	}
	ignored := map[string]bool{"ConfigEpoch": true}

	fields := reflect.TypeFor[RepoSettings]()
	for i := range fields.NumField() {
		name := fields.Field(i).Name
		if _, ok := variants[name]; !ok && !ignored[name] {
			t.Errorf("RepoSettings.%s has no case here — give it one, and equal a clause "+
				"comparing it, or a declared change to it never reaches the log", name)
		}
	}
	for name, change := range variants {
		other := base
		other.Protections = append([]Protection(nil), base.Protections...)
		change(&other)
		if base.equal(other) || other.equal(base) {
			t.Errorf("settings differing only in %s compare equal", name)
		}
	}

	restamped := base
	restamped.ConfigEpoch = 2
	if !base.equal(restamped) {
		t.Error("settings differing only in the activation they came from compare unequal — " +
			"every activation would re-stamp every repository")
	}
	none, empty := base, base
	none.Protections, empty.Protections = nil, []Protection{}
	if !none.equal(empty) {
		t.Error("no rules and an empty rule list compare unequal")
	}
	none.PathOwners, empty.PathOwners = nil, []PathOwner{}
	if !none.equal(empty) {
		t.Error("no path owners and an empty list of them compare unequal")
	}
	copied := base
	from, to := *base.MirrorFrom, *base.MirrorTo
	to.Refs = append([]string(nil), base.MirrorTo.Refs...)
	copied.MirrorFrom, copied.MirrorTo = &from, &to
	if !base.equal(copied) {
		t.Error("the same mirrors read into new values compare unequal — every activation would re-record them")
	}
	none.MirrorFrom, none.MirrorTo = nil, nil
	if none.equal(base) || base.equal(none) {
		t.Error("settings with mirrors and settings with none compare equal")
	}
}
