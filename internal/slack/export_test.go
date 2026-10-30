package slack

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// OpenThrough opens the vendor's session through httpClient, which is how a
// test reaches a fake workspace: Slack's API base is a constant.
func OpenThrough(ctx context.Context, in seataccess.OpenInput, httpClient *http.Client) (seataccess.Session, error) {
	s, err := open(ctx, in, httpClient)
	if err != nil {
		return nil, err
	}
	return s, nil
}
