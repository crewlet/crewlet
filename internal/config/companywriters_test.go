package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// WHO MAY CHANGE THE COMPANY DOCUMENT is one reading of one list, and the
// reading has exactly three answers worth pinning: an empty list is everybody,
// a listed id may, and an unlisted one — the disabled guard's caller among
// them — may not.
func TestOnlyAListedTokenMayWriteAManagedCompany(t *testing.T) {
	t.Parallel()
	open := config.APIAuth{}
	if open.CompanyManaged() || !open.MayWriteCompany("founder") {
		t.Fatal("with no company_writers the document is unmanaged and every token writes it")
	}
	managed := config.APIAuth{CompanyWriters: []string{"operator"}}
	if !managed.CompanyManaged() {
		t.Fatal("a listed writer did not make the document managed")
	}
	for id, want := range map[string]bool{
		"operator": true, "founder": false, "": false, "Operator": false,
		org.ReservedOperatorID: false,
	} {
		if got := managed.MayWriteCompany(id); got != want {
			t.Errorf("MayWriteCompany(%q) = %v, want %v", id, got, want)
		}
	}
}

// AND THE SETTINGS THAT ARE VALID AND ALMOST CERTAINLY A TYPO are said before
// the managing system meets a 403 — each at the field to change.
func TestACompanyWriterThatNamesNoTokenIsWarnedAbout(t *testing.T) {
	t.Parallel()
	base := func() *config.Bootstrap {
		b := config.DefaultBootstrap()
		b.Stream.StoreDir = "/var/lib/crewlet/stream"
		b.Retention.BackupOwner = "platform-oncall"
		b.API.Auth.Tokens = []config.APIToken{
			{ID: "operator", Token: "a"}, {ID: "founder", Token: "b"},
		}
		return &b
	}
	for name, tc := range map[string]struct {
		writers  []string
		disabled bool
		want     map[string]string // path -> fragment
	}{
		"a configured writer is quiet": {
			writers: []string{"operator"}, want: map[string]string{},
		},
		"a typo names no token, and leaves the document frozen": {
			writers: []string{"operater"},
			want: map[string]string{
				"api.auth.company_writers[0]": `"operater" names no token`,
				"api.auth.company_writers":    "freezes the document",
			},
		},
		"one stray id beside a real one is named alone": {
			writers: []string{"operator", "gitops"},
			want: map[string]string{
				"api.auth.company_writers[1]": `"gitops" names no token`,
			},
		},
		"a disabled guard lets nobody write": {
			writers: []string{"operator"}, disabled: true,
			want: map[string]string{
				"api.auth.company_writers": "api.auth.disabled is true",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := base()
			b.API.Auth.CompanyWriters = tc.writers
			b.API.Auth.Disabled = tc.disabled
			if err := b.Validate(); err != nil {
				t.Fatalf("the fixture does not validate: %v", err)
			}
			got := map[string]string{}
			for _, w := range b.Warnings() {
				got[w.Path] = w.Message
			}
			if len(got) != len(tc.want) {
				t.Errorf("warnings = %v, want paths %v", got, tc.want)
			}
			for path, fragment := range tc.want {
				if !strings.Contains(got[path], fragment) {
					t.Errorf("warning at %s = %q, want it to say %q", path, got[path], fragment)
				}
			}
		})
	}
}
