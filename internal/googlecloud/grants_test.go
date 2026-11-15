package googlecloud_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/googlecloud"
)

// A ROLE WRITES UNLESS EVERY PERMISSION ONLY READS, failing closed on a verb
// this package does not know, so a grant of it asks for a step-up.
func TestARoleWritesUnlessEveryPermissionReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		perms []string
		write bool
	}{
		{[]string{"pubsub.topics.get", "pubsub.topics.list", "resourcemanager.projects.getIamPolicy"}, false},
		{[]string{"compute.instances.aggregatedList", "logging.logEntries.list"}, false},
		{[]string{"pubsub.topics.get", "pubsub.topics.create"}, true},
		{[]string{"storage.objects.frobnicate"}, true},
	} {
		if got := googlecloud.RoleWrites(tc.perms); got != tc.write {
			t.Errorf("%v writes %v, want %v", tc.perms, got, tc.write)
		}
	}
}
