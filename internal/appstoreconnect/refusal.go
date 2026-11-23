package appstoreconnect

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// Apple's error codes this package tells apart. Apple documents FORBIDDEN_ERROR
// ("Parsing the Error Response Code") and asks callers to match a code by its
// prefix; the agreement code is the one Apple answers a team with an unsigned
// agreement, and NOT_AUTHORIZED the one it answers a token it does not accept.
const (
	codeRoleDenied      = "FORBIDDEN_ERROR"
	codeAgreements      = "FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED"
	codeNotAuthorized   = "NOT_AUTHORIZED"
	appStoreConnectHome = "https://appstoreconnect.apple.com/"
)

// codeIs reports whether code is want or one of its more specific codes.
func codeIs(code, want string) bool {
	return code == want || strings.HasPrefix(code, want+".")
}

// RoleDenied reports Apple refusing the call because the key's role may not
// make it: a 403 FORBIDDEN_ERROR. Any other 403 (an agreement, a feature the
// team lacks) says nothing about the role.
func (e *APIError) RoleDenied() bool {
	return e.Status == http.StatusForbidden && codeIs(e.Code, codeRoleDenied)
}

// VendorRefusal is Apple's definite answer as a refusal a person acts on, in
// plain words: every 4xx but a rate limit. Nil where Apple failed to serve
// the call, which is [APIError.VendorUnavailable].
func (e *APIError) VendorRefusal() *seataccess.Refusal {
	if e.Status < 400 || e.VendorUnavailable() {
		return nil
	}
	switch {
	case codeIs(e.Code, codeAgreements):
		return &seataccess.Refusal{Actor: seataccess.ByYou, ActionURL: appStoreConnectHome,
			Said: "Apple refuses every API call for this team until its Account Holder accepts the latest agreement.",
			Remedy: "Sign in to App Store Connect as the Account Holder, accept the agreement shown on the home " +
				"page or under Business, then check again."}
	case e.Status == http.StatusUnauthorized:
		return &seataccess.Refusal{Actor: seataccess.ByYou, ActionURL: TeamKeysURL,
			Said: "Apple does not accept the key: it was revoked, or its Key ID or the team's Issuer ID is wrong.",
			Remedy: "In Users and Access, Integrations, Team Keys, check the key is active and its Key ID and " +
				"the Issuer ID match; make a new key if it was revoked."}
	case e.RoleDenied():
		return &seataccess.Refusal{Actor: seataccess.ByYou, ActionURL: TeamKeysURL,
			Said: "Apple refuses the call: the key's role does not allow it.",
			Remedy: "Make a key with a role that allows it in Users and Access, Integrations, Team Keys; Apple " +
				"cannot change a key's role."}
	}
	var words []string
	for _, w := range []string{e.Title, e.Detail} {
		if w = strings.TrimSuffix(strings.TrimSpace(w), "."); w != "" {
			words = append(words, w)
		}
	}
	said := "App Store Connect refused the call (" + strings.TrimSpace(fmt.Sprintf("HTTP %d %s", e.Status, e.Code)) + ")."
	if len(words) > 0 {
		said = "App Store Connect refused the call: " + strings.Join(words, ". ") + "."
	}
	return &seataccess.Refusal{Actor: seataccess.ByYou, ActionURL: appStoreConnectHome, Said: said,
		Remedy: "Do what Apple's answer asks in App Store Connect, then check again."}
}
