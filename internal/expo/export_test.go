package expo

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// OpenAccess opens the registered access vendor with a transport pointed at a
// fake Expo, which the production Open cannot be handed.
func OpenAccess(
	_ context.Context, vendor seataccess.Vendor, in seataccess.OpenInput, hc *http.Client,
) (seataccess.Session, error) {
	return vendor.(accessVendor).open(in, hc)
}
