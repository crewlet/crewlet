package code

import (
	"reflect"
	"testing"
)

// THE SETTINGS COMPARISON READS EVERY FIELD BUT THE ACTIVATION. Under the
// activation fence it is the one thing deciding whether a declared change
// reaches the log, so a field it skips is a change every activation drops
// silently — and a field ADDED to RepoSettings without a clause here is the
// same bug arriving later. So the cases are keyed by field name and held to
// the struct's own fields by reflection: a new field fails this test until it
// is given a variant, and with it a clause.
func TestSettingsEqualReadsEveryFieldButTheActivation(t *testing.T) {
	t.Parallel()
	base := RepoSettings{
		ManagedBy: ManagedConfig, Kind: RepoProject, Owner: Owner{Kind: OwnerUnit, Ref: "ENG"},
		DefaultBranch: "main", Description: "one", Visibility: VisibilityCompany,
		Protections: []Protection{{Pattern: "refs/heads/main", DirectPush: DirectPersons}},
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
		"Protections": func(s *RepoSettings) {
			s.Protections = []Protection{{Pattern: "refs/heads/main", DirectPush: DirectNone}}
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
}
