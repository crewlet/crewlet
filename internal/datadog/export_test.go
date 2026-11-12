package datadog

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// OpenAccess opens the registered access vendor with a transport pointed at a
// fake region, which the production Open cannot be handed.
func OpenAccess(
	_ context.Context, vendor seataccess.Vendor, in seataccess.OpenInput, hc *http.Client,
) (seataccess.Session, error) {
	return vendor.(accessVendor).open(in, hc)
}

// SetWait replaces how a client waits out a rate limit, so a test can record
// the wait rather than spend it.
func SetWait(c *Client, wait func(context.Context, time.Duration) error) { c.limit.Wait = wait }
