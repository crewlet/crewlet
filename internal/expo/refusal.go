package expo

import (
	"strings"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// VendorRefusal is Expo's definite answer as a refusal a person acts on, in
// Expo's own words: every 4xx but a rate limit. Nil where Expo did not serve.
func (e *APIError) VendorRefusal() *seataccess.Refusal {
	if e.Status < 400 || e.VendorUnavailable() {
		return nil
	}
	said := strings.TrimSuffix(strings.TrimSpace(e.Detail), ".")
	if said == "" {
		said = "no reason given"
	}
	return &seataccess.Refusal{Actor: seataccess.ByYou, Said: "Expo refused the call: " + said + ".",
		Remedy: "Do what Expo's answer asks on expo.dev, then check again."}
}
