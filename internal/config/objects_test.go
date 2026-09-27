package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
)

// AN UNSET REPLICA COUNT IS THE DEFAULT, never zero copies. A company that
// says nothing about its object store keeps three copies of every chunk, and a
// named count is kept exactly.
func TestTheObjectReplicaCountDefaultsToThree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		set, want int
	}{
		{0, config.DefaultObjectReplicas},
		{1, 1},
		{5, 5},
	} {
		o := config.Objects{Replicas: tc.set}
		if got := o.ReplicaCount(); got != tc.want {
			t.Errorf("replicas %d resolves to %d, want %d", tc.set, got, tc.want)
		}
	}
	if config.DefaultObjectReplicas != 3 {
		t.Errorf("the default is %d: three is the smallest count that survives a "+
			"loss while a second copy is being rebuilt", config.DefaultObjectReplicas)
	}
}

// THE CEILING IS THE MAP'S. A count this accepted and the placement map
// refused would be a revision every node applies and no maintainer can write.
func TestTheObjectReplicaCeilingIsThePlacementMaps(t *testing.T) {
	t.Parallel()
	if config.MaxObjectReplicas != objplacement.MaxReplicas {
		t.Fatalf("config allows %d copies, the map %d", config.MaxObjectReplicas,
			objplacement.MaxReplicas)
	}
}

// THE BLOCK IS VALIDATED WHERE IT WAS WRITTEN: a count outside 0..10 and a
// failure domain that no node label could carry are refused naming the field,
// and a well-formed block is accepted.
func TestTheObjectsBlockIsValidated(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects config.Objects
		refuse  string // "" = accepted
	}{
		"the zero block":         {config.Objects{}, ""},
		"copies across zones":    {config.Objects{Replicas: 5, FailureDomain: "zone"}, ""},
		"the most copies":        {config.Objects{Replicas: config.MaxObjectReplicas}, ""},
		"negative copies":        {config.Objects{Replicas: -1}, "objects.replicas"},
		"more copies than a map": {config.Objects{Replicas: config.MaxObjectReplicas + 1}, "objects.replicas"},
		"a domain with a space":  {config.Objects{FailureDomain: "zone "}, "objects.failure_domain"},
		"a domain too long":      {config.Objects{FailureDomain: strings.Repeat("z", 64)}, "objects.failure_domain"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Objects = tc.objects
			err := c.Validate()
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %+v", tc.objects)
			}
			if !strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("refusal %q does not name %s", err, tc.refuse)
			}
		})
	}
}

// A DATA NODE MISSING THE COMPANY'S FAILURE-DOMAIN LABEL IS WARNED ABOUT, at
// the label it should carry. Neither tier can see it alone — the company names
// the key, the node carries the labels — and a node without it is placed as a
// domain of its own, which can put two copies in one real zone.
//
// A WARNING, NEVER A REFUSAL: a fleet half-way through labelling its nodes is
// a correct state, and a node that holds no data is placed on by nobody.
func TestADataNodeMissingTheFailureDomainLabelIsWarned(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain string
		labels map[string]string
		roles  []string
		warn   bool
	}{
		"a labelled data node":           {"zone", map[string]string{"zone": "a"}, nil, false},
		"an unlabelled data node":        {"zone", map[string]string{"rack": "r1"}, nil, true},
		"no failure domain at all":       {"", nil, nil, false},
		"an unlabelled stateless node":   {"zone", nil, []string{"seats"}, false},
		"a data node labelled otherwise": {"zone", nil, []string{"data"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = t.TempDir()
			b.Node.Labels = tc.labels
			b.Node.Roles = tc.roles
			c := config.DefaultCompany()
			c.Name = "Acme"
			c.Objects.FailureDomain = tc.domain

			if err := config.CheckTiers(&b, &c); err != nil {
				t.Fatalf("a missing label refused the pair: %v", err)
			}
			got := config.TierWarnings(&b, &c)
			if !tc.warn {
				if len(got) != 0 {
					t.Fatalf("warned: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d warnings, want the one: %+v", len(got), got)
			}
			if got[0].Path != "node.labels.zone" || got[0].Kind != config.WarningAdvisory {
				t.Errorf("warning at %q (%s), want an advisory at node.labels.zone",
					got[0].Path, got[0].Kind)
			}
			if !strings.Contains(got[0].Message, "objects.failure_domain") {
				t.Errorf("the warning does not say what asked for the label: %q",
					got[0].Message)
			}
		})
	}
}

// STREAM REPLICAS ARE BOUNDED BY WHAT THE BROKER CAN KEEP: JetStream's ceiling
// of five everywhere, and on an embedded cluster the members it names — past
// either, every stream and bucket the node provisions is refused at boot.
func TestStreamReplicasAreBoundedByWhatTheBrokerCanKeep(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		replicas int
		peers    []string
		refuse   string
	}{
		"three on three members": {3, []string{"nats://b:6222", "nats://c:6222"}, ""},
		"five on five members": {5, []string{"nats://b:6222", "nats://c:6222",
			"nats://d:6222", "nats://e:6222"}, ""},
		"three on a two-peer list": {3, []string{"nats://b:6222", "nats://c:6222"}, ""},
		"four on three members":    {4, []string{"nats://b:6222", "nats://c:6222"}, "names 3"},
		"six on six members": {6, []string{"nats://b:6222", "nats://c:6222",
			"nats://d:6222", "nats://e:6222", "nats://f:6222"}, "1..5"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := fleetBootstrap(t)
			b.Stream.Replicas = tc.replicas
			b.Stream.Cluster.Peers = tc.peers
			err := b.Validate()
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%d replicas on %d members accepted", tc.replicas, len(tc.peers)+1)
			}
			if !strings.Contains(err.Error(), "stream.replicas") ||
				!strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("refusal %q, want it to name stream.replicas and say %q", err, tc.refuse)
			}
		})
	}
}
