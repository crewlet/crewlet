package googlegrowth

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// VendorRefusal is Google's definite answer as a refusal a person acts on, in
// Google's own words: a call Google refused or did not authorize. Nil for
// any other failure, which Google did not answer or Crewlet caused.
func (e *CallError) VendorRefusal() *seataccess.Refusal {
	api, ok := errors.AsType[*googleauth.APIError](e.Err)
	if !ok || (e.Failure != growth.FailureVendorRefused && e.Failure != growth.FailureUnauthorized) {
		return nil
	}
	said := strings.TrimSuffix(strings.TrimSpace(api.Message), ".")
	if said == "" {
		said = api.Code
	}
	if said == "" {
		said = "no reason given"
	}
	return &seataccess.Refusal{Actor: seataccess.ByYou, Said: "Google refused the call: " + said + ".",
		Remedy: "Do what Google's answer asks in the product's console, then check again."}
}
