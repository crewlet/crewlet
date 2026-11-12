package builtin

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A VENDOR'S LISTING IN THE ATLAS IS AN OUTSIDE PARTY'S TEXT: an entry an
// integration's read reported reads `external` on the answer's envelope,
// whoever else wrote beside it, and one only the company's settings name reads
// as a colleague's.
//
// Mutation: drop the vendor's branch in attribute, and the first answer reads
// as a colleague's.
func TestAVendorsListingInTheAtlasIsAnOutsidePartysText(t *testing.T) {
	t.Parallel()
	listed := atlas.Entry{Sources: []atlas.Source{atlas.SettingsSource("r1"), atlas.IntegrationSource("cloudflare")}}
	declared := atlas.Entry{Sources: []atlas.Source{atlas.SettingsSource("r1")}}
	for _, c := range []struct {
		entry atlas.Entry
		want  provenance.Trust
	}{{listed, provenance.TrustExternal}, {declared, provenance.TrustColleague}} {
		a := atlasRead{read: newForeignRead(nil, "sre")}
		a.attribute([]atlas.Entry{c.entry})
		envelope, labelled := a.read.envelope()
		if !labelled || envelope.Trust != c.want {
			t.Errorf("an entry from %v reads %q (labelled %v), want %q", c.entry.Sources, envelope.Trust,
				labelled, c.want)
		}
	}
}

// A SEAT REACHES THE MODELS ITS OWN CHAINS NAME, and the embeddings every
// seat's searches use, and no other model: a model another seat runs on is
// outside its reach like a server it has no access to.
//
// Mutation: answer every model as within reach, and yankee is reached.
func TestASeatReachesTheModelsItsOwnChainsName(t *testing.T) {
	t.Parallel()
	c := &config.Company{Name: "acme"}
	c.Providers.LLM = map[string]config.LLMProvider{
		"zulu":   {Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"},
		"yankee": {Harness: config.HarnessClaudeCode, Model: "claude-haiku-5-5"},
	}
	c.Roles = []config.Role{
		{Name: "SRE", Handle: "sre", LLM: config.PhaseLLM{Default: config.ProviderKeys{"zulu"}}},
		{Name: "Dev", Handle: "dev", LLM: config.PhaseLLM{Default: config.ProviderKeys{"yankee"}}},
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	reach := seatReach(c, o.AgentSeatByHandle("sre"))
	model := func(key string) atlas.Entry {
		r := atlas.MustRef(atlas.KindModel, key)
		return atlas.Entry{Ref: r, Members: []atlas.Ref{r}}
	}
	for key, want := range map[string]bool{
		atlas.ModelLLMPrefix + "/zulu": true, atlas.ModelLLMPrefix + "/yankee": false, atlas.ModelEmbeddings: true,
	} {
		if got := reach(model(key)); got != want {
			t.Errorf("sre reaches %s: %v, want %v", key, got, want)
		}
	}
}
