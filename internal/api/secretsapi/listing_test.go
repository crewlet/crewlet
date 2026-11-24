package secretsapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A LISTING READS THE BUCKET ONCE: GET /secrets answers the operator's names
// and the engine's count from one pass, since each pass carries every sealed
// value across the node's link, and crewlet-operator's sync lists on every
// reconcile.
//
// Mutation: answer the listing from fleetsecrets' List and EngineKeys, and it
// passes twice.
func TestAListingReadsTheBucketOnce(t *testing.T) {
	t.Parallel()
	h, fleet := countedSurface(t)
	call(t, h, http.MethodPut, "/secrets/A", "first")
	if _, err := fleetsecrets.New(fleet.Fleet, cipherFor(t, "k1")).Estate().Create(t.Context(),
		"iam/blind-index-key", "key", secrets.Author{Name: "node-a", Kind: "system"}, secrets.SourceIAM,
		clock); err != nil {
		t.Fatal(err)
	}
	fleet.passes.Store(0)
	code, body := call(t, h, http.MethodGet, "/secrets", "")
	if code != http.StatusOK || !strings.Contains(body, `"A"`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("GET /secrets = %d %s, want A named and one engine key counted", code, body)
	}
	if n := fleet.passes.Load(); n != 1 {
		t.Errorf("GET /secrets passed over the bucket %d times, want once", n)
	}
}
