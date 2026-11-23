package metaads

import (
	"strings"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// VendorRefusal is Meta's definite answer as a refusal a person acts on, in
// the words Meta writes for a person where it gives them: every 4xx but a
// rate limit or an error Meta calls transient. Nil where Meta did not serve.
func (e *APIError) VendorRefusal() *seataccess.Refusal {
	if e.Status < 400 || e.VendorUnavailable() {
		return nil
	}
	said := strings.TrimSuffix(strings.TrimSpace(e.UserMessage), ".")
	if said == "" {
		said = strings.TrimSuffix(strings.TrimSpace(e.Message), ".")
	}
	if title := strings.TrimSuffix(strings.TrimSpace(e.UserTitle), "."); title != "" {
		said = strings.TrimSuffix(title+". "+said, ". ")
	}
	if said == "" {
		said = "no reason given"
	}
	return &seataccess.Refusal{Actor: seataccess.ByYou, Said: "Meta refused the call: " + said + ".",
		Remedy: "Do what Meta's answer asks in Meta Business Suite, then check again."}
}
