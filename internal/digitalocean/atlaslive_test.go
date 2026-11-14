//go:build atlaslive

package digitalocean

import (
	"os"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/collect/collecttest"
	"github.com/crewlet/crewlet/internal/config"
)

// A LIVE READ OF A REAL TEAM, run by hand and never by the suite:
//
//	DIGITALOCEAN_TEAM=... DIGITALOCEAN_ATLAS_TOKEN=... \
//	  go test -tags atlaslive -count=1 -run TestLiveAtlasRead -v ./internal/digitalocean/
//
// It prints how many entries of each kind and relations of each sort the read
// holds, and which parts it was refused. It prints no name, id, address or
// any other value the team holds.
func TestLiveAtlasRead(t *testing.T) {
	team, token := os.Getenv("DIGITALOCEAN_TEAM"), os.Getenv("DIGITALOCEAN_ATLAS_TOKEN")
	if team == "" || token == "" {
		t.Fatal("set DIGITALOCEAN_TEAM and DIGITALOCEAN_ATLAS_TOKEN")
	}
	c := &config.Company{Name: "live"}
	c.Integrations.DigitalOcean = &config.DigitalOcean{Enabled: true,
		Provisioning: &config.DigitalOceanProvisioning{Team: team},
		Atlas:        &config.DigitalOceanAtlas{Token: "${DIGITALOCEAN_ATLAS_TOKEN}"}}
	env := config.NewResolver(config.MapSource{"DIGITALOCEAN_ATLAS_TOKEN": token})
	read, err := atlasSource{}.Collect(t.Context(), collect.Input{Config: c, Env: env, Now: time.Now().UTC()})
	if err != nil {
		// THE FAULT IS THE ENGINE'S OWN SENTENCE, which carries no value of
		// the team's beyond its name and the scope at fault.
		t.Fatalf("the read failed: %v", err)
	}
	collecttest.Counts(t, read)
}
