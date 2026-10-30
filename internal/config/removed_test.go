package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// retiredSeedYAML is a company.yaml written before per-seat access: the GitHub
// tier, Datadog's company-wide role and the single Mattermost channel, each
// retired and refused by name, and nothing else wrong with it.
const retiredSeedYAML = `
name: Legacy
providers:
  sandbox: {fake: true}
integrations:
  datadog:
    enabled: true
    webhook_token: "${DATADOG_WEBHOOK_TOKEN}"
    route_to: none
    provisioning:
      site: datadoghq.com
      api_key: "${DATADOG_API_KEY}"
      app_key: "${DATADOG_APP_KEY}"
      role: r-1
roles:
  - name: SRE
    handle: sre
    integrations:
      github:
        enabled: false
        tier: full_access
      mattermost:
        enabled: false
        channel: town-square
`

// retiredSeedKeys is where retiredSeedYAML carries each retired key.
var retiredSeedKeys = []string{
	"integrations.datadog.provisioning.role:14",
	"roles[0].integrations.github.tier:21",
	"roles[0].integrations.mattermost.channel:24",
}

// writeSeed writes doc as a company file in a directory of its own.
func writeSeed(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// placesOf is every problem in err as path:line, sorted.
func placesOf(err error) []string {
	var places []string
	for _, p := range Problems(err) {
		places = append(places, p.Path+":"+strconv.Itoa(p.Line))
	}
	slices.Sort(places)
	return places
}

// A SEED FILE WHOSE ONLY FAULT IS A RETIRED KEY LOADS WITHOUT IT, its refusal
// set aside rather than returned, and that refusal is exactly the one every
// other reader of the file gives.
//
// A deployment upgraded to the build that retired a key still carries it in
// the company.yaml its node starts with. Refused at load, the node never
// started, though the company it would have started on is the stored one,
// which reads without the key. The refusal is kept whole, typed as a retired
// key and classified as the unknown field it is, for the one place the file
// would be written.
func TestASeedFileWhoseOnlyFaultIsARetiredKeyLoadsWithoutIt(t *testing.T) {
	t.Parallel()
	path := writeSeed(t, retiredSeedYAML)

	_, refused := LoadCompanyToRun(path, nil)
	if refused == nil {
		t.Fatal("LoadCompanyToRun accepted a file carrying retired keys")
	}
	file, err := LoadCompanySeed(path, nil)
	if err != nil {
		t.Fatalf("LoadCompanySeed refused a file whose only fault is retired keys: %v", err)
	}
	if file.Company == nil || file.Company.Name != "Legacy" || len(file.Company.Roles) != 1 {
		t.Fatalf("the seed is not the file's company: %+v", file.Company)
	}
	if file.Retired == nil || file.Retired.Error() != refused.Error() {
		t.Fatalf("the set-aside refusal is %v,\nwant LoadCompanyToRun's own: %v", file.Retired, refused)
	}
	if !errors.Is(file.Retired, ErrRemovedField) || !errors.Is(file.Retired, ErrUnknownField) {
		t.Errorf("the refusal is not a removed key that is also an unknown field: %v", file.Retired)
	}
	if got := placesOf(file.Retired); !slices.Equal(got, retiredSeedKeys) {
		t.Errorf("the refusal names %v, want each retired key at its line %v", got, retiredSeedKeys)
	}
	for _, p := range Problems(file.Retired) {
		if p.Kind != "unknown_field" || !strings.Contains(p.Message, "was removed") {
			t.Errorf("%s is classified %q as %q, want an unknown_field naming its removal",
				p.Path, p.Kind, p.Message)
		}
	}

	// A FILE WITH NO RETIRED KEY sets nothing aside.
	clean, err := LoadCompanySeed(writeSeed(t, "name: Clean\nproviders:\n  sandbox: {fake: true}\n"), nil)
	if err != nil || clean.Retired != nil {
		t.Errorf("a clean file = %v, retired %v, want it loaded with nothing set aside", err, clean.Retired)
	}
}

// A RETIRED KEY NEVER HIDES A GENUINE FAULT: a file that is wrong in any other
// way fails as it always did, whether the parser finds the fault beside the
// retired key or a rule finds it once the key is set aside, and the refusal
// names the retired keys beside it.
//
// Mutation: set every unknown field aside rather than only the retired ones,
// and the typo loads.
func TestARetiredKeyNeverHidesAGenuineFault(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		amend   func(string) string
		genuine string
	}{
		"a typo the parser finds": {
			amend: func(doc string) string {
				return strings.Replace(doc, "    handle: sre\n", "    handle: sre\n    gaol: ship\n", 1)
			},
			genuine: `"gaol" isn't a setting`,
		},
		"a rule broken once the keys are set aside": {
			amend: func(doc string) string {
				return strings.Replace(doc, "    handle: sre\n", "    handle: sre\n    kind: robot\n", 1)
			},
			genuine: "robot",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadCompanySeed(writeSeed(t, tc.amend(retiredSeedYAML)), nil)
			if err == nil {
				t.Fatal("LoadCompanySeed accepted a file with a genuine fault beside its retired keys")
			}
			for _, want := range []string{tc.genuine, "github.tier", "was removed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}
