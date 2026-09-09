package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// ONE FILE FOR BOTH ESTATES IS REFUSED, and it has to be refused by the
// config because it does not fail as an error anywhere else.
//
// A node keeps two databases and this process's claim on a path is refcounted,
// so pointing both at one file does not collide: it produces one database
// carrying two migration sequences, with the state log's appliers writing
// beside the audit log. It fails as DATA, months later, on a restore.
func TestTheTwoStoreEstatesCannotBeOneFile(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = "/var/lib/crewlet/company.db"
	b.Store.ReplicatedPath = "/var/lib/crewlet/company.db"

	err := b.Validate()
	if err == nil {
		t.Fatal("one file was accepted as both estates")
	}
	if !strings.Contains(err.Error(), "store.replicated_path") {
		t.Errorf("refusal = %q, want it to name the field an operator has to change", err)
	}
	if !strings.Contains(err.Error(), "same file") {
		t.Errorf("refusal = %q, want it to say what is wrong", err)
	}
}

// AND THE SHAPES A DEPLOYMENT USES ARE ACCEPTED: unset (beside the node's own
// file, which is what makes "back up the data directory" true) and a path of
// its own, for the operator putting the replicated estate on another disk.
func TestTheReplicatedPathAcceptsWhatADeploymentSets(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]string{
		"unset":           "",
		"beside it":       "/var/lib/crewlet/crewlet-replicated.db",
		"on another disk": "/mnt/fast/crewlet-replicated.db",
		"whitespace only": "   ",
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			b.Store.Path = "/var/lib/crewlet/company.db"
			b.Store.ReplicatedPath = path
			if err := b.Validate(); err != nil {
				t.Fatalf("replicated_path %q was refused: %v", path, err)
			}
		})
	}
}
