package builtin

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// everyGrant is the caller an INTERNAL case that is not about authority acts
// as.
//
// ITS OWN COPY beside the one in the external suite, because the two test
// packages cannot see each other's helpers and the alternative — exporting a
// fixture from production code — is a back door with a test's name on it.
func everyGrant() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee1"),
		Kind: iam.KindSeat, Seat: "tester", Stage: iam.StageActive,
		Colleague: iam.ColleagueWrite, Grants: iam.AllGrants,
	})
}
