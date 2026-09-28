package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	mapplacement "github.com/crewlet/crewlet/internal/placement"
)

// AN UNSET REPLICA COUNT IS THE DEFAULT, never zero copies. A company that says
// nothing about its estate keeps three copies of each partition, and a named
// count is kept exactly.
func TestTheEstateReplicaCountDefaultsToThree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ set, want int }{
		{0, config.DefaultEstateReplicas},
		{1, 1},
		{5, 5},
	} {
		e := config.Estate{Replicas: tc.set}
		if got := e.ReplicaCount(); got != tc.want {
			t.Errorf("replicas %d resolves to %d, want %d", tc.set, got, tc.want)
		}
	}
	if config.DefaultEstateReplicas != 3 {
		t.Errorf("the default is %d: three is the smallest count that survives a "+
			"loss while a second copy is rebuilt, and that keeps the two snapshots "+
			"a partition's trim waits for while one holder has lost its store",
			config.DefaultEstateReplicas)
	}
}

// THE CEILING IS THE MAP'S, as the object store's is: a count this accepted and
// the estate map refused would be a revision every node applies and no
// maintainer can write.
func TestTheEstateReplicaCeilingIsTheEstateMaps(t *testing.T) {
	t.Parallel()
	if config.MaxEstateReplicas != mapplacement.MaxReplicas {
		t.Fatalf("config allows %d copies, the map %d", config.MaxEstateReplicas,
			mapplacement.MaxReplicas)
	}
}

// THE BLOCK IS VALIDATED WHERE IT WAS WRITTEN: a count outside 0..10 and a
// failure domain no node label could carry are refused naming the field, and a
// well-formed block is accepted.
func TestTheEstateBlockIsValidated(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		estate config.Estate
		refuse string // "" = accepted
	}{
		"the zero block":         {config.Estate{}, ""},
		"copies across zones":    {config.Estate{Replicas: 5, FailureDomain: "zone"}, ""},
		"the most copies":        {config.Estate{Replicas: config.MaxEstateReplicas}, ""},
		"negative copies":        {config.Estate{Replicas: -1}, "estate.replicas"},
		"more copies than a map": {config.Estate{Replicas: config.MaxEstateReplicas + 1}, "estate.replicas"},
		"a domain with a space":  {config.Estate{FailureDomain: "zone "}, "estate.failure_domain"},
		"a domain too long":      {config.Estate{FailureDomain: strings.Repeat("z", 64)}, "estate.failure_domain"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Estate = tc.estate
			err := c.Validate()
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %+v", tc.estate)
			}
			if !strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("refusal %q does not name %s", err, tc.refuse)
			}
		})
	}
}

// A DATA NODE'S SHARE OF THE ESTATE IS ITS OWN, and unset is the default share
// of one, never none: a weight of zero would be a member the map places
// nothing on. Outside 1..64 it is refused naming the field.
func TestADataNodesEstateWeightIsItsShare(t *testing.T) {
	t.Parallel()
	if config.DefaultEstateWeight != 1 {
		t.Errorf("the default share is %d, want 1: equal to every node that names none",
			config.DefaultEstateWeight)
	}
	if got := (config.StoreEstate{}).EstateWeight(); got != config.DefaultEstateWeight {
		t.Errorf("an unset weight resolves to %d, want the default share", got)
	}
	if got := (config.StoreEstate{Weight: 4}).EstateWeight(); got != 4 {
		t.Errorf("a weight of 4 resolves to %d", got)
	}
	for name, tc := range map[string]struct {
		weight int
		refuse bool
	}{
		"the default":  {0, false},
		"the heaviest": {mapplacement.MaxWeight, false},
		"negative":     {-1, true},
		"too heavy":    {mapplacement.MaxWeight + 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Store.Estate.Weight = tc.weight
			err := b.Validate()
			switch {
			case !tc.refuse && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refuse && err == nil:
				t.Fatalf("accepted a weight of %d", tc.weight)
			case tc.refuse && !strings.Contains(err.Error(), "store.estate.weight"):
				t.Errorf("refusal %q does not name store.estate.weight", err)
			}
		})
	}
}

// ONE WARNING PER MISSING LABEL KEY, naming every block that spreads across
// it. The object store and the estate read a failure domain the same way, so a
// data node missing the key both name is one label it lacks, said once; two
// keys it lacks are two labels to set. A node that holds no data is placed on
// by neither map and is warned about nothing.
func TestAMissingFailureDomainLabelIsOneWarningPerKey(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects, estate string
		labels          map[string]string
		roles           []string
		want            map[string][]string // path → blocks it must name
	}{
		"the estate's key missing": {"", "zone", nil, nil,
			map[string][]string{"node.labels.zone": {"estate.failure_domain"}}},
		"the estate's key carried": {"", "zone", map[string]string{"zone": "a"}, nil, nil},
		"one key both blocks name": {"zone", "zone", nil, nil,
			map[string][]string{"node.labels.zone": {"objects.failure_domain", "estate.failure_domain"}}},
		"two keys, both missing": {"zone", "rack", nil, nil, map[string][]string{
			"node.labels.zone": {"objects.failure_domain"},
			"node.labels.rack": {"estate.failure_domain"}}},
		"two keys, one carried": {"zone", "rack", map[string]string{"rack": "r1"}, nil,
			map[string][]string{"node.labels.zone": {"objects.failure_domain"}}},
		"a stateless node": {"zone", "zone", nil, []string{"seats"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = t.TempDir()
			b.Node.Labels = tc.labels
			b.Node.Roles = tc.roles
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Objects.FailureDomain = tc.objects
			c.Estate.FailureDomain = tc.estate

			got := config.TierWarnings(&b, &c)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d warnings, want %d: %+v", len(got), len(tc.want), got)
			}
			for _, w := range got {
				blocks, ok := tc.want[w.Path]
				if !ok || w.Kind != config.WarningAdvisory {
					t.Errorf("an unexpected %s at %q: %s", w.Kind, w.Path, w.Message)
					continue
				}
				for _, block := range blocks {
					if !strings.Contains(w.Message, block) {
						t.Errorf("the warning at %s does not name %s: %q", w.Path, block, w.Message)
					}
				}
			}
		})
	}
}

// ONE COPY OF EACH PARTITION ON A NODE OF A FLEET IS WARNED ABOUT: it is valid,
// and it is a company that loses whatever one disk held. Neither tier can see
// it alone — the company names the count, the node says it is one of several —
// and a solo node, a node holding no data or a count above one is warned about
// nothing.
func TestOneCopyOfTheEstateInAFleetIsWarned(t *testing.T) {
	t.Parallel()
	fleet := []string{"nats://b.example.com:6222"}
	for name, tc := range map[string]struct {
		replicas int
		peers    []string
		roles    []string
		warn     bool
	}{
		"one copy on a member of a fleet": {1, fleet, []string{"data", "seats"}, true},
		"the default on a fleet":          {0, fleet, []string{"data", "seats"}, false},
		"two copies on a fleet":           {2, fleet, []string{"data", "seats"}, false},
		"one copy on a solo node":         {1, nil, []string{"data", "seats"}, false},
		"one copy on a stateless member":  {1, fleet, []string{"seats"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = t.TempDir()
			b.Stream.Cluster.Name = "acme"
			b.Stream.Cluster.Peers = tc.peers
			b.Node.Roles = tc.roles
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Estate.Replicas = tc.replicas

			got := config.TierWarnings(&b, &c)
			if !tc.warn {
				if len(got) != 0 {
					t.Fatalf("warned: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Path != "estate.replicas" ||
				got[0].Kind != config.WarningAdvisory {
				t.Fatalf("want one advisory at estate.replicas, got %+v", got)
			}
		})
	}
}
