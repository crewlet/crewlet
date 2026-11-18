package builtin

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// everyGrant is the caller an INTERNAL case that is not about authority acts
// as: a copy of the external suite's, since the two test packages cannot share
// helpers and exporting a fixture from production code is a back door.
func everyGrant() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee1"),
		Kind: iam.KindSeat, Seat: "tester", Stage: iam.StageActive,
		Grants: iam.AllGrants,
	})
}
