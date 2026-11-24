package kv

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// THE ACCESS REVIEWS' REVISIONS CROSS A LINK AS HEADERS ALONE: a revision is
// all the read answers, so a review at its bound never crosses for it.
//
// Mutation: read the revisions with eachUnder, and the review crosses.
func TestTheAccessReviewsRevisionsCrossALinkAsHeadersAlone(t *testing.T) {
	t.Parallel()
	member, leaf, link := overASlowLeaf(t, 64<<10)
	random := make([]byte, coord.MaxAccessReviewBytes)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	value := []byte(`"` + base64.StdEncoding.EncodeToString(random)[:coord.MaxAccessReviewBytes-2] + `"`)
	if created, err := member.CreateAccessReview(t.Context(), "sre", value); err != nil || !created {
		t.Fatalf("create an access review at its bound = %v, %v", created, err)
	}
	before := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried
	revisions, err := leaf.AccessReviewRevisions(t.Context())
	if err != nil {
		t.Fatalf("the revisions across the link: %v", err)
	}
	if revisions["sre"] == 0 {
		t.Fatalf("revisions = %v, want sre's", revisions)
	}
	if carried := link.Count(server.DEFAULT_GLOBAL_ACCOUNT, linktest.ToNode).Carried - before; carried >= 16<<10 {
		t.Errorf("the revisions carried %d bytes to the leaf: a review's value crossed", carried)
	}
}
