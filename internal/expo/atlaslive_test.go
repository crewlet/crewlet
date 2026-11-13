//go:build atlaslive

package expo_test

import (
	"os"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/collect/collecttest"
	"github.com/crewlet/crewlet/internal/config"
)

// A LIVE READ OF A REAL ACCOUNT, run by hand and never by the suite:
//
//	EXPO_ACCOUNT=... EXPO_ATLAS_TOKEN=... \
//	  go test -tags atlaslive -count=1 -run TestLiveAtlasRead -v ./internal/expo/
//
// It prints how many entries of each kind and relations of each sort the read
// holds. It prints no name, slug, id or any other value the account holds.
func TestLiveAtlasRead(t *testing.T) {
	account, token := os.Getenv("EXPO_ACCOUNT"), os.Getenv("EXPO_ATLAS_TOKEN")
	if account == "" || token == "" {
		t.Fatal("set EXPO_ACCOUNT and EXPO_ATLAS_TOKEN")
	}
	c := &config.Company{Name: "live"}
	c.Integrations.Expo = &config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: account},
		Atlas:        &config.ExpoAtlas{Token: "${EXPO_ATLAS_TOKEN}"}}
	env := config.NewResolver(config.MapSource{"EXPO_ATLAS_TOKEN": token})
	read, err := expoSource(t).Collect(t.Context(), collect.Input{Config: c, Env: env, Now: time.Now().UTC()})
	if err != nil {
		// THE FAULT IS THE ENGINE'S OWN SENTENCE, which carries no value of
		// the account's beyond its name and the robot's.
		t.Fatalf("the read failed: %v", err)
	}
	collecttest.Counts(t, read)
}
