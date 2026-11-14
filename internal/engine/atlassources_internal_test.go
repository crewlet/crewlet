package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
)

// THE COLLECT DUTY READS EVERY REGISTERED VENDOR AND THE SERVERS: the SSH
// source is no vendor package's registration, since it runs its commands
// through this node's runner, so the engine adds it, once, beside the others.
//
// Mutation: hand the duty the registry alone, and no server that opts in is
// ever read for the atlas.
func TestTheCollectDutyReadsTheServersBesideEveryVendor(t *testing.T) {
	t.Parallel()
	var kinds []integration.Kind
	for _, s := range (&Engine{}).atlasSources() {
		kinds = append(kinds, s.Kind())
	}
	if n := len(kinds); n != len(collect.Registered())+1 || kinds[n-1] != integration.KindSSH {
		t.Fatalf("the duty reads %v, want every registered vendor and then SSH", kinds)
	}
	for _, s := range collect.Registered() {
		if s.Kind() == integration.KindSSH {
			t.Fatal("an SSH source is registered too, so the duty would refuse two of one kind")
		}
		if !slices.Contains(kinds, s.Kind()) {
			t.Errorf("the duty does not read %s", s.Kind())
		}
	}
}
