package engine

import (
	"io/fs"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/static"
)

// EVERY SEAT IS TOLD THE NEWEST KIT THIS BUILD EMBEDS. The artifacts skill
// links `/_crewlet/kit/${crewlet_kit_version}/`, and a page pinned to an older
// kit never reads the brand slots a newer one added, so a stale value would
// keep every page an agent writes off the company's brand without anything
// failing. The value is the embedded tree's newest release, whatever else the
// company declares, and a company with no variables of its own still gets it.
// (static's TestTheNewestKitIsTheLedgersLast holds the same release to the
// end of the ledger of shipped kits.)
func TestEverySkillIsToldTheNewestEmbeddedKit(t *testing.T) {
	t.Parallel()
	tree, err := fs.Sub(static.FS(), kitTree)
	if err != nil {
		t.Fatal(err)
	}
	kits, err := artifactupload.ReleasesIn(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := kits.Latest()
	if want == "" {
		t.Fatal("the build embeds no artifact kit, so this case certifies nothing")
	}
	if got := kitVersion(); got != want {
		t.Fatalf("kitVersion() = %q, want the newest embedded kit %q", got, want)
	}
	for name, c := range map[string]*Company{
		"a company with no variables": {Config: &config.Company{}},
		"a company with its own":      {Config: &config.Company{SkillVariables: map[string]string{"wiki": "https://wiki.example.com"}}},
	} {
		vars := skillVariables(config.NewResolver(), c, "", kitVersion())
		if got := vars[config.ReservedKitVersionVariable]; got != want {
			t.Errorf("%s: ${%s} = %q, want %q", name, config.ReservedKitVersionVariable, got, want)
		}
	}
}
