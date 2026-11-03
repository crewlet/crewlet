package expo

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// OpenAccess opens the registered access vendor with a transport pointed at a
// fake Expo and a clock, which the production Open cannot be handed; a nil
// clock is the wall clock.
func OpenAccess(
	_ context.Context, vendor seataccess.Vendor, in seataccess.OpenInput, hc *http.Client, now func() time.Time,
) (seataccess.Session, error) {
	return vendor.(accessVendor).open(in, hc, now)
}
